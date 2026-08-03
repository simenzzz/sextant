package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/agent"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/clock"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts/gen"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/dbreg"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/ratelimit"
)

// Tests for GET /v1/runs/{id}/events. The fixtures and doubles live in
// api_test.go; this file is the stream half, split out to keep both under
// the 400-line rule.

// startRun does the POST and returns the run id.
func (f *fixture) startRun(t *testing.T) string {
	t.Helper()
	var body struct {
		RunID string `json:"run_id"`
	}
	rec := f.ask(t, validQuestion)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ask returned %d: %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding ask response: %v", err)
	}
	return body.RunID
}

// deadlineRecorder is an httptest.ResponseRecorder that also supports write
// deadlines, as every real net/http writer does.
//
// Plain ResponseRecorder does not, and httpx.NewSSEStream refuses to construct
// over a writer whose writes cannot be bounded — deliberately, since a stream
// one slow reader can pin forever is worse than a failed request. Same shim as
// internal/httpx/sse_test.go, for the same reason.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
}

func newStreamRecorder() *deadlineRecorder {
	return &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
}

func (d *deadlineRecorder) SetWriteDeadline(time.Time) error { return nil }

func (f *fixture) stream(t *testing.T, runID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v1/runs/"+runID+"/events", nil)
	rec := newStreamRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec.ResponseRecorder
}

// sseFrames splits a raw SSE body into (event name, data) pairs.
func sseFrames(t *testing.T, body string) []struct{ Event, Data string } {
	t.Helper()
	var out []struct{ Event, Data string }
	var current struct{ Event, Data string }

	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if current.Data != "" {
				out = append(out, current)
			}
			current = struct{ Event, Data string }{}
		case strings.HasPrefix(line, "event: "):
			current.Event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			current.Data += strings.TrimPrefix(line, "data: ")
		}
	}
	return out
}

func TestStreamRunsTheLoopAndWritesTheTrace(t *testing.T) {
	f := newFixture(t)
	runID := f.startRun(t)

	rec := f.stream(t, runID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200. body: %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	// The run starts HERE, not on the POST — so no event can be emitted
	// before a reader exists.
	if f.runner.callCount() != 1 {
		t.Errorf("agent ran %d times, want 1", f.runner.callCount())
	}

	frames := sseFrames(t, rec.Body.String())
	if len(frames) < 3 {
		t.Fatalf("got %d frames, want the trace plus a ledger: %s", len(frames), rec.Body)
	}
}

func TestStreamStampsStepAndElapsedOnEveryEvent(t *testing.T) {
	f := newFixture(t)
	runID := f.startRun(t)
	rec := f.stream(t, runID)

	var steps []int
	for _, frame := range sseFrames(t, rec.Body.String()) {
		if frame.Event != "" {
			continue // the named result/ledger frames
		}
		var ev gen.TraceEventV1
		if err := json.Unmarshal([]byte(frame.Data), &ev); err != nil {
			t.Fatalf("decoding trace frame: %v", err)
		}
		steps = append(steps, ev.Step)
	}

	// step is a property of the stream, not of the transition. Consumers use
	// it to detect gaps, so it has to be monotonic from zero.
	for i, step := range steps {
		if step != i {
			t.Errorf("event %d carries step %d, want a monotonic index from 0", i, step)
		}
	}
}

func TestStreamEmitsNamedFramesForTheResultAndTheLedger(t *testing.T) {
	rows := &gen.ResultSetV1{
		Schema: "result_set.v1", RunId: "r_x",
		Columns: []gen.Column{{Name: "n"}}, Rows: [][]gen.Cell{{int64(2)}},
		RowCount: 1, Truncated: false,
	}
	f := newFixture(t, func(_ *Deps, f *fixture) {
		f.runner.result.Rows = rows
	})
	runID := f.startRun(t)

	rec := f.stream(t, runID)
	names := map[string]bool{}
	for _, frame := range sseFrames(t, rec.Body.String()) {
		names[frame.Event] = true
	}

	// Trace events on the default name so EventSource's onmessage sees them;
	// the result and ledger named, because they are different contracts the
	// client validates against different schemas.
	for _, want := range []string{"", "result_set", "cost_ledger"} {
		if !names[want] {
			t.Errorf("no frame named %q. got %v", want, names)
		}
	}
}

func TestStreamPersistsWhatItSends(t *testing.T) {
	f := newFixture(t)
	runID := f.startRun(t)
	f.stream(t, runID)

	stored := f.store.eventsFor(runID)
	if len(stored) != len(f.runner.events) {
		t.Fatalf("persisted %d events, streamed %d", len(stored), len(f.runner.events))
	}
	// The stored trace and the streamed one are written by the same goroutine
	// from the same value, so they agree by construction.
	for i, ev := range stored {
		if ev.Step != i {
			t.Errorf("stored event %d carries step %d", i, ev.Step)
		}
	}
}

func TestStreamRecordsTheOutcome(t *testing.T) {
	f := newFixture(t)
	runID := f.startRun(t)
	f.stream(t, runID)

	run, err := f.store.LoadRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("LoadRun() error = %v", err)
	}
	if run.Outcome != string(agent.OutcomeAnswered) {
		t.Errorf("outcome = %q, want %q", run.Outcome, agent.OutcomeAnswered)
	}
	if run.FinishedAt == nil {
		t.Error("run was not stamped finished")
	}
}

