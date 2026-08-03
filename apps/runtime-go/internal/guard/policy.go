// Package guard decides whether a generated statement may execute.
//
// The split with the retriever sidecar is deliberate. sqlglot parses and
// reports facts (parse_summary.v1); this package applies the policy. A summary
// is evidence, never a verdict — which is why the sidecar has no notion of a
// "safe" query and this package does no parsing.
//
// Everything here fails closed. A summary that is absent, unparseable, or
// missing a field the policy needs is a rejection, not a default: the guard is
// the last thing between a generation and a database, and the only safe
// reading of "I could not establish that" is "no".
package guard

import (
	"fmt"
	"time"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts/gen"
)

// Policy is what a statement must satisfy to execute.
type Policy struct {
	// Dialect the statement must have been parsed as. A summary parsed as
	// another dialect proves nothing about this one.
	Dialect gen.SqlPlanV1Dialect
	// AllowedTables is the schema subset handed to generation. At P1 there is
	// no retriever, so it is every table in the database; at P5 it is what the
	// retriever surfaced. Either way a statement touching anything outside it
	// is reaching for something the model was never shown.
	AllowedTables []string
	// RowLimit is the cap the statement's LIMIT must respect.
	RowLimit int
	// StatementTimeout is what the executor will enforce on the driver.
	StatementTimeout time.Duration
	// SchemaFingerprint identifies the schema the plan is built against.
	SchemaFingerprint string
}

// AllowedNodeKinds is the guard's allowlist of AST node types.
//
// An ALLOWLIST, not a denylist, and that choice is the whole design. A denylist
// answers "is this one of the bad things I thought of?", which a parser upgrade
// introducing a new node type silently answers wrong. An allowlist answers "is
// this one of the things I have decided are safe?", so the same upgrade causes
// a rejection — visible, and fixable by a human who then adds the kind
// deliberately.
//
// DERIVED, NOT GUESSED. Every entry below came from running real analytical
// statements through the sidecar and reading the node_kinds it reported. A
// hand-written version of this list rejected CASE WHEN, CAST, ||, strftime,
// group_concat and half of what an ordinary question needs — which would not
// have looked like a guard bug, it would have looked like the model writing bad
// SQL, and it would have shown up as a collapsed eval number at P2. Regenerate
// with the corpus in guard_corpus_test.go rather than adding entries by hand.
//
// Absent by design: Drop, Delete, Insert, Update, Create, Alter, Attach,
// Detach, Pragma, Copy, Command, Transaction, Grant.
//
// Anonymous IS present, and that is deliberate — see AllowedFunctions.
var AllowedNodeKinds = map[string]bool{
	// Structure.
	"Select": true, "From": true, "Where": true, "Group": true, "Having": true,
	"Order": true, "Ordered": true, "Limit": true, "Offset": true, "Distinct": true,
	"Subquery": true, "With": true, "CTE": true, "TableAlias": true, "Window": true,
	"Union": true, "Except": true, "Intersect": true, "SetOperation": true,
	"Join": true, "Lateral": true, "Cross": true,

	// References, literals, and types.
	"Table": true, "Column": true, "Identifier": true, "Literal": true,
	"Star": true, "Alias": true, "Null": true, "Boolean": true, "Placeholder": true,
	"DataType": true, "Cast": true, "TryCast": true, "Interval": true, "Tuple": true,

	// Operators and predicates.
	"EQ": true, "NEQ": true, "GT": true, "GTE": true, "LT": true, "LTE": true,
	"And": true, "Or": true, "Not": true, "Is": true, "In": true, "Between": true,
	"Like": true, "ILike": true, "Exists": true, "Paren": true, "Neg": true,
	"Add": true, "Sub": true, "Mul": true, "Div": true, "Mod": true,
	"Case": true, "If": true, "DPipe": true, "Concat": true,

	// Aggregates, scalar functions, and window functions that sqlglot names.
	"Count": true, "Sum": true, "Avg": true, "Min": true, "Max": true,
	"Abs": true, "Round": true, "Floor": true, "Ceil": true, "Length": true,
	"Lower": true, "Upper": true, "Trim": true, "Substring": true,
	"Coalesce": true, "GroupConcat": true, "Extract": true,
	"Date": true, "DateTrunc": true, "DateDiff": true, "DateAdd": true,
	"StrToDate": true, "TimeToStr": true, "TsOrDsToTimestamp": true,
	"RowNumber": true, "Rank": true, "DenseRank": true,

	// A function sqlglot does not have a node type for. See AllowedFunctions:
	// this kind is admitted precisely BECAUSE the function allowlist is what
	// gates it, and excluding it here would refuse printf and julianday along
	// with readfile.
	"Anonymous": true,
}

