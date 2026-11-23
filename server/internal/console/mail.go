package console

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

// Message is one e-mail: plain text and HTML of the same content.
type Message struct {
	To      string
	Subject string
	Text    string
	HTML    string
}

// Mailer sends e-mail. The Log mailer prints it (development, tests); the SMTP one talks to a mail server.
type Mailer interface {
	Send(ctx context.Context, m Message) error
}

// LogMailer writes the e-mail to the log, links included: for development only.
type LogMailer struct{ Log *slog.Logger }

func (l LogMailer) Send(_ context.Context, m Message) error {
	l.Log.Info("e-mail (log driver, not sent)", "to", m.To, "subject", m.Subject, "text", m.Text)
	return nil
}

// SMTPConfig is a mail server. Port 465 is TLS from the start; any other port upgrades with STARTTLS when the server
// offers it. Credentials are optional (a local relay).
type SMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string
	From     string
}

type SMTPMailer struct{ cfg SMTPConfig }

func NewSMTPMailer(cfg SMTPConfig) *SMTPMailer { return &SMTPMailer{cfg: cfg} }

func (s *SMTPMailer) Send(ctx context.Context, m Message) error {
	to, err := mail.ParseAddress(m.To)
	if err != nil {
		return fmt.Errorf("mail: recipient: %w", err)
	}
	from, err := mail.ParseAddress(s.cfg.From)
	if err != nil {
		return fmt.Errorf("mail: sender: %w", err)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(15 * time.Second)
	}
	addr := net.JoinHostPort(s.cfg.Host, fmt.Sprint(s.cfg.Port))
	dialer := &net.Dialer{Deadline: deadline}
	var conn net.Conn
	if s.cfg.Port == 465 {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: &tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}}).DialContext(ctx, "tcp", addr)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("mail: connect: %w", err)
	}
	_ = conn.SetDeadline(deadline)
	c, err := smtp.NewClient(conn, s.cfg.Host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("mail: greeting: %w", err)
	}
	defer func() { _ = c.Close() }()
	if s.cfg.Port != 465 {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: s.cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
				return fmt.Errorf("mail: starttls: %w", err)
			}
		}
	}
	if s.cfg.Username != "" {
		if err := c.Auth(smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, s.cfg.Host)); err != nil {
			return fmt.Errorf("mail: auth: %w", err)
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return fmt.Errorf("mail: from: %w", err)
	}
	if err := c.Rcpt(to.Address); err != nil {
		return fmt.Errorf("mail: rcpt: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("mail: data: %w", err)
	}
	if _, err := w.Write(buildMIME(from, to, m)); err != nil {
		return fmt.Errorf("mail: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mail: send: %w", err)
	}
	return c.Quit()
}

// buildMIME is a multipart/alternative message. Subject and addresses come from this program or were parsed as
// addresses, so no header can be injected through them.
func buildMIME(from, to *mail.Address, m Message) []byte {
	const boundary = "marpokat-alt-7f3c1e"
	var b strings.Builder
	line := func(s string) { b.WriteString(s + "\r\n") }
	line("From: " + from.String())
	line("To: " + to.String())
	line("Subject: " + mime.QEncoding.Encode("utf-8", oneLine(m.Subject)))
	line("Date: " + time.Now().UTC().Format(time.RFC1123Z))
	line("MIME-Version: 1.0")
	line("Content-Type: multipart/alternative; boundary=" + boundary)
	line("")
	line("--" + boundary)
	line("Content-Type: text/plain; charset=utf-8")
	line("Content-Transfer-Encoding: 8bit")
	line("")
	b.WriteString(crlf(m.Text) + "\r\n")
	line("--" + boundary)
	line("Content-Type: text/html; charset=utf-8")
	line("Content-Transfer-Encoding: 8bit")
	line("")
	b.WriteString(crlf(m.HTML) + "\r\n")
	line("--" + boundary + "--")
	return []byte(b.String())
}

func oneLine(s string) string { return strings.NewReplacer("\r", " ", "\n", " ").Replace(s) }

func crlf(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}
