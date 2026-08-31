package guard

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts"
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
	// 0. A policy that cannot produce a conforming plan is a misconfigured
	// runtime, not a bad generation. Checked first so the guard never emits a
	// plan that violates sql_plan.v1 at a boundary further along.
	if err := policy.check(); err != nil {
		return gen.SqlPlanV1{}, err
	}

	// 1. A parser that could not read the statement has told us nothing about
	// it, so fail closed. The sidecar classifies its own failures and never
	// quotes the input — see _safe_reason in retriever-python's sqlguard/parse.py
	// — which is what makes its reason safe to pass to a browser.
	if !summary.Ok {
		return gen.SqlPlanV1{}, Reject(KindSyntaxError, parseFailureReason(summary))
	}

	// 2. A summary parsed as one dialect proves nothing about another, and the
	// executor will run the statement against the policy's database.
	if string(summary.Dialect) != string(policy.Dialect) {
		return gen.SqlPlanV1{}, Reject(KindGuardRejected, fmt.Sprintf(
			"the statement was parsed as %s but would execute on %s",
			summary.Dialect, policy.Dialect))
	}

	// 3. Exactly one statement. Anything else is a stacked query.
	if summary.StatementCount != 1 {
		return gen.SqlPlanV1{}, Reject(KindGuardRejected, fmt.Sprintf(
			"a plan carries exactly one statement; the parser found %d",
			summary.StatementCount))
	}

	// 4. Every node kind must be on the allowlist. This is what refuses DROP,
	// DELETE, INSERT, UPDATE, ATTACH, PRAGMA and COPY — not because they are
	// on a list of bad things, but because they are absent from the list of
	// permitted ones. The kind is named in the reason: it comes from sqlglot's
	// own fixed vocabulary, so it is not model-controlled text.
	// An empty list would walk the loop zero times and pass. parse_summary.v1
	// permits the empty array, and "fail closed on anything you cannot
	// establish" applies most of all to the guard's primary gate: a summary
	// that names no node kind has told us nothing about the statement.
	if len(summary.NodeKinds) == 0 {
		return gen.SqlPlanV1{}, Reject(KindGuardRejected,
			"the parser reported nothing about the statement's structure")
	}
	for _, kind := range summary.NodeKinds {
		if !AllowedNodeKinds[kind] {
			return gen.SqlPlanV1{}, Reject(KindGuardRejected, fmt.Sprintf(
				"the statement uses %s, which is not a permitted operation", kind))
		}
	}

	// 5. Every function name must be on the allowlist. This step is not a
	// second opinion on step 4 — it is the ONLY thing refusing readfile,
	// writefile, load_extension, pg_read_file and lo_import, because sqlglot
	// renders each of them as node kind "Anonymous", exactly as it renders
	// printf and julianday. Only the NAMES separate them.
	//
	// The offending name is deliberately not quoted: unlike a node kind, it is
	// model output on its way to a browser.
	for _, fn := range summary.Functions {
		if !AllowedFunctions[strings.ToLower(fn)] {
			return gen.SqlPlanV1{}, Reject(KindGuardRejected,
				"the statement calls a function that is not on the allowlist")
		}
	}

	// 6. Prove the table subset. The statement must stay inside the schema
	// subset generation was shown; a table outside it is a bug or an
	// injection. A statement with no tables is refused too — sql_plan.v1
	// requires at least one, and a question answered without touching the
	// database is not an answer about the data.
	if err := checkTables(summary.Tables, policy.AllowedTables); err != nil {
		return gen.SqlPlanV1{}, err
	}

	// 7. Settle the LIMIT. The sidecar has already done the mechanical
	// rewrite; the guard decides whether to accept it, and fails closed when
	// there is no rewritten statement to accept.
	if summary.NormalizedSql == nil || *summary.NormalizedSql == "" {
		return gen.SqlPlanV1{}, Reject(KindGuardRejected,
			"the parser returned no statement carrying an enforceable row limit")
	}
	limit, injected, clamped, err := settleLimit(summary, policy)
	if err != nil {
		return gen.SqlPlanV1{}, err
	}
	// Verify the sidecar's account of its own rewrite rather than copying it.
	// The flags describe what happened to the statement this guard is about to
	// authorise, and a summary that contradicts itself is a sidecar that is
	// broken or lying — either way the guard cannot prove what it is signing.
	if injected != summary.LimitInjected || clamped != summary.LimitClamped {
		return gen.SqlPlanV1{}, Reject(KindInternalError,
			"the parser's account of the row limit does not match the limit it reported")
	}

	// 8. Build the plan. Its Sql is the normalized statement, never the raw
	// generation: returning the generation would mean the LIMIT the guard
	// believes it enforced is not on the statement that actually runs.
	plan := gen.SqlPlanV1{
		Schema:             "sql_plan.v1",
		Sql:                *summary.NormalizedSql,
		Dialect:            policy.Dialect,
		Tables:             slices.Clone(summary.Tables),
		LimitValue:         limit,
		LimitInjected:      injected,
		LimitClamped:       clamped,
		StatementTimeoutMs: int(policy.StatementTimeout / time.Millisecond),
		SchemaFingerprint:  policy.SchemaFingerprint,
	}

	// Validated on the way out, like every other document this runtime emits.
	// The plan is conforming by construction today because Policy.check
	// mirrors the schema's bounds — but that is two places that have to stay
	// in agreement, and this is the gate that holds them there. It also covers
	// a Parser implementation that skipped validating the summary, which the
	// interface allows and P3's pure-Go parser may be.
	raw, err := json.Marshal(plan)
	if err != nil {
		return gen.SqlPlanV1{}, Reject(KindInternalError, "the guard could not render the plan")
	}
	if err := contracts.Validate(contracts.SQLPlanV1, raw); err != nil {
		return gen.SqlPlanV1{}, Reject(KindInternalError,
			"the guard could not build a plan that satisfies its own contract")
	}
	return plan, nil
}

