package agent

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts/gen"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/executor"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/guard"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/provider"
)

// The phases of one run. Each returns a non-nil *Result when the run ended in
// it, and nil when the run may continue. Run reads as the sequence of them;
// the reasoning for each step lives on Run's own doc comment.

// retrieve emits the schema subset handed to generation.
//
// There is no retriever at P1, so the subset is the whole schema — but the
// transition is emitted anyway. The eval and the UI both read this stream, and
// a phase that silently skips a transition makes P5's version look like a new
// behaviour rather than a better one.
func (r *runState) retrieve() {
	ev := event(gen.TraceEventV1TypeRetrieved)
	ev.Tables = r.in.Schema.TableNames()
	r.emit(ev)
}

// generate asks the model for a statement and charges what it cost.
func (r *runState) generate(ctx context.Context) (string, *Result) {
	r.emit(modelEvent(gen.TraceEventV1TypeGenerating, r.model))

	started := r.deps.Clock.Now()
	request := BuildRequest(r.model, r.in.Schema, r.in.Question, r.in.Evidence)
	stream, startErr := r.deps.Provider.Stream(ctx, request)
	raw, usage, streamErr := r.drain(stream)

	// Logged before anything else can end the run. A cap that trips on the
	// same step would otherwise bury the provider failure entirely, leaving an
	// operator with `budget_exhausted` and no record that the provider broke.
	switch {
	case startErr != nil:
		r.deps.Logger.Error("the provider stream never started",
			"run_id", r.in.RunID, "model", r.model, "error", startErr)
	case streamErr != nil:
		r.deps.Logger.Error("the provider stream failed",
			"run_id", r.in.RunID, "model", r.model, "error", streamErr)
	}

	// A request that never started made no provider call, so it gets no ledger
	// entry. Recording one would report an unknown-cost provider call on every
	// run of an outage — which is precisely the signal steps_cost_unknown
	// exists to raise, spent on a call that did not happen.
	if startErr == nil {
		r.generation = r.ledger.Record(1, r.model, usage,
			millis(r.deps.Clock.Now().Sub(started)), 0, false)
	}

	// Charge before deciding anything else. A cap checked only at the top of a
	// loop is a cap that can be exceeded by exactly one step, and that step is
	// the expensive one.
	var outcome Outcome
	r.budget, outcome = r.budget.Charge(Step{USD: r.generation.Usd}, r.deps.Clock.Now())
	if outcome.Terminal() {
		// A cap is a recorded outcome, not an error: the eval reports its rate
		// and the UI must not render it as a failure.
		r.emit(withCost(event(terminalType(outcome)), r.generation))
		return "", r.finish(outcome, nil, nil)
	}

	// Checked before the provider errors below, because a stream the deadline
	// killed closes with no error of its own on some adapters and with a
	// context error on others. Either way it ran out of time; calling that a
	// provider failure would misattribute the run.
	if done := r.stopped(ctx); done != nil {
		return "", done
	}
	switch {
	case startErr != nil:
		return "", r.fail(gen.TraceEventV1FailureKindProviderError,
			"the model provider could not be reached")
	case streamErr != nil:
		return "", r.fail(gen.TraceEventV1FailureKindProviderError,
			"the model provider stopped before the statement was complete")
	}
	return raw, nil
}

// drain reads the provider stream to completion.
//
// To completion on every path, whatever arrives. Returning early would leave
// the provider's goroutine blocked forever on a send nobody will receive, and
// the usage that arrives before an error is what makes the ledger honest about
// a failed step.
func (r *runState) drain(stream <-chan provider.StreamEvent) (string, *provider.Usage, error) {
	// A nil channel would block this goroutine forever, holding a run slot for
	// the life of the process. The Provider contract forbids returning one
	// alongside a nil error, and this is what makes a breach of that contract
	// an ended run rather than a leak.
	if stream == nil {
		return "", nil, nil
	}

	var (
		raw       strings.Builder
		usage     *provider.Usage
		streamErr error
	)
	for ev := range stream {
		if ev.Delta != "" {
			// Past the length the contract can carry, the candidate is already
			// refused, so anything further is memory spent on a statement
			// nothing will validate. Keep draining; stop accumulating.
			if raw.Len() < MaxGeneratedSQLChars {
				raw.WriteString(ev.Delta)
			}
			r.emit(withDelta(modelEvent(gen.TraceEventV1TypeGenerating, r.model), ev.Delta))
		}
		if ev.Usage != nil {
			usage = ev.Usage
		}
		if ev.Err != nil {
			streamErr = ev.Err
		}
	}
	return raw.String(), usage, streamErr
}

// extract recovers the statement from the raw generation.
func (r *runState) extract(raw string) (string, *Result) {
	sql, err := ExtractSQL(raw)
	if err != nil {
		// P1 does not repair, so a generation with no statement ends the run.
		return "", r.fail(gen.TraceEventV1FailureKindSyntaxError,
			"the model did not produce a SQL statement")
	}
	r.emit(withCost(sqlEvent(gen.TraceEventV1TypeGenerated, sql), r.generation))
	return sql, nil
}

