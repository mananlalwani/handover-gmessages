package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

// maxEventBytes keeps every emitted line comfortably under the daemon's
// 64 KiB inbound bound, leaving headroom for the envelope. Real
// libraries (86 threads measured 71 KiB) and full message windows
// otherwise break clients deterministically.
const maxEventBytes = 48 * 1024

func eventBytes(evt Event) int {
	raw, err := json.Marshal(evt)
	if err != nil {
		return maxEventBytes + 1
	}
	return len(raw)
}

// getConversation fetches one authoritative thread record.
func (s *Session) getConversation(convID string) (*gmproto.Conversation, error) {
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client == nil {
		return nil, fmt.Errorf("not connected")
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	return client.GetConversation(ctx, convID)
}

// mapConversation converts a thread with the session's identity set.
func (s *Session) mapConversation(conv *gmproto.Conversation) (Conversation, *convMeta, error) {
	s.mu.Lock()
	selfIDs := make(map[string]bool, len(s.selfIDs))
	for id := range s.selfIDs {
		selfIDs[id] = true
	}
	s.mu.Unlock()
	mapped, meta, err := mapConversation(conv, selfIDs)
	if err != nil {
		return Conversation{}, nil, err
	}
	for _, p := range mapped.Participants {
		if p.IsSelf {
			s.mu.Lock()
			s.selfIDs[p.LocalID] = true
			s.mu.Unlock()
		}
	}
	return mapped, meta, nil
}

// Sync re-emits authoritative state for one account. It is the
// catch-up primitive after (re)connects on either side.
func (s *Session) Sync() {
	s.fullSync("sync")
}

// FetchHistory pages one thread window for the daemon.
func (s *Session) FetchHistory(convID string, limit uint32, cursor *string) {
	if limit < 1 || limit > 100 {
		limit = messageWindow
	}
	s.emitWindow(convID, limit, cursor, false, true)
}

// SendResult reports acceptance for commands rejected before any relay
// contact (bad paths, unknown threads). Relayed commands report through
// their own result paths.
func (s *Session) SendResult(requestID string, ok bool, errMsg string) {
	s.result(requestID, ok, errMsg)
}

// fullSync re-emits authoritative state: the thread list (full,
// so the daemon reconciles), one window per thread, and thread-level
// unread flags. Failures are per-thread; one bad thread never aborts
// the sync.
func (s *Session) fullSync(reason string) bool {
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client == nil {
		return false
	}
	list := func() (*gmproto.ListConversationsResponse, error) {
		ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
		defer cancel()
		return client.ListConversations(ctx, &gmproto.ListConversationsRequest{
			Count:  conversationPage,
			Folder: gmproto.ListConversationsRequest_INBOX,
		})
	}
	resp, err := list()
	if err != nil {
		// Match libgm's recovery ladder: a phone that stopped answering
		// needs its push/active session re-armed before retrying RPCs.
		if activeErr := func() error {
			ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
			defer cancel()
			return client.SetActiveSession(ctx)
		}(); activeErr == nil {
			resp, err = list()
		}
	}
	if err != nil {
		s.log.Warn().Err(err).Msg("listing conversations failed")
		s.emit(Event{Type: "error", Account: s.account, Message: "conversation sync failed"})
		return false
	}
	// Google returns the inbox as a broad thread library, and ordering is
	// not stable across sync responses. Present the same useful ordering as
	// a messaging client: most recently active conversations first.
	sort.SliceStable(resp.Conversations, func(i, j int) bool {
		return resp.Conversations[i].GetLastMessageTimestamp() >
			resp.Conversations[j].GetLastMessageTimestamp()
	})
	var threads []Conversation
	type threadResult struct {
		index  int
		thread Conversation
		unread bool
		ok     bool
	}
	results := make([]threadResult, len(resp.GetConversations()))
	var wg sync.WaitGroup
	for i, conv := range resp.GetConversations() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// ListConversations already returns the authoritative conversation
			// record. Upstream maps that record directly; doing a GetConversation
			// RPC for every thread fans one sync into dozens of phone requests
			// and starves sends/history.
			mapped, meta, err := s.mapConversation(conv)
			if err != nil && len(conv.GetParticipants()) == 0 {
				// Older relay responses may omit participants from list rows.
				// Keep the compatibility fallback, but only for those rows.
				full, fetchErr := s.getConversation(conv.GetConversationID())
				if fetchErr != nil {
					s.log.Warn().Str("conversation", conv.GetConversationID()).Msg("skipping thread")
					return
				}
				mapped, meta, err = s.mapConversation(full)
			}
			if err != nil {
				s.log.Warn().Err(err).Msg("skipping thread")
				return
			}
			s.mu.Lock()
			s.metas[conv.GetConversationID()] = meta
			s.mu.Unlock()
			results[i] = threadResult{index: i, thread: mapped, unread: conv.GetUnread(), ok: true}
		}()
	}
	wg.Wait()
	for _, result := range results {
		if result.ok {
			threads = append(threads, result.thread)
		}
	}
	s.emitConversations(threads)
	// Do not fetch a message window for every thread during account sync.
	// A large library can contain hundreds of conversations, and issuing
	// hundreds of concurrent phone RPCs starves the relay and makes sends
	// time out. History is fetched explicitly by the history command; live
	// events continue to populate the current windows.
	unreads := map[string]bool{}
	for _, result := range results {
		if result.ok {
			unreads[result.thread.LocalID] = result.unread
		}
	}
	for _, thread := range threads {
		s.emit(Event{Type: "read", Account: s.account, Conversation: thread.LocalID,
			Unread: unreads[thread.LocalID]})
	}
	if err := s.store.SaveAuth(s.account, s.auth); err != nil {
		s.log.Warn().Err(err).Msg("persisting refreshed session failed")
	}
	s.log.Debug().Str("reason", reason).Int("threads", len(threads)).Msg("sync complete")
	return true
}

