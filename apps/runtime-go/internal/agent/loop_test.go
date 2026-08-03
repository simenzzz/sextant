package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/clock"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts/gen"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/executor"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/provider"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/schema"
)

// These tests fail on a panic until Run is written.

// fakeParser returns a canned summary without a sidecar.
type fakeParser struct {
	summary gen.ParseSummaryV1
	err     error
	calls   int
}

func (f *fakeParser) Parse(_ context.Context, _ string, _ gen.SqlPlanV1Dialect, _ int) (gen.ParseSummaryV1, error) {
	f.calls++
	if f.err != nil {
		return gen.ParseSummaryV1{}, f.err
	}
	return f.summary, nil
}

func okSummary(sql string, tables ...string) gen.ParseSummaryV1 {
	if len(tables) == 0 {
		tables = []string{"orders"}
	}
	hasLimit := false
	return gen.ParseSummaryV1{
		Schema:         "parse_summary.v1",
		Ok:             true,
		Dialect:        gen.ParseSummaryV1DialectSqlite,
		StatementCount: 1,
		NodeKinds:      []string{"Select", "From", "Table", "Identifier", "Count", "Limit"},
		Tables:         tables,
		Functions:      []string{"count"},
		HasLimit:       &hasLimit,
		NormalizedSql:  &sql,
		LimitInjected:  true,
	}
}

