package provider

import (
	"context"
	"slices"
	"sync"
)

// FakeProvider is a deterministic Provider double.
//
// It lives in package provider rather than in a _test.go file so every other
// package — the agent loop, the router, the eval harness — can drive its own
// tests against the same double instead of each growing a private one. The
// tradeoff is that it ships in the binary; promote it to an
// internal/provider/providertest subpackage (the net/http/httptest idiom) once
// an external consumer exists.
//
// It mirrors the real streaming shape exactly: one goroutine, a channel closed
// exactly once, and ctx.Done() checked before every send. A fake that skips
// that discipline lets the code under test pass while the real provider
// deadlocks it.
type FakeProvider struct {
	// Deltas are emitted in order, one StreamEvent each.
	Deltas []string
	// Usage, when non-nil, is emitted as the final event before close —
	// matching a provider that reports what it charged.
	Usage *Usage
	// Err, when non-nil, is emitted as a terminal event after the deltas.
	// Use it to exercise mid-stream failure.
	Err error
	// StreamErr, when non-nil, is returned from Stream itself with a nil
	// channel. Use it to exercise a request that never started.
	StreamErr error

	mu    sync.Mutex
	calls []Request
}

var _ Provider = (*FakeProvider)(nil)

// Stream implements Provider.
func (f *FakeProvider) Stream(ctx context.Context, req Request) (<-chan StreamEvent, error) {
	if f.StreamErr != nil {
		return nil, f.StreamErr
	}

	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()

	out := make(chan StreamEvent)
	go func() {
		defer close(out)
		for _, d := range f.Deltas {
			select {
			case <-ctx.Done():
				return
			case out <- StreamEvent{Delta: d}:
			}
		}
		if f.Usage != nil {
			select {
			case <-ctx.Done():
				return
			case out <- StreamEvent{Usage: f.Usage}:
			}
		}
		if f.Err != nil {
			select {
			case <-ctx.Done():
				return
			case out <- StreamEvent{Err: f.Err}:
			}
		}
	}()
	return out, nil
}

// Calls returns a deep copy of the requests this fake has received, in order.
//
// Deep, not shallow: a shallow copy would share the Messages backing array, so
// a caller inspecting call history could rewrite the prompt a previous call
// was made with — and a test asserting on that history would then be asserting
// on something it had itself modified.
func (f *FakeProvider) Calls() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]Request, len(f.calls))
	for i, c := range f.calls {
		c.Messages = slices.Clone(c.Messages)
		out[i] = c
	}
	return out
}
