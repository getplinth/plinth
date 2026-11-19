// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Config is Plinth's runtime configuration.
type Config struct {
	ListenAddr  string
	DatabaseURL string
	// MasterKey encrypts integration credentials at rest: 32 random bytes,
	// base64. Generate one with `plinth genkey`. In production inject it from
	// a KMS or secret manager, never commit it.
	MasterKey string
	// MasterKeyPrevious are former master keys, kept only so data encrypted
	// with them can still be read while rotating (comma separated in the
	// environment). New data never uses them. Remove them once `plinth keys`
	// shows nothing is encrypted with them any more.
	MasterKeyPrevious []string

	OIDCIssuer   string // identity provider issuer URL
	OIDCAudience string // client id the tokens are issued for
	AdminGroup   string // members of this group are administrators
	GroupsClaim  string // token claim that lists groups

	Workers int // concurrent request workers

	// TrustUnverifiedEmail accepts e-mail addresses as proof of identity even
	// when the identity provider does not say it verified them. This decides
	// whether team invitations can be claimed and whether existing accounts can
	// be added by e-mail. Off by default; see docs/api.md before turning it on.
	TrustUnverifiedEmail bool

	// PublicURL is the address people open Plinth at, used for links in
	// notifications. Optional: without it messages carry no links.
	PublicURL string

	// SMTP is where e-mail notifications go. Empty Host means e-mail is off.
	SMTPHost, SMTPUsername, SMTPPassword, SMTPFrom, SMTPTLS string
	SMTPPort                                                int
	// WebhookURL receives every notification as JSON. Empty means no webhook.
	// WebhookSecret, when set, signs each request.
	WebhookURL, WebhookSecret string
	// NotifyAllowInsecure permits an http:// webhook and an unencrypted mail
	// connection. For local testing only.
	NotifyAllowInsecure bool
}

// EmailEnabled and WebhookEnabled say which notification channels are configured.
func (c Config) EmailEnabled() bool   { return c.SMTPHost != "" }
func (c Config) WebhookEnabled() bool { return c.WebhookURL != "" }

// Load reads and validates the environment. All problems are reported at once.
func Load() (Config, error) {
	return load(os.Getenv, true)
}

// LoadStorage reads only what maintenance commands (such as `plinth rekey`)
// need: the database and the master keys. The identity provider is not required.
func LoadStorage() (Config, error) {
	return load(os.Getenv, false)
}

func load(get func(string) string, full bool) (Config, error) {
	c := Config{
		ListenAddr:        valueOr(get("PLINTH_LISTEN_ADDR"), ":8080"),
		DatabaseURL:       get("PLINTH_DATABASE_URL"),
		MasterKey:         get("PLINTH_MASTER_KEY"),
		MasterKeyPrevious: splitList(get("PLINTH_MASTER_KEY_PREVIOUS")),
		OIDCIssuer:        get("PLINTH_OIDC_ISSUER"),
		OIDCAudience:      get("PLINTH_OIDC_AUDIENCE"),
		AdminGroup:        valueOr(get("PLINTH_ADMIN_GROUP"), "plinth-admins"),
		GroupsClaim:       valueOr(get("PLINTH_GROUPS_CLAIM"), "groups"),
		Workers:           2,
	}
	var problems []string
	need := func(v, name string) {
		if v == "" {
			problems = append(problems, name+" is required")
		}
	}
	need(c.DatabaseURL, "PLINTH_DATABASE_URL")
	need(c.MasterKey, "PLINTH_MASTER_KEY")
	if full {
		need(c.OIDCIssuer, "PLINTH_OIDC_ISSUER")
		need(c.OIDCAudience, "PLINTH_OIDC_AUDIENCE")
	}

	if full && c.OIDCIssuer != "" {
		u, err := url.Parse(c.OIDCIssuer)
		allowHTTP, _ := strconv.ParseBool(get("PLINTH_OIDC_ALLOW_HTTP"))
		switch {
		case err != nil || u.Host == "":
			problems = append(problems, "PLINTH_OIDC_ISSUER is not a valid URL")
		case u.Scheme != "https" && !(u.Scheme == "http" && allowHTTP):
			problems = append(problems, "PLINTH_OIDC_ISSUER must be https (PLINTH_OIDC_ALLOW_HTTP=true is for local testing only)")
		}
	}
	if v := get("PLINTH_TRUST_UNVERIFIED_EMAIL"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			problems = append(problems, "PLINTH_TRUST_UNVERIFIED_EMAIL must be true or false")
		}
		c.TrustUnverifiedEmail = b
	}
	if full {
		problems = append(problems, c.loadNotifications(get)...)
	}
	if v := get("PLINTH_WORKERS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 32 {
			problems = append(problems, "PLINTH_WORKERS must be between 1 and 32")
		} else {
			c.Workers = n
		}
	}
	if len(problems) > 0 {
		return c, errors.New("configuration: " + strings.Join(problems, "; "))
	}
	return c, nil
}

