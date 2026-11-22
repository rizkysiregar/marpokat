package message

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rizkysiregar/marpokat/server/internal/outbox"
	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
	"github.com/rizkysiregar/marpokat/server/internal/store"
)

// Changes to a message that exists (edit, delete, reaction) take the next value of the channel counter as the
// message's rev_seq (PLAN D8), under the same channel lock as a new message: a client that syncs from its cursor
// gets them in order, whatever else it missed.

// loadForChange loads a message for someone who wants to change it, or react to it. To a user who is not in the
// channel the message does not exist. A frozen channel is read-only.
func (s *Service) loadForChange(ctx context.Context, tenantID, userID, messageID uuid.UUID) (store.MessageDetail, store.GetMemberRow, error) {
	d, err := s.q.GetMessageByID(ctx, store.GetMessageByIDParams{ID: messageID, TenantID: tenantID})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.MessageDetail{}, store.GetMemberRow{}, httpx.ErrNotFound("message not found")
	}
	if err != nil {
		return store.MessageDetail{}, store.GetMemberRow{}, fmt.Errorf("message: load: %w", err)
	}
	m, err := s.q.GetMember(ctx, store.GetMemberParams{ChannelID: d.ChannelID, UserID: userID, TenantID: tenantID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && m.LeftAt != nil) {
		return store.MessageDetail{}, store.GetMemberRow{}, httpx.ErrNotFound("message not found")
	}
	if err != nil {
		return store.MessageDetail{}, store.GetMemberRow{}, fmt.Errorf("message: get member: %w", err)
	}
	if m.ChannelFrozenAt != nil {
		return store.MessageDetail{}, store.GetMemberRow{}, httpx.ErrFrozen()
	}
	return d, m, nil
}

// reload returns the message as the reader sees it now.
func (s *Service) reload(ctx context.Context, tenantID, viewerID, messageID uuid.UUID) (Message, error) {
	d, err := s.q.GetMessageByID(ctx, store.GetMessageByIDParams{ID: messageID, TenantID: tenantID})
	if err != nil {
		return Message{}, fmt.Errorf("message: reload: %w", err)
	}
	return s.hydrated(ctx, tenantID, viewerID, FromDetail(d)), nil
}

// bumpRev takes the next counter value for a change, inside the transaction of the change.
func bumpRev(ctx context.Context, q store.Querier, tenantID, channelID uuid.UUID) (int64, error) {
	return bumpRevision(ctx, q, tenantID, channelID, true)
}

func bumpRevision(ctx context.Context, q store.Querier, tenantID, channelID uuid.UUID, checkFrozen bool) (int64, error) {
	rev, err := q.BumpChannelRev(ctx, store.BumpChannelRevParams{ID: channelID, TenantID: tenantID, CheckFrozen: checkFrozen})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, httpx.ErrFrozen() // frozen after the check, or gone
	}
	if err != nil {
		return 0, fmt.Errorf("bump channel revision: %w", err)
	}
	return rev, nil
}

// Touch gives a message a new revision without changing its text: something it shows changed, an attachment that
// became ready. It runs inside the transaction of that change, and works in frozen channels too: this is the system
// catching up, not a user writing. Clients learn of it by sync (the message comes back with the new rev_seq).
func Touch(ctx context.Context, q store.Querier, tenantID, channelID, messageID uuid.UUID) (int64, error) {
	rev, err := bumpRevision(ctx, q, tenantID, channelID, false)
	if err != nil {
		return 0, err
	}
	if err := q.ReviseMessage(ctx, store.ReviseMessageParams{RevSeq: rev, ID: messageID, TenantID: tenantID}); err != nil {
		return 0, fmt.Errorf("message: touch: %w", err)
	}
	return rev, outbox.Enqueue(ctx, q, tenantID, outbox.TopicMessageUpdated, map[string]any{"channel_id": channelID, "message_id": messageID, "rev_seq": rev})
}

// Edit replaces the text of the user's own message.
func (s *Service) Edit(ctx context.Context, tenantID, userID, messageID uuid.UUID, text string) (Message, error) {
	text = strings.TrimSpace(text)
	switch n := utf8.RuneCountInString(text); {
	case n == 0:
		return Message{}, httpx.FieldErrors{"text": "must not be empty"}
	case n > MaxTextLength:
		return Message{}, httpx.FieldErrors{"text": fmt.Sprintf("must be at most %d characters", MaxTextLength)}
	}
	d, _, err := s.loadForChange(ctx, tenantID, userID, messageID)
	if err != nil {
		return Message{}, err
	}
	if d.DeletedAt != nil {
		return Message{}, httpx.ErrNotFound("message not found")
	}
	if d.Type == TypeSystem || !d.SenderID.Valid || d.SenderID.UUID != userID {
		return Message{}, httpx.ErrForbidden("only the sender can edit a message")
	}
	if d.Text != nil && *d.Text == text { // nothing changes, so nothing is announced
		return s.reload(ctx, tenantID, userID, messageID)
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		rev, err := bumpRev(ctx, q, tenantID, d.ChannelID)
		if err != nil {
			return err
		}
		if err := q.ReviseMessageText(ctx, store.ReviseMessageTextParams{Text: &text, RevSeq: rev, ID: messageID, TenantID: tenantID}); err != nil {
			return err
		}
		return outbox.Enqueue(ctx, q, tenantID, outbox.TopicMessageUpdated, map[string]any{"channel_id": d.ChannelID, "message_id": messageID, "rev_seq": rev})
	})
	if err != nil {
		return Message{}, wrapUnlessAPI(err, "edit")
	}
	msg, err := s.reload(ctx, tenantID, userID, messageID)
	if err != nil {
		return Message{}, err
	}
	if s.notifier != nil {
		s.notifier.MessageUpdated(ctx, tenantID, msg)
	}
	return msg, nil
}

