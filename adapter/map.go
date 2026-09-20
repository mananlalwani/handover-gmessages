// Copyright (C) 2026 Handover contributors
// SPDX-License-Identifier: AGPL-3.0-only
package adapter

import (
	"fmt"
	"strings"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

// MaxStagedBytes caps staged attachments, mirroring the daemon bound.
const MaxStagedBytes = 50 * 1024 * 1024

// selfSentinel covers the well-known self participant key.
const selfSentinel = "1"

// Capabilities attested per conversation kind. Names must stay within the
// closed Handover set; there is intentionally no edit, membership, or
// disappearing capability.
var (
	rcsCaps = []string{
		"text", "media", "reactions", "replies", "read_receipts",
		"typing_send", "typing_receive", "group_create",
		"message_delete_own", "attachment_download",
	}
	smsCaps = []string{"text"}
)

// mapTransport converts the relay conversation type. There is no MMS
// conversation type upstream; MMS-ness is per-message, so SMS threads
// stay "sms" rather than guessing.
func mapTransport(t gmproto.ConversationType) string {
	switch t {
	case gmproto.ConversationType_RCS:
		return "rcs"
	case gmproto.ConversationType_SMS:
		return "sms"
	default:
		return "unknown"
	}
}

// senderKey returns the opaque sender key for a participant: the relay
// participant ID when present, otherwise the raw number.
func senderKey(id *gmproto.SmallInfo) string {
	if id == nil {
		return ""
	}
	if id.GetParticipantID() != "" {
		return id.GetParticipantID()
	}
	return id.GetNumber()
}

// mapParticipant converts one relay participant. Display names and numbers
// pass through as hints; identity is the opaque key only.
func mapParticipant(p *gmproto.Participant, selfIDs map[string]bool) Participant {
	var out Participant
	if p == nil {
		return out
	}
	out.LocalID = senderKey(p.GetID())
	out.IsSelf = p.GetIsMe() || selfIDs[out.LocalID] || out.LocalID == selfSentinel
	if name := p.GetFullName(); name != "" {
		out.DisplayName = name
	} else if name := p.GetFirstName(); name != "" {
		out.DisplayName = name
	}
	out.Address = p.GetID().GetNumber()
	return out
}

// mapConversation converts one relay thread. Unknown shapes are rejected
// rather than guessed; DM threads with more than two members are rejected
// as protocol surprises.
func mapConversation(conv *gmproto.Conversation, selfIDs map[string]bool) (Conversation, *convMeta, error) {
	if conv == nil {
		return Conversation{}, nil, fmt.Errorf("nil conversation")
	}
	if conv.GetConversationID() == "" {
		return Conversation{}, nil, fmt.Errorf("conversation without id")
	}
	kind := "direct"
	if conv.GetIsGroupChat() {
		kind = "group"
	}
	participants := make([]Participant, 0, len(conv.GetParticipants()))
	seen := map[string]bool{}
	selfAddresses := map[string]bool{}
	for _, p := range conv.GetParticipants() {
		if p != nil && p.GetIsMe() && p.GetID().GetNumber() != "" {
			selfAddresses[p.GetID().GetNumber()] = true
		}
	}
	selfKept := false
	for _, p := range conv.GetParticipants() {
		mapped := mapParticipant(p, selfIDs)
		if mapped.LocalID == "" {
			continue
		}
		// Some relay records mark one copy of the local identity as
		// IsMe but return another copy (often named "Me") without the
		// flag. The verified address links those records to the same
		// local identity; do not expose the duplicate as a group member.
		if selfAddresses[mapped.Address] {
			mapped.IsSelf = true
		}
		if seen[mapped.LocalID] {
			continue
		}
		// The relay reports each of the user's own identities (multi-SIM,
		// Fi device switching) as separate participants. Extra self
		// entries merge into the first: they are one local user, and
		// keeping them would mislabel 1:1 threads as multi-party.
		if mapped.IsSelf {
			if selfKept {
				continue
			}
			selfKept = true
		}
		seen[mapped.LocalID] = true
		participants = append(participants, mapped)
	}
	if len(participants) == 0 {
		return Conversation{}, nil, fmt.Errorf("conversation %s has no participants", conv.GetConversationID())
	}
	// A thread with more than two parties is a group conversation even
	// when the relay leaves its group flag unset (observed on older
	// multi-party threads). Participant count is relay-attested data,
	// so this promotes by evidence, not by guess.
	if len(participants) > 2 {
		kind = "group"
	}
	caps := smsCaps
	if conv.GetType() == gmproto.ConversationType_RCS {
		caps = rcsCaps
	}
	out := Conversation{
		LocalID:      conv.GetConversationID(),
		Kind:         kind,
		Transport:    mapTransport(conv.GetType()),
		Participants: participants,
		Capabilities: append([]string(nil), caps...),
	}
	if kind == "group" && conv.GetName() != "" {
		out.Title = conv.GetName()
	}
	if conv.GetLatestMessageID() != "" {
		out.LatestMessage = conv.GetLatestMessageID()
	}
	out.LastActivityAt = conv.GetLastMessageTimestamp()
	meta := &convMeta{
		outgoingID: conv.GetDefaultOutgoingID(),
		convType:   conv.GetType(),
		sendMode:   conv.GetSendMode(),
		isGroup:    conv.GetIsGroupChat(),
	}
	return out, meta, nil
}

// convMeta caches the per-thread relay metadata needed for outbound RPCs.
type convMeta struct {
	outgoingID string
	convType   gmproto.ConversationType
	sendMode   gmproto.ConversationSendMode
	isGroup    bool
}

// mapStatus converts a relay message status into a Handover status token.
// The second return is false when the status carries no lifecycle signal
// (drafts, schedules, tombstones): the caller must not invent one.
func mapStatus(status gmproto.MessageStatusType) (string, bool) {
	switch status {
	case gmproto.MessageStatusType_OUTGOING_SENDING,
		gmproto.MessageStatusType_OUTGOING_YET_TO_SEND,
		gmproto.MessageStatusType_OUTGOING_VALIDATING,
		gmproto.MessageStatusType_OUTGOING_SEND_AFTER_PROCESSING,
		gmproto.MessageStatusType_OUTGOING_RESENDING,
		gmproto.MessageStatusType_OUTGOING_NOT_DELIVERED_YET,
		gmproto.MessageStatusType_OUTGOING_AWAITING_RETRY,
		gmproto.MessageStatusType_OUTGOING_RESTRICTED,
		gmproto.MessageStatusType_INCOMING_AUTO_DOWNLOADING,
		gmproto.MessageStatusType_INCOMING_MANUAL_DOWNLOADING,
		gmproto.MessageStatusType_INCOMING_RETRYING_AUTO_DOWNLOAD,
		gmproto.MessageStatusType_INCOMING_RETRYING_MANUAL_DOWNLOAD,
		gmproto.MessageStatusType_INCOMING_AWAITING_AUTO_DOWNLOAD:
		return "accepted", true
	case gmproto.MessageStatusType_OUTGOING_COMPLETE,
		gmproto.MessageStatusType_INCOMING_COMPLETE:
		return "sent", true
	case gmproto.MessageStatusType_OUTGOING_DELIVERED,
		gmproto.MessageStatusType_INCOMING_DELIVERED:
		return "delivered", true
	case gmproto.MessageStatusType_OUTGOING_DISPLAYED,
		gmproto.MessageStatusType_INCOMING_DISPLAYED:
		return "displayed", true
	case gmproto.MessageStatusType_OUTGOING_FAILED_TOO_LARGE,
		gmproto.MessageStatusType_INCOMING_DOWNLOAD_FAILED_TOO_LARGE:
		return "failed:too_large", true
	case gmproto.MessageStatusType_OUTGOING_FAILED_RECIPIENT_LOST_RCS,
		gmproto.MessageStatusType_OUTGOING_FAILED_NO_RETRY_NO_FALLBACK:
		return "failed:no_route", true
	case gmproto.MessageStatusType_OUTGOING_FAILED_TO_ENCRYPT,
		gmproto.MessageStatusType_OUTGOING_FAILED_RECIPIENT_DID_NOT_DECRYPT,
		gmproto.MessageStatusType_OUTGOING_FAILED_RECIPIENT_DID_NOT_DECRYPT_NO_MORE_RETRY,
		gmproto.MessageStatusType_OUTGOING_FAILED_RECIPIENT_LOST_ENCRYPTION,
		gmproto.MessageStatusType_INCOMING_FAILED_TO_DECRYPT,
		gmproto.MessageStatusType_INCOMING_DECRYPTION_ABORTED:
		return "failed:encryption", true
	case gmproto.MessageStatusType_OUTGOING_FAILED_RECIPIENT_NEGATIVE_DELIVERY:
		return "failed:rejected", true
	case gmproto.MessageStatusType_OUTGOING_CANCELED,
		gmproto.MessageStatusType_OUTGOING_DELETED,
		gmproto.MessageStatusType_OUTGOING_FAILED_GENERIC,
		gmproto.MessageStatusType_OUTGOING_FAILED_EMERGENCY_NUMBER,
		gmproto.MessageStatusType_MESSAGE_STATUS_OUTGOING_FAILED_EMERGENCY_PROTOCOL_DETERMINATION_MESSAGE,
		gmproto.MessageStatusType_INCOMING_DOWNLOAD_FAILED,
		gmproto.MessageStatusType_INCOMING_DOWNLOAD_CANCELED,
		gmproto.MessageStatusType_INCOMING_DOWNLOAD_FAILED_SIM_HAS_NO_DATA,
		gmproto.MessageStatusType_INCOMING_DOWNLOAD_RESTRICTED,
		gmproto.MessageStatusType_INCOMING_EXPIRED_OR_NOT_AVAILABLE,
		gmproto.MessageStatusType_INCOMING_UNKNOWN_CONTENT_TYPE:
		return "failed:transport", true
	default:
		return "", false
	}
}

// isDeletion reports relay statuses that mean the message is gone.
func isDeletion(status gmproto.MessageStatusType) bool {
	return status == gmproto.MessageStatusType_MESSAGE_DELETED ||
		status == gmproto.MessageStatusType_INCOMING_DELETED
}

// isTombstone reports protocol-switch/notice tombstones, which carry no
// user content and must never become messages.
func isTombstone(status gmproto.MessageStatusType) bool {
	name := status.String()
	return strings.Contains(name, "TOMBSTONE")
}

// textParts extracts text content parts in order.
func textParts(msg *gmproto.Message) []string {
	var out []string
	for _, info := range msg.GetMessageInfo() {
		if content := info.GetMessageContent(); content != nil {
			out = append(out, content.GetContent())
		}
	}
	return out
}

// mediaParts extracts media content parts in order.
func mediaParts(msg *gmproto.Message) []*gmproto.MediaContent {
	var out []*gmproto.MediaContent
	for _, info := range msg.GetMessageInfo() {
		if media := info.GetMediaContent(); media != nil {
			out = append(out, media)
		}
	}
	return out
}

// mapReactions converts relay reaction entries. Counts derive from the
// reactor lists; empty aggregates are dropped.
func mapReactions(entries []*gmproto.ReactionEntry) []Reaction {
	var out []Reaction
	for _, entry := range entries {
		data := entry.GetData()
		emoji := data.GetUnicode()
		if emoji == "" {
			continue
		}
		reactors := append([]string(nil), entry.GetParticipantIDs()...)
		if len(reactors) == 0 {
			continue
		}
		out = append(out, Reaction{Emoji: emoji, ParticipantIDs: reactors})
	}
	return out
}
