package ratelimit

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/clock"
)

var epoch = time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

func newLimiter(t *testing.T, burst int, perMinute float64) (*Limiter, *clock.Fake) {
	t.Helper()
	clk := clock.NewFake(epoch)
	l, err := New(Config{Burst: burst, PerMinute: perMinute, Clock: clk})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return l, clk
}

func TestBurstThenRefusal(t *testing.T) {
	l, _ := newLimiter(t, 3, 60)

	for i := range 3 {
		if !l.Allow("client") {
			t.Fatalf("request %d was refused inside the burst", i+1)
		}
	}
	if l.Allow("client") {
		t.Error("a fourth request was allowed past a burst of 3")
	}
}

func TestRefillIsDrivenByTheClockNotBySleeping(t *testing.T) {
	// council's limiter could only be tested by waiting. With an injected
	// clock the refill curve is asserted exactly and the test is instant.
	l, clk := newLimiter(t, 2, 60) // one token per second

	l.Allow("client")
	l.Allow("client")
	if l.Allow("client") {
		t.Fatal("bucket was not exhausted")
	}

	clk.Advance(999 * time.Millisecond)
	if l.Allow("client") {
		t.Error("a token appeared before a full second had passed")
	}
	clk.Advance(time.Millisecond)
	if !l.Allow("client") {
		t.Error("no token after a full second of refill")
	}
}

func TestRefillDoesNotExceedTheBurst(t *testing.T) {
	l, clk := newLimiter(t, 2, 60)

	// An hour of silence must not bank an hour of requests.
	clk.Advance(time.Hour)

	if !l.Allow("client") || !l.Allow("client") {
		t.Fatal("the burst was not available after idling")
	}
	if l.Allow("client") {
		t.Error("idling banked more tokens than the burst allows")
	}
}

func TestClientsAreIndependent(t *testing.T) {
	l, _ := newLimiter(t, 1, 60)

	if !l.Allow("a") {
		t.Fatal("first client refused")
	}
	if !l.Allow("b") {
		t.Error("one client exhausting its bucket refused a different client")
	}
	if l.Allow("a") {
		t.Error("the first client got a second token")
	}
}

func TestANewClientStartsFull(t *testing.T) {
	l, _ := newLimiter(t, 1, 1)
	// A client whose first request is refused would see the service as broken.
	if !l.Allow("brand-new") {
		t.Error("a client's very first request was refused")
	}
}

func TestIdleBucketsAreEvicted(t *testing.T) {
	// A stream of unique keys is what a distributed burst looks like. Without
	// eviction the map grows without bound — the limiter becomes the memory
	// exhaustion it exists to prevent.
	clk := clock.NewFake(epoch)
	l, err := New(Config{Burst: 1, PerMinute: 60, Clock: clk, IdleEviction: time.Minute})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	for _, key := range []string{"a", "b", "c"} {
		l.Allow(key)
	}
	if l.Len() != 3 {
		t.Fatalf("Len() = %d, want 3", l.Len())
	}

	clk.Advance(2 * time.Minute)
	l.Allow("d") // any call triggers the amortised sweep

	if l.Len() > 1 {
		t.Errorf("Len() = %d after the eviction window, want the idle buckets dropped", l.Len())
	}
	// Evicting a full bucket loses nothing: a fresh one starts full.
	if !l.Allow("a") {
		t.Error("an evicted client was refused; eviction must not deny service")
	}
}

func TestConcurrentUseIsSafe(t *testing.T) {
	l, _ := newLimiter(t, 100, 6000)

	var wg sync.WaitGroup
	for i := range 50 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l.Allow("shared")
			l.Allow(string(rune('a' + i%5)))
		}(i)
	}
	wg.Wait()
}

func TestNewRejectsAnUnusableConfig(t *testing.T) {
	clk := clock.NewFake(epoch)
	for _, tt := range []struct {
		name string
		cfg  Config
	}{
		{name: "nil clock", cfg: Config{Burst: 1, PerMinute: 1}},
		{name: "zero burst", cfg: Config{PerMinute: 1, Clock: clk}},
		{name: "negative burst", cfg: Config{Burst: -1, PerMinute: 1, Clock: clk}},
		{name: "zero rate", cfg: Config{Burst: 1, Clock: clk}},
		{name: "negative rate", cfg: Config{Burst: 1, PerMinute: -1, Clock: clk}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.cfg); err == nil {
				t.Fatal("New() accepted an unusable config")
			}
		})
	}
}

func TestClientKey(t *testing.T) {
	tests := []struct {
		name       string
		remoteAddr string
		headers    map[string]string
		trustProxy bool
		want       string
	}{
		{
			// RemoteAddr's port changes on every connection. Keying on it
			// unsplit would hand one client a new bucket per request and
			// disable the limiter completely.
			name: "port is stripped", remoteAddr: "203.0.113.7:54321", want: "203.0.113.7",
		},
		{
			name: "ipv6 port is stripped", remoteAddr: "[2001:db8::1]:443", want: "2001:db8::1",
		},
		{
			// The header is client-set. Believing it without being told to
			// lets anyone mint a fresh identity per request.
			name:       "forwarded header ignored when proxies are not trusted",
			remoteAddr: "203.0.113.7:1", headers: map[string]string{"X-Forwarded-For": "1.1.1.1"},
			trustProxy: false, want: "203.0.113.7",
		},
		{
			name:       "forwarded header honoured when proxies are trusted",
			remoteAddr: "10.0.0.1:1", headers: map[string]string{"X-Forwarded-For": "1.1.1.1"},
			trustProxy: true, want: "1.1.1.1",
		},
		{
			// Leftmost is the original client; the rest are proxies.
			name:       "leftmost entry of a chain",
			remoteAddr: "10.0.0.1:1",
			headers:    map[string]string{"X-Forwarded-For": "1.1.1.1, 10.0.0.9, 10.0.0.8"},
			trustProxy: true, want: "1.1.1.1",
		},
		{
			name:       "x-real-ip as a fallback",
			remoteAddr: "10.0.0.1:1", headers: map[string]string{"X-Real-IP": "2.2.2.2"},
			trustProxy: true, want: "2.2.2.2",
		},
		{
			name:       "falls back to the socket when trusted headers are absent",
			remoteAddr: "10.0.0.1:1", trustProxy: true, want: "10.0.0.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/v1/questions", nil)
			r.RemoteAddr = tt.remoteAddr
			for k, v := range tt.headers {
				r.Header.Set(k, v)
			}
			if got := ClientKey(r, tt.trustProxy); got != tt.want {
				t.Errorf("ClientKey() = %q, want %q", got, tt.want)
			}
		})
	}
}
