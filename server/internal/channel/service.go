package channel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rizkysiregar/marpokat/server/internal/message"
	"github.com/rizkysiregar/marpokat/server/internal/outbox"
	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
	"github.com/rizkysiregar/marpokat/server/internal/store"
	"github.com/rizkysiregar/marpokat/server/internal/user"
)

const (
	DefaultPageSize = 30
	MaxPageSize     = 100
	maxLookupIDs    = 300
)

// Actor is who makes a change. The zero value is the tenant backend (server API), which may do everything;
// a valid value is a user, who needs the right role in the channel.
type Actor = uuid.NullUUID

// UserActor is the actor for a user.
func UserActor(id uuid.UUID) Actor { return uuid.NullUUID{UUID: id, Valid: true} }

// Notifier tells connected clients about a change after it was committed (PLAN M3). Delivery is best effort, so
// the calls return nothing: a client that misses an event catches up with sync and updated_since.
type Notifier interface {
	// ChannelAdded: these users now have the channel (each is sent the channel as they see it).
	ChannelAdded(ctx context.Context, tenantID, channelID uuid.UUID, userIDs []uuid.UUID)
	// ChannelUpdated: the profile or frozen state changed, for every active member.
	ChannelUpdated(ctx context.Context, tenantID, channelID uuid.UUID)
	// ChannelRemoved: the user no longer has the channel; reason is "left" or "removed".
	ChannelRemoved(ctx context.Context, tenantID, channelID, userID uuid.UUID, reason string)
	MemberAdded(ctx context.Context, tenantID, channelID uuid.UUID, userIDs []uuid.UUID)
	MemberRemoved(ctx context.Context, tenantID, channelID, userID uuid.UUID)
	// SystemMessages: announcements of the change, by seq, for every active member.
	SystemMessages(ctx context.Context, tenantID, channelID uuid.UUID, seqs ...int64)
}

type Service struct {
	pool     *pgxpool.Pool
	q        store.Querier
	notifier Notifier
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool, q: store.New(pool)} }

// SetNotifier is called once while wiring, before the service takes requests.
func (s *Service) SetNotifier(n Notifier) { s.notifier = n }

// ResolveUsers maps external ids to users of the tenant, in the order given and without duplicates. Users that do
// not exist (or were deleted) are a 404 naming them: the tenant backend must upsert them first.
func (s *Service) ResolveUsers(ctx context.Context, tenantID uuid.UUID, externalIDs []string) ([]store.User, error) {
	if len(externalIDs) > maxLookupIDs {
		return nil, httpx.FieldErrors{"user_ids": fmt.Sprintf("at most %d users at once", maxLookupIDs)}
	}
	seen := make(map[string]bool, len(externalIDs))
	unique := make([]string, 0, len(externalIDs))
	for _, id := range externalIDs {
		if err := user.ValidateExternalID(id); err != nil {
			return nil, httpx.FieldErrors{"user_ids": fmt.Sprintf("%q: %s", id, err)}
		}
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	if len(unique) == 0 {
		return nil, nil
	}
	rows, err := s.q.GetUsersByExternalIDs(ctx, store.GetUsersByExternalIDsParams{TenantID: tenantID, Column2: unique})
	if err != nil {
		return nil, fmt.Errorf("channel: resolve users: %w", err)
	}
	byExt := make(map[string]store.User, len(rows))
	for _, r := range rows {
		byExt[r.ExternalID] = r
	}
	out := make([]store.User, 0, len(unique))
	var missing []string
	for _, id := range unique {
		if u, ok := byExt[id]; ok {
			out = append(out, u)
		} else {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		shown := missing
		if len(shown) > 5 {
			shown = append(shown[:5:5], "...")
		}
		return nil, httpx.ErrNotFound("user not found: %s (create users with PUT /v1/server/users/{external_id} first)", strings.Join(shown, ", "))
	}
	return out, nil
}

// GetOrCreateDM returns the DM of two users, creating it when it does not exist yet. created tells which.
func (s *Service) GetOrCreateDM(ctx context.Context, tenantID, a, b uuid.UUID) (ch store.Channel, created bool, err error) {
	if a == b {
		return store.Channel{}, false, httpx.FieldErrors{"user_b": "cannot start a DM with yourself"}
	}
	key := DMKey(tenantID, a, b)
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		row, err := q.InsertChannel(ctx, store.InsertChannelParams{
			ID: uuid.Must(uuid.NewV7()), TenantID: tenantID, Type: TypeDM, DmKey: &key, CreatedBy: UserActor(a),
		})
		if errors.Is(err, pgx.ErrNoRows) { // the pair already has a DM (maybe created a moment ago by someone else)
			ch, err = q.GetChannelByDMKey(ctx, store.GetChannelByDMKeyParams{DmKey: &key, TenantID: tenantID})
			return err
		}
		if err != nil {
			return err
		}
		ch, created = row, true
		for _, u := range []uuid.UUID{a, b} {
			if _, err := q.AddMember(ctx, store.AddMemberParams{ChannelID: ch.ID, UserID: u, TenantID: tenantID, Role: RoleMember}); err != nil {
				return err
			}
		}
		return outbox.Enqueue(ctx, q, tenantID, outbox.TopicChannelCreated,
			map[string]any{"channel_id": ch.ID, "type": TypeDM, "member_ids": []uuid.UUID{a, b}})
	})
	if err != nil {
		return store.Channel{}, false, fmt.Errorf("channel: get or create dm: %w", err)
	}
	if created && s.notifier != nil {
		s.notifier.ChannelAdded(ctx, tenantID, ch.ID, []uuid.UUID{a, b})
	}
	return ch, created, nil
}

