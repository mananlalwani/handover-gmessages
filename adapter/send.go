// Copyright (C) 2026 Handover contributors
// SPDX-License-Identifier: AGPL-3.0-only
package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

// clientMedia is the narrow relay surface the adapter drives. It matches
// libgm.Client so closely that no wrapper is needed in production; the
// interface exists so mapping and staging stay unit-testable.
type clientMedia interface {
	DownloadMedia(mediaID string, key []byte) (io.ReadCloser, error)
	GetFullSizeImage(ctx context.Context, messageID, actionMessageID string) (*gmproto.GetFullSizeImageResponse, error)
}

func timeoutCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), rpcTimeout)
}

func slowCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), slowTimeout)
}

// result reports acceptance for a request id. ok=true means the relay
// took the request, never that it was delivered or displayed.
func (s *Session) result(requestID string, ok bool, errMsg string) {
	evt := Event{Type: "command_result", RequestID: requestID, OK: ok}
	if !ok && errMsg != "" {
		evt.Error = errMsg
	}
	s.fire(evt)
}

func (s *Session) failure(requestID, msg string) {
	s.result(requestID, false, msg)
}

func (s *Session) relayFailure(convID, txn string) {
	s.fire(Event{Type: "status", Account: s.account, Conversation: convID,
		Message: txn, Status: "failed:transport"})
}

// Login starts Gaia pairing from a user-supplied credential bundle. The
// bundle is JSON: {"cookies": {"SID": "...", ...}}. Values never enter
// logs; only missing key names surface.
func (s *Session) Login(bundle []byte) {
	var parsed LoginBundle
	if err := json.Unmarshal(bundle, &parsed); err != nil || len(parsed.Cookies) == 0 {
		s.fire(Event{Type: "error", Account: s.account, Message: "login bundle rejected"})
		return
	}
	var missing []string
	for _, key := range requiredCookies {
		if parsed.Cookies[key] == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		s.fire(Event{Type: "error", Account: s.account,
			Message: "login bundle missing cookies: " + joinNames(missing)})
		return
	}
	auth := libgm.NewAuthData()
	auth.SetCookies(parsed.Cookies)
	// Persist only after pairing succeeds (see finishPairing). Saving
	// now would leave a partial session on disk that Restore treats
	// as a real session when pairing later fails.
	//
	// A new pairing era starts here: anything still running from the
	// previous login (pairing, syncs, queued events) stops applying.
	generation := s.bumpGeneration()
	s.mu.Lock()
	if s.pairCancel != nil {
		s.pairCancel()
		s.pairCancel = nil
	}
	// Never run two relay sessions for one account: a lingering
	// pairing-era poll makes the server invalidate the new session
	// (observed as an instant GaiaLoggedOut right after auth).
	if s.client != nil {
		s.client.Disconnect()
		s.client = nil
	}
	s.auth = auth
	s.buildClient(auth)
	client := s.client
	s.mu.Unlock()
	s.runEventLoop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	if err := client.FetchConfig(ctx); err != nil {
		cancel()
		s.fire(Event{Type: "error", Account: s.account, Message: "relay unreachable during pairing"})
		return
	}
	pairCtx, pairCancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.pairCancel = pairCancel
	s.mu.Unlock()
	emoji, ps, err := client.StartGaiaPairing(ctx, pairCtx)
	cancel()
	if err != nil {
		s.fire(Event{Type: "error", Account: s.account, Message: classifyPairError(err)})
		return
	}
	s.mu.Lock()
	s.ps = ps
	s.mu.Unlock()
	s.fire(Event{Type: "account", Account: s.account, Label: "Google Messages",
		Connected: true, Authenticated: false})
	// The prompt is an opaque verification string (the emoji to confirm
	// on the phone). Display it; it carries no secret.
	s.fire(Event{Type: "pairing", Account: s.account,
		Prompt: "Tap " + emoji + " in Google Messages on the phone to confirm linking"})
	go s.finishPairing(ps, pairCtx, client, auth, generation)
}

