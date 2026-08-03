package api

import (
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

	mu        sync.Mutex
	calls     int
	block     chan struct{} // when non-nil, Run waits on it before returning
	panicWith string        // when non-empty, Run panics with it
	// started is closed the first time Run is entered. A signal, not a
	// polled deadline: the house rule is no sleeps and no wall-clock in
	// tests, and a poll against time.Now is flaky on a loaded runner.
	started     chan struct{}
	startedOnce sync.Once
}

func (f *fakeRunner) Run(ctx context.Context, _ agent.RunInput, emit agent.Emit) (agent.Result, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if f.started != nil {
		f.startedOnce.Do(func() { close(f.started) })
	}

	if f.panicWith != "" {
		panic(f.panicWith)
	}
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

func (m *memStore) ClaimRun(_ context.Context, runID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	run, ok := m.runs[runID]
	if !ok {
		return false, trace.ErrRunNotFound
	}
	if run.Outcome != "" {
		return false, nil
	}
	run.Outcome = trace.OutcomeRunning
	m.runs[runID] = run
	return true, nil
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
	req.Header.Set("Content-Type", "application/json")
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
