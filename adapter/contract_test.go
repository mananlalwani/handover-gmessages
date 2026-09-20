// Copyright (C) 2026 Handover contributors
// SPDX-License-Identifier: AGPL-3.0-only
package adapter

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

func TestThreadMetaUsesCachedMetadata(t *testing.T) {
	s := NewSession("a", nil, zerolog.Nop(), func(Event) {})
	s.metas["154"] = &convMeta{
		outgoingID: "self",
		convType:   gmproto.ConversationType_SMS,
	}
	meta, err := s.threadMeta("154")
	if err != nil {
		t.Fatalf("threadMeta: %v", err)
	}
	if meta.outgoingID != "self" || meta.convType != gmproto.ConversationType_SMS {
		t.Fatalf("unexpected cached metadata: %+v", meta)
	}
	meta.outgoingID = "mutated"
	if s.metas["154"].outgoingID != "self" {
		t.Fatal("threadMeta returned the cache pointer")
	}
}

// decode emulates the daemon side loosely: required keys must be present
// with the right JSON types. This guards the exact v1 shapes without
// importing anything from the Handover tree.
func requireKeys(t *testing.T, raw []byte, keys map[string]string) {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("event is not an object: %v", err)
	}
	for key, want := range keys {
		got, ok := obj[key]
		if !ok {
			t.Errorf("missing required key %q in %s", key, raw)
			continue
		}
		var probe any
		if err := json.Unmarshal(got, &probe); err != nil {
			t.Errorf("key %q is not JSON: %v", key, err)
			continue
		}
		switch want {
		case "string":
			if _, ok := probe.(string); !ok {
				t.Errorf("key %q must be a string in %s", key, raw)
			}
		case "bool":
			if _, ok := probe.(bool); !ok {
				t.Errorf("key %q must be a bool in %s", key, raw)
			}
		case "array":
			if _, ok := probe.([]any); !ok {
				t.Errorf("key %q must be an array in %s", key, raw)
			}
		case "int":
			if _, ok := probe.(float64); !ok {
				t.Errorf("key %q must be a number in %s", key, raw)
			}
		}
	}
}

