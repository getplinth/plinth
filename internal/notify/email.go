package notify

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// TLS modes for the mail server connection.
const (
	TLSStart    = "starttls" // connect in clear, upgrade with STARTTLS (port 587), required
	TLSImplicit = "tls"      // TLS from the first byte (port 465)
	TLSNone     = "none"     // no encryption: testing only
)

// SMTP delivers messages through a mail server.
type SMTP struct {
	Host, Username, Password string
	Port                     int
	From                     mail.Address
	TLS                      string
	// TLSConfig, when set, replaces the default (the system roots). Tests use it.
	TLSConfig *tls.Config
	Timeout   time.Duration
}

// permanent marks a failure that retrying cannot fix.
type permanent struct{ error }

func (p permanent) Unwrap() error { return p.error }

// IsPermanent reports whether an error from a sender says "do not retry".
func IsPermanent(err error) bool { var p permanent; return errors.As(err, &p) }

func (s SMTP) addr() string { return net.JoinHostPort(s.Host, strconv.Itoa(s.Port)) }

// Send delivers one message to one recipient.
func (s SMTP) Send(ctx context.Context, to string, m Message) error {
	rcpt, err := mail.ParseAddress(to)
	if err != nil || strings.ContainsAny(to, "\r\n") {
		return permanent{fmt.Errorf("not a valid e-mail address: %q", to)}
	}
	timeout := s.Timeout
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	dialer := &net.Dialer{Deadline: deadline}
	cfg := s.TLSConfig
	if cfg == nil {
		cfg = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		cfg = cfg.Clone()
	}
	if cfg.ServerName == "" {
		cfg.ServerName = s.Host
	}

	var conn net.Conn
	if s.TLS == TLSImplicit {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: cfg}).DialContext(ctx, "tcp", s.addr())
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", s.addr())
	}
	if err != nil {
		return fmt.Errorf("connect to the mail server: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(deadline)

	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		return fmt.Errorf("mail server greeting: %w", classify(err))
	}
	defer c.Close()
	if s.TLS != TLSImplicit && s.TLS != TLSNone {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return permanent{errors.New("the mail server does not offer STARTTLS; refusing to send without encryption (set PLINTH_SMTP_TLS=none only for testing)")}
		}
		if err := c.StartTLS(cfg); err != nil {
			return fmt.Errorf("STARTTLS: %w", err)
		}
	}
	if s.Username != "" {
		// net/smtp's PLAIN auth refuses to send the password without TLS, except to localhost.
		auth := smtp.PlainAuth("", s.Username, s.Password, s.Host)
		if s.TLS == TLSNone {
			auth = plainAuthAnyway{username: s.Username, password: s.Password}
		}
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("sign in to the mail server: %w", classify(err))
		}
	}
	if err := c.Mail(s.From.Address); err != nil {
		return fmt.Errorf("sender rejected: %w", classify(err))
	}
	if err := c.Rcpt(rcpt.Address); err != nil {
		return fmt.Errorf("recipient rejected: %w", classify(err))
	}
	w, err := c.Data()
	if err != nil {
		return classify(err)
	}
	if _, err := w.Write(s.compose(rcpt, m)); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("message rejected: %w", classify(err))
	}
	return classify(c.Quit())
}

// classify marks 5xx replies from the server as permanent, 4xx and everything else as retryable.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var te *textproto.Error
	if errors.As(err, &te) && te.Code >= 500 {
		return permanent{err}
	}
	return err
}

type plainAuthAnyway struct{ username, password string }

func (a plainAuthAnyway) Start(*smtp.ServerInfo) (string, []byte, error) {
	return "PLAIN", []byte("\x00" + a.username + "\x00" + a.password), nil
}
func (a plainAuthAnyway) Next([]byte, bool) ([]byte, error) { return nil, nil }

// compose builds the RFC 5322 message: plain text, UTF-8, no HTML.
func (s SMTP) compose(rcpt *mail.Address, m Message) []byte {
	id := make([]byte, 12)
	_, _ = rand.Read(id)
	domain := "plinth.invalid"
	if at := strings.LastIndex(s.From.Address, "@"); at >= 0 {
		domain = s.From.Address[at+1:]
	}
	var b strings.Builder
	h := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	h("From", s.From.String())
	h("To", (&mail.Address{Address: rcpt.Address}).String())
	h("Subject", mime.QEncoding.Encode("utf-8", clean(m.Subject)))
	h("Date", time.Now().Format(time.RFC1123Z))
	h("Message-ID", "<"+hex.EncodeToString(id)+"@"+domain+">")
	h("MIME-Version", "1.0")
	h("Content-Type", "text/plain; charset=UTF-8")
	h("Content-Transfer-Encoding", "quoted-printable")
	h("Auto-Submitted", "auto-generated")
	b.WriteString("\r\n")
	var body strings.Builder
	qp := quotedprintable.NewWriter(&body)
	_, _ = qp.Write([]byte(strings.ReplaceAll(m.Body, "\r\n", "\n") + "\n"))
	_ = qp.Close()
	b.WriteString(body.String())
	return []byte(b.String())
}