func splitList(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == '\r' })
}

func valueOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// Redacted describes the configuration for logs, without secrets.
func (c Config) Redacted() string {
	return fmt.Sprintf("listen=%s issuer=%s audience=%s admin_group=%s workers=%d trust_unverified_email=%v previous_master_keys=%d email=%v webhook=%v",
		c.ListenAddr, c.OIDCIssuer, c.OIDCAudience, c.AdminGroup, c.Workers, c.TrustUnverifiedEmail, len(c.MasterKeyPrevious), c.EmailEnabled(), c.WebhookEnabled())
}

// loadNotifications reads the e-mail and webhook settings and reports what is wrong with them.
func (c *Config) loadNotifications(get func(string) string) []string {
	var problems []string
	c.NotifyAllowInsecure, _ = strconv.ParseBool(get("PLINTH_NOTIFY_ALLOW_INSECURE"))
	c.PublicURL = strings.TrimRight(get("PLINTH_PUBLIC_URL"), "/")
	if c.PublicURL != "" {
		u, err := url.Parse(c.PublicURL)
		switch {
		case err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http"):
			problems = append(problems, "PLINTH_PUBLIC_URL must be an http(s) address such as https://plinth.example.com")
		case u.Scheme == "http" && !c.NotifyAllowInsecure:
			problems = append(problems, "PLINTH_PUBLIC_URL should be https (PLINTH_NOTIFY_ALLOW_INSECURE=true is for local testing only)")
		}
	}

	c.SMTPHost = strings.TrimSpace(get("PLINTH_SMTP_HOST"))
	c.SMTPUsername, c.SMTPPassword = get("PLINTH_SMTP_USERNAME"), get("PLINTH_SMTP_PASSWORD")
	c.SMTPFrom = strings.TrimSpace(get("PLINTH_SMTP_FROM"))
	c.SMTPTLS = valueOr(strings.ToLower(get("PLINTH_SMTP_TLS")), "starttls")
	c.SMTPPort = 587
	if c.SMTPHost != "" {
		if v := get("PLINTH_SMTP_PORT"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 65535 {
				problems = append(problems, "PLINTH_SMTP_PORT must be a port number")
			} else {
				c.SMTPPort = n
			}
		} else if c.SMTPTLS == "tls" {
			c.SMTPPort = 465
		}
		switch c.SMTPTLS {
		case "starttls", "tls":
		case "none":
			if !c.NotifyAllowInsecure {
				problems = append(problems, "PLINTH_SMTP_TLS=none sends mail and the password unencrypted: it needs PLINTH_NOTIFY_ALLOW_INSECURE=true and is for local testing only")
			}
		default:
			problems = append(problems, "PLINTH_SMTP_TLS must be starttls, tls or none")
		}
		if c.SMTPFrom == "" {
			problems = append(problems, "PLINTH_SMTP_FROM is required with PLINTH_SMTP_HOST (the sender address)")
		} else if _, err := mail.ParseAddress(c.SMTPFrom); err != nil || strings.ContainsAny(c.SMTPFrom, "\r\n") {
			problems = append(problems, "PLINTH_SMTP_FROM is not a valid address, for example plinth@example.com or Plinth <plinth@example.com>")
		}
		if (c.SMTPUsername == "") != (c.SMTPPassword == "") {
			problems = append(problems, "PLINTH_SMTP_USERNAME and PLINTH_SMTP_PASSWORD go together")
		}
	} else if c.SMTPUsername != "" || c.SMTPPassword != "" || c.SMTPFrom != "" {
		problems = append(problems, "PLINTH_SMTP_HOST is required when other PLINTH_SMTP_* settings are given")
	}

	c.WebhookURL, c.WebhookSecret = strings.TrimSpace(get("PLINTH_WEBHOOK_URL")), get("PLINTH_WEBHOOK_SECRET")
	if c.WebhookURL != "" {
		u, err := url.Parse(c.WebhookURL)
		switch {
		case err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http"):
			problems = append(problems, "PLINTH_WEBHOOK_URL must be an http(s) address")
		case u.Scheme == "http" && !c.NotifyAllowInsecure:
			problems = append(problems, "PLINTH_WEBHOOK_URL must be https (PLINTH_NOTIFY_ALLOW_INSECURE=true is for local testing only)")
		}
	} else if c.WebhookSecret != "" {
		problems = append(problems, "PLINTH_WEBHOOK_SECRET needs PLINTH_WEBHOOK_URL")
	}
	return problems
}
