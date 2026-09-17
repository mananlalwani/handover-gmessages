package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
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
	s.emitWindow(convID, limit, cursor, false)
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
func (s *Session) fullSync(reason string) {
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	resp, err := client.ListConversations(ctx, &gmproto.ListConversationsRequest{
		Count:  conversationPage,
		Folder: gmproto.ListConversationsRequest_INBOX,
	})
	if err != nil {
		s.log.Warn().Err(err).Msg("listing conversations failed")
		s.emit(Event{Type: "error", Account: s.account, Message: "conversation sync failed"})
		return
	}
	var threads []Conversation
	for _, conv := range resp.GetConversations() {
		full, err := s.getConversation(conv.GetConversationID())
		if err != nil {
			s.log.Warn().Str("conversation", conv.GetConversationID()).Msg("skipping thread")
			continue
		}
		mapped, meta, err := s.mapConversation(full)
		if err != nil {
			s.log.Warn().Err(err).Msg("skipping thread")
			continue
		}
		s.mu.Lock()
		s.metas[full.GetConversationID()] = meta
		s.mu.Unlock()
		threads = append(threads, mapped)
	}
	s.emitConversations(threads)
	for _, thread := range threads {
		s.emitWindow(thread.LocalID, messageWindow, nil, true)
		s.emit(Event{Type: "read", Account: s.account, Conversation: thread.LocalID,
			Unread: s.threadUnread(thread.LocalID)})
	}
	if err := s.store.SaveAuth(s.account, s.auth); err != nil {
		s.log.Warn().Err(err).Msg("persisting refreshed session failed")
	}
	s.log.Debug().Str("reason", reason).Int("threads", len(threads)).Msg("sync complete")
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
			Messages: []Message{}, Full: full})
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
func (s *Session) emitWindow(convID string, limit uint32, cursor *string, full bool) {
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
			s.emit(Event{Type: "error", Account: s.account, Message: "unknown history cursor"})
			return
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
		mapped, removed := s.mapMessage(convID, msg)
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
	if uint32(len(out)) == limit && len(out) > 0 {
		oldest := out[0]
		evtCursor = mintCursor(oldest.LocalID, s.cachedTS(convID, oldest.LocalID))
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
	mapped, removed := s.mapMessage(convID, msg)
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
	if s.isSelfSender(convID, msg) {
		if token, ok := mapStatus(status); ok {
			s.emit(Event{Type: "status", Account: s.account,
				Conversation: convID, Message: msg.GetMessageID(), Status: token})
		}
		s.checkPending(msg)
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
