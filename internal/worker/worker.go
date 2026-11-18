// Package worker runs queued onboarding requests.
//
// Requests live in PostgreSQL. A worker claims one with a lease
// (FOR UPDATE SKIP LOCKED), runs its steps, and refreshes the lease every
// time it saves progress. If a worker dies, the lease expires and another
// worker resumes the request from the saved state; finished steps are not
// repeated.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/getplinth/plinth/internal/onboarding"
	"github.com/getplinth/plinth/internal/orchestrator"
	"github.com/getplinth/plinth/internal/provider"
	"github.com/getplinth/plinth/internal/quota"
	"github.com/getplinth/plinth/internal/store"
)

// Worker polls the queue.
type Worker struct {
	Store   *store.Store
	Log     *slog.Logger
	Poll    time.Duration // idle wait between polls, default 2s
	Lease   time.Duration // how long a silent worker keeps a request, default 15m
	Timeout time.Duration // maximum time for one request, default 30m
	Options orchestrator.Options
}

func (w *Worker) defaults() {
	if w.Log == nil {
		w.Log = slog.Default()
	}
	if w.Poll == 0 {
		w.Poll = 2 * time.Second
	}
	if w.Lease == 0 {
		w.Lease = 15 * time.Minute
	}
	if w.Timeout == 0 {
		w.Timeout = 30 * time.Minute
	}
}

// Run starts n workers and blocks until ctx is cancelled and they have stopped.
func (w *Worker) Run(ctx context.Context, n int) {
	w.defaults()
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("worker-%d-%04x", i, rand.Intn(0xffff))
		wg.Add(1)
		go func() {
			defer wg.Done()
			w.loop(ctx, id)
		}()
	}
	wg.Wait()
}

func (w *Worker) loop(ctx context.Context, id string) {
	for ctx.Err() == nil {
		worked, err := w.RunOne(ctx, id)
		if err != nil {
			w.Log.Error("worker error", "worker", id, "err", err)
		}
		if worked && err == nil {
			continue // more may be waiting
		}
		select {
		case <-ctx.Done():
		case <-time.After(w.Poll + time.Duration(rand.Int63n(int64(w.Poll/2)+1))):
		}
	}
}

// RunOne claims and processes at most one request. It reports whether it did.
func (w *Worker) RunOne(ctx context.Context, workerID string) (bool, error) {
	w.defaults()
	rec, err := w.Store.ClaimNext(ctx, workerID, w.Lease)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	w.process(ctx, workerID, rec)
	return true, nil
}

func (w *Worker) process(ctx context.Context, workerID string, rec store.RequestRecord) {
	log := w.Log.With("request", rec.ID, "tenant", rec.Tenant, "worker", workerID)
	log.Info("request started")

	runCtx, cancel := context.WithTimeout(ctx, w.Timeout)
	defer cancel()

	var runErr error
	func() {
		defer func() {
			if p := recover(); p != nil {
				log.Error("panic while running request", "panic", p, "stack", string(debug.Stack()))
				runErr = fmt.Errorf("internal error while running the request")
			}
		}()
		bp, err := w.resolve(runCtx, rec)
		if err != nil {
			runErr = err
			return
		}
		runErr = onboarding.Execute(runCtx, rec.ID, bp, rec.Payload, &rec.State, w.Store, w.Options)
	}()

	switch {
	case errors.Is(runErr, store.ErrLeaseLost):
		log.Warn("lease lost, another worker owns the request")
	case ctx.Err() != nil:
		// Shutting down: hand the request back with its saved progress.
		rel, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := w.Store.Release(rel, rec.ID, workerID); err != nil {
			log.Error("release on shutdown failed", "err", err)
		}
		log.Info("request released on shutdown")
	default:
		fin, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := w.Store.Finish(fin, rec.ID, runErr); err != nil {
			log.Error("recording the outcome failed", "err", err)
			return
		}
		if runErr != nil {
			log.Warn("request failed", "err", runErr)
		} else {
			log.Info("request succeeded")
		}
	}
}

func (w *Worker) resolve(ctx context.Context, rec store.RequestRecord) (onboarding.Blueprint, error) {
	rb, err := w.Store.ResolveBlueprint(ctx, rec.Blueprint)
	if err != nil {
		return onboarding.Blueprint{}, fmt.Errorf("loading blueprint %q: %w", rec.Blueprint, err)
	}
	tiers, err := w.Store.Tiers(ctx)
	if err != nil {
		return onboarding.Blueprint{}, err
	}
	return Build(rb, tiers, rec.Tenant)
}

// Build instantiates the drivers of a resolved blueprint.
func Build(rb store.ResolvedBlueprint, tiers quota.Tiers, tenant string) (onboarding.Blueprint, error) {
	bp := onboarding.Blueprint{
		GitParent: rb.GitParent, TemplateRepo: rb.TemplateRepo,
		ExtraBranches: rb.ExtraBranches, Tiers: tiers,
	}
	if len(rb.TemplateVars) > 0 {
		vars := rb.TemplateVars
		bp.TemplateVars = func(project string) map[string]string {
			out := make(map[string]string, len(vars))
			r := strings.NewReplacer("{project}", project, "{tenant}", tenant)
			for k, v := range vars {
				out[k] = r.Replace(v)
			}
			return out
		}
	}

	var err error
	if bp.Git, err = driver[provider.GitSource](rb, rb.GitSource, provider.KindGitSource); err != nil {
		return bp, err
	}
	if rb.Registry != "" {
		if bp.Registry, err = driver[provider.Registry](rb, rb.Registry, provider.KindRegistry); err != nil {
			return bp, err
		}
	}
	for _, e := range rb.Environments {
		env := onboarding.Environment{Name: e.Name, AccessPrefix: e.AccessPrefix, IngressNS: e.IngressNS}
		if env.Cluster, err = driver[provider.Cluster](rb, e.Cluster, provider.KindCluster); err != nil {
			return bp, err
		}
		if e.Database != "" {
			if env.Database, err = driver[provider.Database](rb, e.Database, provider.KindDatabase); err != nil {
				return bp, err
			}
		}
		bp.Envs = append(bp.Envs, env)
	}
	return bp, nil
}

func driver[T provider.Provider](rb store.ResolvedBlueprint, name string, kind provider.Kind) (T, error) {
	var zero T
	spec, ok := rb.Integrations[name]
	if !ok {
		return zero, fmt.Errorf("integration %q is missing", name)
	}
	p, err := provider.New(kind, spec.Driver, spec.Config)
	if err != nil {
		return zero, fmt.Errorf("integration %q: %w", name, err)
	}
	// The API validates requests using the driver name stored with the
	// integration, so it must be the name the driver reports for itself.
	if p.Type() != spec.Driver {
		return zero, fmt.Errorf("integration %q: driver registered as %q reports itself as %q", name, spec.Driver, p.Type())
	}
	t, ok := p.(T)
	if !ok {
		return zero, fmt.Errorf("integration %q (%s) does not implement the %s interface", name, spec.Driver, kind)
	}
	return t, nil
}
