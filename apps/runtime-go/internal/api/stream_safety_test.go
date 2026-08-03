package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/agent"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts/gen"
)

// The safety half of the stream suite: panic containment, run claiming,
// persistence that outlives a cancelled context, and the concurrency
// bound. Every test here covers something a review found or something a
// real process was observed doing. Helpers live in api_test.go.

func TestStreamSurvivesAPanicInTheLoop(t *testing.T) {
	// net/http recovers panics on the goroutine running a handler, but not on
	// one the handler spawned — and the loop runs on a spawned goroutine
	// because this one must stay free to drain and write. Without a recover
	// there, one question's bug takes the whole server down for every other
	// client. Verified against a real process before this test existed: it
	// exited with status 2.
	f := newFixture(t, func(_ *Deps, f *fixture) {
		f.runner.panicWith = "the loop blew up"
	})
	runID := f.startRun(t)

	rec := f.stream(t, runID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — the stream was already committed", rec.Code)
	}

	// The panic becomes a recorded error outcome rather than a dead process.
	run, err := f.store.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun() error = %v", err)
	}
	if run.Outcome != string(agent.OutcomeError) {
		t.Errorf("outcome = %q, want %q", run.Outcome, agent.OutcomeError)
	}
	if run.FinishedAt == nil {
		t.Error("a panicking run was never stamped finished")
	}
}

func TestTheServerKeepsServingAfterARunPanics(t *testing.T) {
	f := newFixture(t, func(_ *Deps, f *fixture) {
		f.runner.panicWith = "boom"
	})

	first := f.startRun(t)
	f.stream(t, first)

	// The next question must still work. This is the property that matters:
	// one client's bad run is not every client's outage.
	if code := f.ask(t, validQuestion).Code; code != http.StatusCreated {
		t.Errorf("a later question returned %d after an earlier run panicked", code)
	}
}

func TestStreamClaimsTheRunSoItCannotBeBilledTwice(t *testing.T) {
	// The LoadRun check is a courtesy that gives a clean 409 on the common
	// case. THIS is the one that holds: two concurrent requests can both pass
	// that check, and only one can win the conditional UPDATE. Without it, one
	// POSTed question can be streamed N times and billed N times.
	f := newFixture(t, func(_ *Deps, f *fixture) {
		f.runner.block = make(chan struct{})
		f.runner.started = make(chan struct{})
	})
	runID := f.startRun(t)

	var wg sync.WaitGroup
	codes := make([]int, 4)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = f.stream(t, runID).Code
		}(i)
	}
	// Let the winner reach the loop, then release it.
	<-f.runner.started
	close(f.runner.block)
	wg.Wait()

	if got := f.runner.callCount(); got != 1 {
		t.Errorf("the agent ran %d times for one question, want 1", got)
	}
	var ok int
	for _, c := range codes {
		if c == http.StatusOK {
			ok++
		}
	}
	if ok != 1 {
		t.Errorf("%d of 4 concurrent streams were served, want 1. codes: %v", ok, codes)
	}
}

func TestACappedRunIsPersistedEvenThoughItsContextIsDead(t *testing.T) {
	// The context carries the wall-clock cap, so it is already cancelled in
	// exactly the case where recording matters most. Persisting through it
	// meant the outcome write failed silently, the run row kept an empty
	// outcome, and the stream handler read that as "not started" — turning the
	// cap into an unbounded spend loop, and making deadline_exceeded a
	// recorded outcome that was never actually recorded.
	f := newFixture(t, func(d *Deps, f *fixture) {
		f.runner.result.Outcome = agent.OutcomeDeadlineExceeded
		f.runner.events = []gen.TraceEventV1{
			{Schema: "trace_event.v1", Type: gen.TraceEventV1TypeRunStarted},
			{Schema: "trace_event.v1", Type: gen.TraceEventV1TypeDeadlineExceeded},
		}
		// A cap so small the run context is dead before the loop returns.
		d.WallClock = time.Nanosecond
	})
	runID := f.startRun(t)
	f.stream(t, runID)

	run, err := f.store.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun() error = %v", err)
	}
	if run.Outcome != string(agent.OutcomeDeadlineExceeded) {
		t.Errorf("outcome = %q, want %q — a capped run must be recorded",
			run.Outcome, agent.OutcomeDeadlineExceeded)
	}
	if run.FinishedAt == nil {
		t.Error("a capped run was never stamped finished; it would be re-runnable")
	}
	// And it must not be startable again.
	if code := f.stream(t, runID).Code; code != http.StatusConflict {
		t.Errorf("a capped run could be streamed again (status %d)", code)
	}
}

func TestAskRequiresAJSONContentType(t *testing.T) {
	// Without this a text/plain POST is a CORS "simple request" needing no
	// preflight, so any page a visitor opens could create runs on a reachable
	// instance without the origin allowlist ever being consulted.
	f := newFixture(t)
	for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded", "multipart/form-data"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/questions", strings.NewReader(validQuestion))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		req.RemoteAddr = "203.0.113.7:1234"
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Errorf("Content-Type %q returned %d, want 415", ct, rec.Code)
		}
	}
	// Parameters and casing are legitimate and must still be accepted.
	for _, ct := range []string{"application/json", "application/json; charset=utf-8", "APPLICATION/JSON"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/questions", strings.NewReader(validQuestion))
		req.Header.Set("Content-Type", ct)
		req.RemoteAddr = "203.0.113.7:1234"
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated {
			t.Errorf("Content-Type %q returned %d, want 201", ct, rec.Code)
		}
	}
}

func TestAFailedStartReleasesTheClaim(t *testing.T) {
	// A 503 or a failed schema load must not burn the run: the claim marks it
	// started, and nothing else would ever finish it.
	f := newFixture(t, func(d *Deps, _ *fixture) {
		d.Schemas = fakeSchemas{err: context.DeadlineExceeded}
	})
	runID := f.startRun(t)

	if code := f.stream(t, runID).Code; code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", code)
	}
	run, err := f.store.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun() error = %v", err)
	}
	if run.Outcome != "" {
		t.Errorf("outcome = %q after a failed start, want it released for a retry", run.Outcome)
	}
}
