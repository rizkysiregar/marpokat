package message

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
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

// Notifier tells connected clients about what just happened, after it was committed (PLAN M3). Delivery is best
// effort: clients that miss an event catch up with sync. Messages are handed over hydrated for the actor; the notifier
// completes them for each receiver where that differs (the reactions a receiver made themselves).
type Notifier interface {
	// MessageCreated: origin is the connection that sent the message, which already knows (it gets an ack), or "".
	MessageCreated(ctx context.Context, tenantID uuid.UUID, msg Message, origin string)
	MessageUpdated(ctx context.Context, tenantID uuid.UUID, msg Message)
	MessageDeleted(ctx context.Context, tenantID uuid.UUID, msg Message)
	ReactionChanged(ctx context.Context, tenantID uuid.UUID, msg Message, userID uuid.UUID, emoji string, added bool)
	ReadUpdated(ctx context.Context, tenantID, channelID, userID uuid.UUID, lastReadSeq int64, origin string)
}

// Limiter is the rate limiter (platform/ratelimit): false means too many, retry after the duration.
type Limiter interface {
	Allow(ctx context.Context, capacity int, perSecond float64, keyParts ...string) (bool, time.Duration)
}

// Send rate limits (PLAN section 7): a burst of 10 and 60 per minute per user, and per tenant so that one busy app
// cannot starve the others.
const (
	userBurst       = 10
	userPerSecond   = 1.0
	tenantBurst     = 500
	tenantPerSecond = 100.0
)

type Service struct {
	pool     *pgxpool.Pool
	q        store.Querier
	notifier Notifier
	limiter  Limiter
	hyd      *Hydrator
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool, q: store.New(pool)} }

// SetNotifier, SetLimiter and SetHydrator are called once while wiring, before the service takes requests.
func (s *Service) SetNotifier(n Notifier)  { s.notifier = n }
func (s *Service) SetLimiter(l Limiter)    { s.limiter = l }
func (s *Service) SetHydrator(h *Hydrator) { s.hyd = h }

