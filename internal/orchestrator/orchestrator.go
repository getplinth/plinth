// Package orchestrator runs a request's steps in order. Steps are idempotent;
// state is persisted after every step so a failed request can be resumed
// without repeating steps that already succeeded.
package orchestrator

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type Status string

const (
	Pending   Status = "pending"
	Running   Status = "running"
	Succeeded Status = "succeeded"
	Failed    Status = "failed"
)

// StepName builds a stable step key such as "cluster/shop/dev"
// (kind, project, environment). Stable names are what make resume work.
func StepName(parts ...string) string { return strings.Join(parts, "/") }

// Step is one unit of provisioning work.
type Step struct {
	Name string
	// Run performs the work. It must be safe to call repeatedly. It may read
	// outputs of earlier steps from state and add its own.
	Run func(ctx context.Context, state *State) error
}

// StepResult is the persisted outcome of a step.
type StepResult struct {
	Status   Status `json:"status"`
	Error    string `json:"error,omitempty"`
	Attempts int    `json:"attempts"`
}

// State is the persisted progress of one request.
type State struct {
	Steps   map[string]StepResult `json:"steps"`
	Outputs map[string]string     `json:"outputs"`
}

func NewState() *State {
	return &State{Steps: map[string]StepResult{}, Outputs: map[string]string{}}
}

// Attempt returns how many times the named step has been started in total,
// including the current one while it runs. A step that issues a credential
// treats Attempt > 1 as "an earlier attempt may have issued it without
// delivering it" and rotates instead of assuming the credential exists.
func Attempt(st *State, name string) int { return st.Steps[name].Attempts }

// Saver persists state after each step. Implemented by the store package.
type Saver interface {
	Save(ctx context.Context, requestID string, s *State) error
}

type Options struct {
	MaxAttempts int           // per step; default 3
	Backoff     time.Duration // between attempts; default 2s
}

// Run executes steps in order, skipping those already succeeded.
func Run(ctx context.Context, requestID string, steps []Step, st *State, saver Saver, opt Options) error {
	if opt.MaxAttempts == 0 {
		opt.MaxAttempts = 3
	}
	for _, step := range steps {
		if st.Steps[step.Name].Status == Succeeded {
			continue
		}
		var err error
		res := st.Steps[step.Name]
		// MaxAttempts bounds attempts per Run call. res.Attempts is the
		// lifetime total across resumes: steps use it to tell "first ever
		// attempt" from "a previous attempt may have partly happened".
		for try := 0; try < opt.MaxAttempts; try++ {
			res.Attempts++
			res.Status = Running
			st.Steps[step.Name] = res
			// Persist before running so a crash mid-step leaves a record that
			// the step was attempted (see Attempt).
			if serr := saver.Save(ctx, requestID, st); serr != nil {
				return fmt.Errorf("saving state before %q: %w", step.Name, serr)
			}
			if err = step.Run(ctx, st); err == nil {
				break
			}
			res = st.Steps[step.Name]
			res.Error = err.Error()
			st.Steps[step.Name] = res
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(opt.Backoff):
			}
		}
		if err != nil {
			res.Status = Failed
			st.Steps[step.Name] = res
			_ = saver.Save(ctx, requestID, st)
			return fmt.Errorf("step %q failed after %d attempts: %w", step.Name, res.Attempts, err)
		}
		res.Status, res.Error = Succeeded, ""
		st.Steps[step.Name] = res
		if err := saver.Save(ctx, requestID, st); err != nil {
			return fmt.Errorf("saving state after %q: %w", step.Name, err)
		}
	}
	return nil
}