// AllowedFunctions is the guard's allowlist of callable function names, and it
// is the ONLY thing standing between a generation and a file-reading function.
//
// This is the finding that shaped both lists. A parser renders a function it
// knows as its own node type (COUNT becomes Count) and every function it does
// not know as one shared kind, Anonymous. Measured against sqlglot: readfile,
// writefile and load_extension arrive as Anonymous — and so do printf and
// julianday, which are perfectly ordinary. So the node-kind allowlist cannot
// separate them. Excluding Anonymous would refuse legitimate SQL; including it
// admits every unknown function. Only a list of NAMES distinguishes the two,
// which is why parse_summary.v1 carries `functions` at all and why this list is
// load-bearing rather than belt-and-braces.
//
// Lowercased, matching what the sidecar reports. Note the entries that look
// odd — time_to_str, ts_or_ds_to_timestamp — are sqlglot's canonical names for
// strftime and friends, taken from real output rather than invented.
var AllowedFunctions = map[string]bool{
	// Aggregates.
	"count": true, "sum": true, "avg": true, "min": true, "max": true,
	"total": true, "group_concat": true, "string_agg": true,

	// Scalar.
	"abs": true, "round": true, "floor": true, "ceil": true, "ceiling": true,
	"length": true, "lower": true, "upper": true, "trim": true, "ltrim": true,
	"rtrim": true, "substr": true, "substring": true, "replace": true,
	"coalesce": true, "ifnull": true, "nullif": true, "cast": true,
	"case": true, "if": true, "iif": true, "exists": true, "distinct": true,
	"concat": true, "printf": true, "format": true,
	"greatest": true, "least": true,

	// Dates. The canonical names sqlglot emits, plus the surface spellings.
	"date": true, "time": true, "datetime": true, "julianday": true,
	"strftime": true, "time_to_str": true, "ts_or_ds_to_timestamp": true,
	"date_trunc": true, "date_part": true, "date_diff": true, "date_add": true,
	"extract": true, "now": true, "current_date": true, "current_timestamp": true,
	"str_to_date": true,

	// Window.
	"row_number": true, "rank": true, "dense_rank": true,
	"cume_dist": true, "lag": true, "lead": true, "ntile": true,
}

// Rejection is why a statement may not execute.
//
// It carries a classified failure kind so the repair loop at P4 can choose its
// next prompt — a syntax error and a table-subset violation deserve completely
// different follow-ups — and a Reason that is safe to show a client.
type Rejection struct {
	// Kind is the trace_event.v1 failure_kind this rejection records.
	Kind string
	// Reason is a short, safe explanation. It must never quote the offending
	// SQL: that is model output on its way to a browser.
	Reason string
}

func (r *Rejection) Error() string {
	return fmt.Sprintf("guard: %s: %s", r.Kind, r.Reason)
}

// Failure kinds a rejection may carry, from trace_event.v1's enum.
const (
	KindGuardRejected = "guard_rejected"
	KindSyntaxError   = "syntax_error"
	KindUnknownTable  = "unknown_table"
	KindInternalError = "internal_error"
)

// Reject builds a Rejection.
func Reject(kind, reason string) *Rejection {
	return &Rejection{Kind: kind, Reason: reason}
}