func (s *Session) finishPairing(ps *libgm.PairingSession, ctx context.Context, client *libgm.Client, auth *libgm.AuthData, generation uint64) {
	s.mu.Lock()
	stale := s.generation != generation || s.client != client || s.auth != auth || s.ps != ps
	s.mu.Unlock()
	if stale {
		// A newer login replaced this pairing while it waited on
		// the phone. Do not touch the new session.
		return
	}
	_, err := client.FinishGaiaPairing(ctx, ps)
	s.mu.Lock()
	// Reject a stale completion: a newer login replaced the client,
	// auth, or pairing while this one was waiting on the phone.
	// Touching the new session (disconnect, save) would corrupt it.
	if s.generation != generation || s.client != client || s.auth != auth || s.ps != ps {
		s.mu.Unlock()
		return
	}
	s.ps = nil
	if s.pairCancel != nil {
		s.pairCancel()
		s.pairCancel = nil
	}
	s.mu.Unlock()
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.fire(Event{Type: "error", Account: s.account, Message: classifyPairError(err)})
		return
	}
	// Tear down the pairing-era poll before connecting: two concurrent
	// polls for one identity get the session invalidated server-side
	// (observed as an instant GaiaLoggedOut right after auth).
	client.Disconnect()
	if err := s.saveAuthIfCurrent(generation); err != nil {
		s.fire(Event{Type: "error", Account: s.account, Message: "storing session failed"})
		return
	}
	s.runConnectLoop(context.Background())
}

func classifyPairError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, libgm.ErrIncorrectEmoji):
		return "pairing rejected: wrong confirmation on phone"
	case errors.Is(err, libgm.ErrPairingCancelled):
		return "pairing cancelled on phone"
	case errors.Is(err, libgm.ErrPairingTimeout),
		errors.Is(err, libgm.ErrPairingInitTimeout):
		return "pairing timed out"
	case errors.Is(err, libgm.ErrNoDevicesFound):
		return "pairing found no phone"
	case errors.Is(err, libgm.ErrNoCookies):
		return "pairing requires fresh cookies"
	default:
		return "pairing failed"
	}
}

func ptrOf[T any](v T) *T {
	return &v
}

func joinNames(names []string) string {
	out := ""
	for i, name := range names {
		if i > 0 {
			out += ", "
		}
		out += name
	}
	return out
}

// simPayload returns the relay SIM payload for a thread, nil-safe: a
// missing SIM entry must not block sending.
func (s *Session) simPayload(outgoingID string) *gmproto.SIMPayload {
	s.mu.Lock()
	defer s.mu.Unlock()
	sim := s.sims[outgoingID]
	if sim == nil {
		return nil
	}
	return sim.GetSIMData().GetSIMPayload()
}

// threadMeta refreshes and returns outbound metadata for a thread.
func (s *Session) threadMeta(convID string) (*convMeta, error) {
	// Sync stores the authoritative metadata needed for outbound requests.
	// Do not make a second phone round-trip on every send: on a sleeping or
	// busy phone that lookup can time out before SendMessage is attempted.
	s.mu.Lock()
	if meta := s.metas[convID]; meta != nil {
		copy := *meta
		s.mu.Unlock()
		return &copy, nil
	}
	s.mu.Unlock()

	conv, err := s.getConversation(convID)
	if err != nil {
		return nil, err
	}
	mapped, meta, err := s.mapConversation(conv)
	if err != nil {
		return nil, err
	}
	_ = mapped
	s.mu.Lock()
	s.metas[convID] = meta
	s.mu.Unlock()
	return meta, nil
}

