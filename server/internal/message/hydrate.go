package message

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/rizkysiregar/marpokat/server/internal/store"
)

// previewLength is how much of the text of a replied-to message shows in a reply.
const previewLength = 200

// Hydrator fills in what a plain message row does not carry: its attachments (with fresh download links), the preview
// of the message it replies to, and which reactions the reader made. Each of the three costs a query, and only for
// the messages that need it: a text message without reply and without reactions costs nothing.
type Hydrator struct {
	q      store.Querier
	signer AttachmentSigner // nil: attachments come without links
}

func NewHydrator(q store.Querier, signer AttachmentSigner) *Hydrator {
	return &Hydrator{q: q, signer: signer}
}

func csv(ids []uuid.UUID) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = id.String()
	}
	return strings.Join(parts, ",")
}

// Hydrate completes the messages for the reader viewerID, in place. A nil Hydrator does nothing.
func (h *Hydrator) Hydrate(ctx context.Context, tenantID, viewerID uuid.UUID, msgs []Message) error {
	if h == nil || len(msgs) == 0 {
		return nil
	}
	var mediaIDs, reactedIDs, replyIDs []uuid.UUID
	byID := make(map[uuid.UUID][]int, len(msgs))
	for i := range msgs {
		m := &msgs[i]
		byID[m.ID] = append(byID[m.ID], i)
		if m.DeletedAt != nil {
			continue
		}
		if m.Type == TypeMedia {
			mediaIDs = append(mediaIDs, m.ID)
		}
		if len(m.Reactions.Counts) > 0 {
			reactedIDs = append(reactedIDs, m.ID)
		}
		if m.replyToID != uuid.Nil {
			replyIDs = append(replyIDs, m.replyToID)
		}
	}

	if len(mediaIDs) > 0 {
		rows, err := h.q.ListMessageAttachments(ctx, csv(mediaIDs))
		if err != nil {
			return fmt.Errorf("message: load attachments: %w", err)
		}
		for _, r := range rows {
			a := AttachmentFrom(store.Attachment{
				ID: r.ID, TenantID: r.TenantID, UploaderID: r.UploaderID, ObjectKey: r.ObjectKey, Mime: r.Mime, Size: r.Size,
				FileName: r.FileName, Width: r.Width, Height: r.Height, DurationMs: r.DurationMs, ThumbKey: r.ThumbKey,
				Blurhash: r.Blurhash, Status: r.Status, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
			}, h.signer)
			for _, i := range byID[r.MessageID] {
				msgs[i].Attachments = append(msgs[i].Attachments, a)
			}
		}
	}

	if len(reactedIDs) > 0 {
		rows, err := h.q.ListMyReactions(ctx, store.ListMyReactionsParams{UserID: viewerID, MessageIds: csv(reactedIDs)})
		if err != nil {
			return fmt.Errorf("message: load reactions: %w", err)
		}
		for _, r := range rows {
			for _, i := range byID[r.MessageID] {
				msgs[i].Reactions.Mine = append(msgs[i].Reactions.Mine, r.Emoji)
			}
		}
	}

	if len(replyIDs) > 0 {
		rows, err := h.q.ListMessagePreviews(ctx, store.ListMessagePreviewsParams{TenantID: tenantID, MessageIds: csv(replyIDs)})
		if err != nil {
			return fmt.Errorf("message: load replied messages: %w", err)
		}
		previews := make(map[uuid.UUID]Preview, len(rows))
		for _, r := range rows {
			p := Preview{ID: r.ID, Seq: r.Seq, SenderID: r.SenderExternalID, HasAttachments: r.HasAttachments, Deleted: r.DeletedAt != nil}
			if r.DeletedAt == nil && r.Text != nil {
				t := truncate(*r.Text, previewLength)
				p.Text = &t
			}
			previews[r.ID] = p
		}
		for i := range msgs {
			if p, ok := previews[msgs[i].replyToID]; ok && msgs[i].DeletedAt == nil {
				msgs[i].ReplyTo = &p
			}
		}
	}
	return nil
}

// HydrateOne is Hydrate for a single message.
func (h *Hydrator) HydrateOne(ctx context.Context, tenantID, viewerID uuid.UUID, m *Message) error {
	one := []Message{*m}
	if err := h.Hydrate(ctx, tenantID, viewerID, one); err != nil {
		return err
	}
	*m = one[0]
	return nil
}

func truncate(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	n := 0
	for i := range s {
		if n == max {
			return s[:i]
		}
		n++
	}
	return s
}
