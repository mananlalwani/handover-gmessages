// Copyright (C) 2026 Handover contributors
// SPDX-License-Identifier: AGPL-3.0-only
package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
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
// catch-up primitive after (re)connects on either side. Concurrent
// syncs collapse into the running one.
func (s *Session) Sync() {
	if !s.syncActive.CompareAndSwap(false, true) {
		return
	}
	defer s.syncActive.Store(false)
	s.fullSync("sync")
}

// FetchHistory pages one thread window for the daemon.
func (s *Session) FetchHistory(convID string, limit uint32, cursor *string, fetchID uint64) {
	if limit < 1 || limit > 100 {
		limit = messageWindow
	}
	s.emitWindow(convID, limit, cursor, false, true, fetchID)
}

// SendResult reports acceptance for commands rejected before any relay
// contact (bad paths, unknown threads). Relayed commands report through
// their own result paths.
func (s *Session) SendResult(requestID string, ok bool, errMsg string) {
	s.result(requestID, ok, errMsg)
}

// listPage fetches one conversation page after the given cursor.
func (s *Session) listPage(client *libgm.Client, cursor *gmproto.Cursor) (*gmproto.ListConversationsResponse, error) {
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	return client.ListConversations(ctx, &gmproto.ListConversationsRequest{
		Count:  conversationPage,
		Folder: gmproto.ListConversationsRequest_INBOX,
		Cursor: cursor,
	})
}

// maxSyncPages bounds one sync so a pathological library cannot page
// forever. The cap is logged when hit; the next sync resumes from the
// first page, so capped threads converge over successive syncs.
const maxSyncPages = 10

// recentWindowRefresh bounds post-sync window healing: the newest
// threads get a full window re-emit so dropped messages and deletions
// converge without fanning hundreds of RPCs at the relay.
const recentWindowRefresh = 10

// listAllConversations pages the whole inbox. A single page is never
// treated as authoritative for a large library: accounts with more
// threads than one page would otherwise lose older conversations from
// normalized state on every sync.
// listAllConversations pages the whole inbox, aborting with an
// error when the session retires mid-listing so a stale page set
// never becomes authoritative.
func (s *Session) listAllConversations(client *libgm.Client, lifecycle uint64) ([]*gmproto.Conversation, bool, error) {
	var all []*gmproto.Conversation
	seen := map[string]bool{}
	var cursor *gmproto.Cursor
	for page := 0; page < maxSyncPages; page++ {
		if !s.current(lifecycle) {
			return nil, false, fmt.Errorf("session retired during listing")
		}
		resp, err := s.listPage(client, cursor)
		if err != nil {
			// Match libgm's recovery ladder: a phone that stopped
			// answering needs its push/active session re-armed before
			// retrying RPCs. Only the first page retries; later pages
			// fail the sync instead of mixing stale and fresh windows.
			if page > 0 {
				return nil, false, err
			}
			if activeErr := func() error {
				ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
				defer cancel()
				return client.SetActiveSession(ctx)
			}(); activeErr == nil {
				resp, err = s.listPage(client, cursor)
			}
			if err != nil {
				return nil, false, err
			}
		}
		convs := resp.GetConversations()
		for _, conv := range convs {
			id := conv.GetConversationID()
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			all = append(all, conv)
		}
		next := resp.GetCursor()
		if len(convs) < conversationPage || next == nil || next.GetLastItemID() == "" {
			return all, false, nil
		}
		cursor = next
	}
	s.log.Warn().Int("threads", len(all)).Msg("conversation sync hit the page cap")
	return all, true, nil
}

// fullSync re-emits authoritative state: the paged thread list (the
// closing generation is full, so the daemon reconciles once), and
// thread-level unread flags. Failures are per-thread; one bad thread
// never aborts the sync.
func (s *Session) fullSync(reason string) bool {
	s.syncMu.Lock()
	threads, results, unreads, lifecycle, complete := s.fullSyncLocked(reason)
	s.syncMu.Unlock()
	if threads == nil && results == nil {
		return false
	}
	// Window healing runs unlocked (see fullSyncLocked): slow phones
	// must not stall other syncs behind sequential window RPCs.
	refreshed := 0
	for _, result := range results {
		if !result.ok || refreshed >= recentWindowRefresh {
			continue
		}
		if !s.current(lifecycle) {
			return false
		}
		s.emitWindow(result.thread.LocalID, messageWindow, nil, true, true, 0, lifecycle)
		refreshed++
	}
	for _, thread := range threads {
		if !s.current(lifecycle) {
			return false
		}
		s.fireIfCurrent(lifecycle, Event{Type: "read", Account: s.account, Conversation: thread.LocalID,
			Unread: unreads[thread.LocalID]})
	}
	if !s.current(lifecycle) {
		return false
	}
	if err := s.saveAuthIfCurrent(lifecycle); err != nil {
		s.log.Warn().Err(err).Msg("persisting refreshed session failed")
	}
	s.log.Debug().Str("reason", reason).Int("threads", len(threads)).Msg("sync complete")
	return complete || len(threads) > 0
}

