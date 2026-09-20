// Copyright (C) 2026 Handover contributors
// SPDX-License-Identifier: AGPL-3.0-only
package adapter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fixturePath is the checked-in helper-JSON sample consumed by the
// Handover repository's Rust decoder test. It pins the wire shapes
// this adapter emits so contract drift fails loudly on either side.
//
// Refresh it after any contract change:
//  1. REGENERATE_FIXTURE=1 go test ./adapter/ -run TestContractFixtureIsCurrent
//  2. Copy adapter/testdata/helper-events.jsonl to the Handover
//     repository at crates/handover-gmessages/tests/fixtures/,
//     then run cargo test -p handover-gmessages.
const fixturePath = "testdata/helper-events.jsonl"

func fixtureEvents() []Event {
	conv := func(id string) Conversation {
		return Conversation{
			LocalID:      id,
			Kind:         "direct",
			Transport:    "rcs",
			Participants: []Participant{{LocalID: "peer"}},
			Capabilities: []string{"text"},
		}
	}
	msg := func(id string) Message {
		return Message{LocalID: id, Sender: "peer", Text: "hello"}
	}
	return []Event{
		{Type: "hello", HelperProtocol: HelperProtocol, Name: "handover-gmessages/libgm"},
		{Type: "account", Account: "gmessages:default", Label: "Google Messages", Connected: true, Authenticated: true},
		{Type: "pairing", Account: "gmessages:default", Prompt: "Tap 1 in Google Messages on the phone to confirm linking"},
		{Type: "conversations", Account: "gmessages:default", Conversations: []Conversation{conv("t1")}, Full: false, Generation: 11},
		{Type: "conversations", Account: "gmessages:default", Conversations: []Conversation{conv("t2")}, Full: true, Generation: 11},
		{Type: "conversation_removed", Account: "gmessages:default", Conversation: "t0"},
		{Type: "messages", Account: "gmessages:default", Conversation: "t1", Messages: []Message{msg("m1")}, Full: false, Generation: 12},
		{Type: "messages", Account: "gmessages:default", Conversation: "t1", Messages: []Message{msg("m2")}, Full: true, Generation: 12, CursorNext: "m1:1700000000000", PageComplete: true},
		{Type: "message_removed", Account: "gmessages:default", Conversation: "t1", Message: "m0"},
		{Type: "status", Account: "gmessages:default", Conversation: "t1", Message: "txn-1", Status: "accepted"},
		{Type: "status", Account: "gmessages:default", Conversation: "t1", Message: "m2", Status: "sent"},
		{Type: "status", Account: "gmessages:default", Conversation: "t1", Message: "txn-2", Status: "failed:transport"},
		{Type: "typing", Account: "gmessages:default", Conversation: "t1", Participants: []string{"+15550000000"}},
		{Type: "read", Account: "gmessages:default", Conversation: "t1", LastReadMessage: "m2", Unread: false},
		{Type: "command_result", RequestID: "r1", OK: true},
		{Type: "command_result", RequestID: "r2", OK: false, Error: "send failed"},
		{Type: "error", Account: "gmessages:default", Message: "history fetch failed"},
		{Type: "account_removed", Account: "gmessages:default"},
	}
}

func renderFixture(t *testing.T) string {
	t.Helper()
	var lines []string
	for _, evt := range fixtureEvents() {
		raw, err := json.Marshal(evt)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(raw))
	}
	// Forward compatibility probe: decoders must tolerate keys the
	// current contract does not define.
	probe := `{"type":"messages","account":"gmessages:default","conversation":"t1","messages":[],"page_complete":true,"full":false,"future_flag":true}`
	lines = append(lines, probe)
	return strings.Join(lines, "\n") + "\n"
}

func TestContractFixtureIsCurrent(t *testing.T) {
	want := renderFixture(t)
	if os.Getenv("REGENERATE_FIXTURE") != "" {
		if err := os.MkdirAll(filepath.Dir(fixturePath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fixturePath, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("fixture regenerated at %s", fixturePath)
		return
	}
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("fixture missing (regenerate: %s): %v", fixturePath, err)
	}
	if string(raw) != want {
		t.Fatalf("fixture drifted from emitted shapes; regenerate and sync the Handover copy")
	}
}
