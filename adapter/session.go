// Copyright (C) 2026 Handover contributors
// SPDX-License-Identifier: AGPL-3.0-only
package adapter

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/events"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/util"
	exhttp "go.mau.fi/util/exhttp"
)

// rpcTimeout bounds routine relay RPCs. Slow phone operations (sends,
// opens) use slowTimeout: the daemon reports its own 30s wait first,
// but late relay answers still land as state events rather than being
// abandoned.
const rpcTimeout = 60 * time.Second
const slowTimeout = 3 * time.Minute
const resumeCheckInterval = 30 * time.Second
const resumeGapThreshold = 2 * time.Minute

// syncPage bounds conversation listing and per-thread windows.
const (
	conversationPage = 200
	messageWindow    = 20
	cachePerConv     = 200
	eventQueue       = 256
)

// syncWorkers caps concurrent per-thread mapping during a sync. See
// fullSync: the semaphore covers the fallback phone round-trips.
const syncWorkers = 8

// Session owns one Google account's relay state: the libgm client, SIM
// and thread metadata, message caches, and pending-send correlation.
// Google types never leave this file's boundary except as mapped wire
// records.
type Session struct {
	account string
	store   *Store
	log     zerolog.Logger
	emit    func(Event)

	mu         sync.Mutex
	syncMu     sync.Mutex
	client     *libgm.Client
	clientGen  uint64
	auth       *libgm.AuthData
	sims       map[string]*gmproto.SIMCard
	metas      map[string]*convMeta
	selfIDs    map[string]bool
	cache      map[string][]cachedMessage
	pending    map[string]pendingSend
	pairCancel context.CancelFunc
	ps         *libgm.PairingSession
	fullReq    map[string]time.Time
	// dirtyConvs holds threads whose relay events were dropped,
	// awaiting explicit window refresh. See markDirty.
	dirtyConvs map[string]struct{}

	events    chan queuedEvent
	closed    chan struct{}
	closeOnce sync.Once

	// loopOnce guards the relay event consumer: login, restore, and
	// reconnect all funnel through one goroutine so a second login
	// cannot start a second consumer over the same queue.
	loopOnce sync.Once
	// connectCancel stops the previous reconnect loop before a new
	// one starts, so re-login never leaves two loops racing.
	connectCancel context.CancelFunc
	// generation is a monotonic session fence with two jobs. It
	// groups multi-chunk sync emissions (see sync.go) and it retires
	// one login session against the next: login, logout, and close
	// bump it, while persistence, pairing completion, and queued
	// relay events carry the generation they started with and are
	// dropped when it no longer matches. Without this, a stale sync
	// could recreate a session file logout deleted, an old pairing
	// could persist a newer login's auth, or backlogged events could
	// mutate the new session's state.
	generation uint64
	// resyncing serializes queue-overflow recovery: one catch-up
	// runs at a time no matter how many overflows fire.
	resyncing atomic.Bool

	connected     bool
	authenticated bool
}

// queuedEvent pairs a relay event with the session generation that
// enqueued it. The consumer rechecks at dequeue: backlogged events
// from an old login must not mutate the new session's state.
type queuedEvent struct {
	generation uint64
	event      any
}

type cachedMessage struct {
	id     string
	ts     int64
	status gmproto.MessageStatusType
	self   bool
}

type pendingSend struct {
	requestID string
	convID    string
	// at bounds the entry's life: a send the relay never echoes must
	// not pin correlation state for the life of the process.
	at time.Time
}

// pendingTTL covers the slowest relay round-trip (slowTimeout) with
// margin. Older entries are dropped by the sweep below.
const pendingTTL = 10 * time.Minute

// sweepPending drops correlation state the relay never resolved.
func (s *Session) sweepPending() {
	cutoff := time.Now().Add(-pendingTTL)
	for txn, send := range s.pending {
		if send.at.Before(cutoff) {
			delete(s.pending, txn)
		}
	}
}

// requiredCookies mirrors the upstream Gaia login documentation. Values
// are never logged; only missing key names surface in errors.
var requiredCookies = []string{"SID", "HSID", "SSID", "OSID", "APISID", "SAPISID"}

// LoginBundle is the adapter-defined credential envelope carried inside
// the Handover login bundle_b64 field.
type LoginBundle struct {
	Cookies map[string]string `json:"cookies"`
}

func NewSession(account string, store *Store, log zerolog.Logger, emit func(Event)) *Session {
	session := &Session{
		account: account,
		store:   store,
		log:     log.With().Str("account", account).Logger(),
		emit:    emit,
		sims:    map[string]*gmproto.SIMCard{},
		metas:   map[string]*convMeta{},
		selfIDs: map[string]bool{},
		cache:   map[string][]cachedMessage{},
		pending: map[string]pendingSend{},
		events:  make(chan queuedEvent, eventQueue),
		closed:  make(chan struct{}),
	}
	go session.resumeLoop()
	return session
}

