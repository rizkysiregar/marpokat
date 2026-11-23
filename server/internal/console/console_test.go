package console

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/rizkysiregar/marpokat/server/internal/platform/httpx"
)

func TestPasswordHashingIsSaltedAndVerifiable(t *testing.T) {
	a, err := hashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := hashPassword("correct horse battery")
	if a == b {
		t.Fatal("two hashes of one password are equal: no salt")
	}
	if !strings.HasPrefix(a, "$argon2id$v=19$m=19456,t=2,p=1$") {
		t.Fatalf("not a PHC argon2id string: %s", a)
	}
	if !verifyPassword("correct horse battery", a) || !verifyPassword("correct horse battery", b) {
		t.Fatal("the right password was refused")
	}
	if verifyPassword("correct horse batter", a) || verifyPassword("", a) {
		t.Fatal("a wrong password was accepted")
	}
}

func TestVerifyPasswordRefusesMalformedAndHostileHashes(t *testing.T) {
	for _, h := range []string{
		"", "plain", "$argon2id$", "$bcrypt$v=19$m=1,t=1,p=1$AAAA$AAAA",
		"$argon2id$v=18$m=19456,t=2,p=1$AAAA$AAAA",
		"$argon2id$v=19$m=999999999,t=2,p=1$AAAA$AAAA", // would allocate gigabytes
		"$argon2id$v=19$m=19456,t=99999,p=1$AAAA$AAAA",
		"$argon2id$v=19$m=19456,t=2,p=0$AAAA$AAAA",
		"$argon2id$v=19$m=19456,t=2,p=1$!!!$AAAA",
		"$argon2id$v=19$m=19456,t=2,p=1$AAAA$",
	} {
		if verifyPassword("x", h) {
			t.Errorf("%q accepted", h)
		}
	}
}