// SendText validates and relays one text message.
func (s *Session) SendText(requestID, convID, text, replyTo string) {
	if len([]rune(text)) == 0 || len([]rune(text)) > MaxTextChars {
		s.failure(requestID, "text rejected")
		return
	}
	meta, err := s.threadMeta(convID)
	if err != nil {
		s.failure(requestID, "unknown conversation")
		return
	}
	txn := tmpID()
	req := &gmproto.SendMessageRequest{
		ConversationID: convID,
		MessagePayload: &gmproto.MessagePayload{
			TmpID:          txn,
			ConversationID: convID,
			ParticipantID:  meta.outgoingID,
			TmpID2:         txn,
			MessageInfo: []*gmproto.MessageInfo{{
				Data: &gmproto.MessageInfo_MessageContent{
					MessageContent: &gmproto.MessageContent{Content: text},
				},
			}},
		},
		SIMPayload: metaSIM(s, meta),
		TmpID:      txn,
		// Upstream only forces RCS when an explicit portal setting enables
		// it. Automatic send mode must not be treated as ForceRCS: doing so
		// strands fallback/self sends at "sending as RCS".
		ForceRCS: false,
	}
	if replyTo != "" {
		req.Reply = &gmproto.ReplyPayload{MessageID: replyTo}
	}
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client == nil {
		s.failure(requestID, "not connected")
		return
	}
	// Acceptance means the helper queued the relay operation. The phone's
	// eventual response is reported separately as a lifecycle status.
	s.mu.Lock()
	s.sweepPending()
	s.pending[txn] = pendingSend{requestID: requestID, convID: convID, at: time.Now()}
	s.mu.Unlock()
	s.result(requestID, true, "")
	s.fire(Event{Type: "status", Account: s.account,
		Conversation: convID, Message: txn, Status: "accepted"})
	ctx, cancel := slowCtx()
	defer cancel()
	if _, err := sendToRelay(ctx, client, req); err != nil {
		if errors.Is(err, libgm.ErrPhoneNotResponding) {
			// The upstream relay may have accepted the request even when
			// the phone did not return its echo. Keep pending so a late
			// remote echo can resolve the send.
			return
		}
		s.mu.Lock()
		delete(s.pending, txn)
		s.mu.Unlock()
		s.relayFailure(convID, txn)
		return
	}
}

func metaSIM(s *Session, meta *convMeta) *gmproto.SIMPayload {
	return s.simPayload(meta.outgoingID)
}

func classifySendError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, libgm.ErrPhoneNotResponding) {
		return "phone not responding"
	}
	return "send failed"
}

var sendRetryBackoff = []time.Duration{3 * time.Second, 8 * time.Second, 20 * time.Second}

func sendToRelay(ctx context.Context, client *libgm.Client, req *gmproto.SendMessageRequest) (*gmproto.SendMessageResponse, error) {
	resp, err := client.SendMessage(ctx, req)
	for attempt := 0; err == nil && resp.GetStatus() != gmproto.SendMessageResponse_SUCCESS && attempt < len(sendRetryBackoff); attempt++ {
		if resp.GetStatus() != gmproto.SendMessageResponse_FAILURE_2 && resp.GetStatus() != gmproto.SendMessageResponse_FAILURE_3 {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(sendRetryBackoff[attempt]):
		}
		resp, err = client.SendMessage(ctx, req)
	}
	if err != nil {
		return nil, err
	}
	if resp.GetStatus() != gmproto.SendMessageResponse_SUCCESS {
		return resp, fmt.Errorf("relay rejected send")
	}
	return resp, nil
}

