package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Notification event types.
const (
	EventInvitation       = "invitation.created"
	EventAwaitingApproval = "request.awaiting_approval"
	EventApproved         = "request.approved"
	EventRejected         = "request.rejected"
	EventSucceeded        = "request.succeeded"
	EventFailed           = "request.failed"
	EventAbandoned        = "request.abandoned"
	EventTest             = "test"
)

// SetNotificationChannels says which delivery channels are configured. While
// neither is, nothing is queued, so a deployment without notifications does
// not collect rows nobody will send.
func (s *Store) SetNotificationChannels(email, webhook bool) {
	s.notifyEmail.Store(email)
	s.notifyWebhook.Store(webhook)
}

// NotificationChannels reports what SetNotificationChannels was given.
func (s *Store) NotificationChannels() (email, webhook bool) {
	return s.notifyEmail.Load(), s.notifyWebhook.Load()
}

// enqueue queues one event inside the caller's transaction: a webhook delivery
// (if configured) and one e-mail per distinct, valid recipient (if configured).
// Data holds facts only and must never carry credentials.
func (s *Store) enqueue(ctx context.Context, tx pgx.Tx, event string, data map[string]any, emails ...string) error {
	wantEmail, wantWebhook := s.NotificationChannels()
	if !wantEmail && !wantWebhook {
		return nil
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if wantWebhook {
		if _, err := tx.Exec(ctx, `INSERT INTO notification_outbox (channel, event_type, data) VALUES ('webhook', $1, $2)`, event, raw); err != nil {
			return err
		}
	}
	if wantEmail {
		seen := map[string]bool{}
		for _, e := range emails {
			e = strings.ToLower(strings.TrimSpace(e))
			if _, err := mail.ParseAddress(e); err != nil || seen[e] || strings.ContainsAny(e, "\r\n") {
				continue
			}
			seen[e] = true
			if _, err := tx.Exec(ctx, `INSERT INTO notification_outbox (channel, recipient, event_type, data) VALUES ('email', $1, $2, $3)`, e, event, raw); err != nil {
				return err
			}
		}
	}
	return nil
}

// adminEmails lists administrators' addresses, leaving out one person (the requester).
func adminEmails(ctx context.Context, tx pgx.Tx, except string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT email FROM users WHERE role = 'admin' AND email LIKE '%@%' AND lower(email) <> lower($1) ORDER BY email`, except)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// notifyRequest queues the notification for a request event. The data names
// the request, who asked and what happened; it never includes error text or
// anything a step produced.
func (s *Store) notifyRequest(ctx context.Context, tx pgx.Tx, event, requestID string) error {
	if email, webhook := s.NotificationChannels(); !email && !webhook {
		return nil
	}
	var (
		tenant, blueprint, requester, status string
		decidedBy, note                      *string
		payload, state                       []byte
	)
	if err := tx.QueryRow(ctx, `SELECT t.name, b.name, u.email, r.status, d.email, r.decision_note, r.payload, r.state
		FROM requests r JOIN tenants t ON t.id = r.tenant_id JOIN blueprints b ON b.id = r.blueprint_id
		JOIN users u ON u.id = r.requested_by LEFT JOIN users d ON d.id = r.decided_by
		WHERE r.id = $1`, requestID).Scan(&tenant, &blueprint, &requester, &status, &decidedBy, &note, &payload, &state); err != nil {
		return err
	}
	var req struct {
		Projects []struct {
			Name string `json:"name"`
		} `json:"projects"`
	}
	_ = json.Unmarshal(payload, &req)
	projects := []string{}
	for _, p := range req.Projects {
		projects = append(projects, p.Name)
	}
	var st struct {
		Steps map[string]struct {
			Status string `json:"status"`
		} `json:"steps"`
	}
	_ = json.Unmarshal(state, &st)
	failedStep := ""
	for name, step := range st.Steps {
		if step.Status == "failed" && (failedStep == "" || name < failedStep) {
			failedStep = name
		}
	}
	data := map[string]any{
		"request_id": requestID, "tenant": tenant, "blueprint": blueprint, "projects": projects,
		"requested_by": requester, "status": status,
	}
	if decidedBy != nil {
		data["decided_by"] = *decidedBy
	}
	if note != nil && *note != "" {
		data["note"] = *note
	}
	if event == EventFailed && failedStep != "" {
		data["failed_step"] = failedStep
	}

	var to []string
	switch event {
	case EventAwaitingApproval:
		admins, err := adminEmails(ctx, tx, requester)
		if err != nil {
			return err
		}
		to = admins
	case EventApproved, EventRejected, EventSucceeded, EventFailed:
		to = []string{requester}
	}
	return s.enqueue(ctx, tx, event, data, to...)
}

// ---- delivery -----------------------------------------------------------------------------------------

// Outbound is a queued notification being delivered.
type Outbound struct {
	ID        int64
	Channel   string // "email" or "webhook"
	Recipient string
	Event     string
	Data      map[string]any
	CreatedAt time.Time
	Attempts  int // including the current one
}

// ClaimNotifications leases up to limit due notifications, oldest first. A
// notification whose lease expired (its dispatcher died) is taken again.
// Delivery is at least once: a crash between sending and MarkSent repeats it,
// which is why webhooks carry the notification id.
func (s *Store) ClaimNotifications(ctx context.Context, limit int, lease time.Duration) ([]Outbound, error) {
	rows, err := s.pool.Query(ctx, `UPDATE notification_outbox SET claimed_at = now(), attempts = attempts + 1
		WHERE id IN (SELECT id FROM notification_outbox
		             WHERE sent_at IS NULL AND dead_at IS NULL AND next_attempt_at <= now()
		               AND (claimed_at IS NULL OR claimed_at < now() - $2::interval)
		             ORDER BY id LIMIT $1 FOR UPDATE SKIP LOCKED)
		RETURNING id, channel, recipient, event_type, data, created_at, attempts`, limit, fmt.Sprintf("%d seconds", int(lease.Seconds())))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Outbound
	for rows.Next() {
		var o Outbound
		var raw []byte
		if err := rows.Scan(&o.ID, &o.Channel, &o.Recipient, &o.Event, &raw, &o.CreatedAt, &o.Attempts); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(raw, &o.Data)
		out = append(out, o)
	}
	return out, rows.Err()
}

// MarkSent records a delivery.
func (s *Store) MarkSent(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `UPDATE notification_outbox SET sent_at = now(), claimed_at = NULL, last_error = '' WHERE id = $1`, id)
	return err
}

// MarkFailed records a failed attempt. A permanent failure, or one that
// exhausted maxAttempts, gives up; otherwise it is tried again after retryIn.
func (s *Store) MarkFailed(ctx context.Context, id int64, reason string, permanent bool, maxAttempts int, retryIn time.Duration) error {
	if len(reason) > 500 {
		reason = reason[:500]
	}
	_, err := s.pool.Exec(ctx, `UPDATE notification_outbox SET claimed_at = NULL, last_error = $2,
		dead_at = CASE WHEN $3 OR attempts >= $4 THEN now() END,
		next_attempt_at = now() + $5::interval WHERE id = $1`,
		id, reason, permanent, maxAttempts, fmt.Sprintf("%d seconds", int(retryIn.Seconds())))
	return err
}

// PurgeNotifications deletes delivered notifications older than sentAfter and
// abandoned ones older than deadAfter.
func (s *Store) PurgeNotifications(ctx context.Context, sentAfter, deadAfter time.Duration) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM notification_outbox
		WHERE (sent_at IS NOT NULL AND sent_at < now() - $1::interval) OR (dead_at IS NOT NULL AND dead_at < now() - $2::interval)`,
		fmt.Sprintf("%d seconds", int(sentAfter.Seconds())), fmt.Sprintf("%d seconds", int(deadAfter.Seconds())))
	return err
}