// validate parses the statement and proves it may execute.
func (r *runState) validate(ctx context.Context, sql string) (gen.SqlPlanV1, *Result) {
	if done := r.stopped(ctx); done != nil {
		return gen.SqlPlanV1{}, done
	}

	summary, err := r.deps.Parser.Parse(ctx, sql, r.in.Dialect, r.in.RowLimit)
	if err != nil {
		// The sidecar being down must look nothing like the sidecar approving
		// a statement, and it must be distinguishable in the trace from a
		// guard rejection: the eval separates "our sidecar was down" from
		// "the model wrote a DROP".
		r.deps.Logger.Error("the SQL parser is unavailable", "run_id", r.in.RunID, "error", err)
		return gen.SqlPlanV1{}, r.fail(gen.TraceEventV1FailureKindInternalError,
			"the SQL parser is unavailable")
	}

	plan, err := guard.Validate(summary, r.in.Policy(r.in.Schema.Fingerprint()))
	if err != nil {
		kind, message := rejection(err)
		// Two events, not one. `rejected` records WHY the statement was
		// refused, and the terminal `error` records that the run is over —
		// `rejected` is not a terminal type, so a stream ending there leaves
		// the client with no outcome at all while the stored run says `error`.
		r.emit(failureEvent(gen.TraceEventV1TypeRejected, kind, message))
		return gen.SqlPlanV1{}, r.fail(kind, message)
	}

	// `validated` carries what will actually run. The SQL panel renders this,
	// not the raw generation, and the two differ whenever the guard injected
	// or clamped a LIMIT.
	r.emit(sqlEvent(gen.TraceEventV1TypeValidated, plan.Sql))
	return plan, nil
}

// execute runs the plan and finishes the run.
func (r *runState) execute(ctx context.Context, plan gen.SqlPlanV1) (Result, error) {
	if done := r.stopped(ctx); done != nil {
		return *done, nil
	}
	r.emit(event(gen.TraceEventV1TypeExecuting))

	rows, err := r.deps.Executor.Execute(ctx, r.in.RunID, plan)
	if err != nil {
		// A query the wall clock stopped ran out of time; it did not fail.
		if done := r.stopped(ctx); done != nil {
			return *done, nil
		}
		kind, message := executionFailure(err)
		r.deps.Logger.Error("the statement failed to execute", "run_id", r.in.RunID, "error", err)
		r.emit(failureEvent(gen.TraceEventV1TypeError, kind, message))
		return *r.finish(OutcomeError, &plan, nil), nil
	}

	executed := event(gen.TraceEventV1TypeExecuted)
	rowCount := rows.RowCount
	executed.RowCount = &rowCount
	r.emit(executed)

	r.emit(event(gen.TraceEventV1TypeAnswered))
	return *r.finish(OutcomeAnswered, &plan, &rows), nil
}

// terminalTypes maps an outcome to the event that records it.
//
// A map rather than a string conversion. Every Outcome that ends a run must
// have a trace_event.v1 type, and one added later without one would otherwise
// produce a frame the SSE writer refuses — leaving a run with no terminal
// event at all, which is the failure this table exists to make visible.
var terminalTypes = map[Outcome]gen.TraceEventV1Type{
	OutcomeAnswered:         gen.TraceEventV1TypeAnswered,
	OutcomeAbstained:        gen.TraceEventV1TypeAbstained,
	OutcomeError:            gen.TraceEventV1TypeError,
	OutcomeBudgetExhausted:  gen.TraceEventV1TypeBudgetExhausted,
	OutcomeDepthExhausted:   gen.TraceEventV1TypeDepthExhausted,
	OutcomeDeadlineExceeded: gen.TraceEventV1TypeDeadlineExceeded,
}

func terminalType(outcome Outcome) gen.TraceEventV1Type {
	if t, ok := terminalTypes[outcome]; ok {
		return t
	}
	return gen.TraceEventV1TypeError
}

// millis is a duration in whole milliseconds, never negative.
//
// Never negative because the ledger's ms is a non-negative count, and an
// injected clock a test moves backwards must not be able to produce a document
// that violates its own contract.
func millis(d time.Duration) int {
	if d < 0 {
		return 0
	}
	return int(d / time.Millisecond)
}

// rejection classifies a guard refusal for the trace.
//
// The guard's own kind is carried through rather than flattened, because the
// P4 repair loop keys its next prompt on it: a syntax error and a table-subset
// violation deserve completely different follow-ups.
func rejection(err error) (gen.TraceEventV1FailureKind, string) {
	var r *guard.Rejection
	if errors.As(err, &r) {
		return gen.TraceEventV1FailureKind(r.Kind), r.Reason
	}
	return gen.TraceEventV1FailureKindGuardRejected, "the statement was refused before execution"
}

// executionFailure classifies a driver error for the trace.
//
// The executor has already classified its own; anything else is reported
// generically rather than forwarded, because a raw driver body can carry
// connection and credential detail.
func executionFailure(err error) (gen.TraceEventV1FailureKind, string) {
	var f *executor.Failure
	if errors.As(err, &f) {
		return gen.TraceEventV1FailureKind(f.Kind), f.Message
	}
	return gen.TraceEventV1FailureKindInternalError, "the query could not be run"
}
