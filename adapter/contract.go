// Package adapter implements the Handover helper IPC v1 contract against
// upstream libgm (go.mau.fi/mautrix-gmessages, AGPL-3.0-only).
//
// License boundary: this package links upstream libgm and is therefore
// distributed under AGPL-3.0-only (see ../LICENSE). It is a separate
// component from the MIT Handover repository and shares only the coarse
// JSON contract defined below — no Handover source is vendored here.
package adapter

import "encoding/json"

// HelperProtocol is the Handover helper IPC version this adapter speaks.
const HelperProtocol = 1

// MaxBundleBytes mirrors the daemon-side login bundle cap.
const MaxBundleBytes = 256 * 1024

// MaxTextChars mirrors the Handover normalization bound; longer outbound
// text is rejected before it reaches the relay.
const MaxTextChars = 8000

// Command is one JSON line from the daemon (stdin).
type Command struct {
	Type         string   `json:"type"`
	Account      string   `json:"account,omitempty"`
	BundleB64    string   `json:"bundle_b64,omitempty"`
	Conversation string   `json:"conversation,omitempty"`
	Message      string   `json:"message,omitempty"`
	Text         string   `json:"text,omitempty"`
	ReplyTo      string   `json:"reply_to,omitempty"`
	Caption      string   `json:"caption,omitempty"`
	Path         string   `json:"path,omitempty"`
	Emoji        string   `json:"emoji,omitempty"`
	Add          bool     `json:"add,omitempty"`
	Addresses    []string `json:"addresses,omitempty"`
	Limit        uint32   `json:"limit,omitempty"`
	Cursor       string   `json:"cursor,omitempty"`
	RequestID    string   `json:"request_id,omitempty"`
}

// Event is one JSON line to the daemon (stdout).
type Event struct {
	Type            string         `json:"type"`
	HelperProtocol  int            `json:"helper_protocol,omitempty"`
	Name            string         `json:"name,omitempty"`
	Account         string         `json:"account,omitempty"`
	Label           string         `json:"label,omitempty"`
	Connected       bool           `json:"connected,omitempty"`
	Authenticated   bool           `json:"authenticated,omitempty"`
	Prompt          string         `json:"prompt,omitempty"`
	Conversations   []Conversation `json:"conversations,omitempty"`
	Full            bool           `json:"full,omitempty"`
	Conversation    string         `json:"conversation,omitempty"`
	Messages        []Message      `json:"messages,omitempty"`
	CursorNext      string         `json:"cursor_next,omitempty"`
	PageComplete    bool           `json:"page_complete,omitempty"`
	Message         string         `json:"message,omitempty"`
	Status          string         `json:"status,omitempty"`
	Participants    []string       `json:"participants,omitempty"`
	LastReadMessage string         `json:"last_read_message,omitempty"`
	Unread          bool           `json:"unread,omitempty"`
	RequestID       string         `json:"request_id,omitempty"`
	OK              bool           `json:"ok,omitempty"`
	Error           string         `json:"error,omitempty"`
}

// MarshalJSON emits exactly the Handover helper IPC v1 shapes. Arrays
// that the daemon requires (messages, conversations, participants) are
// always present, even when empty: omitting them would fail decoding.
// Optional scalars stay omitted when empty. Unknown keys are never
// emitted: the daemon ignores them, but silence keeps the boundary
// reviewable.
func (e Event) MarshalJSON() ([]byte, error) {
	out := map[string]any{"type": e.Type}
	str := func(key, value string) {
		if value != "" {
			out[key] = value
		}
	}
	switch e.Type {
	case "hello":
		out["helper_protocol"] = e.HelperProtocol
		out["name"] = e.Name
	case "account":
		out["account"] = e.Account
		out["label"] = e.Label
		out["connected"] = e.Connected
		out["authenticated"] = e.Authenticated
	case "account_removed":
		out["account"] = e.Account
	case "pairing":
		out["account"] = e.Account
		out["prompt"] = e.Prompt
	case "conversations":
		out["account"] = e.Account
		out["conversations"] = nonNilConversations(e.Conversations)
		out["full"] = e.Full
	case "conversation_removed":
		out["account"] = e.Account
		out["conversation"] = e.Conversation
	case "messages":
		out["account"] = e.Account
		out["conversation"] = e.Conversation
		out["messages"] = nonNilMessages(e.Messages)
		str("cursor_next", e.CursorNext)
		out["page_complete"] = e.PageComplete
		out["full"] = e.Full
	case "message_removed":
		out["account"] = e.Account
		out["conversation"] = e.Conversation
		out["message"] = e.Message
	case "status":
		out["account"] = e.Account
		out["conversation"] = e.Conversation
		out["message"] = e.Message
		out["status"] = e.Status
	case "typing":
		out["account"] = e.Account
		out["conversation"] = e.Conversation
		out["participants"] = nonNilStrings(e.Participants)
	case "read":
		out["account"] = e.Account
		out["conversation"] = e.Conversation
		str("last_read_message", e.LastReadMessage)
		out["unread"] = e.Unread
	case "command_result":
		out["request_id"] = e.RequestID
		out["ok"] = e.OK
		str("error", e.Error)
	case "error":
		out["message"] = e.Message
	}
	return json.Marshal(out)
}

func nonNilConversations(in []Conversation) []Conversation {
	if in == nil {
		return []Conversation{}
	}
	return in
}

func nonNilMessages(in []Message) []Message {
	if in == nil {
		return []Message{}
	}
	return in
}

func nonNilStrings(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// Conversation is the wire form of one thread. Field names are generic:
// no Google protocol vocabulary crosses the process boundary.
type Conversation struct {
	LocalID        string        `json:"local_id"`
	Kind           string        `json:"kind"`      // direct|group
	Transport      string        `json:"transport"` // rcs|sms|mms|unknown
	Title          string        `json:"title,omitempty"`
	Participants   []Participant `json:"participants,omitempty"`
	LatestMessage  string        `json:"latest_message,omitempty"`
	LastActivityAt int64         `json:"last_activity_at,omitempty"`
	UnreadCount    *uint64       `json:"unread_count,omitempty"`
	Cursor         string        `json:"cursor,omitempty"`
	Capabilities   []string      `json:"capabilities,omitempty"`
}

// Participant is one thread member with an opaque sender key.
type Participant struct {
	LocalID     string `json:"local_id"`
	DisplayName string `json:"display_name,omitempty"`
	Address     string `json:"address,omitempty"`
	IsSelf      bool   `json:"is_self,omitempty"`
}

// Message is the wire form of one message.
type Message struct {
	LocalID     string       `json:"local_id"`
	Sender      string       `json:"sender"`
	Transport   string       `json:"transport,omitempty"`
	SentAt      *int64       `json:"sent_at,omitempty"`
	Text        string       `json:"text,omitempty"`
	Attachments []Attachment `json:"attachments,omitempty"`
	ReplyTo     string       `json:"reply_to,omitempty"`
	Reactions   []Reaction   `json:"reactions,omitempty"`
	Deleted     bool         `json:"deleted,omitempty"`
}

// Attachment references bytes staged by the adapter; payload bytes never
// ride the contract, only the staged path.
type Attachment struct {
	LocalID    string  `json:"local_id"`
	MIME       string  `json:"mime,omitempty"`
	Name       string  `json:"name,omitempty"`
	SizeBytes  *uint64 `json:"size_bytes,omitempty"`
	StagedPath string  `json:"staged_path,omitempty"`
}

// Reaction aggregates one emoji over its reactors.
type Reaction struct {
	Emoji          string   `json:"emoji"`
	ParticipantIDs []string `json:"participant_ids,omitempty"`
}