// CreateGroup creates a group owned by creator. members must not repeat creator.
func (s *Service) CreateGroup(ctx context.Context, tenantID, creator uuid.UUID, name string, avatar *string, members []uuid.UUID) (store.Channel, error) {
	name, err := normalizeName(name)
	if err != nil {
		return store.Channel{}, err
	}
	avatar, err = normalizeAvatar(avatar)
	if err != nil {
		return store.Channel{}, err
	}
	others := make([]uuid.UUID, 0, len(members))
	seen := map[uuid.UUID]bool{creator: true}
	for _, m := range members {
		if !seen[m] {
			seen[m] = true
			others = append(others, m)
		}
	}
	if len(others)+1 > MaxGroupMembers {
		return store.Channel{}, httpx.FieldErrors{"member_ids": fmt.Sprintf("a group has at most %d members, the creator included", MaxGroupMembers)}
	}

	var ch store.Channel
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		var err error
		ch, err = q.InsertChannel(ctx, store.InsertChannelParams{
			ID: uuid.Must(uuid.NewV7()), TenantID: tenantID, Type: TypeGroup, Name: &name, AvatarUrl: avatar, CreatedBy: UserActor(creator),
		})
		if err != nil {
			return err
		}
		if _, err := q.AddMember(ctx, store.AddMemberParams{ChannelID: ch.ID, UserID: creator, TenantID: tenantID, Role: RoleOwner}); err != nil {
			return err
		}
		for _, m := range others {
			if _, err := q.AddMember(ctx, store.AddMemberParams{ChannelID: ch.ID, UserID: m, TenantID: tenantID, Role: RoleMember}); err != nil {
				return err
			}
		}
		actorExt, err := externalID(ctx, q, tenantID, creator)
		if err != nil {
			return err
		}
		if _, err := systemMessage(ctx, q, tenantID, ch.ID, map[string]any{"event": "channel.created", "actor_id": actorExt, "name": name}); err != nil {
			return err
		}
		return outbox.Enqueue(ctx, q, tenantID, outbox.TopicChannelCreated,
			map[string]any{"channel_id": ch.ID, "type": TypeGroup, "member_ids": append([]uuid.UUID{creator}, others...)})
	})
	if err != nil {
		return store.Channel{}, fmt.Errorf("channel: create group: %w", err)
	}
	if s.notifier != nil { // the channel carries the creation announcement as its last message
		s.notifier.ChannelAdded(ctx, tenantID, ch.ID, append([]uuid.UUID{creator}, others...))
	}
	return ch, nil
}

