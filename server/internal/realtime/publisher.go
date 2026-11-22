package realtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/rizkysiregar/marpokat/server/internal/channel"
	"github.com/rizkysiregar/marpokat/server/internal/message"
	"github.com/rizkysiregar/marpokat/server/internal/platform/redisx"
	"github.com/rizkysiregar/marpokat/server/internal/store"
	"github.com/rizkysiregar/marpokat/server/internal/user"
)

// ErrNotMember: the user is not an active member of the channel.
var ErrNotMember = errors.New("realtime: not a member of the channel")

// ChannelViewer renders a channel as one of its members sees it (channel.Service).
type ChannelViewer interface {
	ForUser(ctx context.Context, tenantID, userID, channelID uuid.UUID) (channel.Channel, error)
}

// userTopic is the Redis topic of one user: every event for them is published here, and the pods that hold a
// connection of theirs are subscribed.
func userTopic(rdb *redisx.Client, tenantID, userID uuid.UUID) string {
	return rdb.Key("u", tenantID.String(), userID.String())
}

// membersCacheTTL bounds how long typing events keep going to someone who was just removed from a channel (on the
// pod that removed them it is instant). Messages never use the cache.
const membersCacheTTL = 5 * time.Second

// Publisher implements message.Notifier and channel.Notifier: it turns committed changes into events for the
// users they concern. Failures are logged and never returned, delivery is best effort.
type Publisher struct {
	rdb   *redisx.Client
	q     store.Querier
	views ChannelViewer
	log   *slog.Logger

	mu    sync.Mutex
	cache map[uuid.UUID]cachedMembers
}

type cachedMembers struct {
	ids     []uuid.UUID
	expires time.Time
}

func NewPublisher(rdb *redisx.Client, q store.Querier, views ChannelViewer, log *slog.Logger) *Publisher {
	return &Publisher{rdb: rdb, q: q, views: views, log: log.With("subsystem", "realtime-publisher"), cache: map[uuid.UUID]cachedMembers{}}
}

var (
	_ message.Notifier = (*Publisher)(nil)
	_ channel.Notifier = (*Publisher)(nil)
)

// publish sends one event to the topics of the given users, in order.
func (p *Publisher) publish(ctx context.Context, tenantID uuid.UUID, userIDs []uuid.UUID, typ string, data any, origin string) {
	if len(userIDs) == 0 {
		return
	}
	raw, err := json.Marshal(data)
	if err != nil {
		p.log.Error("cannot encode event", "type", typ, "error", err)
		return
	}
	payload, err := json.Marshal(wire{Origin: origin, Type: typ, Data: raw})
	if err != nil {
		p.log.Error("cannot encode event", "type", typ, "error", err)
		return
	}
	pipe := p.rdb.Pipeline()
	for _, id := range userIDs {
		pipe.Publish(ctx, userTopic(p.rdb, tenantID, id), payload)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		p.log.Warn("cannot publish event", "type", typ, "recipients", len(userIDs), "error", err)
	}
}

func (p *Publisher) members(ctx context.Context, tenantID, channelID uuid.UUID) ([]uuid.UUID, error) {
	ids, err := p.q.ListActiveMemberIDs(ctx, store.ListActiveMemberIDsParams{ChannelID: channelID, TenantID: tenantID})
	if err != nil {
		return nil, fmt.Errorf("list members of %s: %w", channelID, err)
	}
	return ids, nil
}

// cachedMemberIDs is members with a short cache, for the high volume of typing events.
func (p *Publisher) cachedMemberIDs(ctx context.Context, tenantID, channelID uuid.UUID) ([]uuid.UUID, error) {
	p.mu.Lock()
	if c, ok := p.cache[channelID]; ok && time.Now().Before(c.expires) {
		p.mu.Unlock()
		return c.ids, nil
	}
	p.mu.Unlock()
	ids, err := p.members(ctx, tenantID, channelID)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	if len(p.cache) > 10_000 {
		p.cache = map[uuid.UUID]cachedMembers{}
	}
	p.cache[channelID] = cachedMembers{ids: ids, expires: time.Now().Add(membersCacheTTL)}
	p.mu.Unlock()
	return ids, nil
}

func (p *Publisher) forget(channelID uuid.UUID) {
	p.mu.Lock()
	delete(p.cache, channelID)
	p.mu.Unlock()
}

func (p *Publisher) externalID(ctx context.Context, tenantID, userID uuid.UUID) (string, bool) {
	u, err := p.q.GetUserByID(ctx, store.GetUserByIDParams{ID: userID, TenantID: tenantID})
	if err != nil {
		p.log.Warn("cannot load user for an event", "user_id", userID, "error", err)
		return "", false
	}
	return u.ExternalID, true
}

// ---------------------------------------------------------------- message.Notifier

func (p *Publisher) MessageCreated(ctx context.Context, tenantID uuid.UUID, msg message.Message, origin string) {
	ids, err := p.members(ctx, tenantID, msg.ChannelID)
	if err != nil {
		p.log.Warn("message.new not published", "error", err)
		return
	}
	p.publish(ctx, tenantID, ids, EvMessageNew, map[string]any{"channel_id": msg.ChannelID, "rev_seq": msg.RevSeq, "message": msg}, origin)
}

