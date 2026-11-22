package auth

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

var (
	secret = []byte("mps_test_secret_test_secret_test_secret")
	now    = time.Unix(1_800_000_000, 0)
)

func TestUserTokenRoundTrip(t *testing.T) {
	tok, err := SignUserToken(secret, "u_123", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := VerifyUserToken(tok, secret, now.Add(10*time.Minute))
	if err != nil || sub != "u_123" {
		t.Fatalf("VerifyUserToken = %q, %v", sub, err)
	}
}

func TestUserTokenRejections(t *testing.T) {
	valid, _ := SignUserToken(secret, "u_123", time.Hour, now)
	server, _ := SignServerToken(secret, time.Minute, now)
	noExp, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"sub": "u_1", "iat": now.Unix()}).SignedString(secret)
	noSub, _ := SignUserToken(secret, "", time.Hour, now)
	hs512, _ := jwt.NewWithClaims(jwt.SigningMethodHS512, jwt.MapClaims{"sub": "u_1", "iat": now.Unix(), "exp": now.Add(time.Hour).Unix()}).SignedString(secret)
	future, _ := SignUserToken(secret, "u_1", time.Hour, now.Add(time.Hour))
	unsigned := strings.Join(strings.Split(valid, ".")[:2], ".") + "."

	cases := []struct {
		name  string
		token string
		key   []byte
		at    time.Time
		want  error
		text  string
	}{
		{"expired", valid, secret, now.Add(2 * time.Hour), ErrTokenExpired, ""},
		{"within leeway", valid, secret, now.Add(time.Hour + 20*time.Second), nil, ""},
		{"wrong secret", valid, []byte("other"), now, ErrTokenInvalid, "signature"},
		{"server token as user", server, secret, now, ErrTokenInvalid, "server token"},
		{"missing exp", noExp, secret, now, ErrTokenInvalid, "exp is required"},
		{"empty sub", noSub, secret, now, ErrTokenInvalid, "sub"},
		{"other algorithm", hs512, secret, now, ErrTokenInvalid, "HS256"},
		{"alg none", unsigned, secret, now, ErrTokenInvalid, ""},
		{"issued in the future", future, secret, now, ErrTokenInvalid, "not valid yet"},
		{"garbage", "abc", secret, now, ErrTokenInvalid, "malformed"},
	}
	for _, c := range cases {
		_, err := VerifyUserToken(c.token, c.key, c.at)
		if c.want == nil {
			if err != nil {
				t.Errorf("%s: unexpected error %v", c.name, err)
			}
			continue
		}
		if !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
			continue
		}
		if c.text != "" && !strings.Contains(err.Error(), c.text) {
			t.Errorf("%s: err = %q, want it to mention %q", c.name, err, c.text)
		}
	}
}

func TestServerToken(t *testing.T) {
	tok, _ := SignServerToken(secret, time.Minute, now)
	if err := VerifyServerToken(tok, secret, now.Add(30*time.Second)); err != nil {
		t.Fatalf("valid server token rejected: %v", err)
	}

	user, _ := SignUserToken(secret, "u_1", time.Minute, now)
	if err := VerifyServerToken(user, secret, now); !errors.Is(err, ErrTokenInvalid) {
		t.Fatalf("user token accepted as server token: %v", err)
	}

	long, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"server": true, "iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}).SignedString(secret)
	if err := VerifyServerToken(long, secret, now); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("1h server token accepted: %v", err)
	}

	noIat, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{
		"server": true, "exp": now.Add(time.Minute).Unix(),
	}).SignedString(secret)
	if err := VerifyServerToken(noIat, secret, now); err == nil || !strings.Contains(err.Error(), "iat") {
		t.Fatalf("server token without iat accepted: %v", err)
	}

	capped, _ := SignServerToken(secret, time.Hour, now)
	if err := VerifyServerToken(capped, secret, now); err != nil {
		t.Fatalf("SignServerToken did not cap the lifetime: %v", err)
	}
}
