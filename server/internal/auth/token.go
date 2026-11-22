// Package auth verifies the tokens tenants sign with their api secret (PLAN D10, openapi securitySchemes).
//
//   - user token:   HS256, claims {sub: external user id, iat, exp}. Signed by the tenant backend, used by apps.
//   - server token: HS256, claims {server: true, iat, exp}, exp at most 5 minutes after iat. Used by the tenant
//     backend on /v1/server/*. Short-lived on purpose: a leaked one is useless within minutes.
package auth

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	ErrTokenExpired = errors.New("token expired")
	ErrTokenInvalid = errors.New("token invalid")
)

const (
	// Leeway absorbs clock differences between the tenant backend and Marpokat.
	Leeway = 30 * time.Second
	// MaxServerTokenLifetime is the longest exp - iat accepted for a server token.
	MaxServerTokenLifetime = 5 * time.Minute
	maxExternalIDLength    = 255
)

type claims struct {
	jwt.RegisteredClaims
	Server bool `json:"server,omitempty"`
}

func parse(token string, secret []byte, now time.Time) (*claims, error) {
	// jwt reports a wrong algorithm as a signature error; check it first for a clearer message.
	if unverified, _, err := jwt.NewParser().ParseUnverified(token, jwt.MapClaims{}); err == nil &&
		unverified.Method.Alg() != jwt.SigningMethodHS256.Alg() {
		return nil, fmt.Errorf("%w: algorithm %s is not supported, use HS256", ErrTokenInvalid, unverified.Method.Alg())
	}
	c := &claims{}
	_, err := jwt.ParseWithClaims(token, c,
		func(*jwt.Token) (any, error) { return secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(Leeway),
		jwt.WithTimeFunc(func() time.Time { return now }),
	)
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return nil, ErrTokenExpired
		}
		return nil, fmt.Errorf("%w: %s", ErrTokenInvalid, reason(err))
	}
	return c, nil
}

// VerifyUserToken checks a user token and returns its subject (the external user id).
func VerifyUserToken(token string, secret []byte, now time.Time) (string, error) {
	sub, _, err := VerifyUserTokenExpiry(token, secret, now)
	return sub, err
}

// VerifyUserTokenExpiry is VerifyUserToken that also returns when the token expires. A WebSocket stays open far
// longer than one request, so it must end the connection itself at that moment.
func VerifyUserTokenExpiry(token string, secret []byte, now time.Time) (string, time.Time, error) {
	c, err := parse(token, secret, now)
	if err != nil {
		return "", time.Time{}, err
	}
	if c.Server {
		return "", time.Time{}, fmt.Errorf("%w: a server token cannot be used as a user token", ErrTokenInvalid)
	}
	sub := strings.TrimSpace(c.Subject)
	if sub == "" || len(sub) > maxExternalIDLength {
		return "", time.Time{}, fmt.Errorf("%w: sub must be the external user id (1-%d characters)", ErrTokenInvalid, maxExternalIDLength)
	}
	return sub, c.ExpiresAt.Time, nil
}

// VerifyServerToken checks a server token.
func VerifyServerToken(token string, secret []byte, now time.Time) error {
	c, err := parse(token, secret, now)
	if err != nil {
		return err
	}
	if !c.Server {
		return fmt.Errorf("%w: claim server=true is required", ErrTokenInvalid)
	}
	if c.IssuedAt == nil {
		return fmt.Errorf("%w: claim iat is required", ErrTokenInvalid)
	}
	if c.ExpiresAt.Sub(c.IssuedAt.Time) > MaxServerTokenLifetime {
		return fmt.Errorf("%w: exp must be at most %s after iat", ErrTokenInvalid, MaxServerTokenLifetime)
	}
	return nil
}

// SignUserToken creates a user token. Tenant backends do the same in their own language; Marpokat uses it in
// the CLI (`marpokat tenant token`) and in tests.
func SignUserToken(secret []byte, externalID string, ttl time.Duration, now time.Time) (string, error) {
	return sign(secret, claims{RegisteredClaims: jwt.RegisteredClaims{
		Subject:   externalID,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
	}})
}

// SignServerToken creates a server token valid for ttl (at most MaxServerTokenLifetime).
func SignServerToken(secret []byte, ttl time.Duration, now time.Time) (string, error) {
	if ttl > MaxServerTokenLifetime {
		ttl = MaxServerTokenLifetime
	}
	return sign(secret, claims{Server: true, RegisteredClaims: jwt.RegisteredClaims{
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
	}})
}

func sign(secret []byte, c claims) (string, error) {
	return jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString(secret)
}

// reason turns jwt errors into a short message safe to return to the caller.
func reason(err error) string {
	switch {
	case errors.Is(err, jwt.ErrTokenMalformed):
		return "malformed token"
	case errors.Is(err, jwt.ErrTokenSignatureInvalid), errors.Is(err, jwt.ErrSignatureInvalid):
		return "signature does not match the api secret of this api key"
	case errors.Is(err, jwt.ErrTokenUnverifiable):
		return "unsupported algorithm, use HS256"
	case errors.Is(err, jwt.ErrTokenRequiredClaimMissing):
		return "claim exp is required"
	case errors.Is(err, jwt.ErrTokenUsedBeforeIssued), errors.Is(err, jwt.ErrTokenNotValidYet):
		return "token is not valid yet (check the clock of the signing server)"
	default:
		return "cannot verify token"
	}
}
