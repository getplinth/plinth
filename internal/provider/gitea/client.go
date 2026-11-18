package gitea

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// client is a minimal Gitea REST client. The token is only ever sent in the
// Authorization header and never appears in errors.
type client struct {
	base  *url.URL // e.g. https://git.example.com
	token string
	hc    *http.Client
}

type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string { return fmt.Sprintf("gitea API %d: %s", e.Status, e.Message) }

func isStatus(err error, code int) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Status == code
}

func newClient(base *url.URL, token string, caPEM string) (*client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if caPEM != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(caPEM)) {
			return nil, errors.New("gitea: ca_cert is not valid PEM")
		}
		tr.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &client{base: base, token: token, hc: &http.Client{Transport: tr, Timeout: 30 * time.Second}}, nil
}

// do sends a JSON request. body and out may be nil.
func (c *client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	u := *c.base
	u.Path = strings.TrimRight(c.base.Path, "/") + "/api/v1" + path
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("gitea: %s %s: %w", method, path, scrub(err, c.token))
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		var m struct {
			Message string `json:"message"`
		}
		msg := strings.TrimSpace(string(data))
		if json.Unmarshal(data, &m) == nil && m.Message != "" {
			msg = m.Message
		}
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return &apiError{Status: resp.StatusCode, Message: msg}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// scrub removes a secret from an error message.
func scrub(err error, secret string) error {
	if secret == "" {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), secret, "***"))
}
