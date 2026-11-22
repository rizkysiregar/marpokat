// Package user manages the end users of a tenant. Users are created and updated by the tenant backend through
// the server API; Marpokat never decides who exists (PLAN section 10, NV-04).
package user

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
	"github.com/rizkysiregar/marpokat/server/internal/store"
)

var ErrNotFound = errors.New("user: not found")

const (
	maxExternalID = 255
	maxName       = 100
	maxAvatarURL  = 2048
	maxMetadata   = 4 << 10
)

// User is the public shape of a user (openapi components/schemas/User).
type User struct {
	ID         uuid.UUID       `json:"id"`
	ExternalID string          `json:"external_id"`
	Name       string          `json:"name"`
	AvatarURL  *string         `json:"avatar_url"`
	Metadata   json.RawMessage `json:"metadata"`
	Deleted    bool            `json:"deleted"`
}

// UpsertInput is the body of PUT /v1/server/users/{external_id}.
type UpsertInput struct {
	Name      string          `json:"name"`
	AvatarURL *string         `json:"avatar_url"`
	Metadata  json.RawMessage `json:"metadata"`
}

type Service struct {
	pool *pgxpool.Pool
	q    store.Querier
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool, q: store.New(pool)} }

// ValidateExternalID checks an external id from a path or token.
func ValidateExternalID(id string) error {
	if id == "" || len(id) > maxExternalID || !utf8.ValidString(id) {
		return fmt.Errorf("must be 1-%d bytes of UTF-8", maxExternalID)
	}
	for _, r := range id {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return errors.New("must not contain spaces or control characters")
		}
	}
	return nil
}

// IsHTTPURL reports whether s is an absolute http or https URL.
func IsHTTPURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != ""
}

// Validate normalises the input and returns field errors (httpx.FieldErrors) when it is not acceptable.
func (in *UpsertInput) Validate() error {
	errs := httpx.FieldErrors{}
	in.Name = strings.TrimSpace(in.Name)
	if utf8.RuneCountInString(in.Name) > maxName {
		errs["name"] = fmt.Sprintf("must be at most %d characters", maxName)
	}
	if in.AvatarURL != nil {
		v := strings.TrimSpace(*in.AvatarURL)
		if v == "" {
			in.AvatarURL = nil
		} else if !IsHTTPURL(v) || len(v) > maxAvatarURL {
			errs["avatar_url"] = fmt.Sprintf("must be an absolute http(s) URL of at most %d characters", maxAvatarURL)
		} else {
			in.AvatarURL = &v
		}
	}
	md := bytes.TrimSpace(in.Metadata)
	switch {
	case len(md) == 0 || bytes.Equal(md, []byte("null")):
		in.Metadata = json.RawMessage("{}")
	case md[0] != '{':
		errs["metadata"] = "must be a JSON object"
	case len(md) > maxMetadata:
		errs["metadata"] = fmt.Sprintf("must be at most %d bytes", maxMetadata)
	default:
		in.Metadata = md
	}
	return errs.OrNil()
}

// Upsert creates or updates the user with this external id in the tenant.
func (s *Service) Upsert(ctx context.Context, tenantID uuid.UUID, externalID string, in UpsertInput) (User, error) {
	if err := ValidateExternalID(externalID); err != nil {
		return User{}, httpx.FieldErrors{"external_id": err.Error()}
	}
	if err := in.Validate(); err != nil {
		return User{}, err
	}
	row, err := s.q.UpsertUser(ctx, store.UpsertUserParams{
		ID:         uuid.Must(uuid.NewV7()),
		TenantID:   tenantID,
		ExternalID: externalID,
		Name:       in.Name,
		AvatarUrl:  in.AvatarURL,
		Metadata:   string(in.Metadata),
	})
	if err != nil {
		return User{}, fmt.Errorf("user: upsert: %w", err)
	}
	return fromRow(row), nil
}

// Get returns an existing (not deleted) user.
func (s *Service) Get(ctx context.Context, tenantID uuid.UUID, externalID string) (User, error) {
	row, err := s.q.GetUserByExternalID(ctx, store.GetUserByExternalIDParams{TenantID: tenantID, ExternalID: externalID})
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("user: get: %w", err)
	}
	return fromRow(row), nil
}

// ResolveID maps an external id to the internal id. Used by authentication on every request.
func (s *Service) ResolveID(ctx context.Context, tenantID uuid.UUID, externalID string) (uuid.UUID, error) {
	id, err := s.q.GetUserIDByExternalID(ctx, store.GetUserIDByExternalIDParams{TenantID: tenantID, ExternalID: externalID})
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, fmt.Errorf("user: resolve: %w", err)
	}
	return id, nil
}

// Delete anonymises the user, removes their push tokens and takes them out of every channel. Idempotent.
func (s *Service) Delete(ctx context.Context, tenantID uuid.UUID, externalID string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		id, err := q.AnonymizeUser(ctx, store.AnonymizeUserParams{TenantID: tenantID, ExternalID: externalID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // unknown or already deleted
		}
		if err != nil {
			return fmt.Errorf("user: anonymise: %w", err)
		}
		if err := q.DeleteUserDevices(ctx, store.DeleteUserDevicesParams{TenantID: tenantID, UserID: id}); err != nil {
			return fmt.Errorf("user: delete devices: %w", err)
		}
		if err := q.LeaveAllChannels(ctx, store.LeaveAllChannelsParams{TenantID: tenantID, UserID: id}); err != nil {
			return fmt.Errorf("user: leave channels: %w", err)
		}
		return nil
	})
}

func fromRow(r store.User) User {
	md := json.RawMessage(r.Metadata)
	if len(md) == 0 {
		md = json.RawMessage("{}")
	}
	return User{
		ID:         r.ID,
		ExternalID: r.ExternalID,
		Name:       r.Name,
		AvatarURL:  r.AvatarUrl,
		Metadata:   md,
		Deleted:    r.DeletedAt != nil,
	}
}
