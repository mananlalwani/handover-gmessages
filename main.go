// Copyright (C) 2026 Handover contributors
// SPDX-License-Identifier: AGPL-3.0-only
// Command handover-gmessages speaks the Handover helper IPC v1
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

	"github.com/mananlalwani/handover-gmessages/adapter"
	"github.com/rs/zerolog"
)

const adapterName = "handover-gmessages/libgm"

// Version is the adapter release. Keep in sync with the git tag.
const Version = "0.3.1"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "--version" || os.Args[1] == "-version" || os.Args[1] == "version") {
		fmt.Printf("handover-gmessages %s\n", Version)
		return
	}
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
	// Staged attachments are transient transfer data. Sweep debris
	// from crashed runs at startup so the directory stays bounded.
	if removed, err := store.SweepStaged(); err != nil {
		log.Warn().Err(err).Msg("sweeping staged attachments failed")
	} else if removed > 0 {
		log.Info().Int("removed", removed).Msg("swept staged attachments")
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
	store *adapter.Store
	log   zerolog.Logger
	emit  func(adapter.Event)
	out   *bufio.Writer
	// sessionsMu guards the session map only. Stdout has its own
	// lock: a blocked flush must never stall session lookup or
	// shutdown, and Close must never run under the map lock.
	sessionsMu sync.Mutex
	sessions   map[string]*adapter.Session
	writeMu    sync.Mutex
}

func (h *hub) write(evt adapter.Event) {
	raw, err := json.Marshal(evt)
	if err != nil {
		return
	}
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	h.out.Write(raw)
	h.out.Write([]byte("\n"))
	h.out.Flush()
}

func (h *hub) emitEvent(evt adapter.Event) {
	adapter.LogEvent(h.log, evt)
	h.write(evt)
}

func (h *hub) session(account string) *adapter.Session {
	h.sessionsMu.Lock()
	defer h.sessionsMu.Unlock()
	sess, ok := h.sessions[account]
	if !ok {
		sess = adapter.NewSession(account, h.store, h.log, h.emit)
		h.sessions[account] = sess
	}
	return sess
}

func (h *hub) forget(account string) {
	// Close outside the map lock: Disconnect is an external call
	// that must not run under a global lock.
	h.sessionsMu.Lock()
	sess, ok := h.sessions[account]
	if ok {
		delete(h.sessions, account)
	}
	h.sessionsMu.Unlock()
	if ok {
		sess.Close()
	}
}

func (h *hub) shutdown() {
	h.sessionsMu.Lock()
	sessions := make([]*adapter.Session, 0, len(h.sessions))
	for _, sess := range h.sessions {
		sessions = append(sessions, sess)
	}
	h.sessionsMu.Unlock()
	for _, sess := range sessions {
		sess.Close()
	}
}

// maxCommandWorkers bounds concurrent command goroutines. Every
// helper command fans out with `go`; without admission, repeated
// syncs pile goroutines on syncMu while RPC commands fan out
// unbounded underneath.
const maxCommandWorkers = 16

var commandSem = make(chan struct{}, maxCommandWorkers)

// spawn runs fn under admission control, reporting false when the
// worker pool is full so the caller can fail fast instead of
// queueing without bound.
func (h *hub) spawn(fn func()) bool {
	select {
	case commandSem <- struct{}{}:
		go func() {
			defer func() { <-commandSem }()
			fn()
		}()
		return true
	default:
		return false
	}
}

