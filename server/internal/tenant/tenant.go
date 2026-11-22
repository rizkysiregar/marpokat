// Package tenant manages the apps that use Marpokat (PLAN D1, M1).
//
// A tenant has a public api key (shipped in apps) and an api secret (kept in the tenant backend) that signs user
// and server tokens. The secret is stored encrypted with the master key and shown only when created or rotated.
package tenant

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/rizkysiregar/marpokat/server/internal/platform/secretbox"
	"github.com/rizkysiregar/marpokat/server/internal/store"
)

var (
	ErrNotFound    = errors.New("tenant: not found")
	ErrSlugTaken   = errors.New("tenant: slug already exists")
	ErrInvalidSlug = errors.New("tenant: slug must be 2-63 characters of a-z, 0-9 and '-', starting with a letter or digit")
)

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

const (
	apiKeyPrefix    = "mpk_"
	apiSecretPrefix = "mps_"
	webhookPrefix   = "mpw_"
)

// Config is what a tenant allows its clients to do on their own (PLAN D11). Zero value = most restrictive.
type Config struct {
	ClientCanCreateDM    bool `json:"client_can_create_dm"`
	ClientCanCreateGroup bool `json:"client_can_create_group"`
}

type Tenant struct {
	ID           uuid.UUID
	Slug         string
	Name         string
	APIKey       string
	Config       Config
	HasFCM       bool
	WebhookURL   *string
	CreatedAt    time.Time
	DisabledAt   *time.Time
	apiSecretEnc []byte
}

func (t Tenant) Disabled() bool { return t.DisabledAt != nil }

type Service struct {
	q   store.Querier
	box *secretbox.Box
}

func NewService(q store.Querier, box *secretbox.Box) *Service { return &Service{q: q, box: box} }

func secretContext(id uuid.UUID, field string) string { return "tenant/" + id.String() + "/" + field }

// Create registers a tenant and returns it with its api secret. The secret is never shown again.
func (s *Service) Create(ctx context.Context, slug, name string, cfg Config) (Tenant, string, error) {
	if !slugPattern.MatchString(slug) {
		return Tenant{}, "", ErrInvalidSlug
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = slug
	}
	id := uuid.Must(uuid.NewV7())
	apiKey := apiKeyPrefix + randomHex(16)
	secret := apiSecretPrefix + randomBase64(32)
	cfgJSON, err := json.Marshal(cfg)
	if err != nil {
		return Tenant{}, "", err
	}
	row, err := s.q.CreateTenant(ctx, store.CreateTenantParams{
		ID:           id,
		Slug:         slug,
		Name:         name,
		ApiKey:       apiKey,
		ApiSecretEnc: s.box.Seal([]byte(secret), secretContext(id, "api_secret")),
		Config:       string(cfgJSON),
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Tenant{}, "", ErrSlugTaken
		}
		return Tenant{}, "", fmt.Errorf("tenant: create: %w", err)
	}
	t, err := fromRow(row)
	return t, secret, err
}

func (s *Service) GetBySlug(ctx context.Context, slug string) (Tenant, error) {
	row, err := s.q.GetTenantBySlug(ctx, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return Tenant{}, ErrNotFound
	}
	if err != nil {
		return Tenant{}, fmt.Errorf("tenant: get: %w", err)
	}
	return fromRow(row)
}