// reactors returns who reacted with what on a message, for telling each receiver which reactions are their own.
func (p *Publisher) reactors(ctx context.Context, messageID uuid.UUID) map[uuid.UUID][]string {
	rows, err := p.q.ListReactionsOfMessage(ctx, messageID)
	if err != nil {
		p.log.Warn("cannot load reactions for an event", "message_id", messageID, "error", err)
		return nil
	}
	out := make(map[uuid.UUID][]string)
	for _, r := range rows {
		out[r.UserID] = append(out[r.UserID], r.Emoji)
	}
	return out
}

func mineOf(byUser map[uuid.UUID][]string, id uuid.UUID) []string {
	if mine := byUser[id]; mine != nil {
		return mine
	}
	return []string{}
}

// MessageUpdated tells the members that a message changed (it was edited, or an attachment became ready). The
// reactions that are the receiver's own differ per receiver, so a message that has reactions is sent to each member
// separately; one without is sent to all of them at once.
func (p *Publisher) MessageUpdated(ctx context.Context, tenantID uuid.UUID, msg message.Message) {
	ids, err := p.members(ctx, tenantID, msg.ChannelID)
	if err != nil {
		p.log.Warn("message.updated not published", "error", err)
		return
	}
	data := func(m message.Message) map[string]any {
		return map[string]any{"channel_id": m.ChannelID, "rev_seq": m.RevSeq, "message": m}
	}
	if len(msg.Reactions.Counts) == 0 {
		p.publish(ctx, tenantID, ids, EvMessageUpdated, data(msg), "")
		return
	}
	byUser := p.reactors(ctx, msg.ID)
	for _, id := range ids {
		m := msg
		m.Reactions.Mine = mineOf(byUser, id)
		p.publish(ctx, tenantID, []uuid.UUID{id}, EvMessageUpdated, data(m), "")
	}
}

// MessageDeleted tells the members that a message is now a tombstone.
func (p *Publisher) MessageDeleted(ctx context.Context, tenantID uuid.UUID, msg message.Message) {
	ids, err := p.members(ctx, tenantID, msg.ChannelID)
	if err != nil {
		p.log.Warn("message.deleted not published", "error", err)
		return
	}
	p.publish(ctx, tenantID, ids, EvMessageDeleted, map[string]any{"channel_id": msg.ChannelID, "rev_seq": msg.RevSeq, "message": msg}, "")
}

// ReactionChanged tells the members that someone reacted or took a reaction back, with the summary as each of them
// sees it.
func (p *Publisher) ReactionChanged(ctx context.Context, tenantID uuid.UUID, msg message.Message, userID uuid.UUID, emoji string, added bool) {
	ext, ok := p.externalID(ctx, tenantID, userID)
	if !ok {
		return
	}
	ids, err := p.members(ctx, tenantID, msg.ChannelID)
	if err != nil {
		p.log.Warn("reaction event not published", "error", err)
		return
	}
	typ := EvReactionRemoved
	if added {
		typ = EvReactionAdded
	}
	byUser := p.reactors(ctx, msg.ID)
	for _, id := range ids {
		p.publish(ctx, tenantID, []uuid.UUID{id}, typ, map[string]any{
			"channel_id": msg.ChannelID, "rev_seq": msg.RevSeq, "message_id": msg.ID, "user_id": ext, "emoji": emoji,
			"reactions": message.Reactions{Counts: msg.Reactions.Counts, Mine: mineOf(byUser, id)},
		}, "")
	}
}

func (p *Publisher) ReadUpdated(ctx context.Context, tenantID, channelID, userID uuid.UUID, lastReadSeq int64, origin string) {
	ext, ok := p.externalID(ctx, tenantID, userID)
	if !ok {
		return
	}
	ids, err := p.members(ctx, tenantID, channelID)
	if err != nil {
		p.log.Warn("read.updated not published", "error", err)
		return
	}
	p.publish(ctx, tenantID, ids, EvReadUpdated, map[string]any{"channel_id": channelID, "user_id": ext, "last_read_seq": lastReadSeq}, origin)
}

// ---------------------------------------------------------------- channel.Notifier

// toEach sends every user the channel as they see it, which differs per user (role, unread, DM peer).
func (p *Publisher) toEach(ctx context.Context, tenantID, channelID uuid.UUID, userIDs []uuid.UUID, typ string) {
	for _, id := range userIDs {
		ch, err := p.views.ForUser(ctx, tenantID, id, channelID)
		if err != nil {
			p.log.Warn("cannot render channel for an event", "type", typ, "channel_id", channelID, "user_id", id, "error", err)
			continue
		}
		p.publish(ctx, tenantID, []uuid.UUID{id}, typ, map[string]any{"channel": ch}, "")
	}
}

