// Copyright (C) 2026 Handover contributors
// SPDX-License-Identifier: AGPL-3.0-only
package adapter

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/rs/zerolog"
)

// redactID hashes long opaque identifiers for logs. Short stable ids
// (conversation and message local ids) are safe to log verbatim; this
// helper covers anything else that must never appear in cleartext.
func redactID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:8])
}

// LogEvent describes an outbound event with ids and counts only. Bodies,
// prompts, names, addresses, and paths must never reach logs.
func LogEvent(log zerolog.Logger, evt Event) *zerolog.Event {
	e := log.Debug().Str("event", evt.Type)
	switch evt.Type {
	case "account", "account_removed", "pairing":
		e = e.Str("account", evt.Account)
	case "conversations":
		e = e.Str("account", evt.Account).
			Int("count", len(evt.Conversations)).
			Bool("full", evt.Full)
	case "conversation_removed":
		e = e.Str("account", evt.Account).Str("conversation", evt.Conversation)
	case "messages":
		e = e.Str("account", evt.Account).
			Str("conversation", evt.Conversation).
			Int("count", len(evt.Messages)).
			Bool("full", evt.Full)
	case "message_removed":
		e = e.Str("account", evt.Account).
			Str("conversation", evt.Conversation).
			Str("message", evt.Message)
	case "status":
		e = e.Str("account", evt.Account).
			Str("conversation", evt.Conversation).
			Str("message", evt.Message).
			Str("status", evt.Status)
	case "typing":
		e = e.Str("account", evt.Account).
			Str("conversation", evt.Conversation).
			Int("participants", len(evt.Participants))
	case "read":
		e = e.Str("account", evt.Account).
			Str("conversation", evt.Conversation).
			Bool("unread", evt.Unread)
	case "command_result":
		e = e.Str("request_id", evt.RequestID).Bool("ok", evt.OK)
	case "error":
		e = e.Int("message_len", len(evt.Message))
	}
	return e
}

// SanitizeCommand renders a command for logs with secrets removed.
func SanitizeCommand(cmd Command) string {
	var sb strings.Builder
	sb.WriteString(cmd.Type)
	if cmd.Account != "" {
		sb.WriteString(" account=" + cmd.Account)
	}
	if cmd.Conversation != "" {
		sb.WriteString(" conversation=" + cmd.Conversation)
	}
	if cmd.Message != "" {
		sb.WriteString(" message=" + cmd.Message)
	}
	if cmd.RequestID != "" {
		sb.WriteString(" request=" + cmd.RequestID)
	}
	return sb.String()
}