// ForUser returns one channel as a member sees it. A channel the user is not in does not exist for them.
func (s *Service) ForUser(ctx context.Context, tenantID, userID, channelID uuid.UUID) (Channel, error) {
	rows, err := s.q.ListUserChannels(ctx, store.ListUserChannelsParams{
		UserID: userID, TenantID: tenantID, ChannelID: uuid.NullUUID{UUID: channelID, Valid: true}, Lim: 1,
	})
	if err != nil {
		return Channel{}, fmt.Errorf("channel: get: %w", err)
	}
	if len(rows) == 0 {
		return Channel{}, httpx.ErrNotFound("channel not found")
	}
	chs, err := s.build(ctx, tenantID, userID, rows)
	if err != nil {
		return Channel{}, err
	}
	return chs[0], nil
}

// ListParams selects a page of the channels of a user.
type ListParams struct {
	Since  *time.Time
	Cursor string
	Limit  int
}

// ListForUser returns the channels of a user, newest activity first.
func (s *Service) ListForUser(ctx context.Context, tenantID, userID uuid.UUID, p ListParams) (ChannelPage, error) {
	params := store.ListUserChannelsParams{UserID: userID, TenantID: tenantID, Since: p.Since, Lim: int32(p.Limit + 1)} //nolint:gosec // capped at 100
	if p.Cursor != "" {
		at, id, err := decodeCursor(p.Cursor)
		if err != nil {
			return ChannelPage{}, err
		}
		params.CursorAt, params.CursorID = &at, uuid.NullUUID{UUID: id, Valid: true}
	}
	rows, err := s.q.ListUserChannels(ctx, params)
	if err != nil {
		return ChannelPage{}, fmt.Errorf("channel: list: %w", err)
	}
	var next *string
	if len(rows) > p.Limit {
		rows = rows[:p.Limit]
		c := encodeCursor(rows[len(rows)-1].SortAt, rows[len(rows)-1].ID)
		next = &c
	}
	chs, err := s.build(ctx, tenantID, userID, rows)
	if err != nil {
		return ChannelPage{}, err
	}
	return ChannelPage{Channels: chs, NextCursor: next}, nil
}

// build adds what the list query does not carry: the last message and, for DMs, the other member.
func (s *Service) build(ctx context.Context, tenantID, userID uuid.UUID, rows []store.ListUserChannelsRow) ([]Channel, error) {
	ids := make([]uuid.UUID, 0, len(rows))
	var dmIDs []uuid.UUID
	for _, r := range rows {
		ids = append(ids, r.ID)
		if r.Type == TypeDM {
			dmIDs = append(dmIDs, r.ID)
		}
	}
	lastByChannel := map[uuid.UUID]message.Message{}
	peerByChannel := map[uuid.UUID]user.User{}
	if len(ids) > 0 {
		last, err := s.q.ListLastMessages(ctx, store.ListLastMessagesParams{ChannelIds: joinIDs(ids), TenantID: tenantID})
		if err != nil {
			return nil, fmt.Errorf("channel: last messages: %w", err)
		}
		for _, m := range last {
			lastByChannel[m.ChannelID] = message.FromDetail(m)
		}
	}
	if len(dmIDs) > 0 {
		peers, err := s.q.ListDMPeers(ctx, store.ListDMPeersParams{ChannelIds: joinIDs(dmIDs), TenantID: tenantID, UserID: userID})
		if err != nil {
			return nil, fmt.Errorf("channel: dm peers: %w", err)
		}
		for _, p := range peers {
			peerByChannel[p.ChannelID] = userView(p.ID, p.ExternalID, p.Name, p.AvatarUrl, p.Metadata, p.DeletedAt != nil)
		}
	}
	out := make([]Channel, 0, len(rows))
	for _, r := range rows {
		c := Channel{
			ID: r.ID, Type: r.Type, Name: r.Name, AvatarURL: r.AvatarUrl, MemberCount: int(r.MemberCount),
			LastSeq: r.LastSeq, LastMessageSeq: r.LastMessageSeq, LastMessageAt: r.LastMessageAt,
			Frozen: r.FrozenAt != nil, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
			Membership: Membership{
				Role: r.Role, LastReadSeq: r.LastReadSeq, UnreadCount: int(r.UnreadCount),
				MutedUntil: r.MutedUntil, JoinedAt: r.JoinedAt, LeftAt: r.LeftAt,
			},
		}
		if m, ok := lastByChannel[r.ID]; ok {
			c.LastMessage = &m
		}
		if p, ok := peerByChannel[r.ID]; ok {
			c.DMPeer = &p
		}
		out = append(out, c)
	}
	return out, nil
}

