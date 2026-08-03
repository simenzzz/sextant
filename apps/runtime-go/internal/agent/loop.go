package agent

import "context"

// Run answers one question.
//
// This is the orchestrator: the function PLAN.md section 5.3 draws as
// plan → retrieve → generate → validate → execute → observe. At P1 that path
// is linear — there is no retriever, no repair, and no router — so what you
// are writing is the spine those phases later branch off.
//
// # What it must do
//
// Drive one question to a terminal outcome, emitting a trace event at every
// transition, charging the budget after every step, and returning a Result
// whose Ledger is complete regardless of how the run ended.
//
// # Algorithm (P1)
//
//  1. Start. Take a := a.Deps(), start := deps.Clock.Now(), build
//     budget := NewBudget(in.Caps, start) and ledger := NewLedger(in.RunID).
//     Emit a `run_started` event.
//
//  2. Retrieve. There is no retriever at P1, so the schema subset is the whole
//     schema — but emit `retrieved` with in.Schema.TableNames() anyway. The
//     eval and the UI both read the trace, and a phase that silently skips a
//     transition makes P5's version look like a new behaviour rather than a
//     better one.
//
//  3. Generate. Build the request with BuildRequest(deps.CheapModel,
//     in.Schema, in.Question, in.Evidence). Emit `generating` carrying the
//     model. Call deps.Provider.Stream.
//
//     Drain the channel to completion. Accumulate Delta into a buffer, keep
//     the last Usage you see, and keep any Err. Do NOT return early on an
//     error: the channel must be drained or the provider's goroutine blocks
//     forever on a send nobody will receive, and the usage that arrives before
//     the error is what makes the ledger honest about a failed step.
//
//     Emit `generating` events carrying `delta` as fragments arrive if you
//     want the SQL to stream into the UI; the contract supports it and the
//     panel is nicer for it. It is optional at P1.
//
//  4. Charge. Record the step on the ledger with Ledger.Record(step,
//     model, usage, ms, repairDepth, false) and charge the budget with
//     Charge(Step{USD: entry.Usd}, deps.Clock.Now()). If the outcome is
//     terminal, emit the matching event and stop — with the ledger intact.
//
//     Charge after every provider call, before deciding to continue. A cap
//     checked only at the top of a loop is a cap that can be exceeded by
//     exactly one step, and that step is the expensive one.
//
//  5. Extract. ExtractSQL(buffer). On ErrNoSQL, emit `error` with
//     failure_kind `syntax_error` and finish — the model produced no
//     statement, which at P1 (no repair) ends the run.
//
//  6. Validate. Emit `generated` with the raw statement. Call
//     deps.Parser.Parse(ctx, sql, in.Dialect, in.RowLimit), then
//     guard.Validate(summary, in.Policy(in.Schema.Fingerprint())).
//
//     A parser error and a guard rejection are different things and must not
//     collapse into one. The parser being unreachable is `internal_error`;
//     the guard refusing a statement is `guard_rejected` (or whatever kind the
//     Rejection carries). Both fail the run closed at P1, but the eval
//     separates "our sidecar was down" from "the model wrote a DROP", and it
//     can only do that if the trace does.
//
//     Emit `rejected` with the failure kind on a rejection, and `validated`
//     with the post-guard statement on success. `validated` carries what will
//     actually run — the SQL panel shows this, not the raw generation, and the
//     two differ whenever the guard injected or clamped a LIMIT.
//
//  7. Execute. Emit `executing`. Call deps.Executor.Execute(ctx, in.RunID,
//     plan). Emit `executed` with row_count on success, or `error` with the
//     classified failure on a driver error. Record the execution on the ledger
//     as a step with no usage — it made no provider call, so its cost is zero
//     and known, not unknown.
//
//  8. Finish. Emit `answered` and return a Result carrying the outcome, the
//     plan, the rows, and ledger.Document().
//
// # Invariants
//
//   - Exactly one terminal event per run. The UI's reducer treats the first
//     terminal type it sees as the outcome, and a second one after it is a
//     timeline that contradicts itself.
//   - The ledger is always complete. Every path out of this function returns
//     a Result whose Ledger has an entry for every step that spent anything —
//     including the step that tripped a cap, and including a run that ended
//     in an error.
//   - Ending at a cap is a RESULT. Return it as an Outcome with a nil error.
//     Reserve the error return for a failure of the runtime itself: a nil
//     dependency, a context cancelled by shutdown. If you find yourself
//     returning an error for something the eval should be able to count a
//     rate of, it belongs in the Outcome instead.
//   - Never emit a raw provider or driver error body. Those carry auth and
//     quota state. The provider adapter and the guard client have already
//     classified theirs; keep the classified message and put the detail in
//     deps.Logger.
//   - Respect ctx. It carries the wall-clock cap and the client going away.
//     Check it between steps; a cancelled run stops rather than finishing an
//     execute nobody will read.
//
// # What is NOT P1
//
// Repair (P4), escalation and abstention (P6), the semantic cache (P7). The
// signature already carries them — Caps.MaxRepairDepth, Step.EntersRepair,
// Outcome's abstained and depth_exhausted values, the ledger's escalated flag
// — so those phases add branches to this function rather than changing what
// calls it. Leave the paths unwritten; do not stub them out with TODOs.
//
// # Reference
//
// PLAN.md section 5.3 for the loop and its three caps; loop_test.go for the
// cases, which drive a FakeProvider and a fake parser and assert on the trace
// the run produced.
func (a *Agent) Run(ctx context.Context, in RunInput, emit Emit) (Result, error) {
	panic("TODO(you): implement the plan→generate→validate→execute→observe loop — see the recipe above and loop_test.go")
}
