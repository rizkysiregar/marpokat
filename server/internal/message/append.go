package message

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rizkysiregar/marpokat/server/internal/outbox"
	"github.com/rizkysiregar/marpokat/server/internal/store"
)

// ErrNotAppendable: the channel does not exist, or is frozen when frozen channels are refused.
var ErrNotAppendable = errors.New("message: channel does not accept the message")

// AppendInput is one new message. q passed to Append must be the transaction of the caller.
type AppendInput struct {
	TenantID    uuid.UUID
	ChannelID   uuid.UUID
	SenderID    uuid.NullUUID // invalid for system messages
	ClientMsgID *string
	Type        string
	Text        *string
	ReplyToID   uuid.NullUUID
	// AttachmentIDs are linked to the message, in this order. The caller has checked them (see Service.Send).
	AttachmentIDs []uuid.UUID
	Metadata      map[string]any
	// RefuseFrozen makes a frozen channel reject the message. System messages are allowed into frozen channels.
	RefuseFrozen bool
}

// Appended is what a new message got.
type Appended struct {
	ID  uuid.UUID
	Seq int64
}

// Append takes the next channel counter value, stores the message with it and queues the outbox event, all through
// q. The channel row stays locked until the transaction ends, so messages of one channel commit in seq order.
func Append(ctx context.Context, q store.Querier, in AppendInput) (Appended, error) {
	bumped, err := q.BumpChannelSeq(ctx, store.BumpChannelSeqParams{ID: in.ChannelID, TenantID: in.TenantID, CheckFrozen: in.RefuseFrozen})
	if errors.Is(err, pgx.ErrNoRows) {
		return Appended{}, ErrNotAppendable
	}
	if err != nil {
		return Appended{}, fmt.Errorf("message: bump channel seq: %w", err)
	}
	md := "{}"
	if len(in.Metadata) > 0 {
		b, err := json.Marshal(in.Metadata)
		if err != nil {
			return Appended{}, fmt.Errorf("message: metadata: %w", err)
		}
		md = string(b)
	}
	id := uuid.Must(uuid.NewV7())
	err = q.InsertMessage(ctx, store.InsertMessageParams{
		ChannelID:   in.ChannelID,
		Seq:         bumped.LastSeq,
		ID:          id,
		TenantID:    in.TenantID,
		SenderID:    in.SenderID,
		ClientMsgID: in.ClientMsgID,
		Type:        in.Type,
		Text:        in.Text,
		ReplyToID:   in.ReplyToID,
		Metadata:    md,
		RevSeq:      bumped.LastSeq,
		CreatedAt:   *bumped.LastMessageAt,
	})
	if err != nil {
		return Appended{}, err // unique violations are interpreted by Send
	}
	for i, attachmentID := range in.AttachmentIDs {
		if err := q.LinkAttachment(ctx, store.LinkAttachmentParams{MessageID: id, AttachmentID: attachmentID, Position: int16(i)}); err != nil { //nolint:gosec // at most 10
			return Appended{}, err
		}
	}
	sender := ""
	if in.SenderID.Valid {
		sender = in.SenderID.UUID.String()
	}
	err = outbox.Enqueue(ctx, q, in.TenantID, outbox.TopicMessageCreated, map[string]any{
		"channel_id": in.ChannelID, "message_id": id, "seq": bumped.LastSeq, "sender_id": sender, "type": in.Type,
	})
	if err != nil {
		return Appended{}, err
	}
	return Appended{ID: id, Seq: bumped.LastSeq}, nil
}
