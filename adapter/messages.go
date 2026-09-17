package adapter

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"go.mau.fi/mautrix-gmessages/pkg/libgm/gmproto"
)

// mapMessage converts one relay message into wire form. When download
// is false (bulk sync windows), media parts keep metadata with no
// staged path: bytes resolve on explicit history fetches and live
// deliveries instead of stalling the sync on hundreds of downloads.
func (s *Session) mapMessage(convID string, msg *gmproto.Message, download bool) (*Message, string) {
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
		cleaned := make([]string, 0, len(texts))
		for _, part := range texts {
			cleaned = append(cleaned, sanitizeText(part))
		}
		joined := strings.Join(cleaned, "\n")
		out.Text = joined
	}
	for i, part := range media {
		attachment := s.stageMedia(convID, msg, part, i, download)
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

// sanitizeText normalizes relay text into the daemon's accepted shape:
// CRLF becomes LF, lone carriage returns are dropped, and control
// characters other than \n and \t are removed. Real phone text
// (notably \r\n line endings) would otherwise be rejected downstream
// and the message lost. Nothing readable is altered.
func sanitizeText(text string) string {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	var sb strings.Builder
	sb.Grow(len(text))
	for _, r := range text {
		if r == '\n' || r == '\t' {
			sb.WriteRune(r)
		} else if r < 0x20 || r == 0x7f {
			continue
		} else {
			sb.WriteRune(r)
		}
	}
	return sb.String()
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
// stageMedia records one media part. With download=true the bytes are
// fetched into staging; otherwise metadata only (bulk sync fast path).
// Bytes are bounded; failures keep metadata with no path.
func (s *Session) stageMedia(convID string, msg *gmproto.Message, part *gmproto.MediaContent, index int, download bool) Attachment {
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
	if !download {
		return attachment
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
