package api

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// limiters hands out one token bucket per user for expensive operations
// (submitting and retrying requests).
type limiters struct {
	mu    sync.Mutex
	m     map[string]*rate.Limiter
	burst int
	every time.Duration
}

// Defaults: a burst of 5, refilled at 10 per minute.
const (
	defaultBurst     = 5
	defaultPerMinute = 10
)

func newLimiters(burst, perMinute int) limiters {
	if burst <= 0 {
		burst = defaultBurst
	}
	if perMinute <= 0 {
		perMinute = defaultPerMinute
	}
	return limiters{m: map[string]*rate.Limiter{}, burst: burst, every: time.Minute / time.Duration(perMinute)}
}

// allow reports whether the user may perform another expensive operation.
func (l *limiters) allow(userID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	lim, ok := l.m[userID]
	if !ok {
		lim = rate.NewLimiter(rate.Every(l.every), l.burst)
		l.m[userID] = lim
	}
	return lim.Allow()
}
