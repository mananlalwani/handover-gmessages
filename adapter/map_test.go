package adapter

import (
	"testing"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// ignoredStatuses carry no user-visible lifecycle signal: drafts and
// schedules never left the phone, revocation is internal, and manual
// downloads pending a user tap are represented by the message itself
// once content arrives. They must stay ignored rather than guessed.
var ignoredStatuses = map[gmproto.MessageStatusType]bool{
	gmproto.MessageStatusType_STATUS_UNKNOWN:                  true,
	gmproto.MessageStatusType_OUTGOING_DRAFT:                  true,
	gmproto.MessageStatusType_OUTGOING_SCHEDULED:              true,
	gmproto.MessageStatusType_OUTGOING_REVOCATION_PENDING:     true,
	gmproto.MessageStatusType_INCOMING_YET_TO_MANUAL_DOWNLOAD: true,
}

func TestStatusTaxonomyIsExhaustive(t *testing.T) {
	descriptor := gmproto.MessageStatusType(0).Descriptor()
	seen := map[gmproto.MessageStatusType]bool{}
	for i := 0; i < descriptor.Values().Len(); i++ {
		status := gmproto.MessageStatusType(descriptor.Values().Get(i).Number())
		seen[status] = true
		_, hasSignal := mapStatus(status)
		deleted := isDeletion(status)
		tombstone := isTombstone(status)
		ignored := ignoredStatuses[status]
		classes := 0
		for _, present := range []bool{hasSignal, deleted, tombstone, ignored} {
			if present {
				classes++
			}
		}
		if classes != 1 {
			t.Errorf("status %s classified %d times (want exactly 1)", status, classes)
		}
	}
	if len(seen) < 60 {
		t.Fatalf("only %d statuses enumerated, taxonomy check is stale", len(seen))
	}
}

func TestOutboundLifecycle(t *testing.T) {
	cases := map[gmproto.MessageStatusType]string{
		gmproto.MessageStatusType_OUTGOING_SENDING:                            "accepted",
		gmproto.MessageStatusType_OUTGOING_COMPLETE:                           "sent",
		gmproto.MessageStatusType_OUTGOING_DELIVERED:                          "delivered",
		gmproto.MessageStatusType_OUTGOING_DISPLAYED:                          "displayed",
		gmproto.MessageStatusType_OUTGOING_FAILED_TOO_LARGE:                   "failed:too_large",
		gmproto.MessageStatusType_OUTGOING_FAILED_RECIPIENT_LOST_RCS:          "failed:no_route",
		gmproto.MessageStatusType_OUTGOING_FAILED_TO_ENCRYPT:                  "failed:encryption",
		gmproto.MessageStatusType_INCOMING_FAILED_TO_DECRYPT:                  "failed:encryption",
		gmproto.MessageStatusType_OUTGOING_FAILED_RECIPIENT_NEGATIVE_DELIVERY: "failed:rejected",
		gmproto.MessageStatusType_OUTGOING_FAILED_GENERIC:                     "failed:transport",
	}
	for status, want := range cases {
		if got, ok := mapStatus(status); !ok || got != want {
			t.Errorf("mapStatus(%s) = %q,%v; want %q,true", status, got, ok, want)
		}
	}
}

func TestConversationMapping(t *testing.T) {
	selfIDs := map[string]bool{"self-p": true}
	conv := &gmproto.Conversation{
		ConversationID: "thread-1",
		Type:           gmproto.ConversationType_RCS,
		IsGroupChat:    false,
		Participants: []*gmproto.Participant{
			{
				ID:       &gmproto.SmallInfo{Number: "+15550000", ParticipantID: "self-p"},
				FullName: "Me",
				IsMe:     true,
			},
			{
				ID:         &gmproto.SmallInfo{Number: "+15550001", ParticipantID: "peer-p"},
				FirstName:  "Peer",
				SimPayload: nil,
			},
		},
		DefaultOutgoingID: "self-p",
	}
	mapped, meta, err := mapConversation(conv, selfIDs)
	if err != nil {
		t.Fatalf("mapConversation: %v", err)
	}
	if mapped.Kind != "direct" || mapped.Transport != "rcs" {
		t.Errorf("kind/transport = %s/%s", mapped.Kind, mapped.Transport)
	}
	if len(mapped.Participants) != 2 {
		t.Fatalf("participants = %d", len(mapped.Participants))
	}
	if !mapped.Participants[0].IsSelf || mapped.Participants[1].IsSelf {
		t.Errorf("self flags wrong: %+v", mapped.Participants)
	}
	if mapped.Participants[1].Address != "+15550001" {
		t.Errorf("address = %q", mapped.Participants[1].Address)
	}
	has := map[string]bool{}
	for _, cap := range mapped.Capabilities {
		has[cap] = true
	}
	for _, want := range []string{"text", "reactions", "read_receipts", "typing_send"} {
		if !has[want] {
			t.Errorf("missing capability %s", want)
		}
	}
	for _, forbidden := range []string{"edit", "members_add", "disappearing"} {
		if has[forbidden] {
			t.Errorf("forbidden capability %s attested", forbidden)
		}
	}
	if meta.outgoingID != "self-p" {
		t.Errorf("outgoingID = %q", meta.outgoingID)
	}

	// Unknown transport stays unknown; empty threads are rejected.
	conv.Type = gmproto.ConversationType_UNKNOWN_CONVERSATION_TYPE
	mapped, _, err = mapConversation(conv, selfIDs)
	if err != nil || mapped.Transport != "unknown" {
		t.Errorf("unknown transport: %v %+v", err, mapped)
	}
	conv.Participants = nil
	if _, _, err := mapConversation(conv, selfIDs); err == nil {
		t.Error("empty participants must be rejected")
	}
	_ = protoreflect.Name("")
}

func TestReactionsDropEmpties(t *testing.T) {
	entries := []*gmproto.ReactionEntry{
		{Data: &gmproto.ReactionData{Unicode: "❤"}, ParticipantIDs: []string{"a", "b"}},
		{Data: &gmproto.ReactionData{Unicode: ""}, ParticipantIDs: []string{"a"}},
		{Data: &gmproto.ReactionData{Unicode: "👍"}},
	}
	mapped := mapReactions(entries)
	if len(mapped) != 1 || mapped[0].Emoji != "❤" || len(mapped[0].ParticipantIDs) != 2 {
		t.Errorf("reactions = %+v", mapped)
	}
}