func (p *Publisher) ChannelAdded(ctx context.Context, tenantID, channelID uuid.UUID, userIDs []uuid.UUID) {
	p.forget(channelID)
	p.toEach(ctx, tenantID, channelID, userIDs, EvChannelAdded)
}

func (p *Publisher) ChannelUpdated(ctx context.Context, tenantID, channelID uuid.UUID) {
	ids, err := p.members(ctx, tenantID, channelID)
	if err != nil {
		p.log.Warn("channel.updated not published", "error", err)
		return
	}
	p.toEach(ctx, tenantID, channelID, ids, EvChannelUpdated)
}

func (p *Publisher) ChannelRemoved(ctx context.Context, tenantID, channelID, userID uuid.UUID, reason string) {
	p.forget(channelID)
	p.publish(ctx, tenantID, []uuid.UUID{userID}, EvChannelRemoved, map[string]any{"channel_id": channelID, "reason": reason}, "")
}

func (p *Publisher) MemberAdded(ctx context.Context, tenantID, channelID uuid.UUID, userIDs []uuid.UUID) {
	p.forget(channelID)
	ids, err := p.members(ctx, tenantID, channelID)
	if err != nil {
		p.log.Warn("member.added not published", "error", err)
		return
	}
	added := make(map[uuid.UUID]bool, len(userIDs))
	for _, id := range userIDs {
		added[id] = true
	}
	var others []uuid.UUID // the new members hear of the channel itself, the others hear of them
	for _, id := range ids {
		if !added[id] {
			others = append(others, id)
		}
	}
	for _, id := range userIDs {
		m, err := p.q.GetMemberDetails(ctx, store.GetMemberDetailsParams{ChannelID: channelID, UserID: id, TenantID: tenantID})
		if err != nil {
			p.log.Warn("cannot load member for an event", "channel_id", channelID, "user_id", id, "error", err)
			continue
		}
		member := channel.Member{
			User: user.User{ID: m.UserID, ExternalID: m.ExternalID, Name: m.Name, AvatarURL: m.AvatarUrl,
				Metadata: rawMetadata(m.Metadata), Deleted: m.UserDeletedAt != nil},
			Role: m.Role, LastReadSeq: m.LastReadSeq, JoinedAt: m.JoinedAt,
		}
		p.publish(ctx, tenantID, others, EvMemberAdded, map[string]any{"channel_id": channelID, "member": member}, "")
	}
}

func (p *Publisher) MemberRemoved(ctx context.Context, tenantID, channelID, userID uuid.UUID) {
	p.forget(channelID)
	ext, ok := p.externalID(ctx, tenantID, userID)
	if !ok {
		return
	}
	ids, err := p.members(ctx, tenantID, channelID)
	if err != nil {
		p.log.Warn("member.removed not published", "error", err)
		return
	}
	p.publish(ctx, tenantID, ids, EvMemberRemoved, map[string]any{"channel_id": channelID, "user_id": ext}, "")
}

func (p *Publisher) SystemMessages(ctx context.Context, tenantID, channelID uuid.UUID, seqs ...int64) {
	for _, seq := range seqs {
		d, err := p.q.GetMessageBySeq(ctx, store.GetMessageBySeqParams{ChannelID: channelID, TenantID: tenantID, Seq: seq})
		if err != nil {
			p.log.Warn("cannot load announcement for an event", "channel_id", channelID, "seq", seq, "error", err)
			continue
		}
		p.MessageCreated(ctx, tenantID, message.FromDetail(d), "")
	}
}

// ---------------------------------------------------------------- typing and presence

// Typing tells the other members that the user started or stopped typing. The user must be a member, which is
// answered from a few seconds old data so that typing costs no query.
func (p *Publisher) Typing(ctx context.Context, tenantID, channelID, userID uuid.UUID, external string, typing bool) error {
	ids, err := p.cachedMemberIDs(ctx, tenantID, channelID)
	if err != nil {
		return err
	}
	var others []uuid.UUID
	member := false
	for _, id := range ids {
		if id == userID {
			member = true
			continue
		}
		others = append(others, id)
	}
	if !member {
		return ErrNotMember
	}
	p.publish(ctx, tenantID, others, EvTyping, map[string]any{"channel_id": channelID, "user_id": external, "typing": typing}, "")
	return nil
}

// PresenceChanged tells everyone who shares a channel with the user that they went online or offline.
func (p *Publisher) PresenceChanged(ctx context.Context, tenantID, userID uuid.UUID, external string, online bool, lastSeen *time.Time) {
	audience, err := p.q.ListCoMemberIDs(ctx, store.ListCoMemberIDsParams{TenantID: tenantID, UserID: userID})
	if err != nil {
		p.log.Warn("presence not published", "error", err)
		return
	}
	p.publish(ctx, tenantID, audience, EvPresence, map[string]any{"user_id": external, "online": online, "last_seen_at": lastSeen}, "")
}

func rawMetadata(s string) json.RawMessage {
	if s == "" {
		return json.RawMessage("{}")
	}
	return json.RawMessage(s)
}