// UpdateInput changes a group's profile. A nil Name keeps it; SetAvatar says whether AvatarURL (nil: remove) applies.
type UpdateInput struct {
	Name      *string
	SetAvatar bool
	AvatarURL *string
}

// Update renames a group or changes its avatar. Owner and admins only.
func (s *Service) Update(ctx context.Context, tenantID, actor, channelID uuid.UUID, in UpdateInput) (Channel, error) {
	var name *string
	if in.Name != nil {
		n, err := normalizeName(*in.Name)
		if err != nil {
			return Channel{}, err
		}
		name = &n
	}
	avatar, err := normalizeAvatar(in.AvatarURL)
	if err != nil {
		return Channel{}, err
	}
	var announced []int64
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		ch, err := lockGroup(ctx, q, tenantID, channelID, "direct messages have no name or avatar to change")
		if err != nil {
			return err
		}
		if err := requireManager(ctx, q, tenantID, channelID, UserActor(actor)); err != nil {
			return err
		}
		if _, err := q.UpdateChannelProfile(ctx, store.UpdateChannelProfileParams{
			Name: name, SetAvatar: in.SetAvatar, AvatarUrl: avatar, ID: channelID, TenantID: tenantID,
		}); err != nil {
			return err
		}
		if name != nil && (ch.Name == nil || *ch.Name != *name) {
			actorExt, err := externalID(ctx, q, tenantID, actor)
			if err != nil {
				return err
			}
			appended, err := systemMessage(ctx, q, tenantID, channelID, map[string]any{"event": "channel.renamed", "actor_id": actorExt, "name": *name})
			if err != nil {
				return err
			}
			announced = append(announced, appended.Seq)
		}
		return outbox.Enqueue(ctx, q, tenantID, outbox.TopicChannelUpdated, map[string]any{"channel_id": channelID})
	})
	if err != nil {
		return Channel{}, wrap(err, "update")
	}
	if s.notifier != nil {
		s.notifier.ChannelUpdated(ctx, tenantID, channelID)
		s.notifier.SystemMessages(ctx, tenantID, channelID, announced...)
	}
	return s.ForUser(ctx, tenantID, actor, channelID)
}