func wallClockJumped(previous, current time.Time) bool {
	return current.UnixNano()-previous.UnixNano() > int64(resumeGapThreshold+resumeCheckInterval)
}

// resumeLoop detects suspend using wall time rather than Go's monotonic
// clock, which intentionally stops advancing while Linux is suspended.
// It does not poll messages during normal operation.
func (s *Session) resumeLoop() {
	ticker := time.NewTicker(resumeCheckInterval)
	defer ticker.Stop()
	previous := time.Now()
	for {
		select {
		case current := <-ticker.C:
			if wallClockJumped(previous, current) {
				s.log.Info().Msg("system resume detected; refreshing relay state")
				s.refreshAfterResume()
			}
			previous = current
		case <-s.closed:
			return
		}
	}
}

func (s *Session) refreshAfterResume() {
	s.mu.Lock()
	client := s.client
	connected := s.connected
	s.mu.Unlock()
	if client == nil || !connected {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	err := client.SetActiveSession(ctx)
	cancel()
	if err != nil {
		s.log.Warn().Msg("re-arming relay after resume failed")
	}
	if !s.fullSync("resume") {
		s.log.Warn().Msg("conversation sync after resume failed")
	}
}

func (s *Session) buildClient(auth *libgm.AuthData) {
	// The relay library is chatty at debug (base64 settings dumps that
	// decode to phone numbers); it stays at warn unconditionally while
	// the adapter's own logger follows HANDOVER_ADAPTER_DEBUG.
	relayLog := s.log.With().Str("component", "libgm").Logger().Level(zerolog.WarnLevel)
	s.clientGen++
	gen := s.clientGen
	s.client = libgm.NewClient(
		auth,
		nil,
		relayLog,
		exhttp.ClientSettings{},
	)
	s.client.SetEventHandler(func(evt any) {
		s.mu.Lock()
		current := s.clientGen == gen
		sessionGen := s.generation
		s.mu.Unlock()
		if !current {
			return
		}
		queued := queuedEvent{generation: sessionGen, event: evt}
		select {
		case s.events <- queued:
		default:
			s.log.Warn().Msg("relay event queue full, dropping oldest signal")
			select {
			case <-s.events:
			default:
			}
			select {
			case s.events <- queued:
			default:
			}
			// A dropped relay event is a gap the daemon cannot see:
			// message, delete, and auth signals may now be lost.
			// Record the affected thread for targeted repair and
			// schedule one serialized catch-up so the loss heals
			// instead of lingering.
			if convID := conversationOf(evt); convID != "" {
				s.markDirty(convID)
			}
			s.requestResync()
		}
	})
}

// requestResync schedules one background catch-up after event loss.
// Concurrent overflows collapse into the single running sync.
func (s *Session) requestResync() {
	if !s.resyncing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.resyncing.Store(false)
		s.fullSync("event-queue-overflow")
		s.refreshDirty()
	}()
}

// maxDirtyConvs bounds targeted overflow repair. Beyond the cap the
// oldest dirty marks drop and the next full sync still converges.
const maxDirtyConvs = 64

// markDirty records a conversation whose relay events were dropped,
// so repair can refresh its window explicitly instead of hoping it
// lands in the recent ten.
func (s *Session) markDirty(convID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dirtyConvs == nil {
		s.dirtyConvs = map[string]struct{}{}
	}
	if _, ok := s.dirtyConvs[convID]; ok {
		return
	}
	if len(s.dirtyConvs) >= maxDirtyConvs {
		for old := range s.dirtyConvs {
			delete(s.dirtyConvs, old)
			break
		}
	}
	s.dirtyConvs[convID] = struct{}{}
}

// takeDirty drains the dirty set for one repair pass.
func (s *Session) takeDirty() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.dirtyConvs))
	for convID := range s.dirtyConvs {
		out = append(out, convID)
	}
	s.dirtyConvs = map[string]struct{}{}
	return out
}

// refreshDirty re-emits full windows for conversations whose events
// were dropped, sequentially so the relay never faces fan-out.
func (s *Session) refreshDirty() {
	for _, convID := range s.takeDirty() {
		if !s.alive() {
			return
		}
		s.emitWindow(convID, messageWindow, nil, true, true)
	}
}

