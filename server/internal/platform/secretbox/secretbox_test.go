package secretbox

import (
	"bytes"
	"errors"
	"testing"
)

func newBox(t *testing.T, fill byte) *Box {
	t.Helper()
	b, err := New(bytes.Repeat([]byte{fill}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRoundTrip(t *testing.T) {
	b := newBox(t, 1)
	sealed := b.Seal([]byte("mps_secret"), "tenant/1/api_secret")
	got, err := b.Open(sealed, "tenant/1/api_secret")
	if err != nil || string(got) != "mps_secret" {
		t.Fatalf("Open = %q, %v", got, err)
	}
	if again := b.Seal([]byte("mps_secret"), "tenant/1/api_secret"); bytes.Equal(again, sealed) {
		t.Fatal("two seals of the same value are identical: nonce is not random")
	}
}

func TestOpenRejects(t *testing.T) {
	b := newBox(t, 1)
	sealed := b.Seal([]byte("x"), "tenant/1/api_secret")

	cases := map[string]func() ([]byte, error){
		"other context": func() ([]byte, error) { return b.Open(sealed, "tenant/2/api_secret") },
		"other key":     func() ([]byte, error) { return newBox(t, 2).Open(sealed, "tenant/1/api_secret") },
		"tampered": func() ([]byte, error) {
			c := bytes.Clone(sealed)
			c[len(c)-1] ^= 1
			return b.Open(c, "tenant/1/api_secret")
		},
		"bad version": func() ([]byte, error) {
			c := bytes.Clone(sealed)
			c[0] = 9
			return b.Open(c, "tenant/1/api_secret")
		},
		"too short": func() ([]byte, error) { return b.Open([]byte{1, 2, 3}, "tenant/1/api_secret") },
	}
	for name, open := range cases {
		if _, err := open(); !errors.Is(err, ErrDecrypt) {
			t.Errorf("%s: err = %v, want ErrDecrypt", name, err)
		}
	}
}

func TestNewRejectsShortKey(t *testing.T) {
	if _, err := New(make([]byte, 16)); err == nil {
		t.Fatal("16 byte key accepted")
	}
}