func toyExecutor(t *testing.T, clk clock.Clock) *executor.Executor {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..")
	db, err := executor.OpenSQLiteReadOnly(filepath.Join(root, "infra", "fixtures", "toy.sqlite"))
	if err != nil {
		t.Fatalf("opening the toy fixture: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	e, err := executor.New(db, clk, executor.DefaultMaxResultBytes)
	if err != nil {
		t.Fatalf("executor.New() error = %v", err)
	}
	return e
}

func toySchema(t *testing.T, clk clock.Clock) schema.Schema {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..")
	db, err := executor.OpenSQLiteReadOnly(filepath.Join(root, "infra", "fixtures", "toy.sqlite"))
	if err != nil {
		t.Fatalf("opening the toy fixture: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	s, err := schema.IntrospectSQLite(context.Background(), db)
	if err != nil {
		t.Fatalf("introspecting the toy fixture: %v", err)
	}
	return s
}

// recorder collects the trace a run emitted.
type recorder struct{ events []gen.TraceEventV1 }

func (r *recorder) emit(ev gen.TraceEventV1) { r.events = append(r.events, ev) }

func (r *recorder) types() []string {
	out := make([]string, len(r.events))
	for i, ev := range r.events {
		out[i] = string(ev.Type)
	}
	return out
}

func (r *recorder) has(t string) bool {
	for _, ev := range r.events {
		if string(ev.Type) == t {
			return true
		}
	}
	return false
}

func (r *recorder) find(t string) (gen.TraceEventV1, bool) {
	for _, ev := range r.events {
		if string(ev.Type) == t {
			return ev, true
		}
	}
	return gen.TraceEventV1{}, false
}

const cancelledSQL = "SELECT COUNT(*) AS cancelled FROM orders WHERE status = 'C' LIMIT 500"

type harness struct {
	agent    *Agent
	provider *provider.FakeProvider
	parser   *fakeParser
	clock    *clock.Fake
	input    RunInput
	rec      *recorder
}

func newHarness(t *testing.T, mutate ...func(*harness)) *harness {
	t.Helper()
	clk := clock.NewFake(epoch)

	h := &harness{
		provider: &provider.FakeProvider{
			Deltas: []string{"```sql\n", cancelledSQL, "\n```"},
			Usage:  &provider.Usage{TokensIn: 400, TokensOut: 25},
		},
		parser: &fakeParser{summary: okSummary(cancelledSQL)},
		clock:  clk,
		rec:    &recorder{},
	}
	for _, m := range mutate {
		m(h)
	}

	a, err := New(Deps{
		Provider:   h.provider,
		Parser:     h.parser,
		Executor:   toyExecutor(t, clk),
		Clock:      clk,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		CheapModel: "claude-haiku-4-5",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	h.agent = a
	h.input = RunInput{
		RunID:            "r_test",
		Question:         "how many cancelled orders are there?",
		Schema:           toySchema(t, clk),
		Dialect:          gen.SqlPlanV1DialectSqlite,
		Caps:             Caps{MaxRepairDepth: 0, MaxUSD: 0.05, WallClock: 30 * time.Second},
		RowLimit:         500,
		StatementTimeout: 10 * time.Second,
	}
	return h
}

func TestRunAnswersAQuestionEndToEnd(t *testing.T) {
	h := newHarness(t)

	result, err := h.agent.Run(context.Background(), h.input, h.rec.emit)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Outcome != OutcomeAnswered {
		t.Fatalf("Outcome = %q, want %q. trace: %v", result.Outcome, OutcomeAnswered, h.rec.types())
	}
	if result.Rows == nil {
		t.Fatal("Result carries no rows")
	}
	if result.Rows.RowCount != 1 {
		t.Errorf("RowCount = %d, want 1", result.Rows.RowCount)
	}
	// The toy fixture has exactly two cancelled orders.
	if n, ok := result.Rows.Rows[0][0].(int64); !ok || n != 2 {
		t.Errorf("answer = %v, want int64(2)", result.Rows.Rows[0][0])
	}
}

func TestRunEmitsTheWholeTransitionSequence(t *testing.T) {
	h := newHarness(t)

	if _, err := h.agent.Run(context.Background(), h.input, h.rec.emit); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	// The eval and the UI both read this stream. A phase that silently skips a
	// transition makes P5's version look like a new behaviour rather than a
	// better one.
	for _, want := range []string{
		"run_started", "retrieved", "generating", "generated",
		"validated", "executing", "executed", "answered",
	} {
		if !h.rec.has(want) {
			t.Errorf("no %q event. trace: %v", want, h.rec.types())
		}
	}
}

func TestRunEmitsExactlyOneTerminalEvent(t *testing.T) {
	h := newHarness(t)
	if _, err := h.agent.Run(context.Background(), h.input, h.rec.emit); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	terminal := map[string]bool{
		"answered": true, "abstained": true, "error": true,
		"budget_exhausted": true, "depth_exhausted": true, "deadline_exceeded": true,
	}
	var count int
	for _, ty := range h.rec.types() {
		if terminal[ty] {
			count++
		}
	}
	// The reducer treats the first terminal type it sees as the outcome; a
	// second after it is a timeline that contradicts itself.
	if count != 1 {
		t.Errorf("emitted %d terminal events, want exactly 1. trace: %v", count, h.rec.types())
	}
}

func TestRunEmitsConformingTraceEvents(t *testing.T) {
	h := newHarness(t)
	if _, err := h.agent.Run(context.Background(), h.input, h.rec.emit); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	for i, ev := range h.rec.events {
		raw, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("marshalling event %d: %v", i, err)
		}
		// Every one of these goes on the wire and is validated in the browser.
		if err := contracts.Validate(contracts.TraceEventV1, raw); err != nil {
			t.Errorf("event %d (%s) violates trace_event.v1: %v\n%s", i, ev.Type, err, raw)
		}
	}
}

func TestRunShowsThePostGuardStatementNotTheGeneration(t *testing.T) {
	// The SQL panel renders `validated`. It must show what actually ran: the
	// two differ whenever the guard injected or clamped a LIMIT, and showing
	// the generation would mean the panel and the database disagree.
	const generated = "SELECT COUNT(*) AS cancelled FROM orders WHERE status = 'C'"
	h := newHarness(t, func(h *harness) {
		h.provider.Deltas = []string{"```sql\n", generated, "\n```"}
		h.parser.summary = okSummary(generated + " LIMIT 500")
	})

	if _, err := h.agent.Run(context.Background(), h.input, h.rec.emit); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	ev, ok := h.rec.find("validated")
	if !ok {
		t.Fatal("no validated event")
	}
	if ev.Sql == nil || *ev.Sql != generated+" LIMIT 500" {
		t.Errorf("validated.sql = %v, want the post-guard statement", ev.Sql)
	}
}

func TestRunRecordsTheLedgerFromProviderReportedUsage(t *testing.T) {
	h := newHarness(t)

	result, err := h.agent.Run(context.Background(), h.input, h.rec.emit)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if result.Ledger.Totals.TokensIn != 400 || result.Ledger.Totals.TokensOut != 25 {
		t.Errorf("ledger totals = %d in / %d out, want the provider-reported 400/25",
			result.Ledger.Totals.TokensIn, result.Ledger.Totals.TokensOut)
	}
	if result.Ledger.Totals.Usd <= 0 {
		t.Error("ledger priced the run at zero despite a reported token spend")
	}
	if result.Ledger.Totals.StepsCostUnknown != 0 {
		t.Errorf("StepsCostUnknown = %d, want 0 when usage was reported",
			result.Ledger.Totals.StepsCostUnknown)
	}

	raw, err := json.Marshal(result.Ledger)
	if err != nil {
		t.Fatalf("marshalling ledger: %v", err)
	}
	if err := contracts.Validate(contracts.CostLedgerV1, raw); err != nil {
		t.Fatalf("cost_ledger.v1 validation failed: %v\n%s", err, raw)
	}
}

func TestRunRecordsAnUnknownCostWhenTheProviderReportedNoUsage(t *testing.T) {
	// The runtime never estimates. A step nobody reported is one the ledger
	// declines to price, and it says so rather than recording zero.
	h := newHarness(t, func(h *harness) { h.provider.Usage = nil })

	result, err := h.agent.Run(context.Background(), h.input, h.rec.emit)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Ledger.Totals.StepsCostUnknown == 0 {
		t.Error("a step with no reported usage was recorded as free rather than unknown")
	}
}

func TestRunEndsAtTheBudgetCapAsARecordedOutcome(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		// Enough tokens that a single generation crosses the cap.
		h.provider.Usage = &provider.Usage{TokensIn: 10_000_000, TokensOut: 10_000_000}
	})

	result, err := h.agent.Run(context.Background(), h.input, h.rec.emit)

	// A cap is a result, not an error. The eval reports its rate and the UI
	// must not render it as a failure.
	if err != nil {
		t.Fatalf("Run() returned an error for a capped run: %v", err)
	}
	if result.Outcome != OutcomeBudgetExhausted {
		t.Fatalf("Outcome = %q, want %q. trace: %v", result.Outcome, OutcomeBudgetExhausted, h.rec.types())
	}
	if !h.rec.has("budget_exhausted") {
		t.Errorf("no budget_exhausted event. trace: %v", h.rec.types())
	}
	// The run that cost the most is exactly the run whose cost most needs
	// recording.
	if result.Ledger.Totals.Usd <= 0 {
		t.Error("the ledger dropped the charge that tripped the cap")
	}
}

func TestRunEndsAtTheDeadlineAsARecordedOutcome(t *testing.T) {
	h := newHarness(t)
	// The fake provider advances nothing on its own, so the test drives the
	// clock past the cap while the run is in flight — no sleeping.
	h.provider.OnStream = func() { h.clock.Advance(31 * time.Second) }

	result, err := h.agent.Run(context.Background(), h.input, h.rec.emit)
	if err != nil {
		t.Fatalf("Run() returned an error for a run that ran out of time: %v", err)
	}
	if result.Outcome != OutcomeDeadlineExceeded {
		t.Fatalf("Outcome = %q, want %q. trace: %v",
			result.Outcome, OutcomeDeadlineExceeded, h.rec.types())
	}
}

func TestRunStopsAtAGuardRejectionWithoutExecuting(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		// A DROP that reached the parser. The guard must refuse it, and
		// nothing may execute afterwards.
		h.parser.summary = gen.ParseSummaryV1{
			Schema: "parse_summary.v1", Ok: true,
			Dialect: gen.ParseSummaryV1DialectSqlite, StatementCount: 1,
			NodeKinds: []string{"Drop", "Table", "Identifier"},
			Tables:    []string{"orders"}, Functions: []string{},
		}
	})

	result, err := h.agent.Run(context.Background(), h.input, h.rec.emit)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if h.rec.has("executing") || h.rec.has("executed") {
		t.Errorf("a rejected statement reached the executor. trace: %v", h.rec.types())
	}
	if !h.rec.has("rejected") {
		t.Errorf("no rejected event. trace: %v", h.rec.types())
	}
	if result.Rows != nil {
		t.Error("a rejected run returned rows")
	}
	ev, _ := h.rec.find("rejected")
	if ev.FailureKind == nil || *ev.FailureKind != gen.TraceEventV1FailureKindGuardRejected {
		t.Errorf("rejected.failure_kind = %v, want guard_rejected", ev.FailureKind)
	}
}

func TestRunFailsClosedWhenTheParserIsUnavailable(t *testing.T) {
	// The sidecar being down must look nothing like the sidecar approving a
	// statement, and it must be distinguishable in the trace from a guard
	// rejection — the eval separates "our sidecar was down" from "the model
	// wrote a DROP".
	h := newHarness(t, func(h *harness) {
		h.parser.err = errors.New("guard: the SQL parser is unavailable")
	})

	result, err := h.agent.Run(context.Background(), h.input, h.rec.emit)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if h.rec.has("executing") || h.rec.has("executed") {
		t.Errorf("a statement executed while the parser was down. trace: %v", h.rec.types())
	}
	if result.Rows != nil {
		t.Error("a run with no working parser returned rows")
	}
	ev, ok := h.rec.find("error")
	if !ok {
		t.Fatalf("no error event. trace: %v", h.rec.types())
	}
	if ev.FailureKind == nil || *ev.FailureKind != gen.TraceEventV1FailureKindInternalError {
		t.Errorf("failure_kind = %v, want internal_error — a parser outage is not a "+
			"guard rejection and the eval counts them separately", ev.FailureKind)
	}
}

func TestRunHandlesAGenerationWithNoSQL(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.provider.Deltas = []string{"I'm not sure how to answer that."}
	})

	result, err := h.agent.Run(context.Background(), h.input, h.rec.emit)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Outcome == OutcomeAnswered {
		t.Errorf("a generation with no SQL was recorded as answered. trace: %v", h.rec.types())
	}
	if h.parser.calls != 0 {
		t.Error("prose was sent to the parser; extraction should have refused first")
	}
}

