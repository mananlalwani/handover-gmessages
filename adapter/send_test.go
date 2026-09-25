// Copyright (C) 2026 Handover contributors
// SPDX-License-Identifier: AGPL-3.0-only
package adapter

import (
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-gmessages/pkg/libgm"
	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	exhttp "go.mau.fi/util/exhttp"
	"go.mau.fi/util/pblite"
	"google.golang.org/protobuf/proto"
)

type failingRelayTransport struct {
	requests []*http.Request
	body     []byte
}

func (transport *failingRelayTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	transport.requests = append(transport.requests, req)
	transport.body, _ = io.ReadAll(req.Body)
	return nil, errors.New("relay unavailable")
}

func newFailingRelayClient(transport *failingRelayTransport) *libgm.Client {
	return libgm.NewClient(libgm.NewAuthData(), nil, zerolog.Nop(), exhttp.ClientSettings{
		TransportOverride: func(exhttp.ClientSettings) http.RoundTripper { return transport },
	})
}

func relayAction(t *testing.T, body []byte) (gmproto.ActionType, []byte) {
	t.Helper()
	var outer gmproto.OutgoingRPCMessage
	if err := pblite.Unmarshal(body, &outer); err != nil {
		t.Fatalf("decode relay envelope: %v", err)
	}
	var rpc gmproto.OutgoingRPCData
	if err := proto.Unmarshal(outer.GetData().GetMessageData(), &rpc); err != nil {
		t.Fatalf("decode relay action: %v", err)
	}
	return rpc.GetAction(), rpc.GetEncryptedProtoData()
}

func TestTextSendReportsAcceptanceThenTransportFailure(t *testing.T) {
	transport := &failingRelayTransport{}
	client := newFailingRelayClient(transport)
	var events []Event
	sess := &Session{
		account: "personal",
		client:  client,
		metas:   map[string]*convMeta{"thread": {outgoingID: "self"}},
		pending: map[string]pendingSend{},
		emit:    func(event Event) { events = append(events, event) },
	}

	sess.SendText("request", "thread", "hello", "parent")

	if len(transport.requests) != 1 {
		t.Fatalf("relay requests = %d, want 1", len(transport.requests))
	}
	action, encrypted := relayAction(t, transport.body)
	if action != gmproto.ActionType_SEND_MESSAGE {
		t.Errorf("relay action = %v, want SEND_MESSAGE", action)
	}
	decoded, err := client.AuthData.RequestCrypto.Decrypt(encrypted)
	if err != nil {
		t.Fatalf("decrypt local test payload: %v", err)
	}
	var sent gmproto.SendMessageRequest
	if err := proto.Unmarshal(decoded, &sent); err != nil {
		t.Fatalf("decode text request: %v", err)
	}
	if sent.GetConversationID() != "thread" || sent.GetReply().GetMessageID() != "parent" || sent.GetForceRCS() {
		t.Errorf("text routing, reply, or send mode changed: %+v", &sent)
	}
	infos := sent.GetMessagePayload().GetMessageInfo()
	if len(infos) != 1 || infos[0].GetMessageContent().GetContent() != "hello" {
		t.Errorf("outbound message content = %+v", infos)
	}
	if len(events) != 3 {
		t.Fatalf("events = %+v, want acceptance, status, failure", events)
	}
	if events[0].Type != "command_result" || events[0].RequestID != "request" || !events[0].OK {
		t.Errorf("command acceptance = %+v", events[0])
	}
	if events[1].Type != "status" || events[1].Status != "accepted" || events[1].Conversation != "thread" || events[1].Message == "" {
		t.Errorf("accepted status = %+v", events[1])
	}
	if events[2].Type != "status" || events[2].Status != "failed:transport" || events[2].Message != events[1].Message {
		t.Errorf("transport failure = %+v", events[2])
	}
	if len(sess.pending) != 0 {
		t.Errorf("failed send retained pending correlation: %+v", sess.pending)
	}
}

func TestRelayFailureDoesNotInventActionSuccess(t *testing.T) {
	tests := []struct {
		name   string
		action gmproto.ActionType
		call   func(*Session)
		want   Event
	}{
		{"delete", gmproto.ActionType_DELETE_MESSAGE,
			func(s *Session) { s.DeleteMessage("request", "message") },
			Event{Type: "command_result", RequestID: "request", Error: "delete failed"}},
		{"mark read", gmproto.ActionType_MESSAGE_READ,
			func(s *Session) { s.MarkRead("thread", "message") },
			Event{Type: "error", Message: "mark read failed"}},
		{"typing", gmproto.ActionType_TYPING_UPDATES,
			func(s *Session) { s.Typing("thread") },
			Event{Type: "error", Message: "typing ping failed"}},
		{"open group", gmproto.ActionType_GET_OR_CREATE_CONVERSATION,
			func(s *Session) { s.Open("request", []string{"+15550000001", "+15550000002"}) },
			Event{Type: "command_result", RequestID: "request", Error: "open failed"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			transport := &failingRelayTransport{}
			var events []Event
			sess := &Session{
				account: "personal", client: newFailingRelayClient(transport),
				metas: map[string]*convMeta{}, log: zerolog.Nop(),
				emit: func(event Event) { events = append(events, event) },
			}
			test.call(sess)
			if len(transport.requests) != 1 {
				t.Fatalf("relay requests = %d, want 1", len(transport.requests))
			}
			if action, _ := relayAction(t, transport.body); action != test.action {
				t.Errorf("relay action = %v, want %v", action, test.action)
			}
			if len(events) != 1 || events[0].Type != test.want.Type ||
				events[0].RequestID != test.want.RequestID || events[0].Error != test.want.Error ||
				events[0].Message != test.want.Message || events[0].OK {
				t.Errorf("events = %+v, want one failure %+v", events, test.want)
			}
		})
	}
}

func TestMediaUploadFailureDoesNotReportAcceptance(t *testing.T) {
	store := &Store{dir: t.TempDir()}
	stageDir, err := store.StageDir()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(stageDir, "picture.png")
	if err := os.WriteFile(path, []byte("not an actual image"), 0o600); err != nil {
		t.Fatal(err)
	}
	transport := &failingRelayTransport{}
	var events []Event
	sess := &Session{
		account: "personal", store: store, client: newFailingRelayClient(transport),
		metas: map[string]*convMeta{"thread": {outgoingID: "self"}},
		emit:  func(event Event) { events = append(events, event) },
	}

	sess.SendMedia("request", "thread", path, "caption")

	if len(transport.requests) != 1 {
		t.Fatalf("upload requests = %d, want 1", len(transport.requests))
	}
	if len(events) != 1 || events[0].Type != "command_result" || events[0].OK ||
		events[0].RequestID != "request" || events[0].Error != "media upload failed" {
		t.Errorf("events after failed upload = %+v", events)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("consumed staging copy remains: %v", err)
	}
}
