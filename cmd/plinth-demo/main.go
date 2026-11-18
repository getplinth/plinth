// Command plinth-demo runs Plinth with simulated drivers and a built-in
// stand-in identity provider, for trying the web interface locally.
//
// It needs only a PostgreSQL database (PLINTH_DATABASE_URL, default is the one
// from `docker compose up -d postgres`). It binds to 127.0.0.1, signs in
// without passwords, and talks to no real system: DO NOT use it for anything
// real or expose it to a network.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/getplinth/plinth/internal/api"
	"github.com/getplinth/plinth/internal/auth"
	"github.com/getplinth/plinth/internal/auth/authtest"
	"github.com/getplinth/plinth/internal/crypto"
	"github.com/getplinth/plinth/internal/demo"
	"github.com/getplinth/plinth/internal/orchestrator"
	"github.com/getplinth/plinth/internal/store"
	"github.com/getplinth/plinth/internal/worker"
	"github.com/getplinth/plinth/web"
)

const (
	appAddr   = "127.0.0.1:8080"
	idpAddr   = "127.0.0.1:9100"
	clientID  = "plinth-ui"
	adminGrp  = "plinth-admins"
	defaultDB = "postgres://plinth:plinth@127.0.0.1:5432/plinth?sslmode=disable"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if err := run(log); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// masterKey persists a demo key next to the project so restarts can still
// read what was stored earlier.
func masterKey() (string, error) {
	dir := ".demo"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "master.key")
	if b, err := os.ReadFile(path); err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	k, err := crypto.GenerateKey()
	if err != nil {
		return "", err
	}
	return k, os.WriteFile(path, []byte(k+"\n"), 0o600)
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dbURL := os.Getenv("PLINTH_DATABASE_URL")
	if dbURL == "" {
		dbURL = defaultDB
	}
	key, err := masterKey()
	if err != nil {
		return err
	}
	box, err := crypto.NewBox(key)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, dbURL, box)
	if err != nil {
		return fmt.Errorf("%w\n\nStart a database first:  docker compose up -d postgres", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	demo.Register()
	if err := seed(ctx, st); err != nil {
		return err
	}

	idp, err := authtest.NewAt(clientID, idpAddr)
	if err != nil {
		return err
	}
	defer idp.Close()
	idp.EnableLogin([]authtest.User{
		{Sub: "alice", Email: "alice@example.test", Name: "Alice (developer)", Groups: []string{"developers"}},
		{Sub: "bob", Email: "bob@example.test", Name: "Bob (developer)", Groups: []string{"developers"}},
		{Sub: "root", Email: "root@example.test", Name: "Root (platform admin)", Groups: []string{adminGrp}},
		{Sub: "ada", Email: "ada@example.test", Name: "Ada (platform admin)", Groups: []string{adminGrp}},
	})
	verifier, err := auth.New(ctx, auth.Config{Issuer: idp.Issuer(), Audience: clientID, AdminGroup: adminGrp})
	if err != nil {
		return err
	}
	issuerURL, _ := url.Parse(idp.Issuer())

	srv := &http.Server{
		Addr: appAddr,
		Handler: api.New(api.Deps{
			Store: st, Verifier: verifier, Log: log,
			Public: api.PublicConfig{Issuer: idp.Issuer(), ClientID: clientID},
			UI:     web.Handler(issuerURL.Scheme + "://" + issuerURL.Host),
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		(&worker.Worker{Store: st, Log: log, Poll: 500 * time.Millisecond,
			Options: orchestrator.Options{MaxAttempts: 1, Backoff: 100 * time.Millisecond}}).Run(ctx, 2)
	}()
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sh)
	}()

	fmt.Printf("\nPlinth demo is running (simulated systems, no passwords).\n\n"+
		"  Open   http://%s\n"+
		"  Sign in as Alice or Bob to request a project, or as Root or Ada to administer.\n"+
		"  Try    the \"regulated\" blueprint waits for an administrator other than the requester.\n"+
		"  Tip    a tenant name containing \"fail\" fails once so you can try Retry.\n\n"+
		"Press Ctrl+C to stop.\n\n", appAddr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	wg.Wait()
	return nil
}

// seed creates ready-to-use blueprints on the first run: "standard" runs at
// once, "regulated" waits for an administrator's approval.
func seed(ctx context.Context, st *store.Store) error {
	// A database left over from an earlier run can only be read with the key
	// that was used then (kept in .demo/master.key). Say so, instead of
	// letting every request fail later.
	if _, _, err := st.IntegrationConfig(ctx, "demo-git"); err != nil && !errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("the demo database was created with a different key (.demo/master.key was removed or replaced), so it cannot be read.\nReset it with:  docker compose down -v && docker compose up -d --wait postgres\n(%w)", err)
	}
	if _, err := st.GetBlueprint(ctx, "standard"); err != nil {
		for _, in := range []struct {
			kind, name string
			cfg        map[string]string
		}{
			{"gitsource", "demo-git", map[string]string{"base_url": "https://git.demo.invalid"}},
			{"registry", "demo-registry", map[string]string{"base_url": "https://registry.demo.invalid"}},
			{"cluster", "demo-dev", map[string]string{"name": "dev", "server": "https://dev.demo.invalid"}},
			{"cluster", "demo-prod", map[string]string{"name": "prod", "server": "https://prod.demo.invalid"}},
			{"database", "demo-db", map[string]string{"host": "db.demo.invalid"}},
		} {
			driver := "demo"
			if in.kind == "database" {
				driver = "postgres" // the simulated database presents itself as PostgreSQL
			}
			if _, err := st.CreateIntegration(ctx, in.kind, driver, in.name, in.cfg); err != nil && !errors.Is(err, store.ErrConflict) {
				return err
			}
		}
	}
	for _, bp := range []struct {
		name     string
		approval bool
	}{{"standard", false}, {"regulated", true}} {
		if _, err := st.GetBlueprint(ctx, bp.name); err == nil {
			continue
		}
		if _, err := st.SaveBlueprint(ctx, store.Blueprint{
			Name: bp.name, GitSource: "demo-git", Registry: "demo-registry", GitParent: "tenants",
			TemplateRepo: "platform/app-template", ExtraBranches: []string{"develop", "staging"},
			RequiresApproval: bp.approval,
			Environments: []store.BlueprintEnv{
				{Name: "dev", Cluster: "demo-dev", Database: "demo-db", AccessPrefix: "oidc:", IngressNS: "ingress"},
				{Name: "prod", Cluster: "demo-prod", AccessPrefix: "oidc:", IngressNS: "ingress"},
			},
		}, true); err != nil {
			return err
		}
	}
	return nil
}
