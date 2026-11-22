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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
	"github.com/rizkysiregar/marpokat/server/internal/store"
)

const (
	MaxTextLength        = 4000
	maxClientMsgIDLength = 64
	DefaultPageSize      = 30
	MaxPageSize          = 100
)

type Service struct {
	pool *pgxpool.Pool
	q    store.Querier
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool, q: store.New(pool)} }

// SendInput is one message from a user.
type SendInput struct {
	TenantID    uuid.UUID
	ChannelID   uuid.UUID
	SenderID    uuid.UUID
	ClientMsgID string
	Text        string
}

// Validate normalises the input (trimmed text) and reports field errors.
func (in *SendInput) Validate() error {
	errs := httpx.FieldErrors{}
	in.ClientMsgID = strings.TrimSpace(in.ClientMsgID)
	if n := len(in.ClientMsgID); n == 0 || n > maxClientMsgIDLength || !printable(in.ClientMsgID) {
		errs["client_msg_id"] = fmt.Sprintf("is required: 1-%d printable characters, unique per sender and channel (a UUID works)", maxClientMsgIDLength)
	}
	in.Text = strings.TrimSpace(in.Text)
	switch n := utf8.RuneCountInString(in.Text); {
	case n == 0:
		errs["text"] = "must not be empty"
	case n > MaxTextLength:
		errs["text"] = fmt.Sprintf("must be at most %d characters", MaxTextLength)
	}
	return errs.OrNil()
}

func printable(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return utf8.ValidString(s)
}

// Send stores a message from a member. It is idempotent on client_msg_id: sending the same id again returns the
// stored message and created=false (PLAN D9).
func (s *Service) Send(ctx context.Context, in SendInput) (msg Message, created bool, err error) {
	if err := in.Validate(); err != nil {
		return Message{}, false, err
	}
	member, err := s.q.GetMember(ctx, store.GetMemberParams{ChannelID: in.ChannelID, UserID: in.SenderID, TenantID: in.TenantID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && member.LeftAt != nil) {
		return Message{}, false, httpx.ErrNotFound("channel not found")
	}
	if err != nil {
		return Message{}, false, fmt.Errorf("message: get member: %w", err)
	}
	if member.ChannelFrozenAt != nil {
		return Message{}, false, httpx.ErrFrozen()
	}
	if existing, ok, err := s.byClientID(ctx, in); err != nil || ok {
		return existing, false, err
	}

	var appended Appended
	cmid := in.ClientMsgID
	text := in.Text
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		var err error
		appended, err = Append(ctx, q, AppendInput{
			TenantID: in.TenantID, ChannelID: in.ChannelID,
			SenderID:    uuid.NullUUID{UUID: in.SenderID, Valid: true},
			ClientMsgID: &cmid, Type: TypeText, Text: &text, RefuseFrozen: true,
		})
		if err != nil {
			return err
		}
		// sending marks the channel read up to your own message
		_, err = q.MarkRead(ctx, store.MarkReadParams{Seq: appended.Seq, ChannelID: in.ChannelID, UserID: in.SenderID, TenantID: in.TenantID})
		return err
	})
	switch {
	case errors.Is(err, ErrNotAppendable):
		return Message{}, false, httpx.ErrFrozen() // the channel was frozen after the check above
	case isUniqueViolation(err):
		// the same client_msg_id arrived twice at once: the other request won, answer with its message
		existing, ok, lerr := s.byClientID(ctx, in)
		if lerr != nil || !ok {
			return Message{}, false, errors.Join(err, lerr)
		}
		return existing, false, nil
	case err != nil:
		return Message{}, false, fmt.Errorf("message: send: %w", err)
	}
	d, err := s.q.GetMessageBySeq(ctx, store.GetMessageBySeqParams{ChannelID: in.ChannelID, TenantID: in.TenantID, Seq: appended.Seq})
	if err != nil {
		return Message{}, false, fmt.Errorf("message: load sent message: %w", err)
	}
	return FromDetail(d), true, nil
}

