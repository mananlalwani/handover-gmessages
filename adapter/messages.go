package adapter

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

// mapMessage converts one relay message into wire form. It returns
// (nil, removedID) when the relay marks the message deleted, and
// (nil, "") when the record carries no user content (tombstones,
// undecryptable markers without text). Media parts are downloaded and
// staged; parts that cannot be retrieved keep their metadata with no
// staged path rather than dropping the whole message.
func (s *Session) mapMessage(convID string, msg *gmproto.Message) (*Message, string) {
	if msg == nil || msg.GetMessageID() == "" {
		return nil, ""
	}
	status := msg.GetMessageStatus().GetStatus()
	if isTombstone(status) {
		return nil, ""
	}
	if isDeletion(status) {
		return nil, msg.GetMessageID()
	}
	self := s.isSelfSender(convID, msg)
	s.rememberMessage(convID, msg, self)

	out := &Message{
		LocalID: msg.GetMessageID(),
		Sender:  senderKeyOf(msg),
	}
	if ts := msg.GetTimestamp(); ts != 0 {
		ts := ts
		out.SentAt = &ts
	}
	texts := textParts(msg)
	media := mediaParts(msg)
	hasContent := len(texts) > 0 || len(media) > 0
	if len(texts) > 0 {
		joined := strings.Join(texts, "\n")
		out.Text = joined
	}
	for i, part := range media {
		attachment := s.stageMedia(convID, msg, part, i)
		out.Attachments = append(out.Attachments, attachment)
	}
	if reply := msg.GetReplyMessage(); reply != nil && reply.GetMessageID() != "" {
		// Cross-thread replies cannot be represented; keep the message
		// and drop the dangling reference rather than losing content.
		if reply.GetConversationID() == "" || reply.GetConversationID() == convID {
			out.ReplyTo = reply.GetMessageID()
		}
	}
	out.Reactions = mapReactions(msg.GetReactions())
	out.Deleted = false
	if !hasContent && out.Text == "" && len(out.Attachments) == 0 {
		// Status-only or undecryptable marker without content: nothing
		// truthful to show. The lifecycle signal (if any) travels as a
		// status event from the caller, not as message content.
		if _, ok := mapStatus(status); ok && self {
			return nil, ""
		}
		return nil, ""
	}
	return out, ""
}

// senderKeyOf resolves the sender key, preferring the explicit sender
// participant record and falling back to the flat participant id.
func senderKeyOf(msg *gmproto.Message) string {
	if key := senderKey(msg.GetSenderParticipant().GetID()); key != "" {
		return key
	}
	return msg.GetParticipantID()
}

// stageMedia downloads one media part into the staging directory and
// returns its wire record. Bytes are bounded; failures keep metadata
// with no path.
func (s *Session) stageMedia(convID string, msg *gmproto.Message, part *gmproto.MediaContent, index int) Attachment {
	actionID := ""
	if len(msg.GetMessageInfo()) > index {
		actionID = msg.GetMessageInfo()[index].GetActionMessageID()
	}
	localID := actionID
	if localID == "" {
		localID = fmt.Sprintf("%s-part-%d", msg.GetMessageID(), index)
	}
	attachment := Attachment{
		LocalID: localID,
		MIME:    part.GetMimeType(),
		Name:    part.GetMediaName(),
	}
	if size := part.GetSize(); size > 0 {
		size := int64(size)
		sizeU := uint64(size)
		attachment.SizeBytes = &sizeU
	}
	path, err := s.downloadPart(msg.GetMessageID(), actionID, part)
	if err != nil {
		s.log.Debug().Str("message", msg.GetMessageID()).Msg("media unavailable, keeping metadata only")
		return attachment
	}
	attachment.StagedPath = path
	return attachment
}