// SendMedia relays one staged file with an optional caption.
func (s *Session) SendMedia(requestID, convID, path, caption string) {
	data, name, mime, err := readStagedUpload(path)
	if err != nil {
		s.failure(requestID, "unreadable file")
		return
	}
	meta, err := s.threadMeta(convID)
	if err != nil {
		s.failure(requestID, "unknown conversation")
		return
	}
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client == nil {
		s.failure(requestID, "not connected")
		return
	}
	uploaded, err := client.UploadMedia(data, name, mime)
	if err != nil {
		s.failure(requestID, "media upload failed")
		return
	}
	infos := []*gmproto.MessageInfo{{
		Data: &gmproto.MessageInfo_MediaContent{MediaContent: uploaded},
	}}
	// Upstream documents that RCS does not support captions reliably;
	// adding a second text part can leave the entire media send stuck.
	// Preserve captions only on the legacy path where libgm supports them.
	if caption != "" && caption != name && meta.convType != gmproto.ConversationType_RCS {
		infos = append(infos, &gmproto.MessageInfo{
			Data: &gmproto.MessageInfo_MessageContent{
				MessageContent: &gmproto.MessageContent{Content: caption},
			},
		})
	}
	txn := tmpID()
	req := &gmproto.SendMessageRequest{
		ConversationID: convID,
		MessagePayload: &gmproto.MessagePayload{
			TmpID:          txn,
			ConversationID: convID,
			ParticipantID:  meta.outgoingID,
			TmpID2:         txn,
			MessageInfo:    infos,
		},
		SIMPayload: metaSIM(s, meta),
		TmpID:      txn,
		ForceRCS:   false,
	}
	ctx, cancel := slowCtx()
	defer cancel()
	s.mu.Lock()
	s.sweepPending()
	s.pending[txn] = pendingSend{requestID: requestID, convID: convID, at: time.Now()}
	s.mu.Unlock()
	s.result(requestID, true, "")
	s.fire(Event{Type: "status", Account: s.account,
		Conversation: convID, Message: txn, Status: "accepted"})
	if _, err := sendToRelay(ctx, client, req); err != nil {
		if errors.Is(err, libgm.ErrPhoneNotResponding) {
			return
		}
		s.mu.Lock()
		delete(s.pending, txn)
		s.mu.Unlock()
		s.relayFailure(convID, txn)
		return
	}
}

// readStagedUpload reads a daemon-staged file with a hard cap. The path
// comes from the daemon over the local pipe; it is still validated as a
// sized regular file before reading.
func readStagedUpload(path string) ([]byte, string, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, "", "", fmt.Errorf("not a file")
	}
	if info.Size() > MaxStagedBytes {
		return nil, "", "", fmt.Errorf("too large")
	}
	data, err := io.ReadAll(io.LimitReader(f, MaxStagedBytes+1))
	if err != nil {
		return nil, "", "", err
	}
	if int64(len(data)) > MaxStagedBytes {
		return nil, "", "", fmt.Errorf("too large")
	}
	name := f.Name()
	if i := len(name) - 1; i >= 0 {
		for i >= 0 && name[i] != '/' {
			i--
		}
		name = name[i+1:]
	}
	return data, name, http.DetectContentType(data), nil
}

// React adds, switches, or removes a reaction. Current self-reaction
// state is read back from the relay window first, so repeats are
// idempotent no-ops and emoji changes use SWITCH instead of stacking.
func (s *Session) React(requestID, convID, msgID, emoji string, add bool) {
	if emoji == "" {
		s.failure(requestID, "reaction rejected")
		return
	}
	selfState, err := s.selfReactionState(convID, msgID, emoji)
	if err != nil {
		s.failure(requestID, "unknown message")
		return
	}
	action := gmproto.SendReactionRequest_ADD
	if !add {
		if !selfState.present {
			s.result(requestID, true, "")
			return
		}
		action = gmproto.SendReactionRequest_REMOVE
	} else if selfState.present {
		s.result(requestID, true, "")
		return
	} else if selfState.other {
		action = gmproto.SendReactionRequest_SWITCH
	}
	s.mu.Lock()
	client := s.client
	outgoing := ""
	if meta, ok := s.metas[convID]; ok {
		outgoing = meta.outgoingID
	}
	s.mu.Unlock()
	if client == nil {
		s.failure(requestID, "not connected")
		return
	}
	payload := &gmproto.SendReactionRequest{
		MessageID:    msgID,
		ReactionData: gmproto.MakeReactionData(emoji),
		Action:       action,
		SIMPayload:   s.simPayload(outgoing),
	}
	ctx, cancel := timeoutCtx()
	defer cancel()
	resp, err := client.SendReaction(ctx, payload)
	if err != nil || !resp.GetSuccess() {
		s.failure(requestID, "reaction failed")
		return
	}
	s.result(requestID, true, "")
}

type reactionSelfState struct {
	present bool
	other   bool
}

