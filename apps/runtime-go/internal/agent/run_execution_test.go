package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts/gen"
)

// A statement the guard cleared can still fail at the driver. That is an
// ordinary outcome rather than a runtime fault, and the trace has to carry the
// classified reason so the P4 repair loop can key its next prompt on it.
// Harness lives in loop_test.go.

func TestRunReportsAClassifiedExecutionFailure(t *testing.T) {
	// Well-formed SQL over an allowed table, naming a column that does not
	// exist. Nothing before the executor can catch it: the guard checks the
	// table subset, not the columns.
	const missingColumn = "SELECT nope FROM orders LIMIT 500"
	h := newHarness(t, func(h *harness) {
		h.provider.Deltas = []string{"```sql\n", missingColumn, "\n```"}
		h.parser.summary = okSummary(missingColumn)
	})

	result, err := h.agent.Run(context.Background(), h.input, h.rec.emit)
	// A failed query is a recorded outcome, not a failure of the runtime.
	if err != nil {
		t.Fatalf("Run() returned an error for a query the database refused: %v", err)
	}

	if result.Outcome == OutcomeAnswered {
		t.Errorf("a query the database refused was recorded as answered. trace: %v", h.rec.types())
	}
	if result.Rows != nil {
		t.Error("a failed execution returned rows")
	}
	// The statement was cleared and attempted, so the plan is part of the
	// record even though it produced nothing.
	if result.Plan == nil {
		t.Error("a run that reached the executor carries no plan")
	}
	if !h.rec.has("executing") {
		t.Errorf("no executing event. trace: %v", h.rec.types())
	}
	if h.rec.has("executed") {
		t.Errorf("a failed execution emitted executed. trace: %v", h.rec.types())
	}

	ev, ok := h.rec.find("error")
	if !ok {
		t.Fatalf("no error event. trace: %v", h.rec.types())
	}
	if ev.FailureKind == nil || *ev.FailureKind != gen.TraceEventV1FailureKindUnknownColumn {
		t.Errorf("failure_kind = %v, want unknown_column so P4 can prompt a repair on it",
			ev.FailureKind)
	}
	// The driver's own text stays in the log. A raw body can carry connection
	// and credential detail.
	if ev.Message == nil || *ev.Message == "" {
		t.Error("the error event carries no message")
	}
	if ev.Message != nil && strings.Contains(*ev.Message, "nope") {
		t.Errorf("the message echoed the statement: %q", *ev.Message)
	}

	raw, mErr := json.Marshal(result.Ledger)
	if mErr != nil {
		t.Fatalf("marshalling ledger: %v", mErr)
	}
	if err := contracts.Validate(contracts.CostLedgerV1, raw); err != nil {
		t.Errorf("cost_ledger.v1 validation failed: %v\n%s", err, raw)
	}
}
