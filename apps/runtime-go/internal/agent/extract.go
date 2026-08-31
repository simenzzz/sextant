package agent

import (
	"errors"
	"strings"
	"unicode/utf8"
)

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
	candidate := raw
	if blocks := fencedBlocks(raw); len(blocks) > 0 {
		candidate = pickBlock(blocks)
	}
	candidate = strings.TrimSpace(candidate)

	// Reject rather than guess. An unparseable candidate is something the
	// parser reports cleanly and the guard refuses as a syntax_error, which is
	// a far better outcome than a statement this function invented.
	// Counted in characters, not bytes: the contract's maxLength counts
	// characters, and a byte count would refuse a legitimate statement that
	// happens to contain non-ASCII text in a literal.
	if candidate == "" || utf8.RuneCountInString(candidate) > MaxGeneratedSQLChars {
		return "", ErrNoSQL
	}
	if !startsAStatement(candidate) {
		return "", ErrNoSQL
	}
	return candidate, nil
}

// startsAStatement reports whether the candidate opens like a query.
//
// Needed because of the unfenced fallback: a model that answers "I am not sure
// how to answer that" produces a candidate that is not empty and not too long,
// and without this check the loop would spend a parser round trip to be told
// what the first word already said. A model that declines is a common enough
// generation to be worth refusing here rather than downstream.
//
// Deliberately narrow. Only SELECT and WITH open a statement this system will
// execute — everything else the guard's allowlist refuses anyway — so a
// keyword test costs nothing in recall and cannot admit prose. It errs closed:
// a generation this refuses ends the run, which is the same outcome the guard
// would reach one hop later.
func startsAStatement(candidate string) bool {
	// Leading comments are stripped for the TEST only. The candidate keeps
	// them: this function may not rewrite what it returns, and a comment is
	// something the parser handles perfectly well.
	head := strings.TrimSpace(stripLeadingComments(candidate))

	// A parenthesised query — `(SELECT ...) UNION (SELECT ...)` — opens with a
	// paren rather than a keyword, and refusing it would throw away a whole
	// shape of legitimate answer.
	if strings.HasPrefix(head, "(") {
		return true
	}

	word := head
	if i := strings.IndexAny(head, " \t\r\n("); i >= 0 {
		word = head[:i]
	}
	switch strings.ToUpper(word) {
	case "SELECT", "WITH":
		return true
	default:
		return false
	}
}

// stripLeadingComments removes SQL comments from the front of a candidate.
//
// Models routinely open a generation with `-- Count of cancelled orders`, and
// judging that line as the statement's first word would refuse the statement
// under it. Loop-bounded so a candidate of nothing but comment markers cannot
// spin.
func stripLeadingComments(candidate string) string {
	rest := candidate
	for {
		trimmed := strings.TrimLeft(rest, " \t\r\n")
		switch {
		case strings.HasPrefix(trimmed, "--"):
			nl := strings.IndexByte(trimmed, '\n')
			if nl < 0 {
				return ""
			}
			rest = trimmed[nl+1:]
		case strings.HasPrefix(trimmed, "/*"):
			end := strings.Index(trimmed, "*/")
			if end < 0 {
				return ""
			}
			rest = trimmed[end+2:]
		default:
			return trimmed
		}
	}
}

// fence is the marker models wrap code in.
const fence = "```"

// fencedBlock is one fenced region of a model's output.
type fencedBlock struct {
	// body is the text between the fences, verbatim.
	body string
	// tagged records that the opening fence carried the `sql` language tag,
	// which is the least ambiguous signal a model gives.
	tagged bool
}

// fencedBlocks splits raw into its fenced regions, in order.
//
// It never returns text that was not in raw: every body is a slice of the
// input. That is what keeps ExtractSQL unable to fabricate.
func fencedBlocks(raw string) []fencedBlock {
	var blocks []fencedBlock

	rest := raw
	for {
		open := strings.Index(rest, fence)
		if open < 0 {
			return blocks
		}
		rest = rest[open+len(fence):]

		// What remains of the opening fence's line is the language tag.
		tag := rest
		if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
			tag, rest = rest[:nl], rest[nl+1:]
		} else {
			// An opening fence on the last line encloses nothing.
			rest = ""
		}

		body := rest
		if end := strings.Index(rest, fence); end >= 0 {
			body, rest = rest[:end], rest[end+len(fence):]
		} else {
			// An unterminated fence. A generation cut off by a token cap opens
			// ``` and never closes it, and the statement is very likely
			// complete even when the fence is not — so keep it rather than
			// discard a good answer.
			rest = ""
		}

		blocks = append(blocks, fencedBlock{body: body, tagged: isSQLTag(tag)})
		if rest == "" {
			return blocks
		}
	}
}

// pickBlock chooses which fenced region holds the statement.
//
// A tagged block wins over an untagged one anywhere in the output, and the
// LAST of the winners wins. Two rules, and both come from how models write: a
// model that shows its working writes the draft first and the answer last, so
// taking the first would systematically pick the attempt it rejected — while a
// model that prints its result under the query fences the result WITHOUT the
// sql tag, so taking the last block regardless of tag would pick the output
// table instead of the statement.
func pickBlock(blocks []fencedBlock) string {
	for i := len(blocks) - 1; i >= 0; i-- {
		if blocks[i].tagged {
			return blocks[i].body
		}
	}

	// No block was tagged, so a bare fence it is — and a bare fence's first
	// line may still be the language tag.
	return stripLanguageTagLine(blocks[len(blocks)-1].body)
}

// isSQLTag reports whether a fence's language tag names SQL.
func isSQLTag(tag string) bool {
	return strings.EqualFold(strings.TrimSpace(tag), "sql")
}

// stripLanguageTagLine removes a leading line that is only a language tag.
//
// Safe to cut because a line reading `sql` on its own is not a statement, so
// nothing is lost. Anything else is left exactly as it arrived.
func stripLanguageTagLine(body string) string {
	trimmed := strings.TrimLeft(body, " \t\r\n")
	nl := strings.IndexByte(trimmed, '\n')
	if nl < 0 {
		return body
	}
	if !isSQLTag(trimmed[:nl]) {
		return body
	}
	return trimmed[nl+1:]
}
