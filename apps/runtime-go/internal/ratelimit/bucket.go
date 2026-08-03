// Package ratelimit bounds how often one client may start a run.
//
// P1 is the first phase that can spend real money, and the per-run budget cap
// bounds one question rather than a thousand. This is the other half: a
// per-client token bucket, plus a global ceiling on simultaneously-running
// loops so a burst cannot open unbounded provider connections.
//
// Ported from council's internal/ratelimit, with one change: time comes from
// an injected clock rather than time.Now, so the refill behaviour is testable
// without sleeping. council's version could only be tested by waiting.
package ratelimit

import (
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/clock"
)

// Limiter is a per-key token bucket.
//
// Keys are client identities. Buckets are created lazily and swept when idle,
// so a stream of unique keys — which is what a distributed burst looks like —
// cannot grow the map without bound.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket

	clk      clock.Clock
	capacity float64
	refill   float64 // tokens per second
	idleFor  time.Duration
	lastPass time.Time
}

type bucket struct {
	tokens float64
	seen   time.Time
}

// Config describes a limiter.
type Config struct {
	// Burst is how many runs a client may start back to back.
	Burst int
	// PerMinute is the sustained rate a client refills at.
	PerMinute float64
	// Clock is the time source. Required.
	Clock clock.Clock
	// IdleEviction is how long an untouched bucket survives. Zero takes a
	// sensible default.
	IdleEviction time.Duration
}

// DefaultIdleEviction is how long a silent client's bucket is remembered.
//
// Long enough that a normal user's bucket survives their think time, short
// enough that a scan across many source addresses does not accumulate state.
// Evicting a full bucket loses nothing: a fresh one starts full.
const DefaultIdleEviction = 10 * time.Minute

// New builds a limiter.
func New(cfg Config) (*Limiter, error) {
	if cfg.Clock == nil {
		return nil, errors.New("ratelimit: nil clock")
	}
	if cfg.Burst <= 0 {
		return nil, errors.New("ratelimit: burst must be positive")
	}
	if cfg.PerMinute <= 0 {
		return nil, errors.New("ratelimit: per-minute rate must be positive")
	}
	idle := cfg.IdleEviction
	if idle <= 0 {
		idle = DefaultIdleEviction
	}
	return &Limiter{
		buckets:  make(map[string]*bucket),
		clk:      cfg.Clock,
		capacity: float64(cfg.Burst),
		refill:   cfg.PerMinute / 60,
		idleFor:  idle,
		lastPass: cfg.Clock.Now(),
	}, nil
}

// Allow consumes one token for key, reporting whether it was available.
func (l *Limiter) Allow(key string) bool {
	now := l.clk.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	l.sweep(now)

	b, ok := l.buckets[key]
	if !ok {
		// A new client starts full, so their first request is never the one
		// that gets refused.
		b = &bucket{tokens: l.capacity}
		l.buckets[key] = b
	} else {
		elapsed := now.Sub(b.seen).Seconds()
		if elapsed > 0 {
			b.tokens = min(l.capacity, b.tokens+elapsed*l.refill)
		}
	}
	b.seen = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep drops buckets nobody has touched recently. Caller holds the lock.
//
// Amortised: at most once per eviction window, rather than on every call. A
// full scan per request would make the limiter's own cost scale with the
// number of clients it is protecting against.
func (l *Limiter) sweep(now time.Time) {
	if now.Sub(l.lastPass) < l.idleFor {
		return
	}
	l.lastPass = now
	for key, b := range l.buckets {
		if now.Sub(b.seen) >= l.idleFor {
			delete(l.buckets, key)
		}
	}
}

// Len reports how many buckets are held, for tests and diagnostics.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// ClientKey derives the rate-limit identity of a request.
//
// trustProxy decides whether X-Forwarded-For is believed. It is a header the
// client sets, so trusting it unconditionally lets anyone mint a fresh
// identity per request and defeat the limiter entirely; trusting it never
// makes every client behind a real proxy share one bucket. Only the operator
// knows which deployment this is, so it is configuration
// (SEXTANT_TRUST_PROXY) rather than a guess.
//
// The leftmost entry is taken because that is the original client; the entries
// after it are the proxies. Behind exactly one trusted proxy that is correct.
// Behind a chain, the leftmost is client-controlled again — which is why this
// is off by default.
func ClientKey(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			if first, _, found := strings.Cut(fwd, ","); found {
				return strings.TrimSpace(first)
			}
			return strings.TrimSpace(fwd)
		}
		if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
			return real
		}
	}
	// RemoteAddr carries a port, which changes on every connection. Keying on
	// it unsplit would give one client a new bucket per request.
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