// AddMembers adds users to a group, skipping those already in it. Owner and admins only (or the tenant backend).
func (s *Service) AddMembers(ctx context.Context, tenantID uuid.UUID, actor Actor, channelID uuid.UUID, users []store.User) error {
	var added []uuid.UUID
	var announced int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		ch, err := lockGroup(ctx, q, tenantID, channelID, "the members of a direct message cannot change")
		if err != nil {
			return err
		}
		if err := requireManager(ctx, q, tenantID, channelID, actor); err != nil {
			return err
		}
		added = added[:0]
		var addedExt []string
		for _, u := range users {
			n, err := q.AddMember(ctx, store.AddMemberParams{
				ChannelID: channelID, UserID: u.ID, TenantID: tenantID, Role: RoleMember, LastReadSeq: ch.LastSeq,
			})
			if err != nil {
				return err
			}
			if n == 1 {
				added = append(added, u.ID)
				addedExt = append(addedExt, u.ExternalID)
			}
		}
		if len(added) == 0 {
			return nil
		}
		// checked after adding, under the channel lock, so two requests cannot both squeeze past the limit
		count, err := q.CountActiveMembers(ctx, store.CountActiveMembersParams{ChannelID: channelID, TenantID: tenantID})
		if err != nil {
			return err
		}
		if count > MaxGroupMembers {
			return httpx.FieldErrors{"user_ids": fmt.Sprintf("a group has at most %d members", MaxGroupMembers)}
		}
		meta := map[string]any{"event": "member.added", "user_ids": addedExt}
		if actorExt, ok, err := actorExternalID(ctx, q, tenantID, actor); err != nil {
			return err
		} else if ok {
			meta["actor_id"] = actorExt
		}
		appended, err := systemMessage(ctx, q, tenantID, channelID, meta)
		if err != nil {
			return err
		}
		announced = appended.Seq
		return outbox.Enqueue(ctx, q, tenantID, outbox.TopicChannelMembersAdded, map[string]any{"channel_id": channelID, "user_ids": added})
	})
	if err != nil {
		return wrap(err, "add members")
	}
	if s.notifier != nil && announced != 0 && len(added) > 0 {
		// the new members first learn of the channel, then everyone learns of them
		s.notifier.ChannelAdded(ctx, tenantID, channelID, added)
		s.notifier.MemberAdded(ctx, tenantID, channelID, added)
		s.notifier.SystemMessages(ctx, tenantID, channelID, announced)
	}
	return nil
}

// RemoveMember takes a member out of a group: a member leaves (actor is the target), or an owner/admin removes
// someone, or the tenant backend does. The owner leaving hands ownership on; the last member leaving deletes the group.
func (s *Service) RemoveMember(ctx context.Context, tenantID uuid.UUID, actor Actor, channelID, target uuid.UUID) error {
	var reason string
	var ended bool
	var announced int64
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if _, err := lockGroup(ctx, q, tenantID, channelID, "the members of a direct message cannot change"); err != nil {
			return err
		}
		targetRow, err := q.GetMember(ctx, store.GetMemberParams{ChannelID: channelID, UserID: target, TenantID: tenantID})
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && targetRow.LeftAt != nil) {
			return httpx.ErrNotFound("member not found")
		}
		if err != nil {
			return err
		}
		self := actor.Valid && actor.UUID == target
		if actor.Valid && !self {
			actorRow, err := q.GetMember(ctx, store.GetMemberParams{ChannelID: channelID, UserID: actor.UUID, TenantID: tenantID})
			if errors.Is(err, pgx.ErrNoRows) || (err == nil && actorRow.LeftAt != nil) {
				return httpx.ErrNotFound("channel not found")
			}
			if err != nil {
				return err
			}
			if actorRow.Role != RoleOwner && actorRow.Role != RoleAdmin {
				return httpx.ErrForbidden("only the owner and admins can remove members")
			}
			if targetRow.Role == RoleOwner {
				return httpx.ErrForbidden("the owner cannot be removed, they can leave")
			}
			if targetRow.Role == RoleAdmin && actorRow.Role != RoleOwner {
				return httpx.ErrForbidden("only the owner can remove an admin")
			}
		}
		count, err := q.CountActiveMembers(ctx, store.CountActiveMembersParams{ChannelID: channelID, TenantID: tenantID})
		if err != nil {
			return err
		}
		if targetRow.Role == RoleOwner && count > 1 {
			next, err := q.PickNextOwner(ctx, store.PickNextOwnerParams{ChannelID: channelID, TenantID: tenantID, UserID: target})
			if err != nil {
				return err
			}
			if err := q.SetMemberRole(ctx, store.SetMemberRoleParams{ChannelID: channelID, UserID: next, TenantID: tenantID, Role: RoleOwner}); err != nil {
				return err
			}
		}
		if _, err := q.RemoveMember(ctx, store.RemoveMemberParams{ChannelID: channelID, UserID: target, TenantID: tenantID}); err != nil {
			return err
		}
		reason = "removed"
		if self {
			reason = "left"
		}
		if err := outbox.Enqueue(ctx, q, tenantID, outbox.TopicChannelMemberLeft,
			map[string]any{"channel_id": channelID, "user_id": target, "reason": reason}); err != nil {
			return err
		}
		if count == 1 { // nobody is left
			ended = true
			return q.SoftDeleteChannel(ctx, store.SoftDeleteChannelParams{ID: channelID, TenantID: tenantID})
		}
		targetExt, err := externalID(ctx, q, tenantID, target)
		if err != nil {
			return err
		}
		meta := map[string]any{"event": "member." + reason, "user_id": targetExt}
		if !self {
			if actorExt, ok, err := actorExternalID(ctx, q, tenantID, actor); err != nil {
				return err
			} else if ok {
				meta["actor_id"] = actorExt
			}
		}
		appended, err := systemMessage(ctx, q, tenantID, channelID, meta)
		announced = appended.Seq
		return err
	})
	if err != nil {
		return wrap(err, "remove member")
	}
	if s.notifier != nil {
		s.notifier.ChannelRemoved(ctx, tenantID, channelID, target, reason)
		if !ended {
			s.notifier.MemberRemoved(ctx, tenantID, channelID, target)
			s.notifier.SystemMessages(ctx, tenantID, channelID, announced)
		}
	}
	return nil
}

