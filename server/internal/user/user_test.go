package user

import (
	"errors"
	"strings"
	"testing"

	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
)

func ptr(s string) *string { return &s }

func TestValidateExternalID(t *testing.T) {
	for _, ok := range []string{"u_123", "42", "user@example.com", "ユーザー"} {
		if err := ValidateExternalID(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "a b", "a\nb", strings.Repeat("x", 256), "\xff"} {
		if err := ValidateExternalID(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestUpsertInputValidate(t *testing.T) {
	in := UpsertInput{Name: "  Rizky  ", AvatarURL: ptr(" https://cdn.example.com/a.png "), Metadata: []byte(` {"role":"admin"} `)}
	if err := in.Validate(); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	if in.Name != "Rizky" || *in.AvatarURL != "https://cdn.example.com/a.png" || string(in.Metadata) != `{"role":"admin"}` {
		t.Fatalf("not normalised: %+v", in)
	}

	empty := UpsertInput{AvatarURL: ptr("")}
	if err := empty.Validate(); err != nil || empty.AvatarURL != nil || string(empty.Metadata) != "{}" {
		t.Fatalf("empty optional fields: err=%v %+v", err, empty)
	}

	bad := UpsertInput{
		Name:      strings.Repeat("n", 101),
		AvatarURL: ptr("javascript:alert(1)"),
		Metadata:  []byte(`[1,2]`),
	}
	err := bad.Validate()
	var fields httpx.FieldErrors
	if !errors.As(err, &fields) {
		t.Fatalf("err = %v, want FieldErrors", err)
	}
	for _, f := range []string{"name", "avatar_url", "metadata"} {
		if _, ok := fields[f]; !ok {
			t.Errorf("no error for %s: %v", f, fields)
		}
	}

	big := UpsertInput{Metadata: []byte(`{"x":"` + strings.Repeat("a", 5000) + `"}`)}
	if err := big.Validate(); err == nil {
		t.Fatal("5 KB metadata accepted")
	}
}
