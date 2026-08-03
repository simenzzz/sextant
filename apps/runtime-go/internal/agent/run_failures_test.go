package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/clock"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts/gen"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/provider"
)

// What a run does when something goes wrong: a guard rejection, a parser
// outage, a generation with no SQL, a provider that fails mid-stream or
// never starts, and a cancelled context. Harness lives in loop_test.go.

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