func (s *Session) threadUnread(convID string) bool {
	conv, err := s.getConversation(convID)
	if err != nil {
		return false
	}
	return conv.GetUnread()
}

// emitConversations sends the thread list in size-bounded chunks. The
// first chunk is authoritative so the daemon reconciles; later chunks
// merge. Small libraries keep the single full event; large ones converge
// to the same set with transient remove/re-add churn on re-syncs.
func (s *Session) emitConversations(threads []Conversation) {
	if len(threads) == 0 {
		s.emit(Event{Type: "conversations", Account: s.account,
			Conversations: []Conversation{}, Full: true})
		return
	}
	var chunks [][]Conversation
	var cur []Conversation
	for _, thread := range threads {
		trial := append(append([]Conversation{}, cur...), thread)
		if len(cur) > 0 && eventBytes(Event{Type: "conversations", Account: s.account,
			Conversations: trial, Full: true}) > maxEventBytes {
			chunks = append(chunks, cur)
			cur = nil
		}
		cur = append(cur, thread)
	}
	chunks = append(chunks, cur)
	for i, chunk := range chunks {
		s.emit(Event{Type: "conversations", Account: s.account,
			Conversations: chunk, Full: i == 0})
	}
}

// emitMessages sends one window in size-bounded chunks with the same
// first-authoritative-then-merge pattern. cursorNext rides the last
// chunk only.
func (s *Session) emitMessages(convID string, msgs []Message, full bool, cursorNext string) {
	if len(msgs) == 0 {
		s.emit(Event{Type: "messages", Account: s.account, Conversation: convID,
			Messages: []Message{}, Full: full, PageComplete: true})
		return
	}
	var chunks [][]Message
	var cur []Message
	for _, msg := range msgs {
		trial := append(append([]Message{}, cur...), msg)
		if len(cur) > 0 && eventBytes(Event{Type: "messages", Account: s.account,
			Conversation: convID, Messages: trial, Full: full}) > maxEventBytes {
			chunks = append(chunks, cur)
			cur = nil
		}
		cur = append(cur, msg)
	}
	chunks = append(chunks, cur)
	for i, chunk := range chunks {
		evt := Event{Type: "messages", Account: s.account, Conversation: convID,
			Messages: chunk, Full: full && i == 0}
		if i == len(chunks)-1 {
			evt.CursorNext = cursorNext
			evt.PageComplete = true
		}
		s.emit(evt)
	}
}

// Foreign cursors are an error, never guessed.
// parseCursor decodes an adapter-minted opaque cursor ("id:timestamp").
func parseCursor(cursor string) (id string, ts int64, err error) {
	id, raw, ok := strings.Cut(cursor, ":")
	if !ok || id == "" {
		return "", 0, fmt.Errorf("foreign cursor")
	}
	ts, err = strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("foreign cursor")
	}
	return id, ts, nil
}

func mintCursor(id string, ts int64) string {
	return id + ":" + strconv.FormatInt(ts, 10)
}

