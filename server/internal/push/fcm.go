// Package push sends notifications about new messages to the devices of members who are not connected (PLAN M4).
package push

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/jwt"
)

// ErrInvalidToken: FCM says the device token is not valid (any more): the app was uninstalled or the token rotated.
// The device is removed.
var ErrInvalidToken = errors.New("push: the device token is not valid")

// SendError is a failure of FCM that is not about the token. Retryable ones (FCM overloaded, network, expired
// authorisation) are worth another attempt later; the others (a request FCM refuses, unusable credentials) are not.
type SendError struct {
	Status    int
	Detail    string
	Retryable bool
}

func (e *SendError) Error() string {
	kind := "permanent"
	if e.Retryable {
		kind = "retryable"
	}
	return fmt.Sprintf("push: FCM %s failure (HTTP %d): %s", kind, e.Status, e.Detail)
}

// Message is one notification for one device.
type Message struct {
	Token    string
	Platform string // android or web
	Data     map[string]string
}

// Sender sends a message with the Firebase service account of a tenant.
type Sender interface {
	Send(ctx context.Context, serviceAccount []byte, m Message) error
}

const (
	fcmScope        = "https://www.googleapis.com/auth/firebase.messaging"
	defaultFCMURL   = "https://fcm.googleapis.com"
	defaultTokenURI = "https://oauth2.googleapis.com/token" //nolint:gosec // G101 false positive: a public URL, not a credential
	// a notification that could not be delivered within an hour is not worth showing any more
	messageTTL = time.Hour
	maxClients = 200
)

// FCM sends through the Firebase Cloud Messaging HTTP v1 API. Each tenant brings its own Firebase project, so each
// service account gets its own access token source, kept until the credentials change.
type FCM struct {
	// BaseURL is https://fcm.googleapis.com unless a test points it elsewhere.
	BaseURL string
	HTTP    *http.Client

	mu      sync.Mutex
	clients map[string]*fcmClient
}

type fcmClient struct {
	projectID string
	tokens    oauth2.TokenSource
}

type serviceAccount struct {
	Type        string `json:"type"`
	ProjectID   string `json:"project_id"`
	PrivateKey  string `json:"private_key"`
	ClientEmail string `json:"client_email"`
	TokenURI    string `json:"token_uri"`
}

func NewFCM() *FCM {
	return &FCM{BaseURL: defaultFCMURL, HTTP: &http.Client{Timeout: 15 * time.Second}, clients: map[string]*fcmClient{}}
}

func (f *FCM) client(raw []byte) (*fcmClient, error) {
	sum := sha256.Sum256(raw)
	key := hex.EncodeToString(sum[:])
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.clients[key]; ok {
		return c, nil
	}
	var sa serviceAccount
	if err := json.Unmarshal(raw, &sa); err != nil || sa.ProjectID == "" || sa.PrivateKey == "" || sa.ClientEmail == "" {
		return nil, &SendError{Detail: "the stored Firebase credentials are not a service account key"}
	}
	if sa.TokenURI == "" {
		sa.TokenURI = defaultTokenURI
	}
	cfg := &jwt.Config{Email: sa.ClientEmail, PrivateKey: []byte(sa.PrivateKey), TokenURL: sa.TokenURI, Scopes: []string{fcmScope}}
	// the token request goes through the same HTTP client as the messages
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, f.HTTP)
	c := &fcmClient{projectID: sa.ProjectID, tokens: oauth2.ReuseTokenSource(nil, cfg.TokenSource(ctx))}
	if len(f.clients) >= maxClients {
		f.clients = map[string]*fcmClient{}
	}
	f.clients[key] = c
	return c, nil
}

// forget drops cached credentials, for when FCM rejected the access token it was given.
func (f *FCM) forget(raw []byte) {
	sum := sha256.Sum256(raw)
	f.mu.Lock()
	delete(f.clients, hex.EncodeToString(sum[:]))
	f.mu.Unlock()
}

type fcmRequest struct {
	Message fcmMessage `json:"message"`
}

type fcmMessage struct {
	Token   string            `json:"token"`
	Data    map[string]string `json:"data"`
	Android *struct {
		Priority string `json:"priority"`
		TTL      string `json:"ttl"`
	} `json:"android,omitempty"`
	Webpush *struct {
		Headers map[string]string `json:"headers"`
	} `json:"webpush,omitempty"`
}

type fcmErrorBody struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Status  string `json:"status"`
		Details []struct {
			ErrorCode string `json:"errorCode"`
		} `json:"details"`
	} `json:"error"`
}

func (b fcmErrorBody) errorCode() string {
	for _, d := range b.Error.Details {
		if d.ErrorCode != "" {
			return d.ErrorCode
		}
	}
	return b.Error.Status
}

// Send delivers a data message: the app decides how to show it (spec/events.md, section 11).
func (f *FCM) Send(ctx context.Context, raw []byte, m Message) error {
	c, err := f.client(raw)
	if err != nil {
		return err
	}
	access, err := c.tokens.Token()
	if err != nil {
		f.forget(raw)
		return &SendError{Detail: "cannot get an access token: " + err.Error(), Retryable: true}
	}
	body := fcmRequest{Message: fcmMessage{Token: m.Token, Data: m.Data}}
	ttl := int(messageTTL.Seconds())
	switch m.Platform {
	case "android":
		body.Message.Android = &struct {
			Priority string `json:"priority"`
			TTL      string `json:"ttl"`
		}{Priority: "HIGH", TTL: fmt.Sprintf("%ds", ttl)}
	case "web":
		body.Message.Webpush = &struct {
			Headers map[string]string `json:"headers"`
		}{Headers: map[string]string{"Urgency": "high", "TTL": fmt.Sprint(ttl)}}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return &SendError{Detail: "cannot encode the message: " + err.Error()}
	}
	url := strings.TrimRight(f.BaseURL, "/") + "/v1/projects/" + c.projectID + "/messages:send"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return &SendError{Detail: err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+access.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	res, err := f.HTTP.Do(req)
	if err != nil {
		return &SendError{Detail: "request failed: " + err.Error(), Retryable: true}
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode == http.StatusOK {
		_, _ = io.Copy(io.Discard, res.Body)
		return nil
	}
	raw1, _ := io.ReadAll(io.LimitReader(res.Body, 8<<10))
	var eb fcmErrorBody
	_ = json.Unmarshal(raw1, &eb)
	code := eb.errorCode()
	switch {
	case res.StatusCode == http.StatusNotFound, code == "UNREGISTERED", code == "SENDER_ID_MISMATCH":
		return fmt.Errorf("%w (%s)", ErrInvalidToken, code)
	case res.StatusCode == http.StatusBadRequest && strings.Contains(strings.ToLower(eb.Error.Message), "registration token"):
		return fmt.Errorf("%w (%s)", ErrInvalidToken, eb.Error.Message)
	case res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden:
		f.forget(raw) // the access token may have been revoked: the next attempt asks for a new one
		return &SendError{Status: res.StatusCode, Detail: summary(eb, raw1), Retryable: true}
	case res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500:
		return &SendError{Status: res.StatusCode, Detail: summary(eb, raw1), Retryable: true}
	default:
		return &SendError{Status: res.StatusCode, Detail: summary(eb, raw1)}
	}
}

func summary(eb fcmErrorBody, raw []byte) string {
	if eb.Error.Message != "" {
		return eb.Error.Status + ": " + eb.Error.Message
	}
	if len(raw) > 200 {
		raw = raw[:200]
	}
	return string(raw)
}
