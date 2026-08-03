package api

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/agent"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/clock"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts/gen"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/dbreg"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/ratelimit"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/schema"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/trace"
)

// This package's tests stay GREEN while agent.Run is an open stub, because
// Deps.Agent is an interface and these drive a fake. That is what keeps the CI
// test job red on exactly the packages whose stubs are unwritten rather than on
// everything that transitively imports them.

var epoch = time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

// fakeRunner stands in for the agent loop.
type fakeRunner struct {
	events []gen.TraceEventV1
	result agent.Result
	err    error

	mu    sync.Mutex
	calls int
	block chan struct{} // when non-nil, Run waits on it before returning
}

func (f *fakeRunner) Run(ctx context.Context, _ agent.RunInput, emit agent.Emit) (agent.Result, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()

	for _, ev := range f.events {
		emit(ev)
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
		}
	}
	return f.result, f.err
}

func (f *fakeRunner) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeSchemas returns an empty schema without touching a database.
type fakeSchemas struct{ err error }

func (f fakeSchemas) Load(context.Context, dbreg.Database) (schema.Schema, error) {
	if f.err != nil {
		return schema.Schema{}, f.err
	}
	return schema.Schema{Tables: []schema.Table{{
		Name:    "orders",
		Columns: []schema.Column{{Name: "id", Type: "INTEGER", PrimaryKey: true}},
	}}}, nil
}

// memStore is an in-memory TraceStore.
type memStore struct {
	mu      sync.Mutex
	runs    map[string]trace.Run
	events  map[string][]trace.Event
	costs   map[string][]trace.CostEntry
	startEr error
}

func newMemStore() *memStore {
	return &memStore{
		runs:   map[string]trace.Run{},
		events: map[string][]trace.Event{},
		costs:  map[string][]trace.CostEntry{},
	}
}

func (m *memStore) StartRun(_ context.Context, run trace.Run) error {
	if m.startEr != nil {
		return m.startEr
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs[run.ID] = run
	return nil
}

func (m *memStore) FinishRun(_ context.Context, runID, outcome string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[runID]
	if !ok {
		return trace.ErrRunNotFound
	}
	run.Outcome = outcome
	run.FinishedAt = &at
	m.runs[runID] = run
	return nil
}

func (m *memStore) AppendEvent(_ context.Context, runID string, ev trace.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events[runID] = append(m.events[runID], ev)
	return nil
}

func (m *memStore) AppendCost(_ context.Context, runID string, e trace.CostEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.costs[runID] = append(m.costs[runID], e)
	return nil
}

func (m *memStore) LoadRun(_ context.Context, runID string) (trace.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[runID]
	if !ok {
		return trace.Run{}, trace.ErrRunNotFound
	}
	return run, nil
}

func (m *memStore) eventsFor(runID string) []trace.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]trace.Event(nil), m.events[runID]...)
}

type fixture struct {
	server *Server
	mux    *http.ServeMux
	runner *fakeRunner
	store  *memStore
	clock  *clock.Fake
}

func newFixture(t *testing.T, mutate ...func(*Deps, *fixture)) *fixture {
	t.Helper()
	clk := clock.NewFake(epoch)
	registry, err := dbreg.Parse("toy=sqlite:toy.sqlite")
	if err != nil {
		t.Fatalf("dbreg.Parse() error = %v", err)
	}
	limiter, err := ratelimit.New(ratelimit.Config{Burst: 100, PerMinute: 6000, Clock: clk})
	if err != nil {
		t.Fatalf("ratelimit.New() error = %v", err)
	}

	f := &fixture{
		runner: &fakeRunner{
			events: []gen.TraceEventV1{
				{Schema: "trace_event.v1", Type: gen.TraceEventV1TypeRunStarted},
				{Schema: "trace_event.v1", Type: gen.TraceEventV1TypeAnswered},
			},
			result: agent.Result{
				Outcome: agent.OutcomeAnswered,
				Ledger:  agent.NewLedger("r_x").Document(),
			},
		},
		store: newMemStore(),
		clock: clk,
	}

	deps := Deps{
		Agent: f.runner, Trace: f.store, Databases: registry,
		Schemas: fakeSchemas{}, Limiter: limiter, Clock: clk,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxConcurrentRuns: 4,
		BudgetUSD:         0.05,
		WallClock:         30 * time.Second,
		RowLimit:          500,
		StatementTimeout:  10 * time.Second,
	}
	for _, m := range mutate {
		m(&deps, f)
	}

	s, err := New(deps)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	f.server = s
	f.mux = http.NewServeMux()
	s.Routes(f.mux)
	return f
}

