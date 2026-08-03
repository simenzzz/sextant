package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/provider"
)

// Assertions on what a run produces. The harness lives in loop_test.go;
// split out to keep both files under the 400-line rule.

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