// SendInput is one message from a user.
type SendInput struct {
	TenantID  uuid.UUID
	ChannelID uuid.UUID
	SenderID  uuid.UUID
	// ClientMsgID makes sending idempotent (PLAN D9).
	ClientMsgID string
	Text        string
	// AttachmentIDs are uploads of the sender, in the order to show them (at most 10).
	AttachmentIDs []uuid.UUID
	// ReplyToID is a message of the same channel this one answers.
	ReplyToID uuid.NullUUID
	// Origin is the WebSocket connection that sends, so that its owner is not sent the message back as an event.
	Origin string
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
	case n == 0 && len(in.AttachmentIDs) == 0:
		errs["text"] = "must not be empty unless attachments are sent"
	case n > MaxTextLength:
		errs["text"] = fmt.Sprintf("must be at most %d characters", MaxTextLength)
	}
	if len(in.AttachmentIDs) > MaxAttachmentsPerMessage {
		errs["attachment_ids"] = fmt.Sprintf("at most %d attachments per message", MaxAttachmentsPerMessage)
	}
	seen := map[uuid.UUID]bool{}
	for _, id := range in.AttachmentIDs {
		if seen[id] {
			errs["attachment_ids"] = "the same attachment is listed twice"
		}
		seen[id] = true
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

// hydrate completes a slice of messages for the reader in place (attachments, reply previews, their own reactions).
func (s *Service) hydrate(ctx context.Context, tenantID, viewerID uuid.UUID, msgs []Message) error {
	return s.hyd.Hydrate(ctx, tenantID, viewerID, msgs)
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
	if s.limiter != nil {
		if ok, wait := s.limiter.Allow(ctx, userBurst, userPerSecond, "send", in.TenantID.String(), in.SenderID.String()); !ok {
			return Message{}, false, httpx.ErrRateLimited(wait, "you are sending too fast, try again in %s", wait.Round(time.Millisecond))
		}
		if ok, wait := s.limiter.Allow(ctx, tenantBurst, tenantPerSecond, "send-tenant", in.TenantID.String()); !ok {
			return Message{}, false, httpx.ErrRateLimited(wait, "this app is sending too fast, try again in %s", wait.Round(time.Millisecond))
		}
	}
	if in.ReplyToID.Valid {
		target, err := s.q.GetMessageByID(ctx, store.GetMessageByIDParams{ID: in.ReplyToID.UUID, TenantID: in.TenantID})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && target.ChannelID != in.ChannelID) {
			return Message{}, false, httpx.FieldErrors{"reply_to_id": "must be a message of this channel"}
		}
		if err != nil {
			return Message{}, false, fmt.Errorf("message: load replied message: %w", err)
		}
	}

	var appended Appended
	cmid := in.ClientMsgID
	var text *string
	if in.Text != "" {
		text = &in.Text
	}
	msgType := TypeText
	if len(in.AttachmentIDs) > 0 {
		msgType = TypeMedia
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if err := checkAttachments(ctx, q, in.TenantID, in.SenderID, in.AttachmentIDs); err != nil {
			return err
		}
		var err error
		appended, err = Append(ctx, q, AppendInput{
			TenantID: in.TenantID, ChannelID: in.ChannelID,
			SenderID:    uuid.NullUUID{UUID: in.SenderID, Valid: true},
			ClientMsgID: &cmid, Type: msgType, Text: text, ReplyToID: in.ReplyToID, AttachmentIDs: in.AttachmentIDs,
			RefuseFrozen: true,
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
	case isUniqueViolation(err) && strings.HasPrefix(constraintOf(err), "message_attachments"):
		return Message{}, false, httpx.ErrConflict("an attachment of this message is already used by another message")
	case isUniqueViolation(err):
		// the same client_msg_id arrived twice at once: the other request won, answer with its message
		existing, ok, lerr := s.byClientID(ctx, in)
		if lerr != nil || !ok {
			return Message{}, false, errors.Join(err, lerr)
		}
		return existing, false, nil
	case err != nil:
		return Message{}, false, wrapUnlessAPI(err, "send")
	}
	d, err := s.q.GetMessageBySeq(ctx, store.GetMessageBySeqParams{ChannelID: in.ChannelID, TenantID: in.TenantID, Seq: appended.Seq})
	if err != nil {
		return Message{}, false, fmt.Errorf("message: load sent message: %w", err)
	}
	msg = s.hydrated(ctx, in.TenantID, in.SenderID, FromDetail(d))
	if s.notifier != nil {
		s.notifier.MessageCreated(ctx, in.TenantID, msg, in.Origin)
	}
	return msg, true, nil
}

// hydrated returns the message completed for the reader. The sender's own message needs it for the attachments and
// the preview of the reply; it cannot fail a send that is already committed, so a failure only costs the extras.
func (s *Service) hydrated(ctx context.Context, tenantID, viewerID uuid.UUID, m Message) Message {
	if err := s.hyd.HydrateOne(ctx, tenantID, viewerID, &m); err != nil {
		return m
	}
	return m
}

// checkAttachments: every attachment exists, is the sender's, and has been uploaded (and did not fail).
func checkAttachments(ctx context.Context, q store.Querier, tenantID, senderID uuid.UUID, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	rows, err := q.GetAttachmentsByIDs(ctx, store.GetAttachmentsByIDsParams{TenantID: tenantID, Ids: csv(ids)})
	if err != nil {
		return fmt.Errorf("message: load attachments: %w", err)
	}
	byID := make(map[uuid.UUID]store.Attachment, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
	}
	for _, id := range ids {
		a, ok := byID[id]
		switch {
		case !ok || a.UploaderID != senderID: // someone else's upload does not exist for you
			return httpx.FieldErrors{"attachment_ids": fmt.Sprintf("attachment %s does not exist", id)}
		case a.Status == StatusPending:
			return httpx.FieldErrors{"attachment_ids": fmt.Sprintf("attachment %s is not uploaded yet: upload it, then call complete", id)}
		case a.Status == StatusFailed:
			return httpx.FieldErrors{"attachment_ids": fmt.Sprintf("attachment %s failed: upload it again", id)}
		}
	}
	return nil
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
	m := s.hydrated(ctx, in.TenantID, in.SenderID, FromDetail(d))
	return m, true, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func constraintOf(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.ConstraintName
	}
	return ""
}

// wrapUnlessAPI adds context to unexpected errors and leaves API errors (404, 403, validation) as they are.
func wrapUnlessAPI(err error, what string) error {
	var apiErr *httpx.Error
	var fields httpx.FieldErrors
	if errors.As(err, &apiErr) || errors.As(err, &fields) {
		return err
	}
	return fmt.Errorf("message: %s: %w", what, err)
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
	msgs, hasMore := page(rows, limit)
	if err := s.hydrate(ctx, tenantID, userID, msgs); err != nil {
		return nil, false, err
	}
	return msgs, hasMore, nil
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
	msgs, hasMore := page(rows, limit)
	if err := s.hydrate(ctx, tenantID, userID, msgs); err != nil {
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

func page(rows []store.MessageDetail, limit int) ([]Message, bool) {
	hasMore := len(rows) > limit
	if hasMore {
		rows = rows[:limit]
	}
	out := make([]Message, 0, len(rows))
	for _, r := range rows {
		out = append(out, FromDetail(r))
	}
	return out, hasMore
}

// MarkRead moves the read marker of the user forward and returns where it is now. origin is the WebSocket
// connection that asked, or "".
func (s *Service) MarkRead(ctx context.Context, tenantID, channelID, userID uuid.UUID, seq int64, origin string) (int64, error) {
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
	if s.notifier != nil {
		s.notifier.ReadUpdated(ctx, tenantID, channelID, userID, got, origin)
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
	msg := FromDetail(d)
	if s.notifier != nil {
		s.notifier.MessageCreated(ctx, tenantID, msg, "")
	}
	return msg, nil
}