func (f *fixture) ask(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/questions", strings.NewReader(body))
	req.RemoteAddr = "203.0.113.7:1234"
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec
}

const validQuestion = `{"schema":"question_request.v1","question":"how many orders?","database":"toy"}`

func TestAskReturnsARunIDWithoutStartingTheLoop(t *testing.T) {
	f := newFixture(t)

	rec := f.ask(t, validQuestion)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201. body: %s", rec.Code, rec.Body)
	}

	var body struct {
		RunID  string `json:"run_id"`
		Events string `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if !strings.HasPrefix(body.RunID, "r_") {
		t.Errorf("run_id = %q, want an r_-prefixed id", body.RunID)
	}
	if body.Events != "/v1/runs/"+body.RunID+"/events" {
		t.Errorf("events = %q, want the stream path for this run", body.Events)
	}
	// The whole point of the two-endpoint split: the POST makes no provider
	// call and cannot spend money.
	if f.runner.callCount() != 0 {
		t.Error("POST /v1/questions started the agent loop")
	}
}

func TestAskGeneratesUnguessableRunIDs(t *testing.T) {
	f := newFixture(t)

	seen := map[string]bool{}
	for range 20 {
		var body struct {
			RunID string `json:"run_id"`
		}
		_ = json.Unmarshal(f.ask(t, validQuestion).Body.Bytes(), &body)
		if seen[body.RunID] {
			t.Fatalf("run id %q was issued twice", body.RunID)
		}
		seen[body.RunID] = true
		// A run id is the only thing protecting one client's stream from
		// another. A sequential or timestamp-derived id would let anyone
		// enumerate other people's questions and answers.
		if len(body.RunID) < 20 {
			t.Fatalf("run id %q is too short to be unguessable", body.RunID)
		}
	}
}

func TestAskRejectsAMalformedRequest(t *testing.T) {
	tests := []struct {
		name string
		body string
		want int
	}{
		{name: "not json", body: "{", want: http.StatusBadRequest},
		{name: "missing discriminator", body: `{"question":"q","database":"toy"}`, want: http.StatusBadRequest},
		{name: "empty question", body: `{"schema":"question_request.v1","question":"","database":"toy"}`, want: http.StatusBadRequest},
		{name: "unknown field", body: `{"schema":"question_request.v1","question":"q","database":"toy","x":1}`, want: http.StatusBadRequest},
		{name: "budget above the contract ceiling", body: `{"schema":"question_request.v1","question":"q","database":"toy","budget_usd":99}`, want: http.StatusBadRequest},
		{name: "database not a slug", body: `{"schema":"question_request.v1","question":"q","database":"../etc"}`, want: http.StatusBadRequest},
		{name: "unconfigured database", body: `{"schema":"question_request.v1","question":"q","database":"nope"}`, want: http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newFixture(t).ask(t, tt.body)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d. body: %s", rec.Code, tt.want, rec.Body)
			}
		})
	}
}

func TestAskDoesNotEchoTheRequestBack(t *testing.T) {
	// The contract validator's Error() quotes the offending instance values,
	// which for a request means reflecting the client's own input into the
	// response.
	f := newFixture(t)
	rec := f.ask(t, `{"schema":"question_request.v1","question":"q","database":"<script>alert(1)</script>"}`)

	if strings.Contains(rec.Body.String(), "script") {
		t.Errorf("response echoed the request: %s", rec.Body)
	}
}

func TestAskIsRateLimited(t *testing.T) {
	f := newFixture(t, func(d *Deps, f *fixture) {
		l, err := ratelimit.New(ratelimit.Config{Burst: 2, PerMinute: 60, Clock: f.clock})
		if err != nil {
			t.Fatalf("ratelimit.New() error = %v", err)
		}
		d.Limiter = l
	})

	for i := range 2 {
		if code := f.ask(t, validQuestion).Code; code != http.StatusCreated {
			t.Fatalf("request %d = %d, want 201", i+1, code)
		}
	}
	// The per-run budget bounds one question, not a thousand.
	if code := f.ask(t, validQuestion).Code; code != http.StatusTooManyRequests {
		t.Errorf("status = %d past the burst, want 429", code)
	}
}

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
	// second time for a question already answered.
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
	})

	first := f.startRun(t)
	second := f.startRun(t)

	started := make(chan struct{})
	go func() {
		close(started)
		f.stream(t, first)
	}()
	<-started

	// Wait for the first run to actually occupy the slot.
	deadline := time.Now().Add(2 * time.Second)
	for f.runner.callCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

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
