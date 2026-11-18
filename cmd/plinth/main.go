// Command plinth is the onboarding platform server.
//
//	plinth            run the server (configuration from PLINTH_* variables)
//	plinth genkey     print a new random master key
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
	"sync"
	"syscall"
	"time"

	"github.com/getplinth/plinth/internal/api"
	"github.com/getplinth/plinth/internal/auth"
	"github.com/getplinth/plinth/internal/config"
	"github.com/getplinth/plinth/internal/crypto"
	"github.com/getplinth/plinth/internal/orchestrator"
	"github.com/getplinth/plinth/internal/store"
	"github.com/getplinth/plinth/internal/worker"
	"github.com/getplinth/plinth/web"

	// Drivers register themselves in init().
	_ "github.com/getplinth/plinth/internal/provider/gitea"
	_ "github.com/getplinth/plinth/internal/provider/harbor"
	_ "github.com/getplinth/plinth/internal/provider/kubernetes"
	_ "github.com/getplinth/plinth/internal/provider/postgres"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "genkey" {
		k, err := crypto.GenerateKey()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(k)
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	box, err := crypto.NewBox(cfg.MasterKey)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, cfg.DatabaseURL, box)
	if err != nil {
		return err
	}
	defer st.Close()
	st.SetTrustUnverifiedEmail(cfg.TrustUnverifiedEmail)
	if cfg.TrustUnverifiedEmail {
		log.Warn("PLINTH_TRUST_UNVERIFIED_EMAIL is on: e-mail addresses are trusted without the identity provider's verification")
	}
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	verifier, err := auth.New(ctx, auth.Config{
		Issuer: cfg.OIDCIssuer, Audience: cfg.OIDCAudience,
		AdminGroup: cfg.AdminGroup, GroupsClaim: cfg.GroupsClaim,
	})
	if err != nil {
		return err
	}

	// The browser talks to the identity provider directly, so its origin must
	// be allowed by the page's content security policy.
	issuer, _ := url.Parse(cfg.OIDCIssuer)
	srv := &http.Server{
		Addr: cfg.ListenAddr,
		Handler: api.New(api.Deps{
			Store: st, Verifier: verifier, Log: log,
			Public: api.PublicConfig{Issuer: cfg.OIDCIssuer, ClientID: cfg.OIDCAudience},
			UI:     web.Handler(issuer.Scheme + "://" + issuer.Host),
		}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		(&worker.Worker{
			Store: st, Log: log,
			Options: orchestrator.Options{MaxAttempts: 3, Backoff: 2 * time.Second},
		}).Run(ctx, cfg.Workers)
	}()

	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	log.Info("plinth starting", "config", cfg.Redacted())
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	wg.Wait() // workers hand unfinished requests back before exiting
	log.Info("plinth stopped")
	return nil
}