func TestStreamRecordsACappedRunAsItsOutcomeNotAnError(t *testing.T) {
	f := newFixture(t, func(_ *Deps, f *fixture) {
		f.runner.result.Outcome = agent.OutcomeBudgetExhausted
		f.runner.events = []gen.TraceEventV1{
			{Schema: "trace_event.v1", Type: gen.TraceEventV1TypeRunStarted},
			{Schema: "trace_event.v1", Type: gen.TraceEventV1TypeBudgetExhausted},
		}
	})
	runID := f.startRun(t)

	rec := f.stream(t, runID)
	// A cap is a recorded result. The response is a normal 200 stream, and the
	// UI must not render it as a failure.
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d for a capped run, want 200", rec.Code)
	}
	run, _ := f.store.LoadRun(context.Background(), runID)
	if run.Outcome != string(agent.OutcomeBudgetExhausted) {
		t.Errorf("outcome = %q, want budget_exhausted", run.Outcome)
	}
}

func TestStreamRefusesAnUnknownRun(t *testing.T) {
	f := newFixture(t)
	if code := f.stream(t, "r_does_not_exist").Code; code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", code)
	}
}

func TestStreamRefusesToReplayAFinishedRun(t *testing.T) {
	f := newFixture(t)
	runID := f.startRun(t)
	f.stream(t, runID)

	// Streaming again would mean running again, which would spend money a
	// second time for a question already asked.
	if code := f.stream(t, runID).Code; code != http.StatusConflict {
		t.Errorf("status = %d on a second stream, want 409", code)
	}
	if f.runner.callCount() != 1 {
		t.Errorf("agent ran %d times, want 1", f.runner.callCount())
	}
}

func TestStreamBoundsConcurrentRuns(t *testing.T) {
	f := newFixture(t, func(d *Deps, f *fixture) {
		d.MaxConcurrentRuns = 1
		f.runner.block = make(chan struct{})
		f.runner.started = make(chan struct{})
	})

	first := f.startRun(t)
	second := f.startRun(t)

	go func() { f.stream(t, first) }()
	// Wait on a signal rather than polling a clock.
	<-f.runner.started

	// Bounding concurrent loops bounds concurrent provider connections, which
	// is the thing that actually costs money.
	if code := f.stream(t, second).Code; code != http.StatusServiceUnavailable {
		t.Errorf("status = %d with the only slot taken, want 503", code)
	}
	close(f.runner.block)
}

func TestStreamFailsBeforeCommittingToAStreamWhenTheSchemaCannotBeRead(t *testing.T) {
	f := newFixture(t, func(d *Deps, _ *fixture) {
		d.Schemas = fakeSchemas{err: context.DeadlineExceeded}
	})
	runID := f.startRun(t)

	rec := f.stream(t, runID)
	// Everything that can still answer with a status code must do so before
	// NewSSEStream commits the response.
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct == "text/event-stream" {
		t.Error("the response was committed to a stream before the schema load failed")
	}
}

func TestNewRefusesAnIncompleteDependencySet(t *testing.T) {
	clk := clock.NewFake(epoch)
	registry, _ := dbreg.Parse("toy=sqlite:t.sqlite")
	limiter, _ := ratelimit.New(ratelimit.Config{Burst: 1, PerMinute: 1, Clock: clk})
	full := Deps{
		Agent: &fakeRunner{}, Trace: newMemStore(), Databases: registry,
		Schemas: fakeSchemas{}, Limiter: limiter, Clock: clk, MaxConcurrentRuns: 1,
	}
	without := func(f func(*Deps)) Deps {
		d := full
		f(&d)
		return d
	}

	for _, tt := range []struct {
		name string
		deps Deps
	}{
		{name: "no agent", deps: without(func(d *Deps) { d.Agent = nil })},
		{name: "no trace store", deps: without(func(d *Deps) { d.Trace = nil })},
		{name: "no registry", deps: without(func(d *Deps) { d.Databases = nil })},
		{name: "no schema loader", deps: without(func(d *Deps) { d.Schemas = nil })},
		{name: "no limiter", deps: without(func(d *Deps) { d.Limiter = nil })},
		{name: "no clock", deps: without(func(d *Deps) { d.Clock = nil })},
		{name: "no concurrency bound", deps: without(func(d *Deps) { d.MaxConcurrentRuns = 0 })},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.deps); err == nil {
				t.Fatal("New() accepted an incomplete dependency set")
			}
		})
	}
}
