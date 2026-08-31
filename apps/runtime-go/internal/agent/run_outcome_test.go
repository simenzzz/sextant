package agent

import (
	"context"
	"testing"
	"time"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts/gen"
)

// How a run reports the way it ended. Both cases here are regressions the
// existing suite could not see, because it drives Run with a context that has
// no deadline and asserts on the `rejected` event without asking whether the
// stream ever reached a terminal one. Harness lives in loop_test.go.

// terminalTypesOnTheWire is the set the browser's reducer treats as an
// outcome. It is duplicated from apps/web/src/state/questionReducer.ts on
// purpose: a run whose stream ends outside this set leaves the client with no
// outcome at all, and the duplication is what makes that a test failure here
// rather than a blank panel there.
var terminalTypesOnTheWire = map[string]bool{
	"answered": true, "abstained": true, "error": true,
	"budget_exhausted": true, "depth_exhausted": true, "deadline_exceeded": true,
}

func countTerminal(rec *recorder) int {
	var n int
	for _, ty := range rec.types() {
		if terminalTypesOnTheWire[ty] {
			n++
		}
	}
	return n
}

func TestRunAlwaysReachesExactlyOneTerminalEvent(t *testing.T) {
	// Exactly one, on every path. Zero leaves the browser showing a run that
	// never finished; two is a timeline that contradicts itself.
	for _, tt := range []struct {
		name   string
		mutate func(*harness)
	}{
		{name: "answered", mutate: func(*harness) {}},
		{name: "guard rejection", mutate: func(h *harness) {
			h.parser.summary = gen.ParseSummaryV1{
				Schema: "parse_summary.v1", Ok: true,
				Dialect: gen.ParseSummaryV1DialectSqlite, StatementCount: 1,
				NodeKinds: []string{"Drop", "Table", "Identifier"},
				Tables:    []string{"orders"}, Functions: []string{},
			}
		}},
		{name: "table outside the schema", mutate: func(h *harness) {
			h.parser.summary = okSummary(cancelledSQL, "sqlite_master")
		}},
		{name: "parser outage", mutate: func(h *harness) {
			h.parser.err = context.DeadlineExceeded
		}},
		{name: "no sql in the output", mutate: func(h *harness) {
			h.provider.Deltas = []string{"sorry"}
		}},
		{name: "provider never started", mutate: func(h *harness) {
			h.provider.StreamErr = context.Canceled
		}},
		{name: "execution failure", mutate: func(h *harness) {
			const bad = "SELECT nope FROM orders LIMIT 500"
			h.provider.Deltas = []string{"```sql\n", bad, "\n```"}
			h.parser.summary = okSummary(bad)
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, tt.mutate)
			if _, err := h.agent.Run(context.Background(), h.input, h.rec.emit); err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if n := countTerminal(h.rec); n != 1 {
				t.Errorf("emitted %d terminal events, want exactly 1. trace: %v", n, h.rec.types())
			}
		})
	}
}

func TestRunRecordsAContextDeadlineAsTheDeadlineCap(t *testing.T) {
	// The API layer bounds every run with context.WithTimeout(WallClock), and
	// Budget.StartedAt is taken a moment later on a spawned goroutine — so in
	// production the CONTEXT deadline is what fires, never Budget.Charge's own
	// wall-clock check. Reporting it as internal_error would make
	// deadline_exceeded a value the eval essentially never records, and would
	// paint an ordinary timeout in the UI as a failure.
	h := newHarness(t)

	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	result, err := h.agent.Run(ctx, h.input, h.rec.emit)
	if err != nil {
		t.Fatalf("Run() returned an error for a run that ran out of time: %v", err)
	}
	if result.Outcome != OutcomeDeadlineExceeded {
		t.Errorf("Outcome = %q, want %q. trace: %v",
			result.Outcome, OutcomeDeadlineExceeded, h.rec.types())
	}
	if !h.rec.has("deadline_exceeded") {
		t.Errorf("no deadline_exceeded event. trace: %v", h.rec.types())
	}
	if h.rec.has("error") {
		t.Errorf("a run that ran out of time was also reported as an error. trace: %v", h.rec.types())
	}
}

func TestRunRecordsACancellationAsAnError(t *testing.T) {
	// A cancellation is the client going away, not a cap. The two must not
	// share a branch: the eval counts them separately.
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := h.agent.Run(ctx, h.input, h.rec.emit)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Outcome != OutcomeError {
		t.Errorf("Outcome = %q, want %q for a cancelled run", result.Outcome, OutcomeError)
	}
	if h.rec.has("deadline_exceeded") {
		t.Errorf("a cancelled run was recorded as having run out of time. trace: %v", h.rec.types())
	}
}

func TestRunDoesNotBillAProviderCallThatNeverHappened(t *testing.T) {
	// Recording a ledger entry for a request that never started reports an
	// unknown-cost provider call on every run of an outage — which is exactly
	// the signal steps_cost_unknown exists to raise, spent on a call that did
	// not happen.
	h := newHarness(t, func(h *harness) { h.provider.StreamErr = context.Canceled })

	result, err := h.agent.Run(context.Background(), h.input, h.rec.emit)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Ledger.Totals.ProviderCalls != 0 {
		t.Errorf("ProviderCalls = %d, want 0 when the request never started",
			result.Ledger.Totals.ProviderCalls)
	}
	if result.Ledger.Totals.StepsCostUnknown != 0 {
		t.Errorf("StepsCostUnknown = %d, want 0 when no call was made",
			result.Ledger.Totals.StepsCostUnknown)
	}
}

func TestRunClampsAnOversizedDeltaToWhatTheContractCarries(t *testing.T) {
	// The SSE writer validates every outbound frame and treats a refusal
	// exactly like a reader that went away, so one oversized fragment would
	// cancel an otherwise healthy run after its money was already spent.
	huge := make([]byte, MaxDeltaChars*2)
	for i := range huge {
		huge[i] = 'a'
	}
	h := newHarness(t, func(h *harness) {
		h.provider.Deltas = []string{string(huge), "```sql\n" + cancelledSQL + "\n```"}
	})

	if _, err := h.agent.Run(context.Background(), h.input, h.rec.emit); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	for i, ev := range h.rec.events {
		if ev.Delta != nil && len([]rune(*ev.Delta)) > MaxDeltaChars {
			t.Errorf("event %d carries a delta of %d characters, over the contract's %d",
				i, len([]rune(*ev.Delta)), MaxDeltaChars)
		}
	}
}
