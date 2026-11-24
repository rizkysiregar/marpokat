package console

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
)

// Codes of the robot check (PLAN D72).
const (
	CodeCaptchaFailed      = "captcha_failed"
	CodeCaptchaUnavailable = "captcha_unavailable"
)

// Captcha checks that a request was made by a person. Sign up and password reset ask for it: those are where a robot
// does harm (accounts made by the thousand, e-mail sent to strangers). A nil Captcha means no check.
type Captcha interface {
	// Verify returns nil when the token proves a person, an *httpx.Error to show otherwise.
	Verify(ctx context.Context, token, ip string) error
}

// Turnstile is Cloudflare's robot check. The page shows the widget, which hands the browser a token; the token is sent
// with the form and checked here with the secret key, which never leaves the server. It fails closed: when Cloudflare
// cannot be reached, the form says to try again rather than letting robots through.
type Turnstile struct {
	secret    string
	verifyURL string
	client    *http.Client
	log       *slog.Logger
}

func NewTurnstile(secret, verifyURL string, log *slog.Logger) *Turnstile {
	return &Turnstile{secret: secret, verifyURL: verifyURL, client: &http.Client{Timeout: 5 * time.Second}, log: log}
}

var (
	errCaptchaMissing = httpx.NewError(400, CodeCaptchaFailed, "confirm that you are not a robot and try again")
	errCaptchaRefused = httpx.NewError(400, CodeCaptchaFailed, "the robot check did not pass, try again")
	errCaptchaDown    = httpx.NewError(503, CodeCaptchaUnavailable, "we could not check that you are not a robot, try again in a moment")
)

func (t *Turnstile) Verify(ctx context.Context, token, ip string) error {
	token = strings.TrimSpace(token)
	if token == "" || len(token) > 2048 { // Cloudflare's tokens are far shorter
		return errCaptchaMissing
	}
	form := url.Values{"secret": {t.secret}, "response": {token}}
	if ip != "" {
		form.Set("remoteip", ip)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.verifyURL, strings.NewReader(form.Encode())) //nolint:gosec // the URL is from configuration, not the request
	if err != nil {
		return fmt.Errorf("console: turnstile request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := t.client.Do(req) //nolint:gosec // see above: a configured URL
	if err != nil {
		t.log.Warn("turnstile unreachable", "error", err)
		return errCaptchaDown
	}
	defer func() { _ = res.Body.Close() }()
	var out struct {
		Success    bool     `json:"success"`
		ErrorCodes []string `json:"error-codes"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&out) != nil {
		t.log.Warn("turnstile answered something unexpected", "status", res.StatusCode)
		return errCaptchaDown
	}
	if !out.Success {
		t.log.Info("turnstile refused a request", "errors", out.ErrorCodes)
		return errCaptchaRefused
	}
	return nil
}