// NotificationStatus is one queued or past notification, for administrators.
type NotificationStatus struct {
	ID        int64     `json:"id"`
	Channel   string    `json:"channel"`
	Recipient string    `json:"recipient,omitempty"`
	Event     string    `json:"event"`
	State     string    `json:"state"` // waiting, sent, failed (gave up)
	Attempts  int       `json:"attempts"`
	LastError string    `json:"last_error,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// ListNotifications returns the newest notifications first.
func (s *Store) ListNotifications(ctx context.Context, limit int) ([]NotificationStatus, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `SELECT id, channel, recipient, event_type,
		CASE WHEN sent_at IS NOT NULL THEN 'sent' WHEN dead_at IS NOT NULL THEN 'failed' ELSE 'waiting' END,
		attempts, last_error, created_at FROM notification_outbox ORDER BY id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NotificationStatus{}
	for rows.Next() {
		var n NotificationStatus
		if err := rows.Scan(&n.ID, &n.Channel, &n.Recipient, &n.Event, &n.State, &n.Attempts, &n.LastError, &n.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// QueueTest queues a test notification to the channels, and an e-mail to the given address.
func (s *Store) QueueTest(ctx context.Context, byEmail string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := s.enqueue(ctx, tx, EventTest, map[string]any{"by": byEmail}, byEmail); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
