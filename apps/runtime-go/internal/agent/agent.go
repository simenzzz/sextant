package agent

import (
	"errors"
	"log/slog"
	"time"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/clock"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts/gen"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/executor"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/guard"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/provider"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/schema"
)

// Emit reports one state transition.
//
// A function rather than a channel so the loop reads as a sequence of steps
// rather than as plumbing. The API layer supplies an Emit that sends into the
// single-writer channel, which is what keeps the SSE stream's one-writer rule
// intact while the loop stays unaware of it.
//
// Emit fills in `step` and `elapsed_ms` for its caller: they are properties of
// the stream, not of the transition, and asking the loop to track a monotonic
// counter would be asking it to get one wrong.
type Emit func(ev gen.TraceEventV1)

// Deps are what a run needs to do its work.
type Deps struct {
	Provider provider.Provider
	Parser   guard.Parser
	Executor *executor.Executor
	Clock    clock.Clock
	Logger   *slog.Logger

	// CheapModel is the tier every P1 run uses. The strong tier arrives with
	// the router at P6.
	CheapModel string
}

// Agent runs questions.
type Agent struct {
	deps Deps
}

// New builds an Agent, refusing an incomplete dependency set.
//
// Checked at construction rather than at use: a missing dependency discovered
// mid-run is a nil dereference inside a goroutine that owns a live SSE stream,
// which surfaces to a user as a truncated response rather than as an error.
func New(deps Deps) (*Agent, error) {
	switch {
	case deps.Provider == nil:
		return nil, errors.New("agent: nil provider")
	case deps.Parser == nil:
		return nil, errors.New("agent: nil parser")
	case deps.Executor == nil:
		return nil, errors.New("agent: nil executor")
	case deps.Clock == nil:
		return nil, errors.New("agent: nil clock")
	case deps.CheapModel == "":
		return nil, errors.New("agent: no model configured")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	return &Agent{deps: deps}, nil
}

// Deps exposes the dependency set, for the loop implementation and for tests.
func (a *Agent) Deps() Deps { return a.deps }

// RunInput is one question, fully resolved.
//
// Everything here has already passed a boundary: the question was validated
// against question_request.v1, the database slug was resolved through the
// registry, and the schema was introspected. The loop does no lookups.
type RunInput struct {
	RunID    string
	Question string
	Evidence string

	// Schema is the whole database at P1. At P5 it is the retrieved subset,
	// and the loop does not need to know which.
	Schema schema.Schema
	// Dialect the plan will be built and executed for.
	Dialect gen.SqlPlanV1Dialect
	// Caps bound this run. Already clamped to the server's ceilings.
	Caps Caps
	// RowLimit is the cap the guard enforces on the statement.
	RowLimit int
	// StatementTimeout is what the executor enforces on the driver.
	StatementTimeout time.Duration
}

// Policy renders the guard policy for this run.
//
// Provided so the loop does not assemble it by hand, and so "the allowed table
// set is the retrieved subset" stays one statement in one place rather than a
// convention repeated at each call site.
func (in RunInput) Policy(fingerprint string) guard.Policy {
	return guard.Policy{
		Dialect:           in.Dialect,
		AllowedTables:     in.Schema.TableNames(),
		RowLimit:          in.RowLimit,
		StatementTimeout:  in.StatementTimeout,
		SchemaFingerprint: fingerprint,
	}
}

// Result is what a finished run produced.
type Result struct {
	// Outcome is how the run ended. Never OutcomeNone.
	Outcome Outcome
	// Plan is the statement that executed, when one did.
	Plan *gen.SqlPlanV1
	// Rows is the answer, when there is one.
	Rows *gen.ResultSetV1
	// Ledger is what the run spent. Always present, including on a run that
	// ended at a cap.
	Ledger gen.CostLedgerV1
}