type threadResult struct {
	index  int
	thread Conversation
	unread bool
	ok     bool
}

// fullSyncLocked performs the serialized half of a sync: listing,
// mapping, and conversation emission. Window healing runs after the
// caller releases syncMu so one slow phone cannot stall every other
// sync behind up to ten sequential window RPCs.
func (s *Session) fullSyncLocked(reason string) (threads []Conversation, results []threadResult, unreads map[string]bool, lifecycle uint64, complete bool) {
	if !s.alive() {
		return nil, nil, nil, 0, false
	}
	lifecycle = s.currentLifecycle()
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client == nil {
		return nil, nil, nil, 0, false
	}
	convs, capped, err := s.listAllConversations(client, lifecycle)
	if err != nil {
		s.log.Warn().Err(err).Msg("listing conversations failed")
		s.fire(Event{Type: "error", Account: s.account, Message: "conversation sync failed"})
		return nil, nil, nil, 0, false
	}
	// Google returns the inbox as a broad thread library, and ordering is
	// not stable across sync responses. Present the same useful ordering as
	// a messaging client: most recently active conversations first.
	sort.SliceStable(convs, func(i, j int) bool {
		return convs[i].GetLastMessageTimestamp() >
			convs[j].GetLastMessageTimestamp()
	})
	threads = nil
	results = make([]threadResult, len(convs))
	// Fixed worker pool: one goroutine per thread would park up to
	// two thousand goroutines on the semaphore for a large library.
	// Eight workers pull indices; the pool also bounds the fallback
	// phone round-trips that starve the relay.
	var skipped atomic.Int32
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range syncWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				conv := convs[i]
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
						skipped.Add(1)
						continue
					}
					mapped, meta, err = s.mapConversation(full)
				}
				if err != nil {
					s.log.Warn().Err(err).Msg("skipping thread")
					skipped.Add(1)
					continue
				}
				if !s.current(lifecycle) {
					skipped.Add(1)
					continue
				}
				s.mu.Lock()
				s.metas[conv.GetConversationID()] = meta
				s.mu.Unlock()
				results[i] = threadResult{index: i, thread: mapped, unread: conv.GetUnread(), ok: true}
			}
		}()
	}
	for i := range convs {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	for _, result := range results {
		if result.ok {
			threads = append(threads, result.thread)
		}
	}
	if !s.current(lifecycle) {
		// A relogin retired this sync while it worked: drop the
		// whole result instead of publishing old-account state
		// into the new login.
		return nil, nil, nil, lifecycle, false
	}
	// Only a demonstrably complete listing may close as authoritative.
	// A capped page or a skipped thread means threads are missing:
	// merging without reconcile keeps them instead of deleting live
	// state the daemon still holds.
	complete = !capped && skipped.Load() == 0
	if !complete {
		s.log.Warn().Bool("capped", capped).Int32("skipped", skipped.Load()).Msg("sync incomplete; merging without reconcile")
	}
	s.emitConversations(threads, s.nextGeneration(), complete)
	// Do not fetch message windows under the sync lock (see
	// fullSync): window healing continues after unlock. History is
	// fetched explicitly by the history command; live events
	// continue to populate the current windows.
	unreads = map[string]bool{}
	for _, result := range results {
		if result.ok {
			unreads[result.thread.LocalID] = result.unread
		}
	}
	return threads, results, unreads, lifecycle, complete
}

func (s *Session) threadUnread(convID string) bool {
	conv, err := s.getConversation(convID)
	if err != nil {
		return false
	}
	return conv.GetUnread()
}

// nextGeneration mints a chunk-group id for one multi-chunk sync.
// Generations start at 1; zero on the wire means ungrouped. This
// counter is independent of the login lifecycle: minting emission
// ids must never retire queued events or fence saves.
func (s *Session) nextGeneration() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.syncGen++
	if s.syncGen == 0 {
		s.syncGen = 1
	}
	return s.syncGen
}

// emitConversations sends the thread list in size-bounded chunks that
// share one generation. Only the closing chunk of a complete sync is
// authoritative, so the daemon reconciles once against the whole list
// instead of removing and re-adding threads that arrive in later
// chunks. A list that fits in one chunk keeps the single full event.
// An incomplete sync (page cap or skipped threads) merges everything:
// its threads are a subset, and reconciling a subset would delete
// live state.
func (s *Session) emitConversations(threads []Conversation, generation uint64, authoritative bool) {
	if len(threads) == 0 {
		s.fire(Event{Type: "conversations", Account: s.account,
			Conversations: []Conversation{}, Full: authoritative})
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
		last := i == len(chunks)-1
		evt := Event{Type: "conversations", Account: s.account,
			Conversations: chunk, Full: authoritative && last}
		if authoritative && len(chunks) > 1 {
			evt.Generation = generation
		}
		s.fire(evt)
	}
}