func TestRunDrainsTheProviderStreamOnAMidStreamError(t *testing.T) {
	// Returning early leaves the provider goroutine blocked forever on a send
	// nobody will receive. -race and a leaked goroutine are how that surfaces.
	h := newHarness(t, func(h *harness) {
		h.provider.Err = errors.New("provider: rate limited by the model provider")
	})

	result, err := h.agent.Run(context.Background(), h.input, h.rec.emit)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Outcome == OutcomeAnswered {
		t.Error("a failed generation was recorded as answered")
	}
	// The usage that arrived before the error is what makes the ledger honest
	// about a failed step.
	if result.Ledger.Totals.TokensIn == 0 {
		t.Error("usage reported before the error was dropped from the ledger")
	}
}

func TestRunHandlesAProviderThatNeverStarted(t *testing.T) {
	h := newHarness(t, func(h *harness) {
		h.provider.StreamErr = errors.New("provider: the model provider is unavailable")
	})

	result, err := h.agent.Run(context.Background(), h.input, h.rec.emit)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Outcome == OutcomeAnswered {
		t.Error("a run whose provider never started was recorded as answered")
	}
	if !h.rec.has("error") {
		t.Errorf("no error event. trace: %v", h.rec.types())
	}
}

func TestRunStopsOnACancelledContext(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// A cancelled run stops rather than finishing an execute nobody will read.
	// Whether it returns an error or a recorded outcome is the implementation's
	// call; what must not happen is a clean `answered`.
	result, _ := h.agent.Run(ctx, h.input, h.rec.emit)
	if result.Outcome == OutcomeAnswered {
		t.Error("a cancelled run was recorded as answered")
	}
}