// SetFrozen makes a channel read-only (or writable again). Only the tenant backend does this, e.g. after a block.
func (s *Service) SetFrozen(ctx context.Context, tenantID, channelID uuid.UUID, frozen bool) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		n, err := q.SetChannelFrozen(ctx, store.SetChannelFrozenParams{Frozen: frozen, ID: channelID, TenantID: tenantID})
		if err != nil {
			return err
		}
		if n == 0 {
			return httpx.ErrNotFound("channel not found")
		}
		return outbox.Enqueue(ctx, q, tenantID, outbox.TopicChannelUpdated, map[string]any{"channel_id": channelID})
	})
	if err != nil {
		return wrap(err, "set frozen")
	}
	if s.notifier != nil {
		s.notifier.ChannelUpdated(ctx, tenantID, channelID)
	}
	return nil
}

// Members lists the active members of a channel the user belongs to, oldest member first.
func (s *Service) Members(ctx context.Context, tenantID, userID, channelID uuid.UUID, cursor string, limit int) (MemberPage, error) {
	m, err := s.q.GetMember(ctx, store.GetMemberParams{ChannelID: channelID, UserID: userID, TenantID: tenantID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && m.LeftAt != nil) {
		return MemberPage{}, httpx.ErrNotFound("channel not found")
	}
	if err != nil {
		return MemberPage{}, fmt.Errorf("channel: members: %w", err)
	}
	params := store.ListMembersParams{ChannelID: channelID, TenantID: tenantID, Lim: int32(limit + 1)} //nolint:gosec // capped at 100
	if cursor != "" {
		at, id, err := decodeCursor(cursor)
		if err != nil {
			return MemberPage{}, err
		}
		params.CursorAt, params.CursorID = &at, uuid.NullUUID{UUID: id, Valid: true}
	}
	rows, err := s.q.ListMembers(ctx, params)
	if err != nil {
		return MemberPage{}, fmt.Errorf("channel: list members: %w", err)
	}
	var next *string
	if len(rows) > limit {
		rows = rows[:limit]
		c := encodeCursor(rows[len(rows)-1].JoinedAt, rows[len(rows)-1].UserID)
		next = &c
	}
	out := make([]Member, 0, len(rows))
	for _, r := range rows {
		out = append(out, Member{
			User: userView(r.UserID, r.ExternalID, r.Name, r.AvatarUrl, r.Metadata, r.UserDeletedAt != nil),
			Role: r.Role, LastReadSeq: r.LastReadSeq, JoinedAt: r.JoinedAt,
		})
	}
	return MemberPage{Members: out, NextCursor: next}, nil
}

// ServerView returns a channel for the tenant backend.
func (s *Service) ServerView(ctx context.Context, tenantID, channelID uuid.UUID) (ServerChannel, error) {
	ch, err := s.q.GetChannel(ctx, store.GetChannelParams{ID: channelID, TenantID: tenantID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ServerChannel{}, httpx.ErrNotFound("channel not found")
	}
	if err != nil {
		return ServerChannel{}, fmt.Errorf("channel: server view: %w", err)
	}
	ids, err := s.q.ListMemberExternalIDs(ctx, store.ListMemberExternalIDsParams{ChannelID: channelID, TenantID: tenantID})
	if err != nil {
		return ServerChannel{}, fmt.Errorf("channel: member ids: %w", err)
	}
	if ids == nil {
		ids = []string{}
	}
	return ServerChannel{
		ID: ch.ID, Type: ch.Type, Name: ch.Name, AvatarURL: ch.AvatarUrl, MemberCount: len(ids), MemberIDs: ids,
		Frozen: ch.FrozenAt != nil, CreatedAt: ch.CreatedAt,
	}, nil
}

// lockGroup locks the channel row for the transaction and insists it is a group.
func lockGroup(ctx context.Context, q store.Querier, tenantID, channelID uuid.UUID, dmMessage string) (store.Channel, error) {
	ch, err := q.LockChannel(ctx, store.LockChannelParams{ID: channelID, TenantID: tenantID})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Channel{}, httpx.ErrNotFound("channel not found")
	}
	if err != nil {
		return store.Channel{}, err
	}
	if ch.Type != TypeGroup {
		return store.Channel{}, httpx.ErrForbidden("%s", dmMessage)
	}
	return ch, nil
}

// requireManager: the tenant backend always passes; a user must be an owner or admin of the channel.
func requireManager(ctx context.Context, q store.Querier, tenantID, channelID uuid.UUID, actor Actor) error {
	if !actor.Valid {
		return nil
	}
	m, err := q.GetMember(ctx, store.GetMemberParams{ChannelID: channelID, UserID: actor.UUID, TenantID: tenantID})
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && m.LeftAt != nil) {
		return httpx.ErrNotFound("channel not found")
	}
	if err != nil {
		return err
	}
	if m.Role != RoleOwner && m.Role != RoleAdmin {
		return httpx.ErrForbidden("only the owner and admins can do this")
	}
	return nil
}