// Delete removes a message, leaving a tombstone. The sender may delete their own; in a group the owner and the admins
// may delete anyone's. Deleting a message that is already deleted is not an error.
func (s *Service) Delete(ctx context.Context, tenantID, userID, messageID uuid.UUID) error {
	d, member, err := s.loadForChange(ctx, tenantID, userID, messageID)
	if err != nil {
		return err
	}
	if d.DeletedAt != nil {
		return nil
	}
	own := d.SenderID.Valid && d.SenderID.UUID == userID
	moderator := member.ChannelType == "group" && (member.Role == "owner" || member.Role == "admin")
	if d.Type == TypeSystem || (!own && !moderator) {
		return httpx.ErrForbidden("only the sender, or the owner and admins of a group, can delete a message")
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		rev, err := bumpRev(ctx, q, tenantID, d.ChannelID)
		if err != nil {
			return err
		}
		if err := q.ReviseMessageDeleted(ctx, store.ReviseMessageDeletedParams{RevSeq: rev, ID: messageID, TenantID: tenantID}); err != nil {
			return err
		}
		// the files stay for a day (the cleaner removes attachments nobody uses), the message loses them at once
		if err := q.UnlinkMessageAttachments(ctx, messageID); err != nil {
			return err
		}
		if err := q.DeleteMessageReactions(ctx, messageID); err != nil {
			return err
		}
		return outbox.Enqueue(ctx, q, tenantID, outbox.TopicMessageDeleted, map[string]any{"channel_id": d.ChannelID, "message_id": messageID, "rev_seq": rev})
	})
	if err != nil {
		return wrapUnlessAPI(err, "delete")
	}
	if s.notifier != nil {
		if msg, err := s.reload(ctx, tenantID, userID, messageID); err == nil {
			s.notifier.MessageDeleted(ctx, tenantID, msg)
		}
	}
	return nil
}

// normalizeEmoji accepts any short string without spaces or control characters: apps may define their own reactions.
func normalizeEmoji(s string) (string, error) {
	s = strings.TrimSpace(s)
	bad := httpx.FieldErrors{"emoji": "must be 1-64 bytes without spaces or control characters"}
	if s == "" || len(s) > 64 || !utf8.ValidString(s) {
		return "", bad
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return "", bad
		}
	}
	return s, nil
}

// React adds the user's reaction to a message. Reacting twice with the same emoji changes nothing.
func (s *Service) React(ctx context.Context, tenantID, userID, messageID uuid.UUID, emoji string) (Message, error) {
	return s.reaction(ctx, tenantID, userID, messageID, emoji, true)
}

// Unreact removes the user's reaction. Removing one that is not there changes nothing.
func (s *Service) Unreact(ctx context.Context, tenantID, userID, messageID uuid.UUID, emoji string) (Message, error) {
	return s.reaction(ctx, tenantID, userID, messageID, emoji, false)
}

func (s *Service) reaction(ctx context.Context, tenantID, userID, messageID uuid.UUID, emoji string, add bool) (Message, error) {
	emoji, err := normalizeEmoji(emoji)
	if err != nil {
		return Message{}, err
	}
	d, _, err := s.loadForChange(ctx, tenantID, userID, messageID)
	if err != nil {
		return Message{}, err
	}
	if d.DeletedAt != nil {
		return Message{}, httpx.ErrNotFound("message not found")
	}
	if d.Type == TypeSystem {
		return Message{}, httpx.FieldErrors{"emoji": "system messages cannot be reacted to"}
	}
	changed := false
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		var n int64
		var err error
		if add {
			n, err = q.InsertReaction(ctx, store.InsertReactionParams{MessageID: messageID, UserID: userID, Emoji: emoji})
		} else {
			n, err = q.DeleteReaction(ctx, store.DeleteReactionParams{MessageID: messageID, UserID: userID, Emoji: emoji})
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return nil // already so
		}
		if add {
			count, err := q.CountUserReactions(ctx, store.CountUserReactionsParams{MessageID: messageID, UserID: userID})
			if err != nil {
				return err
			}
			if count > MaxReactionsPerUser {
				return httpx.FieldErrors{"emoji": fmt.Sprintf("at most %d different reactions per message", MaxReactionsPerUser)}
			}
		}
		rev, err := bumpRev(ctx, q, tenantID, d.ChannelID)
		if err != nil {
			return err
		}
		delta := int32(1)
		if !add {
			delta = -1
		}
		if err := q.AdjustReactionCount(ctx, store.AdjustReactionCountParams{Emoji: emoji, Delta: delta, RevSeq: rev, ID: messageID, TenantID: tenantID}); err != nil {
			return err
		}
		changed = true
		return outbox.Enqueue(ctx, q, tenantID, outbox.TopicMessageReaction, map[string]any{
			"channel_id": d.ChannelID, "message_id": messageID, "rev_seq": rev, "user_id": userID, "emoji": emoji, "added": add,
		})
	})
	if err != nil {
		return Message{}, wrapUnlessAPI(err, "reaction")
	}
	msg, err := s.reload(ctx, tenantID, userID, messageID)
	if err != nil {
		return Message{}, err
	}
	if changed && s.notifier != nil {
		s.notifier.ReactionChanged(ctx, tenantID, msg, userID, emoji, add)
	}
	return msg, nil
}