// downloadPart resolves bytes for one media part: inline data first,
// then the full object, then a thumbnail fallback. For content the
// phone has not uploaded yet, it triggers a full-size re-request once;
// the object arrives later as a live re-delivery, which the event path
// maps into an update. Callers keep metadata with no path meanwhile.
func (s *Session) downloadPart(msgID, actionID string, part *gmproto.MediaContent) (string, error) {
	if data := part.GetMediaData(); len(data) > 0 {
		return s.writeStaged(part.GetMediaName(), part.GetMimeType(), data)
	}
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client == nil {
		return "", fmt.Errorf("not connected")
	}
	if part.GetMediaID() != "" && len(part.GetDecryptionKey()) > 0 {
		if path, err := s.streamStaged(client, part.GetMediaID(), part.GetDecryptionKey(), part.GetMediaName(), part.GetMimeType()); err == nil {
			return path, nil
		}
	}
	if part.GetThumbnailMediaID() != "" && len(part.GetThumbnailDecryptionKey()) > 0 {
		if path, err := s.streamStaged(client, part.GetThumbnailMediaID(), part.GetThumbnailDecryptionKey(), part.GetMediaName(), part.GetMimeType()); err == nil {
			return path, nil
		}
	}
	s.requestFullSize(client, msgID, actionID)
	return "", fmt.Errorf("object pending re-delivery")
}

// fullSizeRequests deduplicates full-size triggers per part.
func (s *Session) requestFullSize(client clientMedia, msgID, actionID string) {
	if actionID == "" {
		return
	}
	key := msgID + "\x00" + actionID
	s.mu.Lock()
	if s.fullReq == nil {
		s.fullReq = map[string]bool{}
	}
	if s.fullReq[key] {
		s.mu.Unlock()
		return
	}
	s.fullReq[key] = true
	s.mu.Unlock()
	ctx, cancel := timeoutCtx()
	defer cancel()
	if _, err := client.GetFullSizeImage(ctx, msgID, actionID); err != nil {
		s.log.Debug().Str("message", msgID).Msg("full-size re-request failed")
	}
}

// sanitizeName reduces a relay-supplied name to one safe basename with
// the same rules as the Handover daemon: no separators, dots, NUL, or
// controls, at most 255 UTF-8 bytes.
func sanitizeName(name string) (string, bool) {
	base := name
	if i := strings.LastIndexAny(base, "/\\"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSpace(base)
	if base == "" || base == "." || base == ".." {
		return "", false
	}
	for _, r := range base {
		if r == 0 || r == '\n' || r == '\r' || r == '\t' || (r < 0x20) || r == 0x7f {
			return "", false
		}
	}
	for len(base) > 255 {
		base = base[:len(base)-1]
	}
	if base == "" || base == "." || base == ".." {
		return "", false
	}
	return base, true
}

// writeStaged stores inline bytes with a unique safe name.
func (s *Session) writeStaged(name, mime string, data []byte) (string, error) {
	if int64(len(data)) > MaxStagedBytes {
		return "", fmt.Errorf("attachment too large")
	}
	safe, ok := sanitizeName(name)
	if !ok || safe == "" {
		safe = "attachment"
	}
	dir, err := s.store.StageDir()
	if err != nil {
		return "", err
	}
	path, err := uniquePath(dir, safe)
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

// streamStaged downloads a relay object into staging with a hard byte cap.
func (s *Session) streamStaged(client clientMedia, mediaID string, key []byte, name, mime string) (string, error) {
	rc, err := client.DownloadMedia(mediaID, key)
	if err != nil {
		return "", err
	}
	defer rc.Close()
	safe, ok := sanitizeName(name)
	if !ok || safe == "" {
		safe = "attachment"
	}
	dir, err := s.store.StageDir()
	if err != nil {
		return "", err
	}
	path, err := uniquePath(dir, safe)
	if err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	wrote, err := io.CopyN(f, rc, MaxStagedBytes+1)
	f.Close()
	if err != nil && err != io.EOF {
		os.Remove(path)
		return "", err
	}
	if wrote > MaxStagedBytes {
		os.Remove(path)
		return "", fmt.Errorf("attachment too large")
	}
	return path, nil
}

func uniquePath(dir, name string) (string, error) {
	candidate := filepath.Join(dir, name)
	if _, err := os.Stat(candidate); os.IsNotExist(err) {
		return candidate, nil
	}
	for i := 2; i < 1000; i++ {
		candidate = filepath.Join(dir, fmt.Sprintf("%d-%s", i, name))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("no staged name available")
}