// systemMessage records what happened in a channel as a message without sender. The text stays empty: clients
// word it themselves from metadata.event, in the language of their user.
func systemMessage(ctx context.Context, q store.Querier, tenantID, channelID uuid.UUID, metadata map[string]any) (message.Appended, error) {
	return message.Append(ctx, q, message.AppendInput{
		TenantID: tenantID, ChannelID: channelID, Type: message.TypeSystem, Metadata: metadata,
	})
}

func externalID(ctx context.Context, q store.Querier, tenantID, userID uuid.UUID) (string, error) {
	u, err := q.GetUserByID(ctx, store.GetUserByIDParams{ID: userID, TenantID: tenantID})
	if err != nil {
		return "", fmt.Errorf("channel: load user %s: %w", userID, err)
	}
	return u.ExternalID, nil
}

func actorExternalID(ctx context.Context, q store.Querier, tenantID uuid.UUID, actor Actor) (string, bool, error) {
	if !actor.Valid {
		return "", false, nil
	}
	ext, err := externalID(ctx, q, tenantID, actor.UUID)
	return ext, err == nil, err
}

// joinIDs is the comma separated form the queries take.
func joinIDs(ids []uuid.UUID) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = id.String()
	}
	return strings.Join(parts, ",")
}

func userView(id uuid.UUID, ext, name string, avatar *string, metadata string, deleted bool) user.User {
	md := json.RawMessage(metadata)
	if len(md) == 0 {
		md = json.RawMessage("{}")
	}
	return user.User{ID: id, ExternalID: ext, Name: name, AvatarURL: avatar, Metadata: md, Deleted: deleted}
}

// wrap adds context to unexpected errors and leaves API errors (404, 403, validation) as they are.
func wrap(err error, what string) error {
	if err == nil {
		return nil
	}
	var apiErr *httpx.Error
	var fields httpx.FieldErrors
	if errors.As(err, &apiErr) || errors.As(err, &fields) {
		return err
	}
	return fmt.Errorf("channel: %s: %w", what, err)
}
