package agent

import (
	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts/gen"
)

// Constructors for the trace events a run emits.
//
// They exist so the loop reads as a sequence of transitions rather than as
// struct literals, and so the two fields every event shares are set in one
// place. Step and elapsed_ms are deliberately NOT set here: they are
// properties of the stream rather than of the transition, and Emit fills them
// in for its caller — see the Emit doc comment in agent.go.

// traceEventSchema is the contract discriminator every event carries.
const traceEventSchema = "trace_event.v1"

// event starts a plain transition.
func event(t gen.TraceEventV1Type) gen.TraceEventV1 {
	return gen.TraceEventV1{Schema: traceEventSchema, Type: t}
}

// modelEvent is a transition attributable to a model tier.
func modelEvent(t gen.TraceEventV1Type, model string) gen.TraceEventV1 {
	ev := event(t)
	ev.Model = &model
	return ev
}

// sqlEvent carries a statement.
//
// On `generated` this is the raw generation; on `validated` it is the exact
// statement cleared for execution. The two differ whenever the guard injected
// or clamped a LIMIT, and the SQL panel renders the second — showing the first
// would make the panel and the database disagree.
func sqlEvent(t gen.TraceEventV1Type, sql string) gen.TraceEventV1 {
	ev := event(t)
	ev.Sql = &sql
	return ev
}

// failureEvent carries a classified failure and a message safe to render.
//
// The message must never be a raw provider or driver body: those carry auth
// and quota state. Every caller passes text that the provider adapter, the
// guard, or the executor has already classified.
func failureEvent(t gen.TraceEventV1Type, kind gen.TraceEventV1FailureKind, message string) gen.TraceEventV1 {
	ev := event(t)
	ev.FailureKind = &kind
	ev.Message = &message
	return ev
}

// withCost attaches what a step cost.
//
// A step whose cost is unknown carries no figure at all rather than a zero:
// the runtime records provider-reported counts and never estimates, and a
// rendered `$0.00` would read as free rather than as unmeasured.
func withCost(ev gen.TraceEventV1, entry gen.Entry) gen.TraceEventV1 {
	if !entry.CostKnown {
		return ev
	}
	usd := entry.Usd
	ev.Usd = &usd
	return ev
}

// MaxDeltaChars bounds one streamed fragment.
//
// Mirrors trace_event.v1's maxLength for `delta`. The cap is load-bearing
// rather than cosmetic: the SSE writer validates every outbound frame and
// treats a refusal exactly like a reader that went away, so one oversized
// fragment would cancel an otherwise healthy run after its money was spent.
const MaxDeltaChars = 8192

// withDelta attaches a streamed token fragment, clamped to what the contract
// can carry.
//
// Clamped rather than dropped: the fragment is a progress indicator for the
// SQL panel, and the statement itself is recovered from the accumulated
// generation rather than from these events.
func withDelta(ev gen.TraceEventV1, delta string) gen.TraceEventV1 {
	if len(delta) > MaxDeltaChars {
		// Cut on a rune boundary. A byte-truncated fragment is invalid UTF-8,
		// which json.Marshal silently rewrites into replacement characters.
		delta = string([]rune(delta)[:min(len([]rune(delta)), MaxDeltaChars)])
	}
	ev.Delta = &delta
	return ev
}
