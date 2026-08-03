package guard

import (
	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts/gen"
)

// Validate decides whether a parsed statement may execute, and builds the plan
// that lets it.
//
// This is the last thing between a language model's output and a database.
// Nothing downstream re-checks it: the executor takes a sql_plan.v1 and runs
// whatever statement it carries, so a plan this function returns is a
// statement that WILL execute.
//
// # What it must do
//
// Return a sql_plan.v1 when the summary satisfies the policy, or a *Rejection
// when it does not. Never both, and never a zero plan with a nil error.
//
// # Algorithm
//
//  1. Fail closed on a summary that establishes nothing. If summary.Ok is
//     false, reject with KindSyntaxError, using summary.Error as the reason
//     when it is present and a fixed string when it is not. A parser that
//     could not read the statement has told you nothing about it.
//
//  2. Check the dialect. summary.Dialect must equal policy.Dialect. A summary
//     parsed as SQLite proves nothing about how Postgres will read the same
//     text, and the executor will run it against the policy's database.
//
//  3. Require exactly one statement. summary.StatementCount != 1 is a stacked
//     query — `SELECT 1; DELETE FROM users` — and is rejected. Note that the
//     sidecar already filters the empty statement a trailing semicolon
//     produces, so a well-formed `SELECT 1;` reports 1 here.
//
//  4. Walk the node kinds against the ALLOWLIST. Every entry in
//     summary.NodeKinds must be present in AllowedNodeKinds. Reject on the
//     first that is not, and name it in the reason.
//
//     This is the step that refuses DROP, DELETE, INSERT, UPDATE, ATTACH,
//     PRAGMA and COPY — not because those are on a list of bad things, but
//     because they are absent from the list of permitted ones. Do not add a
//     denylist alongside it: the allowlist is what makes a sqlglot upgrade
//     that introduces a new node type fail loudly instead of passing
//     silently, and a denylist next to it would quietly restore the old
//     failure mode.
//
//  5. Walk the function names against AllowedFunctions, lowercased.
//
//     This step is not a second opinion — it is the ONLY thing refusing
//     readfile, writefile, load_extension, pg_read_file and lo_import. Step 4
//     cannot help: a parser renders functions it knows as their own node kind
//     and everything else as one shared kind, "Anonymous", and measurement
//     showed that printf and julianday arrive as Anonymous too. Excluding that
//     kind would refuse ordinary SQL; including it admits every unknown
//     function. Only the NAMES separate them.
//
//     So do not treat this step as redundant with step 4, and do not "harden"
//     the guard by dropping Anonymous from AllowedNodeKinds — that trades a
//     real defence for a broken one.
//
//  6. Require at least one table, and prove the subset. summary.Tables must
//     be non-empty and every entry must appear in policy.AllowedTables.
//     Compare case-insensitively — the sidecar lowercases, but the policy's
//     list comes from schema introspection and preserves the database's
//     casing. Reject with KindUnknownTable, which is the kind the P4 repair
//     loop keys a "you used a table that does not exist" prompt on.
//
//     A statement with no tables (`SELECT 1`) is rejected too: sql_plan.v1
//     requires at least one, and a question answered without touching the
//     database is not an answer about the data.
//
//  7. Settle the LIMIT. The statement that executes must carry one no larger
//     than policy.RowLimit.
//
//     The sidecar has already done the mechanical rewrite and reports what it
//     did: summary.NormalizedSql is the statement re-rendered under the cap,
//     with summary.LimitInjected and summary.LimitClamped saying which
//     happened. Your job is to decide whether to accept that rewrite, not to
//     redo it — and to fail closed when it is missing. If NormalizedSql is
//     absent or empty on an otherwise acceptable statement, reject: you have
//     no statement you can prove carries a bound.
//
//     Verify rather than assume. If summary.HasLimit is true and
//     summary.LimitValue is present and already at or below the cap, no
//     rewrite was needed and the two flags should both be false. If
//     LimitValue is absent while HasLimit is true, the parser could not
//     resolve the limit to a constant — treat that as no bound at all.
//
//  8. Build the plan. Fill every required field of sql_plan.v1: Schema
//     ("sql_plan.v1"), Sql (the normalized statement), Dialect, Tables,
//     LimitValue (the limit actually in force), StatementTimeoutMs (from
//     policy.StatementTimeout), and SchemaFingerprint. Set LimitInjected and
//     LimitClamped from the summary so the UI can show that the executed
//     statement differs from the generation, and why.
//
// # Invariants
//
//   - Fail closed, always. Any field you need and cannot establish is a
//     rejection. There is no default that is safe here.
//   - Never quote the offending SQL in a Rejection.Reason. It is model output
//     travelling to a browser; name the rule that was broken instead.
//   - The returned plan's Sql is what executes. If you return the raw
//     generation rather than the normalized statement, the LIMIT the guard
//     believes it enforced will not be on the statement that runs.
//   - Return `(plan, nil)` or `(zero, *Rejection)`. A caller distinguishing
//     those two by inspecting the plan would be a caller that can get it
//     wrong.
//
// # Reference
//
// PLAN.md section 5.2 for the rules and the adversarial fixture set;
// packages/contracts/schemas/{parse_summary,sql_plan}.v1.schema.json for the
// two documents; guard_test.go for the cases, including every adversarial
// statement PLAN.md names.
func Validate(summary gen.ParseSummaryV1, policy Policy) (gen.SqlPlanV1, error) {
	panic("TODO(you): implement the guard's allowlist walk, table-subset proof, and LIMIT settlement — see the recipe above and guard_test.go")
}
