package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/rizkysiregar/marpokat/server/internal/broker"
	"github.com/rizkysiregar/marpokat/server/internal/message"
	"github.com/rizkysiregar/marpokat/server/internal/outbox"
	"github.com/rizkysiregar/marpokat/server/internal/platform/metrics"
	"github.com/rizkysiregar/marpokat/server/internal/store"
)

// OnlineChecker says which of the users have a live connection (and so need no push).
type OnlineChecker interface {
	Online(ctx context.Context, tenantID uuid.UUID, userIDs []uuid.UUID) (map[uuid.UUID]bool, error)
}

// Credentials gives the Firebase service account of a tenant, and false when it has none.
type Credentials interface {
	FCMCredentials(ctx context.Context, tenantID uuid.UUID) ([]byte, bool, error)
}

// DefaultMaxAge: a message older than this when its event is handled gets no push. After an outage or a backlog,
// notifications about things from an hour ago only confuse.
const DefaultMaxAge = 10 * time.Minute

// Processor handles the event "a message was created": it decides who is sent a notification and sends it.
type Processor struct {
	Q       store.Querier
	Online  OnlineChecker
	Tenants Credentials
	Sender  Sender
	Dedup   Dedup
	Log     *slog.Logger
	Metrics *metrics.Registry // may be nil
	MaxAge  time.Duration
	Now     func() time.Time
}

func (p *Processor) skipped(reason string) {
	if p.Metrics != nil {
		p.Metrics.PushSkipped.WithLabelValues(reason).Inc()
	}
}

