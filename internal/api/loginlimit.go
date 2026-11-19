package api

import (
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// loginLimiter slows password guessing. It keeps one token bucket per username
// (so one account cannot be attacked quickly, whatever the sender's address) and
// one per remote address (so one machine cannot try many accounts). Unknown
// usernames get buckets too, so the limit does not reveal which exist. The maps
// are emptied when they grow large, which only ever makes the limit more lenient
// for a moment, never stricter for real people.
type loginLimiter struct {
	mu                   sync.Mutex
	byName               map[string]*rate.Limiter
	byAddr               map[string]*rate.Limiter
	maxKeys              int
	nameBurst, addrBurst int
}

const (
	loginPerNameBurst = 10 // then one attempt every 6 seconds
	loginPerAddrBurst = 30 // then one attempt every 2 seconds
)

// newLoginLimiter uses the default bursts unless burst is positive.
func newLoginLimiter(burst int) *loginLimiter {
	l := &loginLimiter{byName: map[string]*rate.Limiter{}, byAddr: map[string]*rate.Limiter{}, maxKeys: 20000,
		nameBurst: loginPerNameBurst, addrBurst: loginPerAddrBurst}
	if burst > 0 {
		l.nameBurst, l.addrBurst = burst, burst*3
	}
	return l
}

func bucket(m map[string]*rate.Limiter, key string, every time.Duration, burst, max int) *rate.Limiter {
	if l, ok := m[key]; ok {
		return l
	}
	if len(m) >= max {
		clear(m)
	}
	l := rate.NewLimiter(rate.Every(every), burst)
	m[key] = l
	return l
}

// allow reports whether this username from this address may try a password now.
func (l *loginLimiter) allow(username, addr string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	byName := bucket(l.byName, username, 6*time.Second, l.nameBurst, l.maxKeys).Allow()
	byAddr := bucket(l.byAddr, addr, 2*time.Second, l.addrBurst, l.maxKeys).Allow()
	return byName && byAddr
}

// remoteAddr is the peer address. Behind a reverse proxy it is the proxy's, so
// the per-address bucket is then shared by everyone and the per-username one
// does the real work; forwarding headers are not trusted, since anyone can send them.
func remoteAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