// dispatchAsync runs a session command under hub admission control.
// Request-ID commands fail fast with a busy result when the pool is
// full; fire-and-forget commands surface a busy error event.
func (h *hub) dispatchAsync(account, requestID string, fn func(*adapter.Session)) {
	sess := h.session(account)
	if h.spawn(func() { fn(sess) }) {
		return
	}
	if requestID != "" {
		sess.SendResult(requestID, false, "busy")
		return
	}
	h.write(adapter.Event{Type: "error", Account: account, Message: "busy"})
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
		// Pairing runs for minutes. Never hold the command loop for
		// it: shutdown and other commands must stay responsive.
		account := cmd.Account
		h.dispatchAsync(account, "", func(sess *adapter.Session) { sess.Login(bundle) })
	case "logout":
		if !validAccount(cmd.Account) {
			return
		}
		// Only forget the session when the revoke actually tore it
		// down. Forgetting after a failed revoke would disconnect a
		// live session while the daemon believes access ended. The
		// revoke runs async so a hung phone cannot trap the loop.
		account := cmd.Account
		h.dispatchAsync(account, "", func(sess *adapter.Session) {
			if sess.Logout() {
				h.forget(account)
			}
		})
	case "list_conversations", "sync":
		if !validAccount(cmd.Account) {
			return
		}
		account := cmd.Account
		h.dispatchAsync(account, "", func(sess *adapter.Session) { sess.Sync() })
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
		account, conversation := cmd.Account, cmd.Conversation
		h.dispatchAsync(account, "", func(sess *adapter.Session) {
			sess.FetchHistory(conversation, limit, cursor, cmd.FetchID)
		})
	case "send_text":
		if !validAccount(cmd.Account) || cmd.Conversation == "" || cmd.RequestID == "" {
			return
		}
		account, conversation, text, replyTo, requestID := cmd.Account, cmd.Conversation, cmd.Text, cmd.ReplyTo, cmd.RequestID
		h.dispatchAsync(account, requestID, func(sess *adapter.Session) {
			sess.SendText(requestID, conversation, text, replyTo)
		})
	case "send_media":
		if !validAccount(cmd.Account) || cmd.Conversation == "" || cmd.RequestID == "" || cmd.Path == "" {
			h.session(cmd.Account).SendResult(cmd.RequestID, false, "unreadable file")
			return
		}
		account, conversation, path, caption, requestID := cmd.Account, cmd.Conversation, cmd.Path, cmd.Caption, cmd.RequestID
		h.dispatchAsync(account, requestID, func(sess *adapter.Session) {
			sess.SendMedia(requestID, conversation, path, caption)
		})
	case "react":
		if !validAccount(cmd.Account) || cmd.RequestID == "" {
			return
		}
		account, conversation, message, emoji, add, requestID := cmd.Account, cmd.Conversation, cmd.Message, cmd.Emoji, cmd.Add, cmd.RequestID
		h.dispatchAsync(account, requestID, func(sess *adapter.Session) {
			sess.React(requestID, conversation, message, emoji, add)
		})
	case "mark_read":
		if !validAccount(cmd.Account) || cmd.Conversation == "" {
			return
		}
		account, conversation, message := cmd.Account, cmd.Conversation, cmd.Message
		h.dispatchAsync(account, "", func(sess *adapter.Session) { sess.MarkRead(conversation, message) })
	case "typing":
		if !validAccount(cmd.Account) || cmd.Conversation == "" {
			return
		}
		account, conversation := cmd.Account, cmd.Conversation
		h.dispatchAsync(account, "", func(sess *adapter.Session) { sess.Typing(conversation) })
	case "delete_message":
		if !validAccount(cmd.Account) || cmd.RequestID == "" {
			return
		}
		account, message, requestID := cmd.Account, cmd.Message, cmd.RequestID
		h.dispatchAsync(account, requestID, func(sess *adapter.Session) {
			sess.DeleteMessage(requestID, message)
		})
	case "open_conversation":
		if !validAccount(cmd.Account) || cmd.RequestID == "" {
			return
		}
		account, addresses, requestID := cmd.Account, cmd.Addresses, cmd.RequestID
		h.dispatchAsync(account, requestID, func(sess *adapter.Session) {
			sess.Open(requestID, addresses)
		})
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
	// Same rule as the storage gate (accountFile): anything that
	// cannot become a session file name is rejected at the command
	// gate, so no account is accepted by one layer and rejected by
	// the other.
	if account == "" || len(account) > 128 {
		return false
	}
	for _, r := range account {
		if r == '/' || r == '\\' || r == '.' || r == 0 || r < 0x20 {
			return false
		}
	}
	return true
}
