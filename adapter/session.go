package adapter

import (
	"context"
	"errors"
	"fmt"
	"sync"
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

// syncPage bounds conversation listing and per-thread windows.
const (
	conversationPage = 200
	messageWindow    = 20
	cachePerConv     = 200
	eventQueue       = 256
)

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
	client     *libgm.Client
	auth       *libgm.AuthData
	sims       map[string]*gmproto.SIMCard
	metas      map[string]*convMeta
	selfIDs    map[string]bool
	cache      map[string][]cachedMessage
	pending    map[string]pendingSend
	pairCancel context.CancelFunc
	ps         *libgm.PairingSession
	fullReq    map[string]bool

	events    chan any
	closed    chan struct{}
	closeOnce sync.Once

	connected     bool
	authenticated bool
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
	return &Session{
		account: account,
		store:   store,
		log:     log.With().Str("account", account).Logger(),
		emit:    emit,
		sims:    map[string]*gmproto.SIMCard{},
		metas:   map[string]*convMeta{},
		selfIDs: map[string]bool{},
		cache:   map[string][]cachedMessage{},
		pending: map[string]pendingSend{},
		events:  make(chan any, eventQueue),
		closed:  make(chan struct{}),
	}
}

func (s *Session) buildClient(auth *libgm.AuthData) {
	// The relay library is chatty at debug (base64 settings dumps that
	// decode to phone numbers); it stays at warn unconditionally while
	// the adapter's own logger follows HANDOVER_ADAPTER_DEBUG.
	relayLog := s.log.With().Str("component", "libgm").Logger().Level(zerolog.WarnLevel)
	s.client = libgm.NewClient(
		auth,
		nil,
		relayLog,
		exhttp.ClientSettings{},
	)
	s.client.SetEventHandler(func(evt any) {
		select {
		case s.events <- evt:
		default:
			s.log.Warn().Msg("relay event queue full, dropping oldest signal")
			select {
			case <-s.events:
			default:
			}
			select {
			case s.events <- evt:
			default:
			}
		}
	})
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
	s.emit(Event{Type: "account", Account: s.account, Label: "Google Messages",
		Connected: false, Authenticated: s.authenticated})
	go s.eventLoop()
	go s.connectLoop(ctx)
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
			s.emit(Event{Type: "account", Account: s.account, Label: "Google Messages",
				Connected: false, Authenticated: false})
			s.emit(Event{Type: "error", Account: s.account, Message: "session expired, re-pair required"})
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
	if err := client.Connect(timeout); err != nil {
		return err
	}
	s.mu.Lock()
	s.connected = true
	s.authenticated = true
	s.mu.Unlock()
	s.emit(Event{Type: "account", Account: s.account, Label: "Google Messages",
		Connected: true, Authenticated: true})
	if err := s.store.SaveAuth(s.account, s.auth); err != nil {
		s.log.Warn().Err(err).Msg("persisting refreshed session failed")
	}
	s.fullSync("connect")
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
		case evt := <-s.events:
			s.handleRelayEvent(evt)
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
		s.emit(Event{Type: "conversations", Account: s.account,
			Conversations: []Conversation{mapped}})
	case *libgm.WrappedMessage:
		s.handleMessage(evt.Message, evt.IsOld)
	case *gmproto.TypingData:
		participants := []string{}
		if evt.GetType() == gmproto.TypingTypes_STARTED_TYPING && evt.GetUser().GetNumber() != "" {
			participants = append(participants, evt.GetUser().GetNumber())
		}
		s.emit(Event{Type: "typing", Account: s.account,
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
	case *events.GaiaLoggedOut:
		s.log.Warn().Msg("relay reported logout")
		s.mu.Lock()
		s.connected = false
		s.authenticated = false
		s.mu.Unlock()
		s.emit(Event{Type: "account", Account: s.account, Label: "Google Messages",
			Connected: false, Authenticated: false})
		s.emit(Event{Type: "error", Account: s.account, Message: "session logged out by Google, re-pair required"})
	case *events.ListenFatalError:
		s.log.Warn().Err(evt.Error).Msg("relay fatal error")
		if isFatalAuth(evt.Error) {
			s.mu.Lock()
			s.connected = false
			s.authenticated = false
			s.mu.Unlock()
			s.emit(Event{Type: "account", Account: s.account, Label: "Google Messages",
				Connected: false, Authenticated: false})
			s.emit(Event{Type: "error", Account: s.account, Message: "session expired, re-pair required"})
		} else {
			s.emit(Event{Type: "error", Account: s.account, Message: "relay connection lost, retrying"})
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
