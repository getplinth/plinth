package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// Webhook posts events as JSON to a URL.
//
// The body is {"id", "type", "time", "text", "data"}. "text" is a one-line
// summary, so a Slack-style incoming webhook shows something useful as it is.
// When Secret is set, each request carries
//
//	X-Plinth-Timestamp: unix seconds
//	X-Plinth-Signature: sha256=<hex HMAC-SHA256 of "<timestamp>.<body>">
//
// so the receiver can check who sent it and reject replays. X-Plinth-Delivery
// carries the notification id: delivery is at least once, so deduplicate on it.
type Webhook struct {
	URL, Secret string
	Client      *http.Client
}

func (w Webhook) client() *http.Client {
	if w.Client != nil {
		return w.Client
	}
	return &http.Client{
		Timeout: 10 * time.Second,
		// A redirect would send the signed body somewhere else: treat it as a failure.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// Body is the JSON sent for a notification.
func Body(id int64, event string, created time.Time, data map[string]any, base string) ([]byte, error) {
	return json.Marshal(map[string]any{
		"id": id, "type": event, "time": created.UTC().Format(time.RFC3339),
		"text": Render(event, data, base).Text, "data": data,
	})
}

// Sign returns the signature header value for a body sent at the given time.
func Sign(secret string, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(strconv.FormatInt(ts, 10) + "."))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// Send posts one notification.
func (w Webhook) Send(ctx context.Context, id int64, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(body))
	if err != nil {
		return permanent{err}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "plinth-webhook")
	req.Header.Set("X-Plinth-Delivery", strconv.FormatInt(id, 10))
	if w.Secret != "" {
		ts := time.Now().Unix()
		req.Header.Set("X-Plinth-Timestamp", strconv.FormatInt(ts, 10))
		req.Header.Set("X-Plinth-Signature", Sign(w.Secret, ts, body))
	}
	resp, err := w.client().Do(req)
	if err != nil {
		return fmt.Errorf("post to the webhook: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return fmt.Errorf("the webhook answered %d", resp.StatusCode)
	default: // other 3xx and 4xx: the receiver says the request itself is wrong
		return permanent{fmt.Errorf("the webhook refused it with %d", resp.StatusCode)}
	}
}