func (s *Service) List(ctx context.Context) ([]Tenant, error) {
	rows, err := s.q.ListTenants(ctx)
	if err != nil {
		return nil, fmt.Errorf("tenant: list: %w", err)
	}
	out := make([]Tenant, 0, len(rows))
	for _, r := range rows {
		t, err := fromRow(r)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// Credentials returns the enabled tenant that owns apiKey and its decrypted api secret. Used by authentication.
func (s *Service) Credentials(ctx context.Context, apiKey string) (Tenant, []byte, error) {
	if !strings.HasPrefix(apiKey, apiKeyPrefix) {
		return Tenant{}, nil, ErrNotFound
	}
	row, err := s.q.GetTenantByAPIKey(ctx, apiKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return Tenant{}, nil, ErrNotFound
	}
	if err != nil {
		return Tenant{}, nil, fmt.Errorf("tenant: get by api key: %w", err)
	}
	t, err := fromRow(row)
	if err != nil {
		return Tenant{}, nil, err
	}
	secret, err := s.box.Open(t.apiSecretEnc, secretContext(t.ID, "api_secret"))
	if err != nil {
		return Tenant{}, nil, fmt.Errorf("tenant %s: api secret: %w", t.Slug, err)
	}
	return t, secret, nil
}

// Secret decrypts the api secret of a tenant (CLI: signing test tokens).
func (s *Service) Secret(t Tenant) ([]byte, error) {
	return s.box.Open(t.apiSecretEnc, secretContext(t.ID, "api_secret"))
}

// RotateSecret replaces the api secret at once: tokens signed with the old one stop working within the
// authentication cache TTL. The tenant backend must switch to the new secret right after.
func (s *Service) RotateSecret(ctx context.Context, slug string) (string, error) {
	t, err := s.GetBySlug(ctx, slug)
	if err != nil {
		return "", err
	}
	secret := apiSecretPrefix + randomBase64(32)
	if _, err := s.q.UpdateTenantSecret(ctx, store.UpdateTenantSecretParams{
		ID:           t.ID,
		ApiSecretEnc: s.box.Seal([]byte(secret), secretContext(t.ID, "api_secret")),
	}); err != nil {
		return "", fmt.Errorf("tenant: rotate secret: %w", err)
	}
	return secret, nil
}

func (s *Service) SetConfig(ctx context.Context, slug string, cfg Config) error {
	t, err := s.GetBySlug(ctx, slug)
	if err != nil {
		return err
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	if _, err := s.q.UpdateTenantConfig(ctx, store.UpdateTenantConfigParams{ID: t.ID, Config: string(b)}); err != nil {
		return fmt.Errorf("tenant: set config: %w", err)
	}
	return nil
}

func (s *Service) SetDisabled(ctx context.Context, slug string, disabled bool) error {
	t, err := s.GetBySlug(ctx, slug)
	if err != nil {
		return err
	}
	if _, err := s.q.SetTenantDisabled(ctx, store.SetTenantDisabledParams{ID: t.ID, Disabled: disabled}); err != nil {
		return fmt.Errorf("tenant: set disabled: %w", err)
	}
	return nil
}

// SetFCMCredentials stores the Firebase service account JSON used to send pushes for this tenant (M4).
func (s *Service) SetFCMCredentials(ctx context.Context, slug string, serviceAccountJSON []byte) error {
	var sa struct {
		Type        string `json:"type"`
		ProjectID   string `json:"project_id"`
		PrivateKey  string `json:"private_key"`
		ClientEmail string `json:"client_email"`
	}
	if err := json.Unmarshal(serviceAccountJSON, &sa); err != nil {
		return fmt.Errorf("tenant: FCM credentials are not JSON: %w", err)
	}
	if sa.Type != "service_account" || sa.ProjectID == "" || sa.PrivateKey == "" || sa.ClientEmail == "" {
		return errors.New("tenant: FCM credentials must be a Firebase service account key (type, project_id, private_key, client_email)")
	}
	t, err := s.GetBySlug(ctx, slug)
	if err != nil {
		return err
	}
	if _, err := s.q.UpdateTenantFCMCredentials(ctx, store.UpdateTenantFCMCredentialsParams{
		ID:                t.ID,
		FcmCredentialsEnc: s.box.Seal(serviceAccountJSON, secretContext(t.ID, "fcm_credentials")),
	}); err != nil {
		return fmt.Errorf("tenant: set FCM credentials: %w", err)
	}
	return nil
}

// SetWebhook sets the webhook URL and returns a new signing secret. An empty URL removes the webhook.
func (s *Service) SetWebhook(ctx context.Context, slug, rawURL string) (string, error) {
	t, err := s.GetBySlug(ctx, slug)
	if err != nil {
		return "", err
	}
	params := store.UpdateTenantWebhookParams{ID: t.ID}
	secret := ""
	if rawURL != "" {
		u, err := url.Parse(rawURL)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return "", errors.New("tenant: webhook URL must be an absolute https URL")
		}
		secret = webhookPrefix + randomBase64(32)
		params.WebhookUrl = &rawURL
		params.WebhookSecretEnc = s.box.Seal([]byte(secret), secretContext(t.ID, "webhook_secret"))
	}
	if _, err := s.q.UpdateTenantWebhook(ctx, params); err != nil {
		return "", fmt.Errorf("tenant: set webhook: %w", err)
	}
	return secret, nil
}

func fromRow(r store.Tenant) (Tenant, error) {
	var cfg Config
	if len(r.Config) > 0 {
		if err := json.Unmarshal([]byte(r.Config), &cfg); err != nil {
			return Tenant{}, fmt.Errorf("tenant %s: config: %w", r.Slug, err)
		}
	}
	return Tenant{
		ID:           r.ID,
		Slug:         r.Slug,
		Name:         r.Name,
		APIKey:       r.ApiKey,
		Config:       cfg,
		HasFCM:       len(r.FcmCredentialsEnc) > 0,
		WebhookURL:   r.WebhookUrl,
		CreatedAt:    r.CreatedAt,
		DisabledAt:   r.DisabledAt,
		apiSecretEnc: r.ApiSecretEnc,
	}, nil
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("tenant: crypto/rand failed: " + err.Error())
	}
	return b
}

func randomHex(n int) string    { return hex.EncodeToString(randomBytes(n)) }
func randomBase64(n int) string { return base64.RawURLEncoding.EncodeToString(randomBytes(n)) }