func (s *Service) byClientID(ctx context.Context, in SendInput) (Message, bool, error) {
	d, err := s.q.GetMessageByClientID(ctx, store.GetMessageByClientIDParams{
		ChannelID: in.ChannelID, TenantID: in.TenantID,
		SenderID:    uuid.NullUUID{UUID: in.SenderID, Valid: true},
		ClientMsgID: &in.ClientMsgID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return Message{}, false, nil
	}
	if err != nil {
		return Message{}, false, fmt.Errorf("message: lookup client_msg_id: %w", err)
	}
	return FromDetail(d), true, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// requireMember is the access check of every read: a channel you are not in (or that is in another tenant) does
// not exist for you.
func (s *Service) requireMember(ctx context.Context, tenantID, channelID, userID uuid.UUID) error {
	m, err := s.q.GetMember(ctx, store.GetMemberParams{ChannelID: channelID, UserID: userID, TenantID: tenantID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && m.LeftAt != nil) {
		return httpx.ErrNotFound("channel not found")
	}
	if err != nil {
		return fmt.Errorf("message: get member: %w", err)
	}
	return nil
}

// History returns up to limit messages older than beforeSeq (0: from the latest), newest first.
func (s *Service) History(ctx context.Context, tenantID, channelID, userID uuid.UUID, beforeSeq int64, limit int) ([]Message, bool, error) {
	if err := s.requireMember(ctx, tenantID, channelID, userID); err != nil {
		return nil, false, err
	}
	rows, err := s.q.ListMessages(ctx, store.ListMessagesParams{
		ChannelID: channelID, TenantID: tenantID, BeforeSeq: beforeSeq, Lim: int32(limit + 1), //nolint:gosec // limit is capped at 100
	})
	if err != nil {
		return nil, false, fmt.Errorf("message: list: %w", err)
	}
	return page(rows, limit)
}

// SyncPage is the answer of Sync.
type SyncPage struct {
	Messages       []Message `json:"messages"`
	NextRev        int64     `json:"next_rev"`
	HasMore        bool      `json:"has_more"`
	ChannelLastSeq int64     `json:"channel_last_seq"`
}

// Sync returns the messages that changed after sinceRev, oldest change first. The cursor to continue from is the
// rev_seq of the last message returned, never the channel counter: a change committed while this ran has a higher
// value than what was returned and must not be skipped.
func (s *Service) Sync(ctx context.Context, tenantID, channelID, userID uuid.UUID, sinceRev int64, limit int) (SyncPage, error) {
	if err := s.requireMember(ctx, tenantID, channelID, userID); err != nil {
		return SyncPage{}, err
	}
	rows, err := s.q.SyncMessages(ctx, store.SyncMessagesParams{
		ChannelID: channelID, TenantID: tenantID, RevSeq: sinceRev, Limit: int32(limit + 1), //nolint:gosec // limit is capped at 100
	})
	if err != nil {
		return SyncPage{}, fmt.Errorf("message: sync: %w", err)
	}
	msgs, hasMore, err := page(rows, limit)
	if err != nil {
		return SyncPage{}, err
	}
	ch, err := s.q.GetChannel(ctx, store.GetChannelParams{ID: channelID, TenantID: tenantID})
	if err != nil {
		return SyncPage{}, fmt.Errorf("message: load channel: %w", err)
	}
	next := sinceRev
	if len(msgs) > 0 {
		next = msgs[len(msgs)-1].RevSeq
	}
	return SyncPage{Messages: msgs, NextRev: next, HasMore: hasMore, ChannelLastSeq: ch.LastSeq}, nil
}

func page(rows []store.MessageDetail, limit int) ([]Message, bool, error) {
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	out := make([]Message, 0, len(rows))
	for _, r := range rows {
		out = append(out, FromDetail(r))
	}
	return out, hasMore, nil
}

// MarkRead moves the read marker of the user forward and returns where it is now.
func (s *Service) MarkRead(ctx context.Context, tenantID, channelID, userID uuid.UUID, seq int64) (int64, error) {
	if seq < 0 {
		return 0, httpx.FieldErrors{"seq": "must be 0 or more"}
	}
	got, err := s.q.MarkRead(ctx, store.MarkReadParams{Seq: seq, ChannelID: channelID, UserID: userID, TenantID: tenantID})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, httpx.ErrNotFound("channel not found")
	}
	if err != nil {
		return 0, fmt.Errorf("message: mark read: %w", err)
	}
	return got, nil
}

// SendSystem posts a system message from the tenant backend (server API).
func (s *Service) SendSystem(ctx context.Context, tenantID, channelID uuid.UUID, text string, metadata map[string]any) (Message, error) {
	text = strings.TrimSpace(text)
	switch n := utf8.RuneCountInString(text); {
	case n == 0:
		return Message{}, httpx.FieldErrors{"text": "must not be empty"}
	case n > MaxTextLength:
		return Message{}, httpx.FieldErrors{"text": fmt.Sprintf("must be at most %d characters", MaxTextLength)}
	}
	var appended Appended
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		appended, err = Append(ctx, store.New(tx), AppendInput{
			TenantID: tenantID, ChannelID: channelID, Type: TypeSystem, Text: &text, Metadata: metadata,
		})
		return err
	})
	if errors.Is(err, ErrNotAppendable) {
		return Message{}, httpx.ErrNotFound("channel not found")
	}
	if err != nil {
		return Message{}, fmt.Errorf("message: send system: %w", err)
	}
	d, err := s.q.GetMessageBySeq(ctx, store.GetMessageBySeqParams{ChannelID: channelID, TenantID: tenantID, Seq: appended.Seq})
	if err != nil {
		return Message{}, fmt.Errorf("message: load system message: %w", err)
	}
	return FromDetail(d), nil
}
