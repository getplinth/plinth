// Package config loads runtime configuration from environment variables.
package config

import (
	"errors"
	"fmt"
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
}

// Load reads and validates the environment. All problems are reported at once.
func Load() (Config, error) {
	return load(os.Getenv)
}

func load(get func(string) string) (Config, error) {
	c := Config{
		ListenAddr:   valueOr(get("PLINTH_LISTEN_ADDR"), ":8080"),
		DatabaseURL:  get("PLINTH_DATABASE_URL"),
		MasterKey:    get("PLINTH_MASTER_KEY"),
		OIDCIssuer:   get("PLINTH_OIDC_ISSUER"),
		OIDCAudience: get("PLINTH_OIDC_AUDIENCE"),
		AdminGroup:   valueOr(get("PLINTH_ADMIN_GROUP"), "plinth-admins"),
		GroupsClaim:  valueOr(get("PLINTH_GROUPS_CLAIM"), "groups"),
		Workers:      2,
	}
	var problems []string
	need := func(v, name string) {
		if v == "" {
			problems = append(problems, name+" is required")
		}
	}
	need(c.DatabaseURL, "PLINTH_DATABASE_URL")
	need(c.MasterKey, "PLINTH_MASTER_KEY")
	need(c.OIDCIssuer, "PLINTH_OIDC_ISSUER")
	need(c.OIDCAudience, "PLINTH_OIDC_AUDIENCE")

	if c.OIDCIssuer != "" {
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

func valueOr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// Redacted describes the configuration for logs, without secrets.
func (c Config) Redacted() string {
	return fmt.Sprintf("listen=%s issuer=%s audience=%s admin_group=%s workers=%d trust_unverified_email=%v",
		c.ListenAddr, c.OIDCIssuer, c.OIDCAudience, c.AdminGroup, c.Workers, c.TrustUnverifiedEmail)
}
