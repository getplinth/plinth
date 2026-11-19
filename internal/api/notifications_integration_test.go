//go:build integration

package api_test

// Notifications end to end: events written by the store in the same
// transaction as the change, delivered by the dispatcher to a real SMTP server
// (Mailpit, in a disposable Docker container) and to a webhook receiver.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getplinth/plinth/internal/notify"
)

type mailpit struct {
	smtpPort int
	api      string
}

func startMailpit(t *testing.T) mailpit {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	name := fmt.Sprintf("plinth-itest-mail-%06d", rand.Intn(1_000_000))
	if out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-e", "MP_SMTP_AUTH_ACCEPT_ANY=1", "-e", "MP_SMTP_AUTH_ALLOW_INSECURE=1",
		"-p", "127.0.0.1::1025", "-p", "127.0.0.1::8025", "axllent/mailpit").CombinedOutput(); err != nil {
		t.Skipf("cannot start mailpit: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	port := func(p string) int {
		out, err := exec.Command("docker", "port", name, p+"/tcp").Output()
		if err != nil {
			t.Fatal(err)
		}
		first := strings.TrimSpace(strings.Split(string(out), "\n")[0])
		n, _ := strconv.Atoi(first[strings.LastIndex(first, ":")+1:])
		return n
	}
	m := mailpit{smtpPort: port("1025"), api: fmt.Sprintf("http://127.0.0.1:%d", port("8025"))}
	deadline := time.Now().Add(60 * time.Second)
	for {
		resp, err := http.Get(m.api + "/api/v1/messages")
		if err == nil {
			resp.Body.Close()
			return m
		}
		if time.Now().After(deadline) {
			t.Fatalf("mailpit not ready: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

type mailed struct{ ID, Subject, To, Body string }

func (m mailpit) messages(t *testing.T) []mailed {
	t.Helper()
	resp, err := http.Get(m.api + "/api/v1/messages?limit=200")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var list struct {
		Messages []struct {
			ID      string
			Subject string
			To      []struct{ Address string }
		}
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	var out []mailed
	for _, msg := range list.Messages {
		r, err := http.Get(m.api + "/api/v1/message/" + msg.ID)
		if err != nil {
			t.Fatal(err)
		}
		var full struct{ Text string }
		_ = json.NewDecoder(r.Body).Decode(&full)
		r.Body.Close()
		to := ""
		if len(msg.To) > 0 {
			to = msg.To[0].Address
		}
		out = append(out, mailed{ID: msg.ID, Subject: msg.Subject, To: to, Body: full.Text})
	}
	return out
}

func (m mailpit) clear(t *testing.T) {
	t.Helper()
	req, _ := http.NewRequest("DELETE", m.api+"/api/v1/messages", nil)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		resp.Body.Close()
	}
}

type hookLog struct {
	mu     sync.Mutex
	status int
	got    []hookCall
}

type hookCall struct {
	Headers http.Header
	Body    []byte
	Event   map[string]any
}

func (h *hookLog) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	h.mu.Lock()
	defer h.mu.Unlock()
	var ev map[string]any
	_ = json.Unmarshal(body, &ev)
	h.got = append(h.got, hookCall{Headers: r.Header.Clone(), Body: body, Event: ev})
	if h.status != 0 {
		w.WriteHeader(h.status)
	}
}
func (h *hookLog) setStatus(s int) { h.mu.Lock(); h.status = s; h.mu.Unlock() }
func (h *hookLog) calls() []hookCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]hookCall(nil), h.got...)
}
func (h *hookLog) reset() { h.mu.Lock(); h.got, h.status = nil, 0; h.mu.Unlock() }
func (h *hookLog) types() []string {
	var out []string
	for _, c := range h.calls() {
		out = append(out, fmt.Sprint(c.Event["type"]))
	}
	return out
}