// selfReactionState reads the relay's current reaction roster for one
// message: whether self reacted with this emoji, and whether self
// reacted with any other emoji.
func (s *Session) selfReactionState(convID, msgID, emoji string) (reactionSelfState, error) {
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client == nil {
		return reactionSelfState{}, fmt.Errorf("not connected")
	}
	ctx, cancel := timeoutCtx()
	defer cancel()
	resp, err := client.FetchMessages(ctx, convID, cachePerConv, nil)
	if err != nil {
		return reactionSelfState{}, err
	}
	var state reactionSelfState
	found := false
	for _, msg := range resp.GetMessages() {
		if msg.GetMessageID() != msgID {
			continue
		}
		found = true
		for _, entry := range msg.GetReactions() {
			mine := false
			for _, reactor := range entry.GetParticipantIDs() {
				if s.isSelfID(reactor) {
					mine = true
					break
				}
			}
			if !mine {
				continue
			}
			if entry.GetData().GetUnicode() == emoji {
				state.present = true
			} else {
				state.other = true
			}
		}
	}
	if !found {
		return reactionSelfState{}, fmt.Errorf("unknown message")
	}
	return state, nil
}

func (s *Session) isSelfID(id string) bool {
	if id == "" || id == selfSentinel {
		return id == selfSentinel
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.selfIDs[id]
}

// MarkRead relays a read receipt and re-emits the attested thread state.
func (s *Session) MarkRead(convID, msgID string) {
	if msgID == "" {
		conv, err := s.getConversation(convID)
		if err != nil {
			s.fire(Event{Type: "error", Account: s.account, Message: "unknown conversation"})
			return
		}
		msgID = conv.GetLatestMessageID()
	}
	if msgID == "" {
		s.fire(Event{Type: "error", Account: s.account, Message: "nothing to mark read"})
		return
	}
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client == nil {
		s.fire(Event{Type: "error", Account: s.account, Message: "not connected"})
		return
	}
	ctx, cancel := timeoutCtx()
	defer cancel()
	if err := client.MarkRead(ctx, convID, msgID); err != nil {
		s.fire(Event{Type: "error", Account: s.account, Message: "mark read failed"})
		return
	}
	s.fire(Event{Type: "read", Account: s.account, Conversation: convID,
		LastReadMessage: msgID, Unread: false})
	if conv, err := s.getConversation(convID); err == nil {
		if mapped, meta, err := s.mapConversation(conv); err == nil {
			s.mu.Lock()
			s.metas[convID] = meta
			s.mu.Unlock()
			s.fire(Event{Type: "conversations", Account: s.account,
				Conversations: []Conversation{mapped}})
		}
	}
}

// Typing relays a typing-start ping. There is no typing-stop upstream,
// so none is representable here.
func (s *Session) Typing(convID string) {
	s.mu.Lock()
	client := s.client
	outgoing := ""
	if meta, ok := s.metas[convID]; ok {
		outgoing = meta.outgoingID
	}
	s.mu.Unlock()
	if client == nil {
		s.fire(Event{Type: "error", Account: s.account, Message: "not connected"})
		return
	}
	ctx, cancel := timeoutCtx()
	defer cancel()
	if err := client.SetTyping(ctx, convID, s.simPayload(outgoing)); err != nil {
		s.fire(Event{Type: "error", Account: s.account, Message: "typing ping failed"})
	}
}

// DeleteMessage relays an own-device deletion. Removal itself is only
// reported when the relay echoes it back.
func (s *Session) DeleteMessage(requestID, msgID string) {
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client == nil {
		s.failure(requestID, "not connected")
		return
	}
	ctx, cancel := timeoutCtx()
	defer cancel()
	if _, err := client.DeleteMessage(ctx, msgID); err != nil {
		s.failure(requestID, "delete failed")
		return
	}
	s.result(requestID, true, "")
}

// Open resolves or creates a thread for addresses. Groups are created
// without a name because the Handover contract carries none; the relay
// may reject nameless groups, which surfaces as a failure, not a guess.
func (s *Session) Open(requestID string, addresses []string) {
	if len(addresses) == 0 {
		s.failure(requestID, "no addresses")
		return
	}
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client == nil {
		s.failure(requestID, "not connected")
		return
	}
	req := &gmproto.GetOrCreateConversationRequest{}
	for _, address := range addresses {
		req.Numbers = append(req.Numbers, &gmproto.ContactNumber{
			MysteriousInt: 2,
			Number:        address,
			Number2:       address,
		})
	}
	ctx, cancel := slowCtx()
	defer cancel()
	start := time.Now()
	resp, err := client.GetOrCreateConversation(ctx, req)
	s.log.Info().Str("stage", "open-response").Dur("elapsed", time.Since(start)).Msg("relay answered")
	if err != nil {
		s.failure(requestID, "open failed")
		return
	}
	// Group creation is a two-step dance upstream: a CREATE_RCS status
	// asks for an explicit retry with the create flag set. The group
	// name stays empty because the Handover contract carries none.
	if len(addresses) > 1 &&
		resp.GetStatus() == gmproto.GetOrCreateConversationResponse_CREATE_RCS {
		req.RCSGroupName = ptrOf("")
		req.CreateRCSGroup = ptrOf(true)
		resp, err = client.GetOrCreateConversation(ctx, req)
		if err != nil {
			s.failure(requestID, "open failed")
			return
		}
	}
	if resp.GetConversation().GetConversationID() == "" {
		s.failure(requestID, "open failed")
		return
	}
	s.result(requestID, true, "")
	conv, err := s.getConversation(resp.GetConversation().GetConversationID())
	if err != nil {
		return
	}
	if mapped, meta, err := s.mapConversation(conv); err == nil {
		s.mu.Lock()
		s.metas[conv.GetConversationID()] = meta
		s.mu.Unlock()
		s.fire(Event{Type: "conversations", Account: s.account,
			Conversations: []Conversation{mapped}})
		s.emitWindow(conv.GetConversationID(), messageWindow, nil, false, true)
	}
}

// Logout revokes remotely, then deletes local state only after the
// revoke succeeds. A failed revoke keeps the session and reports it:
// access may still exist server-side, and saying otherwise would lie.
// It reports whether the session was actually torn down so the caller
// only forgets the session on success.
func (s *Session) Logout() bool {
	s.mu.Lock()
	if s.pairCancel != nil {
		s.pairCancel()
		s.pairCancel = nil
	}
	client := s.client
	s.mu.Unlock()
	if client != nil {
		ctx, cancel := timeoutCtx()
		defer cancel()
		if err := client.Unpair(ctx); err != nil {
			s.fire(Event{Type: "error", Account: s.account,
				Message: "remote revoke failed, session kept"})
			return false
		}
		client.Disconnect()
	}
	if err := s.store.DeleteAuth(s.account); err != nil {
		// The remote side is already revoked and the client is
		// disconnected, so tear down the live state and say so: the
		// daemon must see this account go offline, not stay
		// authenticated. Only the on-disk deletion is left to heal
		// (the next save overwrites it).
		s.teardown()
		s.fire(Event{Type: "account", Account: s.account, Label: "Google Messages",
			Connected: false, Authenticated: false})
		s.fire(Event{Type: "error", Account: s.account, Message: "deleting session failed"})
		return false
	}
	s.teardown()
	s.fire(Event{Type: "account_removed", Account: s.account})
	return true
}

// teardown drops the live relay state without touching storage.
// It retires the session generation so stale syncs, pairings, and
// queued events stop applying.
func (s *Session) teardown() {
	s.mu.Lock()
	s.client = nil
	s.auth = nil
	s.connected = false
	s.authenticated = false
	s.generation++
	if s.generation == 0 {
		s.generation = 1
	}
	s.mu.Unlock()
}

// Close stops background loops. It does not delete stored sessions:
// restarts recover from disk.
func (s *Session) Close() {
	s.closeOnce.Do(func() { close(s.closed) })
	s.mu.Lock()
	if s.pairCancel != nil {
		s.pairCancel()
		s.pairCancel = nil
	}
	if s.connectCancel != nil {
		s.connectCancel()
		s.connectCancel = nil
	}
	client := s.client
	s.mu.Unlock()
	if client != nil {
		client.Disconnect()
	}
}