func marshal(t *testing.T, evt Event) []byte {
	t.Helper()
	raw, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func TestHelloShape(t *testing.T) {
	raw := marshal(t, Event{Type: "hello", HelperProtocol: 1, Name: "x"})
	requireKeys(t, raw, map[string]string{"type": "string", "helper_protocol": "int", "name": "string"})
}

func TestAccountShapes(t *testing.T) {
	raw := marshal(t, Event{Type: "account", Account: "a", Label: "l", Connected: true})
	requireKeys(t, raw, map[string]string{"type": "string", "account": "string", "label": "string", "connected": "bool", "authenticated": "bool"})
	raw = marshal(t, Event{Type: "account_removed", Account: "a"})
	requireKeys(t, raw, map[string]string{"type": "string", "account": "string"})
	raw = marshal(t, Event{Type: "pairing", Account: "a", Prompt: "tap"})
	requireKeys(t, raw, map[string]string{"type": "string", "account": "string", "prompt": "string"})
}

func TestConversationsShapeAlwaysCarriesArray(t *testing.T) {
	// Even empty lists must carry the array: the daemon requires it.
	raw := marshal(t, Event{Type: "conversations", Account: "a", Full: true})
	requireKeys(t, raw, map[string]string{"type": "string", "account": "string", "conversations": "array", "full": "bool"})
	if !strings.Contains(string(raw), `"conversations":[]`) {
		t.Errorf("empty list must encode as [], got %s", raw)
	}
	raw = marshal(t, Event{Type: "conversations", Account: "a",
		Conversations: []Conversation{{
			LocalID: "t", Kind: "direct", Transport: "rcs",
			Participants: []Participant{{LocalID: "p", IsSelf: true}},
			Capabilities: []string{"text"},
		}}})
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	var convs []map[string]json.RawMessage
	if err := json.Unmarshal(obj["conversations"], &convs); err != nil || len(convs) != 1 {
		t.Fatalf("conversations array: %v", err)
	}
	for _, key := range []string{"local_id", "kind", "transport", "participants", "capabilities"} {
		if _, ok := convs[0][key]; !ok {
			t.Errorf("conversation missing %q", key)
		}
	}
}

func TestMessagesShapeAlwaysCarriesArray(t *testing.T) {
	raw := marshal(t, Event{Type: "messages", Account: "a", Conversation: "c", Full: true})
	requireKeys(t, raw, map[string]string{"type": "string", "account": "string", "conversation": "string", "messages": "array", "full": "bool"})
	if strings.Contains(string(raw), "cursor_next") {
		t.Errorf("absent cursor must be omitted, got %s", raw)
	}
	raw = marshal(t, Event{Type: "messages", Account: "a", Conversation: "c",
		Messages:   []Message{{LocalID: "m", Sender: "p", Attachments: []Attachment{}, Reactions: []Reaction{}}},
		CursorNext: "m:123"})
	requireKeys(t, raw, map[string]string{"messages": "array", "cursor_next": "string"})
	var obj map[string]json.RawMessage
	json.Unmarshal(raw, &obj)
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(obj["messages"], &msgs); err != nil || len(msgs) != 1 {
		t.Fatalf("messages array: %v", err)
	}
	for _, key := range []string{"local_id", "sender"} {
		if _, ok := msgs[0][key]; !ok {
			t.Errorf("message missing %q", key)
		}
	}
}

func TestTypingShapeAlwaysCarriesArray(t *testing.T) {
	// Typing-clear is an empty array; omitting it would drop the clear.
	raw := marshal(t, Event{Type: "typing", Account: "a", Conversation: "c"})
	requireKeys(t, raw, map[string]string{"type": "string", "account": "string", "conversation": "string", "participants": "array"})
	if !strings.Contains(string(raw), `"participants":[]`) {
		t.Errorf("cleared typing must encode as [], got %s", raw)
	}
}

func TestStatusReadResultShapes(t *testing.T) {
	raw := marshal(t, Event{Type: "status", Account: "a", Conversation: "c", Message: "m", Status: "displayed"})
	requireKeys(t, raw, map[string]string{"type": "string", "account": "string", "conversation": "string", "message": "string", "status": "string"})
	raw = marshal(t, Event{Type: "read", Account: "a", Conversation: "c", Unread: false})
	requireKeys(t, raw, map[string]string{"type": "string", "account": "string", "conversation": "string", "unread": "bool"})
	if strings.Contains(string(raw), "last_read_message") {
		t.Errorf("absent last_read_message must be omitted, got %s", raw)
	}
	raw = marshal(t, Event{Type: "command_result", RequestID: "r", OK: true})
	requireKeys(t, raw, map[string]string{"type": "string", "request_id": "string", "ok": "bool"})
	if strings.Contains(string(raw), `"error"`) {
		t.Errorf("absent error must be omitted, got %s", raw)
	}
	raw = marshal(t, Event{Type: "error", Message: "busy"})
	requireKeys(t, raw, map[string]string{"type": "string", "message": "string"})
}

func TestNoGoogleVocabularyCrosses(t *testing.T) {
	events := []Event{
		{Type: "conversations", Account: "a", Conversations: []Conversation{{LocalID: "t", Kind: "group", Transport: "rcs"}}},
		{Type: "messages", Account: "a", Conversation: "t", Messages: []Message{{LocalID: "m", Sender: "p"}}},
		{Type: "status", Account: "a", Conversation: "t", Message: "m", Status: "failed:encryption"},
	}
	for _, evt := range events {
		raw := strings.ToLower(string(marshal(t, evt)))
		for _, forbidden := range []string{"bugle", "tachyon", "ukey", "proto", "gaia", "sapisid", "cookie", "token"} {
			if strings.Contains(raw, forbidden) {
				t.Errorf("event leaks %q: %s", forbidden, raw)
			}
		}
	}
}

func TestChunkedEmissionStaysUnderBound(t *testing.T) {
	var got []Event
	sess := &Session{account: "a", emit: func(e Event) { got = append(got, e) }}
	threads := make([]Conversation, 0, 400)
	for i := 0; i < 400; i++ {
		threads = append(threads, Conversation{
			LocalID:      "thread",
			Kind:         "direct",
			Transport:    "rcs",
			Participants: []Participant{{LocalID: "p"}, {LocalID: "q"}},
			Capabilities: rcsCaps,
		})
	}
	sess.emitConversations(threads, 7, true)
	if len(got) < 2 {
		t.Fatalf("400 threads must chunk, got %d events", len(got))
	}
	total := 0
	for i, evt := range got {
		raw, _ := json.Marshal(evt)
		if len(raw) > maxEventBytes {
			t.Errorf("chunk %d is %d bytes", i, len(raw))
		}
		if evt.Generation != 7 {
			t.Errorf("chunk %d must share generation 7, got %d", i, evt.Generation)
		}
		if i < len(got)-1 && evt.Full {
			t.Errorf("chunk %d must merge, only the closing chunk is authoritative", i)
		}
		if i == len(got)-1 && !evt.Full {
			t.Error("closing chunk must be authoritative")
		}
		total += len(evt.Conversations)
	}
	if total != len(threads) {
		t.Errorf("chunks cover %d of %d threads", total, len(threads))
	}

	// An incomplete sync merges without authority: no chunk may
	// carry full or a generation, so the daemon cannot mistake the
	// subset for the whole library.
	got = nil
	sess.emitConversations(threads[:10], 0, false)
	if len(got) == 0 {
		t.Fatal("merge-only emission must still emit")
	}
	for i, evt := range got {
		if evt.Full {
			t.Errorf("chunk %d of an incomplete sync must merge", i)
		}
		if evt.Generation != 0 {
			t.Errorf("chunk %d of an incomplete sync must be ungrouped", i)
		}
	}

	got = nil
	msgs := make([]Message, 0, 30)
	for i := 0; i < 30; i++ {
		msgs = append(msgs, Message{LocalID: "m", Sender: "p", Text: string(make([]byte, 3000))})
	}
	sess.emitMessages("c", msgs, true, "m:1")
	if len(got) < 2 {
		t.Fatalf("30 large messages must chunk, got %d events", len(got))
	}
	for i, evt := range got {
		raw, _ := json.Marshal(evt)
		if len(raw) > maxEventBytes {
			t.Errorf("chunk %d is %d bytes", i, len(raw))
		}
	}
	last := got[len(got)-1]
	if last.CursorNext != "m:1" {
		t.Errorf("cursor rides the last chunk, got %q", last.CursorNext)
	}
	for i, evt := range got {
		if evt.PageComplete != (i == len(got)-1) {
			t.Errorf("only final chunk completes the page: chunk %d complete=%v", i, evt.PageComplete)
		}
	}
	if got[len(got)-1].Full == false {
		t.Error("only the closing chunk is authoritative")
	}
	for i, evt := range got[:len(got)-1] {
		if evt.Full {
			t.Errorf("chunk %d must merge", i)
		}
	}
}

func TestSanitizeText(t *testing.T) {
	if got := sanitizeText("a\r\nb\rc\x00d\u007fe\tf"); got != "a\nbcde\tf" {
		t.Errorf("sanitized = %q", got)
	}
	if got := sanitizeText("plain ✓ text"); got != "plain ✓ text" {
		t.Errorf("readable text altered: %q", got)
	}
}
