package notify

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"strings"
	"testing"
	"time"

	"mime"
	"mime/quotedprintable"

	"github.com/getplinth/plinth/internal/store"
)

func data(kv ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

func TestEveryEventRendersAndLinksOnlyWithAPublicAddress(t *testing.T) {
	d := data("tenant", "payments", "blueprint", "standard", "projects", []any{"shop", "blog"}, "requested_by", "alice@example.test",
		"decided_by", "root@example.test", "note", "use a smaller size", "request_id", "r-1", "failed_step", "cluster/shop/dev",
		"invited_by", "alice@example.test", "email", "bob@example.test", "role", "owner", "expires_at", "2026-12-19T10:00:00Z", "by", "root@example.test")
	for _, ev := range []string{store.EventInvitation, store.EventAwaitingApproval, store.EventApproved, store.EventRejected, store.EventSucceeded, store.EventFailed, store.EventAbandoned, store.EventTest, "something.new"} {
		with, without := Render(ev, d, "https://plinth.example.com"), Render(ev, d, "")
		if with.Subject == "" || with.Body == "" || with.Text == "" {
			t.Errorf("%s: incomplete %+v", ev, with)
		}
		if strings.Contains(without.Body, "http") || strings.Contains(without.Text, "http") {
			t.Errorf("%s: no public address means no links: %q", ev, without.Body)
		}
		if ev != store.EventTest && ev != "something.new" && !strings.Contains(with.Body, "https://plinth.example.com/") {
			t.Errorf("%s: the message should link to the page: %q", ev, with.Body)
		}
		if strings.ContainsAny(with.Subject, "\r\n") || strings.ContainsAny(with.Text, "\r\n") {
			t.Errorf("%s: a subject or one-line text with a line break", ev)
		}
	}
	if m := Render(store.EventRejected, d, ""); !strings.Contains(m.Body, "use a smaller size") || !strings.Contains(m.Subject, "shop and blog") {
		t.Errorf("rejection should carry the reason and name the projects: %+v", m)
	}
	if m := Render(store.EventInvitation, d, ""); !strings.Contains(m.Body, "an owner") || !strings.Contains(m.Body, "19 December 2026") || !strings.Contains(m.Body, "bob@example.test") {
		t.Errorf("invitation: %q", m.Body)
	}
	if m := Render(store.EventFailed, d, ""); !strings.Contains(m.Body, "cluster/shop/dev") {
		t.Errorf("failure should name the step: %q", m.Body)
	}
}

func TestPeopleCannotInjectHeadersThroughNames(t *testing.T) {
	evil := data("tenant", "x\r\nBcc: victim@example.test", "invited_by", "a@b.c\nSubject: pwned", "role", "member", "email", "e@f.g")
	m := Render(store.EventInvitation, evil, "")
	if strings.ContainsAny(m.Subject, "\r\n") || strings.ContainsAny(m.Text, "\r\n") {
		t.Fatalf("subject or text kept a line break: %q %q", m.Subject, m.Text)
	}
	msg := string(SMTP{From: mail.Address{Address: "plinth@example.com"}}.compose(&mail.Address{Address: "r@example.com"}, m))
	parsed, err := mail.ReadMessage(strings.NewReader(msg))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Header["Bcc"]) != 0 || len(parsed.Header["Subject"]) != 1 {
		t.Errorf("an injected header got through: %v", parsed.Header)
	}
}

// ---- webhook ------------------------------------------------------------------------------------------

