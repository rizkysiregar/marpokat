package message

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
)

func TestNormalizeMime(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"image/jpeg", "image/jpeg", true},
		{"  IMAGE/PNG ", "image/png", true},
		{"text/plain; charset=utf-8", "text/plain", true},
		{"application/vnd.ms-excel", "application/vnd.ms-excel", true},
		{"application/vnd.openxmlformats-officedocument.wordprocessingml.document", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", true},
		{"photo", "", false},
		{"image/", "", false},
		{"/jpeg", "", false},
		{"a b/c", "", false},
		{"image/jpeg\r\nX-Evil: 1", "", false},
		{strings.Repeat("a", 200) + "/b", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := NormalizeMime(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("NormalizeMime(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestKindsAndLimits(t *testing.T) {
	for mime, kind := range map[string]string{
		"image/jpeg": KindImage, "image/png": KindImage, "image/webp": KindImage, "image/heic": KindImage,
		"video/mp4": KindVideo, "video/quicktime": KindVideo,
		"image/svg+xml": KindFile, // can carry script: never shown inline
		"image/tiff":    KindFile, "application/pdf": KindFile, "text/html": KindFile, "audio/mpeg": KindFile,
	} {
		if got := KindOf(mime); got != kind {
			t.Errorf("KindOf(%q) = %q, want %q", mime, got, kind)
		}
	}
	if MaxSize(KindImage) != 10<<20 || MaxSize(KindVideo) != 50<<20 || MaxSize(KindFile) != 25<<20 {
		t.Error("limits differ from the spec")
	}
}

func TestSendInputValidate(t *testing.T) {
	id := uuid.New()
	ok := func(in SendInput) bool { return in.Validate() == nil }
	if !ok(SendInput{ClientMsgID: "c1", Text: "hi"}) {
		t.Error("a plain message was refused")
	}
	if !ok(SendInput{ClientMsgID: "c1", AttachmentIDs: []uuid.UUID{id}}) {
		t.Error("a message with only an attachment was refused")
	}
	if ok(SendInput{ClientMsgID: "c1"}) || ok(SendInput{ClientMsgID: "c1", Text: "   "}) {
		t.Error("an empty message was accepted")
	}
	if ok(SendInput{Text: "hi"}) || ok(SendInput{ClientMsgID: "has space", Text: "hi"}) || ok(SendInput{ClientMsgID: strings.Repeat("x", 65), Text: "hi"}) {
		t.Error("a bad client_msg_id was accepted")
	}
	if ok(SendInput{ClientMsgID: "c1", AttachmentIDs: []uuid.UUID{id, id}}) {
		t.Error("the same attachment twice was accepted")
	}
	many := make([]uuid.UUID, MaxAttachmentsPerMessage+1)
	for i := range many {
		many[i] = uuid.New()
	}
	if ok(SendInput{ClientMsgID: "c1", AttachmentIDs: many}) {
		t.Error("more attachments than allowed was accepted")
	}
	if ok(SendInput{ClientMsgID: "c1", Text: strings.Repeat("é", MaxTextLength+1)}) || !ok(SendInput{ClientMsgID: "c1", Text: strings.Repeat("é", MaxTextLength)}) {
		t.Error("the text limit counts characters, not bytes")
	}
	in := SendInput{ClientMsgID: " c1 ", Text: "  padded  "}
	_ = in.Validate()
	if in.ClientMsgID != "c1" || in.Text != "padded" {
		t.Errorf("input was not trimmed: %+v", in)
	}
}

func TestParseSendRefs(t *testing.T) {
	good := uuid.NewString()
	ids, reply, err := ParseSendRefs([]string{good}, []byte(`"`+good+`"`))
	if err != nil || len(ids) != 1 || ids[0].String() != good || !reply.Valid || reply.UUID.String() != good {
		t.Fatalf("valid refs: %v %v %v", ids, reply, err)
	}
	if _, reply, err := ParseSendRefs(nil, []byte(`null`)); err != nil || reply.Valid {
		t.Fatalf("null reply: %v %v", reply, err)
	}
	if _, reply, err := ParseSendRefs(nil, nil); err != nil || reply.Valid {
		t.Fatalf("absent reply: %v %v", reply, err)
	}
	for name, run := range map[string]func() error{
		"attachment id":      func() error { _, _, e := ParseSendRefs([]string{"nope"}, nil); return e },
		"reply not a uuid":   func() error { _, _, e := ParseSendRefs(nil, []byte(`"nope"`)); return e },
		"reply not a string": func() error { _, _, e := ParseSendRefs(nil, []byte(`42`)); return e },
	} {
		var fields httpx.FieldErrors
		if err := run(); !errors.As(err, &fields) {
			t.Errorf("%s: err = %v, want field errors", name, err)
		}
	}
}

func TestNormalizeEmoji(t *testing.T) {
	for _, in := range []string{"👍", "❤️", "👨‍👩‍👧", "+1", " 🎉 "} {
		if _, err := normalizeEmoji(in); err != nil {
			t.Errorf("%q refused: %v", in, err)
		}
	}
	for _, in := range []string{"", "   ", "a b", "tab\there", "new\nline", strings.Repeat("x", 65), "\xff"} {
		if _, err := normalizeEmoji(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestTruncateCutsAtACharacter(t *testing.T) {
	if got := truncate(strings.Repeat("é", 300), 200); len([]rune(got)) != 200 {
		t.Errorf("%d characters", len([]rune(got)))
	}
	if got := truncate("short", 200); got != "short" {
		t.Errorf("short text changed: %q", got)
	}
}
