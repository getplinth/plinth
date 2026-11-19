// Command plinth is the onboarding platform server.
//
//	plinth            run the server (configuration from PLINTH_* variables)
//	plinth genkey     print a new random master key
//	plinth bootstrap-admin  create the first administrator, or with --reset set a new password
//	plinth keys       show which master key each stored credential is encrypted with
//	plinth rekey      move every stored credential to the current master key
//	                  (--dry-run only reports); see docs/api.md, "Rotating the master key"
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/mail"
	"os"
	"os/signal"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/getplinth/plinth/internal/api"
	"github.com/getplinth/plinth/internal/config"
	"github.com/getplinth/plinth/internal/crypto"
	"github.com/getplinth/plinth/internal/notify"
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
	if len(os.Args) > 1 && os.Args[1] == "bootstrap-admin" {
		if err := cmdBootstrapAdmin(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "plinth:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && (os.Args[1] == "keys" || os.Args[1] == "rekey") {
		if err := maintain(os.Args[1], os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "plinth:", err)
			os.Exit(1)
		}
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

	box, err := crypto.NewRing(cfg.MasterKey, cfg.MasterKeyPrevious...)
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
	warnAboutOldKeys(ctx, log, st, len(cfg.MasterKeyPrevious))
	dispatcher := notifier(cfg, st, log)
	ident, err := setupIdentity(ctx, log, cfg, st)
	if err != nil {
		return err
	}
	if cfg.ForceLocalLogin {
		log.Warn("PLINTH_FORCE_LOCAL_LOGIN is on: password sign-in works whatever the control panel says")
	}

	// The browser talks to identity providers directly, so their origins must
	// be allowed by the page's content security policy; the list follows the
	// providers an administrator configures.
	srv := &http.Server{
		Addr: cfg.ListenAddr,
		Handler: api.New(api.Deps{
			Store: st, Verifier: ident.registry, Providers: ident.registry, EnvProvider: ident.env,
			AllowLoopbackIssuers: cfg.OIDCAllowHTTP, ForceLocalLogin: cfg.ForceLocalLogin, Log: log,
			UI: web.HandlerFunc(ident.connectOrigins),
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

	wg.Add(1)
	go func() { // expired sessions
		defer wg.Done()
		tick := time.NewTicker(time.Hour)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if err := st.PurgeSessions(ctx); err != nil && ctx.Err() == nil {
					log.Warn("could not remove expired sessions", "err", err)
				}
			}
		}
	}()

	if dispatcher != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dispatcher.Run(ctx)
		}()
	}

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

// warnAboutOldKeys tells the operator, at startup, whether stored credentials
// still depend on a previous master key, and whether any cannot be read at all.
func warnAboutOldKeys(ctx context.Context, log *slog.Logger, st *store.Store, previous int) {
	usage, err := st.KeyUsage(ctx)
	if err != nil {
		log.Warn("could not check which master keys stored credentials use", "err", err)
		return
	}
	var old int
	for id, n := range usage {
		if id != st.PrimaryKeyID() {
			old += n
		}
	}
	switch {
	case old > 0:
		log.Warn("some stored credentials are not on the current master key: run `plinth rekey`", "records", old, "current_key", st.PrimaryKeyID())
	case previous > 0:
		log.Info("every stored credential is on the current master key: PLINTH_MASTER_KEY_PREVIOUS can be removed", "current_key", st.PrimaryKeyID())
	}
}

// maintain runs the `keys` and `rekey` commands. They need the database and
// the master keys but not the identity provider.
func maintain(cmd string, args []string) error {
	dry := false
	for _, a := range args {
		if a == "--dry-run" && cmd == "rekey" {
			dry = true
			continue
		}
		return fmt.Errorf("unknown argument %q", a)
	}
	cfg, err := config.LoadStorage()
	if err != nil {
		return err
	}
	box, err := crypto.NewRing(cfg.MasterKey, cfg.MasterKeyPrevious...)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	st, err := store.Open(ctx, cfg.DatabaseURL, box)
	if err != nil {
		return err
	}
	defer st.Close()

	describe := func(id string) string {
		switch {
		case id == st.PrimaryKeyID():
			return id + " (current)"
		case id == "legacy":
			return id + " (written before keys had ids)"
		}
		for _, k := range box.KeyIDs()[1:] {
			if k == id {
				return id + " (previous)"
			}
		}
		return id + " (NOT CONFIGURED: add that key to PLINTH_MASTER_KEY_PREVIOUS)"
	}
	report := func() error {
		usage, err := st.KeyUsage(ctx)
		if err != nil {
			return err
		}
		if len(usage) == 0 {
			fmt.Println("nothing is encrypted yet")
		}
		ids := make([]string, 0, len(usage))
		for id := range usage {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, id := range ids {
			fmt.Printf("%4d record(s) encrypted with key %s\n", usage[id], describe(id))
		}
		return nil
	}

	if cmd == "keys" {
		return report()
	}
	rep, err := st.Rekey(ctx, dry)
	if err != nil {
		return err
	}
	verb := "moved to the current key"
	if dry {
		verb = "would be moved (dry run, nothing written)"
	}
	fmt.Printf("%d record(s): %d already current, %d %s, %d failed\n", rep.Total, rep.Current, rep.Rewrapped, verb, len(rep.Failed))
	for _, f := range rep.Failed {
		fmt.Println("  failed:", f)
	}
	if err := report(); err != nil {
		return err
	}
	if len(rep.Failed) > 0 {
		return fmt.Errorf("%d record(s) could not be moved: keep the previous keys configured until they are fixed", len(rep.Failed))
	}
	if !dry {
		fmt.Println("done: the previous keys are no longer needed once every instance has restarted without them")
	}
	return nil
}

// notifier switches on the configured notification channels and returns the
// dispatcher that delivers them, or nil when neither is configured.
func notifier(cfg config.Config, st *store.Store, log *slog.Logger) *notify.Dispatcher {
	st.SetNotificationChannels(cfg.EmailEnabled(), cfg.WebhookEnabled())
	if !cfg.EmailEnabled() && !cfg.WebhookEnabled() {
		return nil
	}
	d := &notify.Dispatcher{Store: st, Log: log, PublicURL: cfg.PublicURL}
	if cfg.EmailEnabled() {
		from, _ := mail.ParseAddress(cfg.SMTPFrom) // validated when the configuration was loaded
		d.Email = &notify.SMTP{Host: cfg.SMTPHost, Port: cfg.SMTPPort, Username: cfg.SMTPUsername, Password: cfg.SMTPPassword, From: *from, TLS: cfg.SMTPTLS}
		log.Info("e-mail notifications are on", "server", cfg.SMTPHost, "tls", cfg.SMTPTLS)
	}
	if cfg.WebhookEnabled() {
		d.Webhook = &notify.Webhook{URL: cfg.WebhookURL, Secret: cfg.WebhookSecret}
		log.Info("webhook notifications are on", "signed", cfg.WebhookSecret != "")
	}
	if cfg.PublicURL == "" {
		log.Warn("PLINTH_PUBLIC_URL is not set: notifications will not contain links")
	}
	if cfg.NotifyAllowInsecure {
		log.Warn("PLINTH_NOTIFY_ALLOW_INSECURE is on: notifications may use unencrypted connections")
	}
	return d
}