// conversationOf extracts the thread id from relay events that carry
// one. Events without a thread return "".
func conversationOf(evt any) string {
	switch evt := evt.(type) {
	case *gmproto.Conversation:
		return evt.GetConversationID()
	case *libgm.WrappedMessage:
		if evt.Message != nil {
			return evt.Message.GetConversationID()
		}
	case *gmproto.TypingData:
		return evt.GetConversationID()
	}
	return ""
}

// alive reports whether the session still accepts work. Events fired
// after Close are dropped so a late reconnect cannot emit state for a
// disposed session.
func (s *Session) alive() bool {
	select {
	case <-s.closed:
		return false
	default:
		return true
	}
}

// bumpGeneration retires the current login session: queued events,
// in-flight pairing, and pending saves from the old generation stop
// applying. Callers hold no locks; this takes its own.
func (s *Session) bumpGeneration() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.generation++
	if s.generation == 0 {
		s.generation = 1
	}
	return s.generation
}

// currentGeneration reads the session generation under lock.
func (s *Session) currentGeneration() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generation
}

// saveAuthIfCurrent persists auth only when the caller's generation
// is still current and auth is present. A stale sync must never
// recreate a session file logout deleted, and teardown must never
// persist a nil auth as JSON null.
func (s *Session) saveAuthIfCurrent(generation uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if generation != s.generation || s.auth == nil {
		return nil
	}
	return s.store.SaveAuth(s.account, s.auth)
}

// fire emits unless the session was closed.
func (s *Session) fire(evt Event) {
	if !s.alive() {
		return
	}
	s.emit(evt)
}

// runEventLoop starts the single relay event consumer.
func (s *Session) runEventLoop() {
	s.loopOnce.Do(func() { go s.eventLoop() })
}

// runConnectLoop starts a reconnect loop, stopping the previous one
// first so re-login never leaves two loops racing over one account.
func (s *Session) runConnectLoop(ctx context.Context) {
	s.mu.Lock()
	if s.connectCancel != nil {
		s.connectCancel()
		s.connectCancel = nil
	}
	loopCtx, cancel := context.WithCancel(ctx)
	s.connectCancel = cancel
	s.mu.Unlock()
	go s.connectLoop(loopCtx)
}

// Restore loads a persisted session and connects in the background.
// It reports restored-but-reconnecting state immediately so a restarted
// daemon recovers without a new ceremony.
func (s *Session) Restore(ctx context.Context) error {
	auth, err := s.store.LoadAuth(s.account)
	if err != nil {
		return err
	}
	if auth == nil {
		return fmt.Errorf("no stored session")
	}
	s.mu.Lock()
	s.auth = auth
	s.authenticated = auth.Mobile != nil
	s.buildClient(auth)
	s.mu.Unlock()
	s.fire(Event{Type: "account", Account: s.account, Label: "Google Messages",
		Connected: false, Authenticated: s.authenticated})
	s.runEventLoop()
	s.runConnectLoop(ctx)
	return nil
}

func (s *Session) connectLoop(ctx context.Context) {
	backoff := 5 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.closed:
			return
		default:
		}
		if err := s.connectOnce(ctx); err == nil {
			return
		} else if isFatalAuth(err) {
			s.log.Warn().Msg("stored session rejected, re-pair required")
			s.mu.Lock()
			s.authenticated = false
			s.connected = false
			s.mu.Unlock()
			s.fire(Event{Type: "account", Account: s.account, Label: "Google Messages",
				Connected: false, Authenticated: false})
			s.fire(Event{Type: "error", Account: s.account, Message: "session expired, re-pair required"})
			return
		} else {
			s.log.Warn().Err(err).Dur("backoff", backoff).Msg("relay connect failed, retrying")
		}
		select {
		case <-ctx.Done():
			return
		case <-s.closed:
			return
		case <-time.After(backoff):
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}

