package orchestrator

import (
	"context"
	"errors"
	"testing"
	"time"
)

type nopSaver struct{}

func (nopSaver) Save(context.Context, string, *State) error { return nil }

func TestRunRetriesThenSucceeds(t *testing.T) {
	calls := 0
	steps := []Step{{Name: "a", Run: func(ctx context.Context, s *State) error {
		calls++
		if calls < 2 {
			return errors.New("transient")
		}
		return nil
	}}}
	st := NewState()
	if err := Run(context.Background(), "r1", steps, st, nopSaver{}, Options{Backoff: time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || st.Steps["a"].Status != Succeeded {
		t.Fatalf("calls=%d status=%s", calls, st.Steps["a"].Status)
	}
}

func TestRunResumeSkipsSucceeded(t *testing.T) {
	ran := map[string]int{}
	mk := func(name string, fail bool) Step {
		return Step{Name: name, Run: func(ctx context.Context, s *State) error {
			ran[name]++
			if fail {
				return errors.New("boom")
			}
			return nil
		}}
	}
	st := NewState()
	opt := Options{MaxAttempts: 1, Backoff: time.Millisecond}
	if err := Run(context.Background(), "r", []Step{mk("a", false), mk("b", true)}, st, nopSaver{}, opt); err == nil {
		t.Fatal("expected failure")
	}
	// Resume with the state as saved: no manual reset. The failed step must
	// really run again (it used up its attempts in the first Run call).
	if err := Run(context.Background(), "r", []Step{mk("a", false), mk("b", false)}, st, nopSaver{}, opt); err != nil {
		t.Fatal(err)
	}
	if ran["a"] != 1 {
		t.Fatalf("step a re-ran %d times", ran["a"])
	}
	if ran["b"] != 2 || st.Steps["b"].Status != Succeeded {
		t.Fatalf("step b ran %d times, status %s", ran["b"], st.Steps["b"].Status)
	}
	if got := Attempt(st, "b"); got != 2 {
		t.Fatalf("lifetime attempts = %d, want 2", got)
	}
}

// recordingSaver remembers the status of a step at each save.
type recordingSaver struct{ seen []Status }

func (r *recordingSaver) Save(_ context.Context, _ string, s *State) error {
	r.seen = append(r.seen, s.Steps["a"].Status)
	return nil
}

func TestStateIsSavedBeforeAnAttemptRuns(t *testing.T) {
	rs := &recordingSaver{}
	steps := []Step{{Name: "a", Run: func(ctx context.Context, s *State) error {
		if len(rs.seen) == 0 || rs.seen[len(rs.seen)-1] != Running {
			t.Error("state with status running must be persisted before the step runs")
		}
		return nil
	}}}
	if err := Run(context.Background(), "r", steps, NewState(), rs, Options{Backoff: time.Millisecond}); err != nil {
		t.Fatal(err)
	}
}

func TestStepSeesItsAttemptNumber(t *testing.T) {
	var seen []int
	steps := []Step{{Name: "a", Run: func(ctx context.Context, s *State) error {
		seen = append(seen, Attempt(s, "a"))
		if len(seen) < 3 {
			return errors.New("transient")
		}
		return nil
	}}}
	if err := Run(context.Background(), "r", steps, NewState(), nopSaver{}, Options{Backoff: time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 || seen[0] != 1 || seen[1] != 2 || seen[2] != 3 {
		t.Fatalf("attempt numbers: %v", seen)
	}
}