// emitWindow pages one thread window and emits it. full=false pages merge
// downstream; the daemon reconciles only full sync windows.
func (s *Session) emitWindow(convID string, limit uint32, cursor *string, full, download bool) {
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client == nil {
		s.emit(Event{Type: "error", Account: s.account, Message: "not connected"})
		return
	}
	var rpcCursor *gmproto.Cursor
	if cursor != nil {
		id, ts, err := parseCursor(*cursor)
		if err != nil {
			// The daemon's public history cursor is intentionally just the
			// oldest message ID. Recover the relay timestamp from our cache;
			// accepting the daemon cursor here avoids leaking relay details
			// across the IPC boundary.
			id = *cursor
			ts = s.cachedTS(convID, id)
			if id == "" || ts == 0 {
				s.emit(Event{Type: "error", Account: s.account, Message: "unknown history cursor"})
				return
			}
			ts /= 1000
		}
		rpcCursor = &gmproto.Cursor{LastItemID: id, LastItemTimestamp: ts}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	resp, err := client.FetchMessages(ctx, convID, int64(limit), rpcCursor)
	if err != nil {
		s.log.Warn().Str("conversation", convID).Msg("fetching history failed")
		s.emit(Event{Type: "error", Account: s.account, Message: "history fetch failed"})
		return
	}
	var out []Message
	for _, msg := range resp.GetMessages() {
		mapped, removed := s.mapMessage(convID, msg, download)
		if removed != "" {
			s.emit(Event{Type: "message_removed", Account: s.account,
				Conversation: convID, Message: removed})
			continue
		}
		if mapped != nil {
			out = append(out, *mapped)
		}
	}
	evtCursor := ""
	if relayCursor := resp.GetCursor(); relayCursor != nil && relayCursor.GetLastItemID() != "" {
		evtCursor = mintCursor(relayCursor.GetLastItemID(), relayCursor.GetLastItemTimestamp())
	} else if uint32(len(out)) == limit && len(out) > 0 {
		oldest := out[0]
		// libgm's cursor timestamp is milliseconds. Message timestamps in
		// the normalized/cache path are Google microseconds.
		ts := s.cachedTS(convID, oldest.LocalID)
		if ts > 0 {
			ts /= 1000
		}
		evtCursor = mintCursor(oldest.LocalID, ts)
	}
	s.emitMessages(convID, out, full, evtCursor)
}

// handleMessage processes one live relay message: tombstones are skipped,
// deletions become removals, content becomes upserts, and own-message
// statuses become lifecycle events.
func (s *Session) handleMessage(msg *gmproto.Message, isOld bool) {
	if msg == nil {
		return
	}
	convID := msg.GetConversationID()
	status := msg.GetMessageStatus().GetStatus()
	if isTombstone(status) {
		return
	}
	if isDeletion(status) {
		if msg.GetMessageID() != "" {
			s.emit(Event{Type: "message_removed", Account: s.account,
				Conversation: convID, Message: msg.GetMessageID()})
		}
		return
	}
	mapped, removed := s.mapMessage(convID, msg, true)
	if removed != "" {
		s.emit(Event{Type: "message_removed", Account: s.account,
			Conversation: convID, Message: removed})
		return
	}
	if mapped == nil {
		return
	}
	s.emit(Event{Type: "messages", Account: s.account, Conversation: convID,
		Messages: []Message{*mapped}})
	// Remote echoes are authoritative even when the relay's sender field
	// uses an alias not present in selfIDs. Correlate pending sends before
	// applying the self-sender heuristic so late send statuses are not lost.
	s.checkPending(msg)
	if s.isSelfSender(convID, msg) {
		if token, ok := mapStatus(status); ok {
			s.emit(Event{Type: "status", Account: s.account,
				Conversation: convID, Message: msg.GetMessageID(), Status: token})
		}
	}
}

// checkPending correlates relay echoes of our sends by transaction id.
func (s *Session) checkPending(msg *gmproto.Message) {
	if msg.GetTmpID() == "" {
		return
	}
	s.mu.Lock()
	pending, ok := s.pending[msg.GetTmpID()]
	if ok {
		delete(s.pending, msg.GetTmpID())
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	if token, ok := mapStatus(msg.GetMessageStatus().GetStatus()); ok {
		s.emit(Event{Type: "status", Account: s.account,
			Conversation: pending.convID, Message: msg.GetMessageID(), Status: token})
	}
}

func (s *Session) isSelfSender(convID string, msg *gmproto.Message) bool {
	sender := msg.GetParticipantID()
	if sender == "" {
		sender = msg.GetSenderParticipant().GetID().GetParticipantID()
	}
	if sender == selfSentinel {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.selfIDs[sender]
}

func (s *Session) cachedTS(convID, msgID string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, entry := range s.cache[convID] {
		if entry.id == msgID {
			return entry.ts
		}
	}
	return 0
}

func (s *Session) rememberMessage(convID string, msg *gmproto.Message, self bool) {
	entry := cachedMessage{id: msg.GetMessageID(), ts: msg.GetTimestamp(), self: self,
		status: msg.GetMessageStatus().GetStatus()}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := s.cache[convID]
	for i, existing := range entries {
		if existing.id == entry.id {
			entries[i] = entry
			return
		}
	}
	entries = append(entries, entry)
	for len(entries) > cachePerConv {
		entries = entries[1:]
	}
	s.cache[convID] = entries
}