func TestWebhookSignsAndDeliversTheEvent(t *testing.T) {
	var gotBody []byte
	var gotHdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotHdr = r.Header.Clone()
	}))
	defer srv.Close()
	body, err := Body(42, store.EventApproved, time.Unix(1700000000, 0), data("tenant", "payments", "projects", []any{"shop"}, "decided_by", "root@example.test"), "https://p.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := (Webhook{URL: srv.URL, Secret: "s3cret"}).Send(context.Background(), 42, body); err != nil {
		t.Fatal(err)
	}
	if string(gotBody) != string(body) || gotHdr.Get("X-Plinth-Delivery") != "42" || gotHdr.Get("Content-Type") != "application/json" {
		t.Errorf("headers %v", gotHdr)
	}
	ts := gotHdr.Get("X-Plinth-Timestamp")
	if want := Sign("s3cret", mustInt(t, ts), gotBody); gotHdr.Get("X-Plinth-Signature") != want {
		t.Errorf("signature %q, want %q", gotHdr.Get("X-Plinth-Signature"), want)
	}
	if Sign("other", mustInt(t, ts), gotBody) == gotHdr.Get("X-Plinth-Signature") {
		t.Error("a different secret must give a different signature")
	}
	if Sign("s3cret", mustInt(t, ts)+1, gotBody) == gotHdr.Get("X-Plinth-Signature") {
		t.Error("the timestamp is part of the signature, so a replay with a new time fails")
	}
	for _, want := range []string{`"id":42`, `"type":"request.approved"`, `"text":"root@example.test approved`, `"tenant":"payments"`, `"time":"2023-11-14T22:13:20Z"`} {
		if !strings.Contains(string(gotBody), want) {
			t.Errorf("body lacks %s: %s", want, gotBody)
		}
	}
	// Without a secret there are no signature headers.
	if err := (Webhook{URL: srv.URL}).Send(context.Background(), 1, body); err != nil || gotHdr.Get("X-Plinth-Signature") != "" {
		t.Errorf("unsigned: %v %v", err, gotHdr)
	}
}

func mustInt(t *testing.T, s string) int64 {
	t.Helper()
	var n int64
	for _, c := range s {
		n = n*10 + int64(c-'0')
	}
	return n
}

func TestWebhookFailuresAreSortedIntoRetryAndGiveUp(t *testing.T) {
	status := 200
	redirected := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			redirected = true
		}
		if status == 302 {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	send := func() error { return (Webhook{URL: srv.URL}).Send(context.Background(), 1, []byte(`{}`)) }
	for code, wantPermanent := range map[int]bool{200: false, 204: false, 500: false, 502: false, 503: false, 429: false, 408: false, 400: true, 401: true, 403: true, 404: true, 410: true, 302: true} {
		status = code
		err := send()
		switch {
		case code < 300 && err != nil:
			t.Errorf("%d should succeed: %v", code, err)
		case code >= 300 && err == nil:
			t.Errorf("%d should fail", code)
		case code >= 300 && IsPermanent(err) != wantPermanent:
			t.Errorf("%d: permanent = %v, want %v (%v)", code, IsPermanent(err), wantPermanent, err)
		}
	}
	if redirected {
		t.Error("a redirect must not be followed: it would send the signed body elsewhere")
	}
	if err := (Webhook{URL: "http://127.0.0.1:1"}).Send(context.Background(), 1, []byte(`{}`)); err == nil || IsPermanent(err) {
		t.Errorf("an unreachable receiver is worth retrying: %v", err)
	}
}

// ---- SMTP ---------------------------------------------------------------------------------------------

func smtpFor(s *fakeSMTP, mode string) SMTP {
	return SMTP{Host: "localhost", Port: s.Port(), From: mail.Address{Name: "Plinth", Address: "plinth@example.com"},
		TLS: mode, TLSConfig: s.client(), Username: "robot", Password: "pw-secret"}
}

func parse(t *testing.T, raw string) (*mail.Message, string) {
	t.Helper()
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("%v\n%s", err, raw)
	}
	b, _ := io.ReadAll(quotedprintable.NewReader(msg.Body))
	return msg, string(b)
}

func TestSMTPDeliversOverSTARTTLSWithLogin(t *testing.T) {
	srv := newFakeSMTP(t, false)
	srv.Want = [2]string{"robot", "pw-secret"}
	m := Render(store.EventRejected, data("tenant", "päyments", "projects", []any{"shop"}, "decided_by", "root@example.test", "note", "größer ändern"), "https://p.example.com")
	if err := smtpFor(srv, TLSStart).Send(context.Background(), "Bob <bob@example.test>", m); err != nil {
		t.Fatal(err)
	}
	got := srv.got()
	if len(got) != 1 || got[0].From != "plinth@example.com" || got[0].To != "bob@example.test" {
		t.Fatalf("%+v", got)
	}
	if !srv.SawTLS || srv.Plain {
		t.Error("the password must only be sent after the connection is encrypted")
	}
	msg, body := parse(t, got[0].Data)
	subj, _ := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if subj != m.Subject || !strings.Contains(subj, "päyments") {
		t.Errorf("subject %q", subj)
	}
	if !strings.Contains(body, "größer ändern") || !strings.Contains(body, "https://p.example.com/requests/") {
		t.Errorf("body: %s", body)
	}
	if msg.Header.Get("Content-Type") != "text/plain; charset=UTF-8" || msg.Header.Get("Auto-Submitted") != "auto-generated" ||
		msg.Header.Get("Message-ID") == "" || msg.Header.Get("Date") == "" || !strings.Contains(msg.Header.Get("From"), "plinth@example.com") {
		t.Errorf("headers: %v", msg.Header)
	}
}

