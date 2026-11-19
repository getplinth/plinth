package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"

	"github.com/getplinth/plinth/internal/auth"
	"github.com/getplinth/plinth/internal/config"
	identitypkg "github.com/getplinth/plinth/internal/identity"
	"github.com/getplinth/plinth/internal/store"
)

// identity is who may sign in: the identity providers (from the environment
// and from the control panel) and the first administrator.
type identity struct {
	registry *auth.Registry
	env      *auth.Provider
}

func setupIdentity(ctx context.Context, log *slog.Logger, cfg config.Config, st *store.Store) (*identity, error) {
	id := &identity{}
	if cfg.OIDCIssuer != "" {
		id.env = &auth.Provider{
			Name: "environment", DisplayName: cfg.OIDCName, Issuer: cfg.OIDCIssuer, ClientID: cfg.OIDCAudience,
			Scopes: "openid profile email groups", GroupsClaim: cfg.GroupsClaim, AdminGroup: cfg.AdminGroup,
			TrustUnverifiedEmail: cfg.TrustUnverifiedEmail,
		}
	}
	id.registry = identitypkg.NewRegistry(st, id.env, cfg.OIDCAllowHTTP, log)
	if err := id.registry.Refresh(ctx); err != nil {
		return nil, err
	}
	if id.env != nil {
		// A provider that is down must not stop Plinth, but say so at once.
		if err := id.registry.Check(ctx, cfg.OIDCIssuer); err != nil {
			log.Warn("the identity provider from the environment cannot be reached yet", "err", err)
		}
	}
	if err := bootstrapAdmin(ctx, log, cfg, st); err != nil {
		return nil, err
	}
	return id, nil
}

func (id *identity) connectOrigins() []string { return identitypkg.ConnectOrigins(id.registry) }

// bootstrapAdmin creates the first administrator from PLINTH_BOOTSTRAP_ADMIN_*
// when no password account exists, and tells the operator when nobody could
// sign in at all.
func bootstrapAdmin(ctx context.Context, log *slog.Logger, cfg config.Config, st *store.Store) error {
	if cfg.BootstrapAdminPassword != "" {
		username, ok := store.NormalizeUsername(cfg.BootstrapAdminUsername)
		if !ok {
			return errors.New("PLINTH_BOOTSTRAP_ADMIN_USERNAME must be 2 to 63 characters: lowercase letters, digits, dots, dashes, underscores or @")
		}
		if err := auth.ValidatePassword(cfg.BootstrapAdminPassword, username, ""); err != nil {
			return fmt.Errorf("PLINTH_BOOTSTRAP_ADMIN_PASSWORD: %w", err)
		}
		hash, err := auth.HashPassword(cfg.BootstrapAdminPassword)
		if err != nil {
			return err
		}
		created, err := st.EnsureBootstrapAdmin(ctx, username, hash, true)
		if err != nil {
			return err
		}
		if created {
			log.Info("the first administrator was created: sign in and choose a new password", "username", username)
		} else if accounts, err := st.ListLocalAccounts(ctx); err == nil && len(accounts) == 0 {
			log.Info("this installation already has users, so no password administrator was created; add one with `plinth bootstrap-admin` if you want one")
		}
		return nil
	}
	accounts, err := st.ListLocalAccounts(ctx)
	if err != nil {
		return err
	}
	if len(accounts) == 0 && cfg.OIDCIssuer == "" {
		providers, err := st.ListOIDCProviders(ctx)
		if err != nil {
			return err
		}
		if len(providers) == 0 {
			log.Warn("nobody can sign in yet: set PLINTH_BOOTSTRAP_ADMIN_PASSWORD, or run `plinth bootstrap-admin` against this database")
		}
	}
	return nil
}

// cmdBootstrapAdmin is `plinth bootstrap-admin`: create the first administrator
// or, with --reset, set a new password for a password account (the way back in
// for whoever has database access). The password comes from
// PLINTH_BOOTSTRAP_ADMIN_PASSWORD or is generated and printed once.
func cmdBootstrapAdmin(args []string) error {
	fs := flag.NewFlagSet("bootstrap-admin", flag.ContinueOnError)
	username := fs.String("username", "", "username (default: PLINTH_BOOTSTRAP_ADMIN_USERNAME or admin)")
	reset := fs.Bool("reset", false, "set a new password for an existing account")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.LoadDatabase()
	if err != nil {
		return err
	}
	name := cfg.BootstrapAdminUsername
	if *username != "" {
		name = *username
	}
	name, ok := store.NormalizeUsername(name)
	if !ok {
		return errors.New("the username must be 2 to 63 characters: lowercase letters, digits, dots, dashes, underscores or @")
	}
	password, generated := cfg.BootstrapAdminPassword, false
	if password == "" {
		if password, err = auth.GeneratePassword(); err != nil {
			return err
		}
		generated = true
	} else if err := auth.ValidatePassword(password, name, ""); err != nil {
		return fmt.Errorf("PLINTH_BOOTSTRAP_ADMIN_PASSWORD: %w", err)
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return err
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.DatabaseURL, nil)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	if *reset {
		created, err := st.ResetLocalPassword(ctx, name, hash)
		if err != nil {
			return err
		}
		if created {
			fmt.Printf("created the administrator %q\n", name)
		} else {
			fmt.Printf("reset the password of %q\n", name)
		}
	} else {
		created, err := st.EnsureBootstrapAdmin(ctx, name, hash, false)
		if err != nil {
			return err
		}
		if !created {
			return errors.New("a password account already exists, so nothing was created; use --reset to set a new password for one")
		}
		fmt.Printf("created the administrator %q\n", name)
	}
	if generated {
		fmt.Printf("password: %s\n", password)
	}
	fmt.Fprintln(os.Stderr, "The password has to be changed at the first sign-in.")
	return nil
}
