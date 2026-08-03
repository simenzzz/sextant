package agent

import "errors"

// ErrNoSQL is returned when nothing in the model's output is a statement.
var ErrNoSQL = errors.New("agent: no SQL statement in the model output")

// MaxGeneratedSQLChars bounds the statement ExtractSQL may return.
//
// Mirrors sql_plan.v1 and parse_summary.v1's maxLength. A longer statement
// could not be carried by either contract, so recovering one would only push
// the failure to a boundary further along.
const MaxGeneratedSQLChars = 20000

// ExtractSQL recovers the statement from a model's raw output.
//
// This is a validation boundary, and it is the one that decides what the guard
// even sees. Everything downstream — the parser, the allowlist walk, the
// table-subset proof — operates on whatever this returns, so a sloppy
// extraction does not produce a wrong answer, it produces a *different
// statement being validated than the one that was generated*.
//
// # What it must do
//
// Return exactly the SQL, with no fences, no prose, and no trailing prattle —
// or ErrNoSQL when there is none. Never return a best guess.
//
// # Algorithm
//
//  1. Prefer a fenced block. Look for ```sql … ``` first, then a bare ``` …
//     ```. Take its contents. Models fence SQL far more often than not, and a
//     fence is the least ambiguous signal available.
//
//  2. If there are several fenced blocks, take the LAST one. A model that
//     shows its working writes the draft first and the answer last; taking
//     the first would systematically pick the rejected attempt.
//
//  3. If there is no fence, fall back to the raw text with surrounding
//     whitespace removed. A model asked for SQL and nothing else often
//     complies exactly.
//
//  4. Handle an unterminated fence. A generation cut off by a token cap can
//     open ``` and never close it. Treat everything after the opening fence as
//     the candidate rather than discarding it — the statement is very likely
//     complete even when the fence is not.
//
//  5. Strip a leading language tag. A bare ``` fence's first line may still
//     be `sql`; it is not part of the statement.
//
//  6. Reject rather than guess. Return ErrNoSQL when the candidate is empty
//     or whitespace, or when it exceeds MaxGeneratedSQLChars. Do NOT try to
//     repair prose into SQL, strip a trailing sentence, or splice two blocks
//     together — an unparseable candidate is something the parser will report
//     cleanly and the guard will reject with a syntax_error, which is a far
//     better outcome than a statement this function invented.
//
// # Invariants
//
//   - Never fabricate. Every character returned must have been present in the
//     input; this function may only cut, never add or reorder.
//   - Deterministic. The same output always extracts to the same statement,
//     or P2's record/replay cannot reproduce a run.
//   - Total. It returns a value and an error and never panics, whatever the
//     model produced — including empty output, only prose, only a fence, or
//     binary garbage.
//
// # Reference
//
// extract_test.go carries the shapes real models produce, including the
// truncated-fence and multiple-block cases that motivate steps 2 and 4.
func ExtractSQL(raw string) (string, error) {
	panic("TODO(you): implement fenced-block extraction with the truncation and multi-block rules — see the recipe above and extract_test.go")
}