func TestSMTPImplicitTLS(t *testing.T) {
	srv := newFakeSMTP(t, true)
	if err := smtpFor(srv, TLSImplicit).Send(context.Background(), "bob@example.test", Message{Subject: "s", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if len(srv.got()) != 1 || !srv.SawTLS {
		t.Error("expected one message over TLS")
	}
}

func TestSMTPRefusesToSendInTheClear(t *testing.T) {
	srv := newFakeSMTP(t, false)
	srv.NoSTARTTLS = true
	err := smtpFor(srv, TLSStart).Send(context.Background(), "bob@example.test", Message{Subject: "s", Body: "b"})
	if err == nil || !IsPermanent(err) || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("a server without STARTTLS must not receive the message or the password: %v", err)
	}
	if len(srv.got()) != 0 || len(srv.Auths) != 0 {
		t.Error("something was sent in the clear")
	}
}

func TestSMTPChecksTheServerCertificate(t *testing.T) {
	srv := newFakeSMTP(t, false)
	s := smtpFor(srv, TLSStart)
	s.TLSConfig = &tls.Config{} // system roots: the test server's certificate is not trusted
	if err := s.Send(context.Background(), "bob@example.test", Message{Subject: "s", Body: "b"}); err == nil {
		t.Fatal("an untrusted certificate must be refused")
	}
	if len(srv.Auths) != 0 {
		t.Error("credentials were sent to an untrusted server")
	}
}

func TestSMTPUnencryptedOnlyWhenAskedFor(t *testing.T) {
	srv := newFakeSMTP(t, false)
	srv.NoSTARTTLS = true
	if err := smtpFor(srv, TLSNone).Send(context.Background(), "bob@example.test", Message{Subject: "s", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if len(srv.got()) != 1 || !srv.Plain {
		t.Error("tls=none should send without encryption, including the login")
	}
}

func TestSMTPFailuresAreSortedIntoRetryAndGiveUp(t *testing.T) {
	cases := []struct {
		name      string
		set       func(*fakeSMTP)
		permanent bool
	}{
		{"mailbox unknown", func(s *fakeSMTP) { s.RcptReply = "550 5.1.1 no such user" }, true},
		{"mailbox busy", func(s *fakeSMTP) { s.RcptReply = "450 4.2.1 try later" }, false},
		{"message refused", func(s *fakeSMTP) { s.DataReply = "554 5.7.1 spam" }, true},
		{"message deferred", func(s *fakeSMTP) { s.DataReply = "451 4.3.0 local error" }, false},
		{"bad password", func(s *fakeSMTP) { s.Want = [2]string{"someone", "else"} }, true},
	}
	for _, c := range cases {
		srv := newFakeSMTP(t, false)
		c.set(srv)
		err := smtpFor(srv, TLSStart).Send(context.Background(), "bob@example.test", Message{Subject: "s", Body: "b"})
		if err == nil {
			t.Errorf("%s: expected an error", c.name)
		} else if IsPermanent(err) != c.permanent {
			t.Errorf("%s: permanent = %v, want %v (%v)", c.name, IsPermanent(err), c.permanent, err)
		}
	}
	// A mail server that is not there is worth retrying.
	s := SMTP{Host: "127.0.0.1", Port: 1, From: mail.Address{Address: "plinth@example.com"}, TLS: TLSNone}
	if err := s.Send(context.Background(), "bob@example.test", Message{Subject: "s", Body: "b"}); err == nil || IsPermanent(err) {
		t.Errorf("unreachable: %v", err)
	}
	// An address that cannot be right is never retried.
	if err := smtpFor(newFakeSMTP(t, false), TLSStart).Send(context.Background(), "bob@example.test\r\nBcc: x@y.z", Message{}); err == nil || !IsPermanent(err) {
		t.Errorf("a bad address: %v", err)
	}
}