func TestNormalizeEmail(t *testing.T) {
	ok := map[string]string{"A@B.co": "a@b.co", "  user.name+tag@Example.COM ": "user.name+tag@example.com"}
	for in, want := range ok {
		if got, err := normalizeEmail(in); err != nil || got != want {
			t.Errorf("%q -> %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "plain", "a@b", "a@@b.co", "Name <a@b.co>", "a@b.co\r\nBcc: x@y.z", strings.Repeat("a", 250) + "@b.co", "a b@c.co"} {
		if _, err := normalizeEmail(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestValidatePassword(t *testing.T) {
	var fields httpx.FieldErrors
	for _, p := range []string{"short", strings.Repeat("a", 129), "me@example.com", "ME@EXAMPLE.COM", "has\x00nul12345"} {
		if err := validatePassword(p, "me@example.com"); !errors.As(err, &fields) {
			t.Errorf("%q accepted", p)
		}
	}
	for _, p := range []string{"0123456789", strings.Repeat("é", 128), "a long passphrase with spaces"} {
		if err := validatePassword(p, "me@example.com"); err != nil {
			t.Errorf("%q refused: %v", p, err)
		}
	}
}

func TestValidateName(t *testing.T) {
	if got, err := validateName("  Rizky  "); err != nil || got != "Rizky" {
		t.Errorf("got %q, %v", got, err)
	}
	for _, in := range []string{"", "   ", strings.Repeat("x", 101), "a\nb", "a\x00b"} {
		if _, err := validateName(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{
		"My Chat App": "my-chat-app", "  Narrativ!! ": "narrativ", "Ünï/cödé": "n-c-d", "---": "project", "a": "project", "": "project",
		strings.Repeat("ab", 40): strings.Repeat("ab", 25),
	} {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
		if !slugPattern.MatchString(slugify(in)) {
			t.Errorf("slugify(%q) = %q is not a valid slug", in, slugify(in))
		}
	}
}

func TestTokensAreRandomAndOnlyTheirHashIsKept(t *testing.T) {
	a, b := randomToken(), randomToken()
	if a == b || len(a) < 40 {
		t.Fatalf("tokens %q %q", a, b)
	}
	if string(hashToken(a)) == a || len(hashToken(a)) != 32 {
		t.Fatal("hash is not a SHA-256")
	}
}

func TestMIMEHasNoInjectableHeaders(t *testing.T) {
	m := Message{To: "a@b.co", Subject: "Hello\r\nBcc: evil@x.y", Text: "line1\nline2", HTML: "<p>x</p>"}
	from, _ := parseAddr("Marpokat <noreply@marpokat.test>")
	to, _ := parseAddr(m.To)
	raw := string(buildMIME(from, to, m))
	if strings.Contains(raw, "\r\nBcc:") {
		t.Fatalf("a header was injected:\n%s", raw)
	}
	if !strings.Contains(raw, "line1\r\nline2") || !strings.Contains(raw, "multipart/alternative") {
		t.Fatalf("bad body:\n%s", raw)
	}
}

func TestEmailsEscapeTheNameAndCarryTheLink(t *testing.T) {
	m := verifyEmailMessage("a@b.co", `<script>alert(1)</script>`, "https://x.test/verify-email?token=abc&x=1")
	if strings.Contains(m.HTML, "<script>") || !strings.Contains(m.HTML, "&lt;script&gt;") {
		t.Fatal("the name was not escaped in the HTML")
	}
	if !strings.Contains(m.Text, "https://x.test/verify-email?token=abc&x=1") {
		t.Fatal("link missing from the text part")
	}
}

func parseAddr(s string) (*mail.Address, error) { return mail.ParseAddress(s) }

// fakeSMTP speaks just enough SMTP to receive one message and hand back what it was given.
func fakeSMTP(t *testing.T) (port int, got chan string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	got = make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		r := bufio.NewReader(conn)
		say := func(s string) { _, _ = conn.Write([]byte(s + "\r\n")) }
		say("220 fake ESMTP")
		var data strings.Builder
		inData := false
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			line = strings.TrimRight(line, "\r\n")
			switch {
			case inData && line == ".":
				inData = false
				say("250 queued")
				got <- data.String()
			case inData:
				data.WriteString(line + "\n")
			case strings.HasPrefix(line, "EHLO"), strings.HasPrefix(line, "HELO"):
				say("250 fake")
			case strings.HasPrefix(line, "MAIL FROM"), strings.HasPrefix(line, "RCPT TO"):
				data.WriteString("ENVELOPE " + line + "\n")
				say("250 ok")
			case line == "DATA":
				inData = true
				say("354 go")
			case line == "QUIT":
				say("221 bye")
				return
			}
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, got
}

func TestSMTPMailerDeliversAMultipartMessage(t *testing.T) {
	port, got := fakeSMTP(t)
	m := NewSMTPMailer(SMTPConfig{Host: "127.0.0.1", Port: port, From: "Marpokat <noreply@marpokat.test>"})
	err := m.Send(context.Background(), Message{To: "ani@example.com", Subject: "Hello é", Text: "plain", HTML: "<p>html</p>"})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case raw := <-got:
		for _, want := range []string{"ENVELOPE MAIL FROM:<noreply@marpokat.test>", "ENVELOPE RCPT TO:<ani@example.com>", "multipart/alternative", "plain", "<p>html</p>", "=?utf-8?q?Hello_=C3=A9?="} {
			if !strings.Contains(raw, want) {
				t.Errorf("missing %q in:\n%s", want, raw)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing delivered")
	}
}

func TestSMTPMailerRefusesBadAddressesAndReportsAnUnreachableServer(t *testing.T) {
	m := NewSMTPMailer(SMTPConfig{Host: "127.0.0.1", Port: 1, From: "noreply@marpokat.test"})
	if err := m.Send(context.Background(), Message{To: "not an address", Subject: "x"}); err == nil || !strings.Contains(err.Error(), "recipient") {
		t.Errorf("bad recipient: %v", err)
	}
	if err := m.Send(context.Background(), Message{To: "a@b.co", Subject: "x"}); err == nil || !strings.Contains(err.Error(), "connect") {
		t.Errorf("unreachable server: %v", err)
	}
}
