// Package message stores and serves messages (PLAN M2, M5). A channel has one counter (PLAN D8): a new message takes the
// next value as its seq (its position, never changes) and as its rev_seq (the counter value of its latest change).
package message

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/rizkysiregar/marpokat/server/internal/store"
	"github.com/rizkysiregar/marpokat/server/internal/user"
)

const (
	TypeText   = "text"
	TypeMedia  = "media"
	TypeSystem = "system"
)

// Message is the public shape (openapi components/schemas/Message).
type Message struct {
	ID          uuid.UUID       `json:"id"`
	ChannelID   uuid.UUID       `json:"channel_id"`
	Seq         int64           `json:"seq"`
	RevSeq      int64           `json:"rev_seq"`
	ClientMsgID *string         `json:"client_msg_id"`
	Type        string          `json:"type"`
	Sender      *user.User      `json:"sender"`
	Text        *string         `json:"text"`
	Attachments []Attachment    `json:"attachments"`
	ReplyTo     *Preview        `json:"reply_to"`
	Reactions   Reactions       `json:"reactions"`
	Metadata    json.RawMessage `json:"metadata"`
	CreatedAt   time.Time       `json:"created_at"`
	EditedAt    *time.Time      `json:"edited_at"`
	DeletedAt   *time.Time      `json:"deleted_at"`

	// replyToID is what ReplyTo is made from by Hydrate.
	replyToID uuid.UUID
}

// Preview is the short form of a replied-to message.
type Preview struct {
	ID             uuid.UUID `json:"id"`
	Seq            int64     `json:"seq"`
	SenderID       *string   `json:"sender_id"`
	Text           *string   `json:"text"`
	HasAttachments bool      `json:"has_attachments"`
	Deleted        bool      `json:"deleted"`
}

// Reactions summarises reactions on a message. Mine is computed for the reader.
type Reactions struct {
	Counts map[string]int `json:"counts"`
	Mine   []string       `json:"mine"`
}

// FromDetail converts a row of the message_details view. Attachments, the preview of a reply and the reader's own
// reactions come from Hydrate, which needs more queries than a plain row has.
func FromDetail(d store.MessageDetail) Message {
	m := Message{
		ID:          d.ID,
		ChannelID:   d.ChannelID,
		Seq:         d.Seq,
		RevSeq:      d.RevSeq,
		ClientMsgID: d.ClientMsgID,
		Type:        d.Type,
		Text:        d.Text,
		Attachments: []Attachment{},
		Reactions:   Reactions{Counts: map[string]int{}, Mine: []string{}},
		Metadata:    rawJSON(d.Metadata, "{}"),
		CreatedAt:   d.CreatedAt,
		EditedAt:    d.EditedAt,
		DeletedAt:   d.DeletedAt,
	}
	if d.ReplyToID.Valid {
		m.replyToID = d.ReplyToID.UUID
	}
	if counts := map[string]int{}; json.Unmarshal([]byte(d.ReactionCounts), &counts) == nil && counts != nil {
		m.Reactions.Counts = counts
	}
	if d.DeletedAt != nil { // a tombstone keeps its place and nothing of its content
		m.Text = nil
		m.Reactions.Counts = map[string]int{}
	}
	if d.SenderID.Valid && d.SenderExternalID != nil {
		s := user.User{
			ID:         d.SenderID.UUID,
			ExternalID: *d.SenderExternalID,
			AvatarURL:  d.SenderAvatarUrl,
			Metadata:   rawJSON(deref(d.SenderMetadata), "{}"),
			Deleted:    d.SenderDeletedAt != nil,
		}
		if d.SenderName != nil {
			s.Name = *d.SenderName
		}
		m.Sender = &s
	}
	return m
}

func rawJSON(s, fallback string) json.RawMessage {
	if s == "" {
		return json.RawMessage(fallback)
	}
	return json.RawMessage(s)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