func TestRunAlwaysReturnsALedger(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(*harness)
	}{
		{name: "happy path", mutate: func(*harness) {}},
		{name: "provider never started", mutate: func(h *harness) {
			h.provider.StreamErr = errors.New("unavailable")
		}},
		{name: "no sql in the output", mutate: func(h *harness) {
			h.provider.Deltas = []string{"sorry"}
		}},
		{name: "guard rejection", mutate: func(h *harness) {
			h.parser.summary = gen.ParseSummaryV1{
				Schema: "parse_summary.v1", Ok: false,
				Dialect:   gen.ParseSummaryV1DialectSqlite,
				NodeKinds: []string{}, Tables: []string{}, Functions: []string{},
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, tt.mutate)
			result, err := h.agent.Run(context.Background(), h.input, h.rec.emit)
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			// Every path returns accounting. A run whose cost went unrecorded
			// is a hole in cost per correct answer.
			raw, mErr := json.Marshal(result.Ledger)
			if mErr != nil {
				t.Fatalf("marshalling ledger: %v", mErr)
			}
			if err := contracts.Validate(contracts.CostLedgerV1, raw); err != nil {
				t.Errorf("cost_ledger.v1 validation failed: %v\n%s", err, raw)
			}
		})
	}
}

func TestNewRefusesAnIncompleteDependencySet(t *testing.T) {
	clk := clock.NewFake(epoch)
	full := Deps{
		Provider: &provider.FakeProvider{}, Parser: &fakeParser{},
		Executor: toyExecutor(t, clk), Clock: clk, CheapModel: "m",
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
		{name: "no provider", deps: without(func(d *Deps) { d.Provider = nil })},
		{name: "no parser", deps: without(func(d *Deps) { d.Parser = nil })},
		{name: "no executor", deps: without(func(d *Deps) { d.Executor = nil })},
		{name: "no clock", deps: without(func(d *Deps) { d.Clock = nil })},
		{name: "no model", deps: without(func(d *Deps) { d.CheapModel = "" })},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// A missing dependency found mid-run is a nil dereference inside a
			// goroutine that owns a live SSE stream, which a user sees as a
			// truncated response rather than an error.
			if _, err := New(tt.deps); err == nil {
				t.Fatal("New() accepted an incomplete dependency set")
			}
		})
	}
}