// parseFailureReason is why the parser refused the statement.
func parseFailureReason(summary gen.ParseSummaryV1) string {
	if summary.Error != nil {
		if reason := strings.TrimSpace(*summary.Error); reason != "" {
			return reason
		}
	}
	return "the parser could not read the statement"
}

// checkTables proves every referenced table is in the allowed subset.
//
// The comparison is case-insensitive on purpose: the sidecar lowercases what
// it resolves, while the policy's list comes from schema introspection and
// preserves the database's casing. A case-sensitive comparison would reject
// every legitimate query on a schema whose tables are not already lowercase.
func checkTables(referenced, allowed []string) error {
	if len(referenced) == 0 {
		return Reject(KindUnknownTable,
			"the statement reads no table, so it cannot answer a question about the data")
	}

	permitted := make(map[string]bool, len(allowed))
	for _, t := range allowed {
		permitted[strings.ToLower(t)] = true
	}
	for _, t := range referenced {
		if !permitted[strings.ToLower(t)] {
			// The name is not quoted: it is model output bound for a browser.
			return Reject(KindUnknownTable,
				"the statement reads a table that is not in the schema it was given")
		}
	}
	return nil
}

// settleLimit reports the row limit that will be in force on the statement.
//
// It verifies rather than assumes. The statement keeps its own LIMIT only when
// the parser resolved it to a constant at or under the cap; in every other
// case the cap applies. A LIMIT the parser could not resolve is not a bound
// the guard can trust, so it counts as no bound at all.
func settleLimit(summary gen.ParseSummaryV1, policy Policy) (limit int, injected, clamped bool, err error) {
	if summary.HasLimit == nil || !*summary.HasLimit || summary.LimitValue == nil {
		// The statement carried no bound the guard can trust, so the cap is
		// one the rewrite must have added.
		return policy.RowLimit, true, false, nil
	}
	switch own := *summary.LimitValue; {
	case own < 1:
		// sql_plan.v1 requires a limit of at least 1. Raising a zero to the
		// cap would report a bound the statement does not carry, so refuse
		// instead of describing the plan wrongly.
		return 0, false, false, Reject(KindGuardRejected,
			"the statement is bounded to zero rows, so it cannot answer a question")
	case own <= policy.RowLimit:
		// The generation's own smaller limit is respected. Raising it to the
		// cap would return more rows than the query asked for, and no rewrite
		// was needed.
		return own, false, false, nil
	default:
		return policy.RowLimit, false, true, nil
	}
}

// check reports whether the policy can produce a conforming plan.
//
// The bounds mirror sql_plan.v1's own. A policy outside them is a
// misconfigured runtime rather than a bad generation, so it is an
// internal_error and never reaches the model as a repair prompt.
func (p Policy) check() error {
	timeoutMs := p.StatementTimeout / time.Millisecond
	switch {
	case p.RowLimit < 1 || p.RowLimit > 10000:
		return Reject(KindInternalError, "the row limit is outside the range a plan may carry")
	case timeoutMs < 100 || timeoutMs > 120000:
		return Reject(KindInternalError, "the statement timeout is outside the range a plan may carry")
	case strings.TrimSpace(p.SchemaFingerprint) == "":
		return Reject(KindInternalError, "the run has no schema fingerprint")
	case len(p.AllowedTables) == 0:
		return Reject(KindInternalError, "the run has no allowed tables")
	}
	return nil
}
