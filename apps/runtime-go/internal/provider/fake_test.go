package provider

import (
	"context"
	"errors"
	"testing"
)

// drain collects a stream to completion. It exists so tests assert on whole
// streams rather than racing individual receives.
func drain(t *testing.T, ch <-chan StreamEvent) []StreamEvent {
	t.Helper()
	var got []StreamEvent
	for ev := range ch {
		got = append(got, ev)
	}
	return got
}

func TestFakeProviderStreams(t *testing.T) {
	// Pointers, not values: FakeProvider holds a mutex, so a table of values
	// would copy a lock on every iteration (go vet catches this).
	tests := []struct {
		name       string
		fake       *FakeProvider
		wantDeltas []string
		wantUsage  bool
		wantErr    bool
	}{
		{
			name:       "deltas only",
			fake:       &FakeProvider{Deltas: []string{"SELECT ", "1"}},
			wantDeltas: []string{"SELECT ", "1"},
		},
		{
			name:       "deltas then usage",
			fake:       &FakeProvider{Deltas: []string{"SELECT 1"}, Usage: &Usage{TokensIn: 10, TokensOut: 3}},
			wantDeltas: []string{"SELECT 1"},
			wantUsage:  true,
		},
		{
			name:       "mid-stream error after partial output",
			fake:       &FakeProvider{Deltas: []string{"SELE"}, Err: errors.New("upstream closed")},
			wantDeltas: []string{"SELE"},
			wantErr:    true,
		},
		{
			name: "no output at all",
			fake: &FakeProvider{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch, err := tt.fake.Stream(context.Background(), Request{Model: "m"})
			if err != nil {
				t.Fatalf("Stream() error = %v, want nil", err)
			}

			var deltas []string
			var sawUsage, sawErr bool
			for _, ev := range drain(t, ch) {
				switch {
				case ev.Err != nil:
					sawErr = true
				case ev.Usage != nil:
					sawUsage = true
				default:
					deltas = append(deltas, ev.Delta)
				}
			}

			if len(deltas) != len(tt.wantDeltas) {
				t.Fatalf("deltas = %q, want %q", deltas, tt.wantDeltas)
			}
			for i, w := range tt.wantDeltas {
				if deltas[i] != w {
					t.Errorf("delta[%d] = %q, want %q", i, deltas[i], w)
				}
			}
			if sawUsage != tt.wantUsage {
				t.Errorf("saw usage = %v, want %v", sawUsage, tt.wantUsage)
			}
			if sawErr != tt.wantErr {
				t.Errorf("saw error = %v, want %v", sawErr, tt.wantErr)
			}
		})
	}
}

// A construction failure must yield a nil channel, so a caller that forgets to
// check the error ranges over nil and blocks forever rather than silently
// treating "no stream" as "empty stream".
func TestFakeProviderConstructionError(t *testing.T) {
	want := errors.New("bad credentials")
	f := &FakeProvider{StreamErr: want}

	ch, err := f.Stream(context.Background(), Request{})
	if !errors.Is(err, want) {
		t.Fatalf("Stream() error = %v, want %v", err, want)
	}
	if ch != nil {
		t.Error("Stream() returned a non-nil channel alongside an error")
	}
	if len(f.Calls()) != 0 {
		t.Errorf("Calls() = %d, want 0 — a request that never started is not a call", len(f.Calls()))
	}
}

// The goroutine must observe cancellation rather than blocking on a send no
// one will receive. Without the ctx.Done() guard this test hangs, which is
// exactly the failure the guard prevents in the real provider.
func TestFakeProviderStopsOnCancel(t *testing.T) {
	f := &FakeProvider{Deltas: []string{"a", "b", "c", "d"}}
	ctx, cancel := context.WithCancel(context.Background())

	ch, err := f.Stream(ctx, Request{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	if ev, ok := <-ch; !ok || ev.Delta != "a" {
		t.Fatalf("first event = %+v (ok=%v), want delta %q", ev, ok, "a")
	}
	cancel()

	// Ranging to completion proves the channel closes. If the producer ignored
	// cancellation it would block on an unreceived send and this never returns.
	for range ch { //nolint:revive // draining is the assertion
	}
}

func TestFakeProviderRecordsCalls(t *testing.T) {
	f := &FakeProvider{Deltas: []string{"x"}}
	for _, model := range []string{"cheap", "strong"} {
		ch, err := f.Stream(context.Background(), Request{Model: model, Temperature: 0.7})
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		drain(t, ch)
	}

	calls := f.Calls()
	if len(calls) != 2 {
		t.Fatalf("Calls() = %d, want 2", len(calls))
	}
	if calls[0].Model != "cheap" || calls[1].Model != "strong" {
		t.Errorf("models = %q,%q, want cheap,strong", calls[0].Model, calls[1].Model)
	}

	// Calls returns a copy: mutating it must not corrupt the fake's history.
	calls[0].Model = "tampered"
	if f.Calls()[0].Model != "cheap" {
		t.Error("Calls() handed out the internal slice; a caller can rewrite call history")
	}
}
