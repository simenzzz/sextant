package agent

import "time"

// Outcome is how a run ended.
//
// Every value except OutcomeNone is a trace_event.v1 type, because the run's
// last event IS its outcome. Three of them are caps tripping, and those are
// recorded results rather than errors: the eval reports their rates and the UI
// must not render them as failures.
type Outcome string

const (
	// OutcomeNone means the run may continue. Not a trace event.
	OutcomeNone Outcome = ""

	OutcomeAnswered  Outcome = "answered"
	OutcomeAbstained Outcome = "abstained"
	OutcomeError     Outcome = "error"

	OutcomeBudgetExhausted  Outcome = "budget_exhausted"
	OutcomeDepthExhausted   Outcome = "depth_exhausted"
	OutcomeDeadlineExceeded Outcome = "deadline_exceeded"
)

// Terminal reports whether an outcome ends the run.
func (o Outcome) Terminal() bool { return o != OutcomeNone }

// Caps are the three independent bounds on a run, from PLAN.md section 5.3.
//
// Independent is the operative word: they are not a priority list and not a
// combined score. Whichever trips first ends the run, and the run records
// which one it was.
type Caps struct {
	// MaxRepairDepth bounds repair iterations. Zero means no repair is
	// permitted, which is P1's shape — the loop generates once.
	MaxRepairDepth int
	// MaxUSD bounds what one question may spend.
	MaxUSD float64
	// WallClock bounds how long one question may take.
	WallClock time.Duration
}

// Step is what one unit of work consumed.
type Step struct {
	// USD is what this step cost. Zero for a step that made no paid call.
	USD float64
	// EntersRepair marks a step that begins a new repair iteration, so the
	// depth cap advances only when a repair actually starts rather than on
	// every charge.
	EntersRepair bool
}

// Budget is a run's remaining allowance.
//
// A value, not a handle: Charge returns a new Budget rather than mutating this
// one. That is the house immutability rule, and here it also buys something
// concrete — a caller can evaluate a hypothetical charge without committing to
// it, which is what the router will want at P6 when deciding whether an
// escalation fits inside what is left.
type Budget struct {
	Caps      Caps
	StartedAt time.Time

	// SpentUSD is what the run has been charged so far.
	SpentUSD float64
	// RepairDepth is how many repair iterations have been consumed.
	RepairDepth int
}

// NewBudget starts a budget at now.
func NewBudget(caps Caps, now time.Time) Budget {
	return Budget{Caps: caps, StartedAt: now}
}

// Charge records a step and reports whether the run may continue.
//
// # What it must do
//
// Return the budget after recording step, and the Outcome that ended the run —
// OutcomeNone when it may continue. All three caps are checked here; there is
// no other place in the runtime that decides a run is over because it ran out
// of something.
//
// # Algorithm
//
//  1. Build the new budget. Add step.USD to SpentUSD, and add one to
//     RepairDepth when step.EntersRepair. Do not mutate the receiver — return
//     a new value.
//
//  2. Check the wall clock. If now is at or after StartedAt.Add(WallClock),
//     the run is over with OutcomeDeadlineExceeded.
//
//  3. Check the dollar budget against the NEW SpentUSD. If it is at or above
//     Caps.MaxUSD, the run is over with OutcomeBudgetExhausted.
//
//     At or above, not above: a run that has spent exactly its cap has no
//     allowance left, and letting it continue would let the next step exceed
//     the cap outright.
//
//  4. Check the repair depth against the NEW RepairDepth. If it is above
//     Caps.MaxRepairDepth, the run is over with OutcomeDepthExhausted.
//
//     Above, not at or above — and the asymmetry with step 3 is deliberate
//     rather than an oversight. MaxRepairDepth counts iterations that are
//     ALLOWED, so depth 3 with a cap of 3 is the third permitted repair and
//     must run; MaxUSD is an amount that must not be reached. Getting this
//     backwards costs the run its last legitimate repair, which is exactly
//     the repair most likely to have succeeded.
//
//  5. Return the new budget and the outcome. Always return the updated budget
//     even when a cap tripped: the spend happened, and a ledger that dropped
//     the charge that ended the run would understate exactly the run that
//     cost the most.
//
// # Invariants
//
//   - Whichever cap trips first wins, and the recorded outcome names the one
//     that actually tripped. A run that ran out of time must not be recorded
//     as having run out of money — the eval reports these rates separately
//     and a miscategorised cap corrupts both numbers.
//   - The three caps are independent. Do not combine, weight, or short-circuit
//     them into a single check.
//   - Ending at a cap is a RESULT, not an error. Charge returns no error and
//     never panics; a tripped cap is an Outcome.
//   - Immutable: the receiver is unchanged on every path.
//
// # Reference
//
// PLAN.md section 5.3 for the three axes; budget_test.go for the boundary
// cases, which are where the at/above asymmetry above is pinned down.
func (b Budget) Charge(step Step, now time.Time) (Budget, Outcome) {
	// The new budget. The receiver is untouched on every path below: Budget
	// holds only value types, so this copy is a complete one.
	next := b
	next.SpentUSD += step.USD
	if step.EntersRepair {
		next.RepairDepth++
	}

	// The three caps are independent, and the recorded outcome names the one
	// that actually tripped. The eval reports their rates separately, so a run
	// that ran out of time recorded as having run out of money corrupts both
	// numbers.
	//
	// The at/above asymmetry between the dollar cap and the depth cap is
	// deliberate. MaxUSD is an amount that must not be reached; MaxRepairDepth
	// counts iterations that are ALLOWED, so depth 3 under a cap of 3 is the
	// third permitted repair and must run.
	switch {
	case !now.Before(b.StartedAt.Add(b.Caps.WallClock)):
		return next, OutcomeDeadlineExceeded
	case next.SpentUSD >= b.Caps.MaxUSD:
		return next, OutcomeBudgetExhausted
	case next.RepairDepth > b.Caps.MaxRepairDepth:
		return next, OutcomeDepthExhausted
	default:
		// The updated budget goes back even when a cap tripped: the spend
		// happened, and a ledger that dropped the charge which ended the run
		// would understate exactly the run that cost the most.
		return next, OutcomeNone
	}
}

// Remaining reports what is left, for diagnostics and for the P6 router.
func (b Budget) Remaining(now time.Time) (usd float64, wall time.Duration, repairs int) {
	usd = b.Caps.MaxUSD - b.SpentUSD
	if usd < 0 {
		usd = 0
	}
	wall = b.StartedAt.Add(b.Caps.WallClock).Sub(now)
	if wall < 0 {
		wall = 0
	}
	repairs = b.Caps.MaxRepairDepth - b.RepairDepth
	if repairs < 0 {
		repairs = 0
	}
	return usd, wall, repairs
}
