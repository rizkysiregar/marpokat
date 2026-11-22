// Package channel manages DMs and groups, their members and the list of channels of a user (PLAN M2).
package channel

import (
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/rizkysiregar/marpokat/server/internal/message"
	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
	"github.com/rizkysiregar/marpokat/server/internal/user"
)

const (
	TypeDM    = "dm"
	TypeGroup = "group"

	RoleOwner  = "owner"
	RoleAdmin  = "admin"
	RoleMember = "member"

	// MaxGroupMembers counts every active member, the owner included (openapi Limits).
	MaxGroupMembers = 256
	MaxNameLength   = 100
	maxAvatarURL    = 2048
)

// Channel is the public shape for a member (openapi components/schemas/Channel).
type Channel struct {
	ID             uuid.UUID        `json:"id"`
	Type           string           `json:"type"`
	Name           *string          `json:"name"`
	AvatarURL      *string          `json:"avatar_url"`
	MemberCount    int              `json:"member_count"`
	DMPeer         *user.User       `json:"dm_peer"`
	LastSeq        int64            `json:"last_seq"`
	LastMessageSeq int64            `json:"last_message_seq"`
	LastMessage    *message.Message `json:"last_message"`
	LastMessageAt  *time.Time       `json:"last_message_at"`
	Frozen         bool             `json:"frozen"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
	Membership     Membership       `json:"membership"`
}

// Membership is the reader's own membership.
type Membership struct {
	Role        string     `json:"role"`
	LastReadSeq int64      `json:"last_read_seq"`
	UnreadCount int        `json:"unread_count"`
	MutedUntil  *time.Time `json:"muted_until"`
	JoinedAt    time.Time  `json:"joined_at"`
	LeftAt      *time.Time `json:"left_at"`
}

// Member is one member of a channel (openapi components/schemas/Member).
type Member struct {
	User        user.User `json:"user"`
	Role        string    `json:"role"`
	LastReadSeq int64     `json:"last_read_seq"`
	JoinedAt    time.Time `json:"joined_at"`
}

// ServerChannel is the channel as the tenant backend sees it: no reader, members by external id.
type ServerChannel struct {
	ID          uuid.UUID `json:"id"`
	Type        string    `json:"type"`
	Name        *string   `json:"name"`
	AvatarURL   *string   `json:"avatar_url"`
	MemberCount int       `json:"member_count"`
	MemberIDs   []string  `json:"member_ids"`
	Frozen      bool      `json:"frozen"`
	CreatedAt   time.Time `json:"created_at"`
}

type ChannelPage struct {
	Channels   []Channel `json:"channels"`
	NextCursor *string   `json:"next_cursor"`
}

type MemberPage struct {
	Members    []Member `json:"members"`
	NextCursor *string  `json:"next_cursor"`
}

// DMKey is the same for both orders of the pair, so a pair of users has at most one DM (UNIQUE dm_key).
func DMKey(tenantID, a, b uuid.UUID) string {
	x, y := a.String(), b.String()
	if y < x {
		x, y = y, x
	}
	return tenantID.String() + ":" + x + ":" + y
}

// cursor is a position in a list: a timestamp and an id, as an opaque string.
func encodeCursor(at time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("%d|%s", at.UnixMicro(), id)))
}

func decodeCursor(s string) (time.Time, uuid.UUID, error) {
	bad := httpx.FieldErrors{"cursor": "is not a cursor returned by this API"}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, uuid.Nil, bad
	}
	micros, idStr, ok := strings.Cut(string(raw), "|")
	if !ok {
		return time.Time{}, uuid.Nil, bad
	}
	var n int64
	if _, err := fmt.Sscanf(micros, "%d", &n); err != nil {
		return time.Time{}, uuid.Nil, bad
	}
	id, err := uuid.Parse(idStr)
	if err != nil {
		return time.Time{}, uuid.Nil, bad
	}
	return time.UnixMicro(n).UTC(), id, nil
}

// normalizeName trims and validates a group name.
func normalizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > MaxNameLength {
		return "", httpx.FieldErrors{"name": fmt.Sprintf("is required: 1-%d characters", MaxNameLength)}
	}
	return name, nil
}

// normalizeAvatar validates an optional avatar URL; empty means none.
func normalizeAvatar(v *string) (*string, error) {
	if v == nil {
		return nil, nil
	}
	s := strings.TrimSpace(*v)
	if s == "" {
		return nil, nil
	}
	if !user.IsHTTPURL(s) || len(s) > maxAvatarURL {
		return nil, httpx.FieldErrors{"avatar_url": fmt.Sprintf("must be an absolute http(s) URL of at most %d characters", maxAvatarURL)}
	}
	return &s, nil
}