// emitMessages sends one window in size-bounded chunks. The closing
// chunk carries the authority flag and the cursor, so multi-chunk
// windows reconcile once instead of dropping later chunks' messages.
func (s *Session) emitMessages(convID string, msgs []Message, full bool, cursorNext string, fetchID uint64) {
	if len(msgs) == 0 {
		s.fire(Event{Type: "messages", Account: s.account, Conversation: convID,
			Messages: []Message{}, Full: full, PageComplete: true, FetchID: fetchID})
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
	var generation uint64
	if full && len(chunks) > 1 {
		generation = s.nextGeneration()
	}
	for i, chunk := range chunks {
		last := i == len(chunks)-1
		evt := Event{Type: "messages", Account: s.account, Conversation: convID,
			Messages: chunk, Full: full && last, Generation: generation, FetchID: fetchID}
		if last {
			evt.CursorNext = cursorNext
			evt.PageComplete = true
		}
		s.fire(evt)
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
func (s *Session) emitWindow(convID string, limit uint32, cursor *string, full, download bool, fetchID uint64, expectedLifecycle ...uint64) {
	s.mu.Lock()
	client := s.client
	lifecycle := s.lifecycle
	if len(expectedLifecycle) > 0 {
		lifecycle = expectedLifecycle[0]
	}
	s.mu.Unlock()
	if client == nil {
		s.fireIfCurrent(lifecycle, Event{Type: "error", Account: s.account, Message: "not connected"})
		return
	}
	if !s.ownsClient(lifecycle, client) {
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
				s.fireIfCurrent(lifecycle, Event{Type: "error", Account: s.account, Message: "unknown history cursor"})
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
		s.fireIfCurrent(lifecycle, Event{Type: "error", Account: s.account, Message: "history fetch failed"})
		return
	}
	if !s.ownsClient(lifecycle, client) {
		return
	}
	var out []Message
	for _, msg := range resp.GetMessages() {
		mapped, removed := s.mapMessage(convID, msg, download, lifecycle)
		if removed != "" {
			s.fireIfCurrent(lifecycle, Event{Type: "message_removed", Account: s.account,
				Conversation: convID, Message: removed})
			continue
		}
		if mapped != nil {
			out = append(out, *mapped)
		}
	}
	if !s.current(lifecycle) {
		return
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
	if !s.current(lifecycle) {
		return
	}
	s.emitMessages(convID, out, full, evtCursor, fetchID)
}

// handleMessage processes one live relay message: tombstones are skipped,
// deletions become removals, content becomes upserts, and own-message
// statuses become lifecycle events.
func (s *Session) handleMessage(msg *gmproto.Message, isOld bool, lifecycle uint64) {
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
			s.fireIfCurrent(lifecycle, Event{Type: "message_removed", Account: s.account,
				Conversation: convID, Message: msg.GetMessageID()})
		}
		return
	}
	mapped, removed := s.mapMessage(convID, msg, true, lifecycle)
	if removed != "" {
		s.fireIfCurrent(lifecycle, Event{Type: "message_removed", Account: s.account,
			Conversation: convID, Message: removed})
		return
	}
	if mapped == nil {
		return
	}
	if !s.current(lifecycle) {
		return
	}
	s.fireIfCurrent(lifecycle, Event{Type: "messages", Account: s.account, Conversation: convID,
		Messages: []Message{*mapped}})
	// Remote echoes are authoritative even when the relay's sender field
	// uses an alias not present in selfIDs. Correlate pending sends before
	// applying the self-sender heuristic so late send statuses are not lost.
	s.checkPending(msg, lifecycle)
	if s.isSelfSender(convID, msg) {
		if token, ok := mapStatus(status); ok {
			s.fireIfCurrent(lifecycle, Event{Type: "status", Account: s.account,
				Conversation: convID, Message: msg.GetMessageID(), Status: token})
		}
	}
}

// checkPending correlates relay echoes of our sends by transaction id.
// The lifecycle gate keeps a stale echo from consuming a newer
// session's pending entry.
func (s *Session) checkPending(msg *gmproto.Message, lifecycle uint64) {
	if msg.GetTmpID() == "" {
		return
	}
	if !s.current(lifecycle) {
		return
	}
	s.mu.Lock()
	pending, ok := s.pending[msg.GetTmpID()]
	if ok {
		if time.Since(pending.at) > pendingTTL {
			// The relay answered after the correlation expired.
			// The send already timed out from the daemon's view;
			// fall through to the self-sender path instead of
			// attributing a stale request.
			delete(s.pending, msg.GetTmpID())
			ok = false
		} else {
			delete(s.pending, msg.GetTmpID())
		}
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	if token, ok := mapStatus(msg.GetMessageStatus().GetStatus()); ok {
		s.fire(Event{Type: "status", Account: s.account,
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
	sweepCaches(s.cache)
}

// maxCachedConvs bounds the message cache across conversations. Window
// contents stay exact; only the number of retained conversation windows
// is capped. Eviction picks an arbitrary window, which is safe because
// the cache only backs cursor recovery and dedupe, never authority.
const maxCachedConvs = 1024

func sweepCaches(cache map[string][]cachedMessage) {
	for len(cache) > maxCachedConvs {
		for convID := range cache {
			delete(cache, convID)
			break
		}
	}
}