func (s *Session) connectOnce(ctx context.Context) error {
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client == nil {
		return fmt.Errorf("no client")
	}
	timeout, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := client.FetchConfig(timeout); err != nil {
		return err
	}
	// Connect starts libgm's long-poll loop asynchronously. Do not pass the
	// short setup timeout here: cancelling it when this function returns
	// immediately kills the receive loop and leaves the account falsely
	// online with no conversation/status events.
	if err := client.Connect(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	s.connected = true
	s.authenticated = true
	s.mu.Unlock()
	s.fire(Event{Type: "account", Account: s.account, Label: "Google Messages",
		Connected: true, Authenticated: true})
	if err := s.saveAuthIfCurrent(s.currentGeneration()); err != nil {
		s.log.Warn().Err(err).Msg("persisting refreshed session failed")
	}
	// libgm's postConnect callback activates the phone session asynchronously
	// after Connect returns. Start our catch-up after that small hand-off;
	// running ListConversations immediately races the callback and can leave
	// the first sync waiting for a response that the phone never produces.
	go func() {
		for attempt, delay := range []time.Duration{3 * time.Second, 5 * time.Second, 15 * time.Second} {
			timer := time.NewTimer(delay)
			select {
			case <-timer.C:
				if s.fullSync("connect") {
					return
				}
				s.log.Warn().Int("attempt", attempt+1).Msg("conversation sync will retry")
			case <-s.closed:
				if !timer.Stop() {
					<-timer.C
				}
				return
			}
		}
	}()
	return nil
}

func isFatalAuth(err error) bool {
	return errors.Is(err, events.ErrInvalidCredentials) ||
		errors.Is(err, events.ErrRequestedEntityNotFound)
}

func (s *Session) eventLoop() {
	for {
		select {
		case <-s.closed:
			return
		case queued := <-s.events:
			// Recheck at dequeue: backlogged events from an old
			// login must not mutate the new session's state.
			s.mu.Lock()
			current := s.generation == queued.generation
			s.mu.Unlock()
			if !current {
				continue
			}
			s.handleRelayEvent(queued.event)
		}
	}
}

func (s *Session) handleRelayEvent(evt any) {
	switch evt := evt.(type) {
	case *gmproto.Conversation:
		// Refresh the full record: event parts may be partial.
		conv, err := s.getConversation(evt.GetConversationID())
		if err != nil {
			s.log.Warn().Str("conversation", evt.GetConversationID()).Msg("refreshing conversation failed")
			return
		}
		mapped, meta, err := s.mapConversation(conv)
		if err != nil {
			s.log.Warn().Err(err).Msg("dropping invalid conversation")
			return
		}
		s.mu.Lock()
		s.metas[conv.GetConversationID()] = meta
		s.mu.Unlock()
		s.fire(Event{Type: "conversations", Account: s.account,
			Conversations: []Conversation{mapped}})
	case *libgm.WrappedMessage:
		s.handleMessage(evt.Message, evt.IsOld)
	case *gmproto.TypingData:
		participants := []string{}
		if evt.GetType() == gmproto.TypingTypes_STARTED_TYPING && evt.GetUser().GetNumber() != "" {
			participants = append(participants, evt.GetUser().GetNumber())
		}
		s.fire(Event{Type: "typing", Account: s.account,
			Conversation: evt.GetConversationID(), Participants: participants})
	case *gmproto.Settings:
		changed := false
		s.mu.Lock()
		for _, sim := range evt.GetSIMCards() {
			id := sim.GetSIMParticipant().GetID()
			if id == "" {
				continue
			}
			s.sims[id] = sim
			if !s.selfIDs[id] {
				s.selfIDs[id] = true
				changed = true
			}
		}
		s.mu.Unlock()
		if changed {
			s.log.Debug().Int("sims", len(evt.GetSIMCards())).Msg("relay identity updated")
		}
	case *events.AuthTokenRefreshed:
		// libgm refreshes auth tokens in the background. Persist on
		// every refresh: a restart between refresh and the next
		// unrelated save would otherwise resurrect a stale token.
		s.mu.Lock()
		auth := s.auth
		s.mu.Unlock()
		if auth == nil {
			break
		}
		if err := s.saveAuthIfCurrent(s.currentGeneration()); err != nil {
			s.log.Warn().Err(err).Msg("persisting refreshed session failed")
		}
	case *events.GaiaLoggedOut:
		s.log.Warn().Msg("relay reported logout")
		s.mu.Lock()
		s.connected = false
		s.authenticated = false
		s.mu.Unlock()
		s.fire(Event{Type: "account", Account: s.account, Label: "Google Messages",
			Connected: false, Authenticated: false})
		s.fire(Event{Type: "error", Account: s.account, Message: "session logged out by Google, re-pair required"})
	case *events.ListenFatalError:
		s.log.Warn().Err(evt.Error).Msg("relay fatal error")
		if isFatalAuth(evt.Error) {
			s.mu.Lock()
			s.connected = false
			s.authenticated = false
			s.mu.Unlock()
			s.fire(Event{Type: "account", Account: s.account, Label: "Google Messages",
				Connected: false, Authenticated: false})
			s.fire(Event{Type: "error", Account: s.account, Message: "session expired, re-pair required"})
		} else {
			s.fire(Event{Type: "error", Account: s.account, Message: "relay connection lost, retrying"})
		}
	case *events.AccountChange:
		s.log.Debug().Msg("relay account change ignored")
	default:
		s.log.Debug().Str("event", fmt.Sprintf("%T", evt)).Msg("ignoring relay event")
	}
}

// tmpID generates a relay transaction id.
func tmpID() string {
	return util.GenerateTmpID()
}