// Handle is the broker.Handler of the push queue. It returns an error only for failures worth a retry; what it
// already sent is remembered per device, so a retry sends only what is still missing.
func (p *Processor) Handle(ctx context.Context, env broker.Envelope, _ broker.Delivery) error {
	if env.Topic != outbox.TopicMessageCreated { // the queue is bound to this topic only, but be careful anyway
		return nil
	}
	var pl struct {
		ChannelID uuid.UUID `json:"channel_id"`
		Seq       int64     `json:"seq"`
		Type      string    `json:"type"`
	}
	if err := json.Unmarshal(env.Payload, &pl); err != nil {
		return broker.Permanent(fmt.Errorf("undecodable payload: %w", err))
	}
	tenantID, err := uuid.Parse(env.TenantID)
	if err != nil {
		return broker.Permanent(fmt.Errorf("bad tenant id %q", env.TenantID))
	}
	if pl.Type == "system" { // announcements of the channel never ring a phone
		p.skipped("system")
		return nil
	}

	msg, err := p.Q.GetMessageBySeq(ctx, store.GetMessageBySeqParams{ChannelID: pl.ChannelID, TenantID: tenantID, Seq: pl.Seq})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // the channel is gone
	}
	if err != nil {
		return fmt.Errorf("load message: %w", err)
	}
	if msg.DeletedAt != nil || !msg.SenderID.Valid {
		return nil
	}
	if now := p.Now(); now.Sub(msg.CreatedAt) > p.MaxAge {
		p.skipped("stale")
		return nil
	}

	creds, ok, err := p.Tenants.FCMCredentials(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("load credentials: %w", err)
	}
	if !ok {
		p.skipped("no_credentials")
		return nil
	}

	candidates, err := p.Q.ListPushRecipients(ctx, store.ListPushRecipientsParams{
		ChannelID: pl.ChannelID, TenantID: tenantID, SenderID: msg.SenderID.UUID, Seq: msg.Seq,
	})
	if err != nil {
		return fmt.Errorf("list recipients: %w", err)
	}
	if len(candidates) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(candidates))
	for i, c := range candidates {
		ids[i] = c.UserID
	}
	online, err := p.Online.Online(ctx, tenantID, ids)
	if err != nil {
		return fmt.Errorf("check presence: %w", err) // better a retry than a push to someone who is looking at the chat
	}

	type recipient struct {
		id     uuid.UUID
		unread int
	}
	var todo []recipient
	for _, c := range candidates {
		if online[c.UserID] {
			p.skipped("online")
			continue
		}
		todo = append(todo, recipient{c.UserID, int(c.UnreadCount)})
	}
	if len(todo) == 0 {
		return nil
	}

	userIDs := make([]string, len(todo))
	for i, r := range todo {
		userIDs[i] = r.id.String()
	}
	devices, err := p.Q.ListDevicesOfUsers(ctx, store.ListDevicesOfUsersParams{TenantID: tenantID, UserIds: strings.Join(userIDs, ",")})
	if err != nil {
		return fmt.Errorf("list devices: %w", err)
	}
	byUser := map[uuid.UUID][]store.ListDevicesOfUsersRow{}
	for _, d := range devices {
		byUser[d.UserID] = append(byUser[d.UserID], d)
	}
	ch, err := p.Q.GetChannel(ctx, store.GetChannelParams{ID: pl.ChannelID, TenantID: tenantID})
	if err != nil {
		return fmt.Errorf("load channel: %w", err)
	}

	content := Content{
		ChannelID: msg.ChannelID.String(), ChannelType: ch.Type, MessageID: msg.ID.String(), Seq: msg.Seq,
	}
	if ch.Name != nil {
		content.ChannelName = *ch.Name
	}
	if msg.SenderExternalID != nil {
		content.SenderID = *msg.SenderExternalID
	}
	if msg.SenderName != nil {
		content.SenderName = *msg.SenderName
	}
	if msg.SenderAvatarUrl != nil {
		content.SenderAvatarURL = *msg.SenderAvatarUrl
	}
	if msg.Text != nil {
		content.Text = *msg.Text
	}
	if msg.Type == "media" { // the notification says what was sent: a photo, a video, a file
		atts, err := p.Q.ListMessageAttachments(ctx, msg.ID.String())
		if err != nil {
			return fmt.Errorf("load attachments: %w", err)
		}
		if len(atts) > 0 {
			content.AttachmentKind = message.KindOf(atts[0].Mime)
		}
	}

	var retry error
	for _, r := range todo {
		devs := byUser[r.id]
		if len(devs) == 0 {
			continue // nothing registered: not a failure, and nothing to remember
		}
		content.UnreadCount = r.unread
		data := content.Data()
		for _, d := range devs {
			if done, err := p.Dedup.Done(ctx, env.EventID, r.id, d.PushToken); err == nil && done {
				p.skipped("duplicate")
				continue
			}
			err := p.Sender.Send(ctx, creds, Message{Token: d.PushToken, Platform: d.Platform, Data: data})
			switch {
			case err == nil:
				if p.Metrics != nil {
					p.Metrics.PushSent.WithLabelValues(d.Platform).Inc()
				}
				if merr := p.Dedup.MarkDone(ctx, env.EventID, r.id, d.PushToken); merr != nil {
					p.Log.Warn("cannot remember that a push was sent", "error", merr)
				}
			case errors.Is(err, ErrInvalidToken):
				p.count("invalid_token")
				p.Log.Info("removing a device FCM does not know", "user_id", r.id, "platform", d.Platform, "reason", err)
				if derr := p.Q.DeleteDeviceByToken(ctx, store.DeleteDeviceByTokenParams{TenantID: tenantID, PushToken: d.PushToken}); derr != nil {
					p.Log.Warn("cannot remove the device", "error", derr)
				}
			default:
				var se *SendError
				if errors.As(err, &se) && !se.Retryable {
					p.count("permanent")
					p.Log.Error("push refused, not retrying", "user_id", r.id, "platform", d.Platform, "error", err)
					continue
				}
				p.count("retryable")
				retry = errors.Join(retry, err)
			}
		}
	}
	return retry
}

func (p *Processor) count(reason string) {
	if p.Metrics != nil {
		p.Metrics.PushFailed.WithLabelValues(reason).Inc()
	}
}