func slogDiscard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestNotifications(t *testing.T) {
	e := newEnv(t)
	mp := startMailpit(t)
	hook := &hookLog{}
	hs := httptest.NewServer(http.HandlerFunc(hook.handler))
	defer hs.Close()
	ctx := context.Background()

	from, _ := mail.ParseAddress("Plinth <plinth@example.com>")
	d := &notify.Dispatcher{
		Store: e.st, Log: slogDiscard(), PublicURL: "https://plinth.example.com",
		Email:   &notify.SMTP{Host: "127.0.0.1", Port: mp.smtpPort, From: *from, TLS: notify.TLSNone, Username: "robot", Password: "pw"},
		Webhook: &notify.Webhook{URL: hs.URL, Secret: "whsec"},
		Backoff: 50 * time.Millisecond, MaxBackoff: 50 * time.Millisecond, MaxAttempts: 3, Batch: 50,
	}
	drain := func() {
		t.Helper()
		for i := 0; i < 20; i++ {
			if d.Once(ctx) == 0 {
				return
			}
		}
	}
	reset := func() { mp.clear(t); hook.reset() }
	find := func(prefix, to string) (mailed, bool) {
		for _, m := range mp.messages(t) {
			if strings.HasPrefix(m.Subject, prefix) && m.To == to {
				return m, true
			}
		}
		return mailed{}, false
	}
	count := func(q string, args ...any) string { return e.rawQuery(q, args...) }

	admin, ada, alice, bob := e.admin(), e.token("ada", "plinth-admins"), e.alice(), e.bob()
	e.expect(201, admin, "POST", "/api/v1/integrations", map[string]any{"kind": "gitsource", "driver": "fake", "name": "git", "config": map[string]string{"a": "b"}})
	e.expect(201, admin, "POST", "/api/v1/integrations", map[string]any{"kind": "cluster", "driver": "fake", "name": "cl", "config": map[string]string{"name": "dev"}})
	for name, approval := range map[string]bool{"open": false, "gated": true} {
		e.expect(201, admin, "POST", "/api/v1/blueprints", map[string]any{"name": name, "git_source": "git", "requires_approval": approval,
			"environments": []map[string]any{{"name": "dev", "cluster": "cl"}}})
	}
	for _, tok := range []string{ada, alice, bob} {
		e.expect(200, tok, "GET", "/api/v1/me", nil)
	}
	submit := func(token, blueprint, team, project string) string {
		v, _ := e.expect(202, token, "POST", "/api/v1/requests", fmt.Sprintf(`{"blueprint":%q,"tenant":%q,"projects":[{"name":%q}]}`, blueprint, team, project))
		return get(v, "id").(string)
	}

	t.Run("nothing is queued while no channel is configured", func(t *testing.T) {
		e.waitStatus(alice, submit(alice, "open", "quiet", "app"), "succeeded")
		if n := count(`SELECT count(*)::text FROM notification_outbox`); n != "0" {
			t.Fatalf("queued %s notifications with no channel", n)
		}
		e.expect(409, admin, "POST", "/api/v1/notifications/test", nil)
		e.expect(403, alice, "POST", "/api/v1/notifications/test", nil)
		if me, _ := e.expect(200, alice, "GET", "/api/v1/me", nil); get(me, "notify_email") != false {
			t.Errorf("/me should say e-mail is off: %v", me)
		}
	})

	e.st.SetNotificationChannels(true, true)
	if me, _ := e.expect(200, alice, "GET", "/api/v1/me", nil); get(me, "notify_email") != true {
		t.Errorf("/me should say e-mail is on: %v", me)
	}

	t.Run("an administrator can send a test message", func(t *testing.T) {
		e.expect(202, admin, "POST", "/api/v1/notifications/test", nil)
		drain()
		if m, ok := find("Plinth test notification", "admin@example.test"); !ok || !strings.Contains(m.Body, "admin@example.test") {
			t.Errorf("test mail missing: %v", mp.messages(t))
		}
		if got := hook.types(); len(got) != 1 || got[0] != "test" {
			t.Errorf("webhook: %v", got)
		}
		reset()
	})

	e.waitStatus(alice, submit(alice, "open", "notif", "first"), "succeeded")
	drain()
	if _, ok := find("Ready:", "alice@example.test"); !ok {
		t.Errorf("a finished request should tell its requester: %v", mp.messages(t))
	}
	reset()

	t.Run("an invitation is e-mailed to the invitee and sent to the webhook", func(t *testing.T) {
		e.expect(202, alice, "POST", "/api/v1/tenants/notif/members", map[string]string{"email": "Newbie@Example.test", "role": "member"})
		drain()
		m, ok := find("alice@example.test invited you to the team notif", "newbie@example.test")
		if !ok {
			t.Fatalf("no invitation mail: %v", mp.messages(t))
		}
		for _, want := range []string{"https://plinth.example.com/", "newbie@example.test", "a member", "join automatically"} {
			if !strings.Contains(m.Body, want) {
				t.Errorf("invitation body lacks %q:\n%s", want, m.Body)
			}
		}
		calls := hook.calls()
		if len(calls) != 1 || calls[0].Event["type"] != "invitation.created" {
			t.Fatalf("webhook: %v", hook.types())
		}
		c := calls[0]
		ts, _ := strconv.ParseInt(c.Headers.Get("X-Plinth-Timestamp"), 10, 64)
		if c.Headers.Get("X-Plinth-Signature") != notify.Sign("whsec", ts, c.Body) {
			t.Error("the webhook signature does not verify")
		}
		if _, err := strconv.Atoi(c.Headers.Get("X-Plinth-Delivery")); err != nil {
			t.Errorf("delivery id: %q", c.Headers.Get("X-Plinth-Delivery"))
		}
		if get(c.Event, "data", "email") != "newbie@example.test" || get(c.Event, "data", "tenant") != "notif" {
			t.Errorf("event data: %v", c.Event)
		}
		if _, ok := find("alice@example.test invited you", "alice@example.test"); ok {
			t.Error("the inviter must not get the invitation")
		}
		reset()

		// Someone who already has an account is added at once: that is not an invitation.
		before := count(`SELECT count(*)::text FROM notification_outbox`)
		e.expect(201, alice, "POST", "/api/v1/tenants/notif/members", map[string]string{"email": "bob@example.test", "role": "member"})
		if count(`SELECT count(*)::text FROM notification_outbox`) != before {
			t.Error("adding an existing account queued a notification")
		}
	})

	t.Run("approval: administrators are asked, the requester is told", func(t *testing.T) {
		id := submit(alice, "gated", "notif", "gated-app")
		drain()
		for _, who := range []string{"admin@example.test", "ada@example.test"} {
			m, ok := find("Approval needed: gated-app for team notif", who)
			if !ok || !strings.Contains(m.Body, "https://plinth.example.com/admin/approvals") || !strings.Contains(m.Body, "alice@example.test") {
				t.Errorf("approval request for %s: %v", who, ok)
			}
		}
		if _, ok := find("Approval needed", "alice@example.test"); ok {
			t.Error("the requester must not be asked to approve")
		}
		if got := hook.types(); len(got) != 1 || got[0] != "request.awaiting_approval" {
			t.Errorf("webhook: %v", got)
		}
		reset()

		e.expect(200, ada, "POST", "/api/v1/requests/"+id+"/approve", nil)
		e.waitStatus(alice, id, "succeeded")
		drain()
		if m, ok := find("Approved: gated-app", "alice@example.test"); !ok || !strings.Contains(m.Body, "ada@example.test") || !strings.Contains(m.Body, "/requests/"+id) {
			t.Errorf("approval mail: %v", ok)
		}
		if _, ok := find("Ready: gated-app", "alice@example.test"); !ok {
			t.Error("the finished request should be announced")
		}
		reset()

		rejected := submit(alice, "gated", "notif", "too-big")
		drain()
		reset()
		e.expect(200, admin, "POST", "/api/v1/requests/"+rejected+"/reject", map[string]string{"note": "use a smaller size"})
		drain()
		m, ok := find("Rejected: too-big", "alice@example.test")
		if !ok || !strings.Contains(m.Body, "use a smaller size") || !strings.Contains(m.Body, "admin@example.test") {
			t.Errorf("rejection mail: %v", ok)
		}
		if calls := hook.calls(); len(calls) != 1 || get(calls[0].Event, "data", "note") != "use a smaller size" {
			t.Errorf("webhook: %v", hook.types())
		}
		reset()
	})

	t.Run("a failure names the step but never the error", func(t *testing.T) {
		world.mu.Lock()
		world.failNS["notif-boom"] = true
		world.mu.Unlock()
		id := submit(alice, "open", "notif", "boom")
		e.waitStatus(alice, id, "failed")
		drain()
		m, ok := find("Failed: boom for team notif", "alice@example.test")
		if !ok || !strings.Contains(m.Body, "cluster/boom/dev") {
			t.Fatalf("failure mail: %v", mp.messages(t))
		}
		for _, text := range []string{m.Body, string(hook.calls()[0].Body)} {
			if strings.Contains(text, "unreachable") {
				t.Errorf("the error text must stay out of notifications: %s", text)
			}
		}
		if get(hook.calls()[0].Event, "data", "failed_step") != "cluster/boom/dev" {
			t.Errorf("webhook: %v", hook.calls()[0].Event)
		}
		reset()

		e.expect(200, admin, "POST", "/api/v1/requests/"+id+"/release", map[string]string{"note": "gave up on it"})
		drain()
		if got := hook.types(); len(got) != 1 || got[0] != "request.abandoned" {
			t.Errorf("webhook: %v", got)
		}
		if len(mp.messages(t)) != 0 {
			t.Error("giving up is for the webhook only")
		}
		reset()
	})

	t.Run("a webhook that is down is retried with the same delivery id, then recovers", func(t *testing.T) {
		hook.setStatus(500)
		e.expect(202, admin, "POST", "/api/v1/notifications/test", nil)
		d.Once(ctx) // attempt 1: the mail is delivered, the webhook answers 500
		if n := count(`SELECT attempts::text FROM notification_outbox WHERE channel = 'webhook' ORDER BY id DESC LIMIT 1`); n != "1" {
			t.Fatalf("attempts = %s", n)
		}
		if e.rawQuery(`SELECT last_error FROM notification_outbox WHERE channel = 'webhook' ORDER BY id DESC LIMIT 1`) == "" {
			t.Error("the reason should be recorded")
		}
		hook.setStatus(0)
		time.Sleep(80 * time.Millisecond)
		d.Once(ctx)
		calls := hook.calls()
		if len(calls) != 2 || calls[0].Headers.Get("X-Plinth-Delivery") != calls[1].Headers.Get("X-Plinth-Delivery") {
			t.Fatalf("a retry must reuse the delivery id: %d calls", len(calls))
		}
		if st := count(`SELECT CASE WHEN sent_at IS NOT NULL THEN 'sent' ELSE 'no' END FROM notification_outbox WHERE channel = 'webhook' ORDER BY id DESC LIMIT 1`); st != "sent" {
			t.Errorf("state = %s", st)
		}
		if n := len(mp.messages(t)); n != 1 {
			t.Errorf("the mail must be delivered once, not once per webhook attempt: %d", n)
		}
		reset()
	})

	t.Run("it gives up after the attempts allowed, and at once when refused for good", func(t *testing.T) {
		hook.setStatus(500)
		e.expect(202, admin, "POST", "/api/v1/notifications/test", nil)
		for i := 0; i < 5; i++ {
			d.Once(ctx)
			time.Sleep(80 * time.Millisecond)
		}
		if n := len(hook.calls()); n != 3 {
			t.Errorf("tried %d times, want 3 (MaxAttempts)", n)
		}
		v, _ := e.expect(200, admin, "GET", "/api/v1/notifications", nil)
		var failed map[string]any
		for _, n := range get(v, "recent").([]any) {
			if get(n, "channel") == "webhook" && get(n, "state") == "failed" && get(n, "attempts") == float64(3) {
				failed = n.(map[string]any)
				break
			}
		}
		if failed == nil || !strings.Contains(fmt.Sprint(failed["last_error"]), "500") {
			t.Fatalf("a notification that gave up should say so, with the reason: %v", v)
		}
		reset()

		hook.setStatus(410)
		e.expect(202, admin, "POST", "/api/v1/notifications/test", nil)
		d.Once(ctx)
		time.Sleep(80 * time.Millisecond)
		d.Once(ctx)
		if n := len(hook.calls()); n != 1 {
			t.Errorf("a receiver that refuses for good must not be retried: %d calls", n)
		}
		reset()
	})

	t.Run("several dispatchers deliver each notification exactly once", func(t *testing.T) {
		const events = 15
		for i := 0; i < events; i++ {
			if err := e.st.QueueTest(ctx, "admin@example.test"); err != nil {
				t.Fatal(err)
			}
		}
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				other := *d
				for j := 0; j < 20; j++ {
					other.Once(ctx)
				}
			}()
		}
		wg.Wait()
		ids := map[string]int{}
		for _, c := range hook.calls() {
			ids[c.Headers.Get("X-Plinth-Delivery")]++
		}
		if len(ids) != events || len(hook.calls()) != events {
			t.Errorf("webhook: %d deliveries for %d notifications (%d distinct)", len(hook.calls()), events, len(ids))
		}
		if n := len(mp.messages(t)); n != events {
			t.Errorf("mail: %d messages for %d notifications", n, events)
		}
		reset()
	})

	t.Run("a notification claimed by a dispatcher that died is taken over", func(t *testing.T) {
		if err := e.st.QueueTest(ctx, "admin@example.test"); err != nil {
			t.Fatal(err)
		}
		first, err := e.st.ClaimNotifications(ctx, 10, time.Second)
		if err != nil || len(first) != 2 {
			t.Fatalf("%d claimed: %v", len(first), err)
		}
		again, _ := e.st.ClaimNotifications(ctx, 10, time.Second)
		if len(again) != 0 {
			t.Fatalf("a claimed notification must not be handed out twice: %d", len(again))
		}
		time.Sleep(1200 * time.Millisecond)
		taken, _ := e.st.ClaimNotifications(ctx, 10, time.Second)
		if len(taken) != 2 || taken[0].Attempts != 2 {
			t.Fatalf("after the lease it is taken over: %+v", taken)
		}
		for _, o := range taken {
			_ = e.st.MarkSent(ctx, o.ID)
		}
	})

	t.Run("old notifications are cleaned up", func(t *testing.T) {
		e.exec(`UPDATE notification_outbox SET sent_at = now() - interval '8 days' WHERE sent_at IS NOT NULL`)
		e.exec(`UPDATE notification_outbox SET dead_at = now() - interval '31 days' WHERE dead_at IS NOT NULL`)
		if err := e.st.PurgeNotifications(ctx, 7*24*time.Hour, 30*24*time.Hour); err != nil {
			t.Fatal(err)
		}
		if n := count(`SELECT count(*)::text FROM notification_outbox WHERE sent_at IS NOT NULL OR dead_at IS NOT NULL`); n != "0" {
			t.Errorf("%s old rows left", n)
		}
	})

	t.Run("only administrators see the queue", func(t *testing.T) {
		e.expect(403, alice, "GET", "/api/v1/notifications", nil)
		v, _ := e.expect(200, admin, "GET", "/api/v1/notifications", nil)
		if get(v, "email") != true || get(v, "webhook") != true {
			t.Errorf("channels: %v", v)
		}
		_, raw := e.expect(200, admin, "GET", "/api/v1/audit?limit=300", nil)
		if !strings.Contains(raw, "notifications.test") {
			t.Error("sending a test should be audited")
		}
	})

	t.Run("switching a channel off stops new messages from being queued", func(t *testing.T) {
		e.st.SetNotificationChannels(false, true)
		defer e.st.SetNotificationChannels(true, true)
		before := count(`SELECT count(*)::text FROM notification_outbox WHERE channel = 'email'`)
		e.expect(202, alice, "POST", "/api/v1/tenants/notif/members", map[string]string{"email": "later@example.test", "role": "member"})
		if count(`SELECT count(*)::text FROM notification_outbox WHERE channel = 'email'`) != before {
			t.Error("e-mail was queued with e-mail switched off")
		}
	})
}
