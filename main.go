// Command handover-gmessages-adapter speaks the Handover helper IPC v1
// contract on stdin/stdout and drives the real Google Messages companion
// service through upstream libgm.
//
// This binary is a separate AGPL-3.0-only component (see LICENSE). It
// links go.mau.fi/mautrix-gmessages and must never be vendored into the
// MIT Handover repository. Only coarse JSON records cross the process
// boundary; Google protocol types, cookies, tokens, and keys stay here.
//
// Never pass credentials on argv. Logs (stderr) carry ids and counts
// only: no bodies, cookies, tokens, keys, contacts, or media bytes.
package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/rs/zerolog"
	"handover-gmessages-adapter/adapter"
)

const adapterName = "handover-gmessages-adapter/libgm"

func main() {
	level := zerolog.InfoLevel
	if os.Getenv("HANDOVER_ADAPTER_DEBUG") != "" {
		level = zerolog.DebugLevel
	}
	var writer = zerolog.ConsoleWriter{Out: os.Stderr}
	if path := os.Getenv("HANDOVER_ADAPTER_LOG"); path != "" {
		if file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			writer.Out = file
		}
	}
	log := zerolog.New(writer).
		Level(level).
		With().Str("component", "adapter").Logger()

	store, err := adapter.NewStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, "state directory unavailable")
		os.Exit(1)
	}

	h := &hub{store: store, log: log, sessions: map[string]*adapter.Session{}}
	h.emit = h.emitEvent

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	stdout := bufio.NewWriter(os.Stdout)
	defer stdout.Flush()
	h.out = stdout

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(trimSpace(line)) == 0 {
			continue
		}
		var cmd adapter.Command
		if err := json.Unmarshal(line, &cmd); err != nil {
			h.write(adapter.Event{Type: "error", Message: "malformed command"})
			continue
		}
		if cmd.Type == "shutdown" {
			h.shutdown()
			return
		}
		h.dispatch(cmd)
	}
	h.shutdown()
}

func trimSpace(b []byte) []byte {
	start := 0
	for start < len(b) && (b[start] == ' ' || b[start] == '\t' || b[start] == '\r' || b[start] == '\n') {
		start++
	}
	return b[start:]
}

type hub struct {
	store    *adapter.Store
	log      zerolog.Logger
	emit     func(adapter.Event)
	out      *bufio.Writer
	mu       sync.Mutex
	sessions map[string]*adapter.Session
}

func (h *hub) write(evt adapter.Event) {
	raw, err := json.Marshal(evt)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.out.Write(raw)
	h.out.Write([]byte("\n"))
	h.out.Flush()
}

func (h *hub) emitEvent(evt adapter.Event) {
	adapter.LogEvent(h.log, evt)
	h.write(evt)
}

func (h *hub) session(account string) *adapter.Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	sess, ok := h.sessions[account]
	if !ok {
		sess = adapter.NewSession(account, h.store, h.log, h.emit)
		h.sessions[account] = sess
	}
	return sess
}

func (h *hub) forget(account string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if sess, ok := h.sessions[account]; ok {
		sess.Close()
		delete(h.sessions, account)
	}
}

func (h *hub) shutdown() {
	h.mu.Lock()
	sessions := make([]*adapter.Session, 0, len(h.sessions))
	for _, sess := range h.sessions {
		sessions = append(sessions, sess)
	}
	h.mu.Unlock()
	for _, sess := range sessions {
		sess.Close()
	}
}

func (h *hub) dispatch(cmd adapter.Command) {
	h.log.Debug().Str("command", adapter.SanitizeCommand(cmd)).Msg("daemon command")
	switch cmd.Type {
	case "hello":
		h.write(adapter.Event{Type: "hello", HelperProtocol: adapter.HelperProtocol, Name: adapterName})
		h.restore()
	case "login":
		if !validAccount(cmd.Account) || cmd.BundleB64 == "" {
			h.write(adapter.Event{Type: "error", Account: cmd.Account, Message: "login rejected"})
			return
		}
		bundle, err := base64.StdEncoding.DecodeString(cmd.BundleB64)
		if err != nil || len(bundle) == 0 || len(bundle) > adapter.MaxBundleBytes {
			h.write(adapter.Event{Type: "error", Account: cmd.Account, Message: "login bundle rejected"})
			return
		}
		h.session(cmd.Account).Login(bundle)
	case "logout":
		if !validAccount(cmd.Account) {
			return
		}
		h.session(cmd.Account).Logout()
		h.forget(cmd.Account)
	case "list_conversations", "sync":
		if !validAccount(cmd.Account) {
			return
		}
		h.session(cmd.Account).Sync()
	case "fetch_history":
		if !validAccount(cmd.Account) || cmd.Conversation == "" {
			return
		}
		limit := cmd.Limit
		if limit < 1 || limit > 100 {
			limit = 20
		}
		var cursor *string
		if cmd.Cursor != "" {
			cursor = &cmd.Cursor
		}
		h.session(cmd.Account).FetchHistory(cmd.Conversation, limit, cursor)
	case "send_text":
		if !validAccount(cmd.Account) || cmd.Conversation == "" || cmd.RequestID == "" {
			return
		}
		h.session(cmd.Account).SendText(cmd.RequestID, cmd.Conversation, cmd.Text)
	case "send_media":
		if !validAccount(cmd.Account) || cmd.Conversation == "" || cmd.RequestID == "" || cmd.Path == "" {
			h.session(cmd.Account).SendResult(cmd.RequestID, false, "unreadable file")
			return
		}
		h.session(cmd.Account).SendMedia(cmd.RequestID, cmd.Conversation, cmd.Path, cmd.Caption)
	case "react":
		if !validAccount(cmd.Account) || cmd.RequestID == "" {
			return
		}
		h.session(cmd.Account).React(cmd.RequestID, cmd.Conversation, cmd.Message, cmd.Emoji, cmd.Add)
	case "mark_read":
		if !validAccount(cmd.Account) || cmd.Conversation == "" {
			return
		}
		h.session(cmd.Account).MarkRead(cmd.Conversation, cmd.Message)
	case "typing":
		if !validAccount(cmd.Account) || cmd.Conversation == "" {
			return
		}
		h.session(cmd.Account).Typing(cmd.Conversation)
	case "delete_message":
		if !validAccount(cmd.Account) || cmd.RequestID == "" {
			return
		}
		h.session(cmd.Account).DeleteMessage(cmd.RequestID, cmd.Message)
	case "open_conversation":
		if !validAccount(cmd.Account) || cmd.RequestID == "" {
			return
		}
		h.session(cmd.Account).Open(cmd.RequestID, cmd.Addresses)
	default:
		h.write(adapter.Event{Type: "error", Message: "unknown command"})
	}
}

// restore announces persisted sessions so a restarted daemon recovers
// without a new ceremony, then reconnects each in the background.
func (h *hub) restore() {
	for _, account := range h.store.Accounts() {
		if !validAccount(account) {
			continue
		}
		sess := h.session(account)
		if err := sess.Restore(context.Background()); err != nil {
			h.log.Warn().Str("account", account).Msg("stored session unreadable, re-pair required")
		}
	}
}

func validAccount(account string) bool {
	if account == "" || len(account) > 128 {
		return false
	}
	for _, r := range account {
		if r == '/' || r == '\\' || r == 0 || r < 0x20 {
			return false
		}
	}
	return true
}
