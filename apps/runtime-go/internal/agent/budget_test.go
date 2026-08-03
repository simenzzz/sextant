package agent

import (
	"testing"
	"time"
)

// These tests fail on a panic until Charge is written. That red is the
// worklist — see .claude/CLAUDE.md, "The two-job CI gate".

var epoch = time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

func caps() Caps {
	return Caps{MaxRepairDepth: 3, MaxUSD: 0.05, WallClock: 30 * time.Second}
}

func TestChargeAllowsAStepInsideEveryCap(t *testing.T) {
	b := NewBudget(caps(), epoch)

	got, outcome := b.Charge(Step{USD: 0.001}, epoch.Add(time.Second))

	if outcome != OutcomeNone {
		t.Fatalf("outcome = %q, want OutcomeNone for a step well inside every cap", outcome)
	}
	if got.SpentUSD != 0.001 {
		t.Errorf("SpentUSD = %v, want 0.001", got.SpentUSD)
	}
}

func TestChargeDoesNotMutateTheReceiver(t *testing.T) {
	// The house immutability rule, and here it also buys the P6 router the
	// ability to evaluate a hypothetical escalation without committing to it.
	b := NewBudget(caps(), epoch)

	b.Charge(Step{USD: 0.04, EntersRepair: true}, epoch.Add(time.Second))

	if b.SpentUSD != 0 {
		t.Errorf("receiver SpentUSD = %v after Charge, want 0", b.SpentUSD)
	}
	if b.RepairDepth != 0 {
		t.Errorf("receiver RepairDepth = %d after Charge, want 0", b.RepairDepth)
	}
}

func TestChargeRecordsTheSpendThatTrippedTheCap(t *testing.T) {
	// A ledger that dropped the charge which ended the run would understate
	// exactly the run that cost the most.
	b := NewBudget(caps(), epoch)

	got, outcome := b.Charge(Step{USD: 0.09}, epoch.Add(time.Second))

	if outcome != OutcomeBudgetExhausted {
		t.Fatalf("outcome = %q, want OutcomeBudgetExhausted", outcome)
	}
	if got.SpentUSD != 0.09 {
		t.Errorf("SpentUSD = %v, want the tripping charge recorded", got.SpentUSD)
	}
}

func TestChargeTripsEachCapIndependently(t *testing.T) {
	tests := []struct {
		name  string
		start Budget
		step  Step
		now   time.Time
		want  Outcome
	}{
		{
			name:  "well inside everything",
			start: NewBudget(caps(), epoch),
			step:  Step{USD: 0.001},
			now:   epoch.Add(time.Second),
			want:  OutcomeNone,
		},
		{
			name:  "dollars exceeded",
			start: NewBudget(caps(), epoch),
			step:  Step{USD: 0.06},
			now:   epoch.Add(time.Second),
			want:  OutcomeBudgetExhausted,
		},
		{
			// At the cap, not above it. A run that has spent exactly its
			// allowance has none left, and continuing would let the next step
			// exceed the cap outright.
			name:  "dollars exactly at the cap",
			start: NewBudget(caps(), epoch),
			step:  Step{USD: 0.05},
			now:   epoch.Add(time.Second),
			want:  OutcomeBudgetExhausted,
		},
		{
			name:  "dollars a hair under the cap",
			start: NewBudget(caps(), epoch),
			step:  Step{USD: 0.0499},
			now:   epoch.Add(time.Second),
			want:  OutcomeNone,
		},
		{
			name:  "wall clock exceeded",
			start: NewBudget(caps(), epoch),
			step:  Step{USD: 0.001},
			now:   epoch.Add(31 * time.Second),
			want:  OutcomeDeadlineExceeded,
		},
		{
			name:  "wall clock exactly at the cap",
			start: NewBudget(caps(), epoch),
			step:  Step{USD: 0.001},
			now:   epoch.Add(30 * time.Second),
			want:  OutcomeDeadlineExceeded,
		},
		{
			name:  "wall clock a hair under the cap",
			start: NewBudget(caps(), epoch),
			step:  Step{USD: 0.001},
			now:   epoch.Add(29999 * time.Millisecond),
			want:  OutcomeNone,
		},
		{
			// MaxRepairDepth counts iterations that are ALLOWED, so the third
			// repair under a cap of 3 must run. Tripping here would cost the
			// run its last legitimate repair — the one most likely to have
			// succeeded, since it follows the most feedback.
			name:  "the last permitted repair still runs",
			start: Budget{Caps: caps(), StartedAt: epoch, RepairDepth: 2},
			step:  Step{USD: 0.001, EntersRepair: true},
			now:   epoch.Add(time.Second),
			want:  OutcomeNone,
		},
		{
			name:  "one repair past the cap",
			start: Budget{Caps: caps(), StartedAt: epoch, RepairDepth: 3},
			step:  Step{USD: 0.001, EntersRepair: true},
			now:   epoch.Add(time.Second),
			want:  OutcomeDepthExhausted,
		},
		{
			// A step that is not a repair must not advance the depth counter,
			// or an ordinary generation would burn a repair allowance.
			name:  "a non-repair step does not advance depth",
			start: Budget{Caps: caps(), StartedAt: epoch, RepairDepth: 3},
			step:  Step{USD: 0.001},
			now:   epoch.Add(time.Second),
			want:  OutcomeNone,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, got := tt.start.Charge(tt.step, tt.now)
			if got != tt.want {
				t.Errorf("Charge() outcome = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestChargeNamesTheCapThatActuallyTripped(t *testing.T) {
	// The eval reports these rates separately, so a run that ran out of time
	// recorded as having run out of money corrupts both numbers.
	b := NewBudget(caps(), epoch)

	_, outcome := b.Charge(Step{USD: 0.001}, epoch.Add(time.Hour))
	if outcome != OutcomeDeadlineExceeded {
		t.Errorf("a run far past its deadline recorded %q, want OutcomeDeadlineExceeded", outcome)
	}

	_, outcome = b.Charge(Step{USD: 10}, epoch.Add(time.Second))
	if outcome != OutcomeBudgetExhausted {
		t.Errorf("a run far past its dollar cap recorded %q, want OutcomeBudgetExhausted", outcome)
	}
}

func TestChargeAdvancesDepthOnlyOnRepairSteps(t *testing.T) {
	b := NewBudget(caps(), epoch)

	plain, _ := b.Charge(Step{USD: 0.001}, epoch.Add(time.Second))
	if plain.RepairDepth != 0 {
		t.Errorf("RepairDepth = %d after a non-repair step, want 0", plain.RepairDepth)
	}

	repair, _ := b.Charge(Step{USD: 0.001, EntersRepair: true}, epoch.Add(time.Second))
	if repair.RepairDepth != 1 {
		t.Errorf("RepairDepth = %d after a repair step, want 1", repair.RepairDepth)
	}
}

func TestChargeAccumulatesAcrossSteps(t *testing.T) {
	b := NewBudget(caps(), epoch)

	for i := range 3 {
		var outcome Outcome
		b, outcome = b.Charge(Step{USD: 0.01}, epoch.Add(time.Duration(i+1)*time.Second))
		if outcome != OutcomeNone {
			t.Fatalf("step %d ended the run early with %q", i+1, outcome)
		}
	}
	if b.SpentUSD < 0.0299 || b.SpentUSD > 0.0301 {
		t.Errorf("SpentUSD = %v after three 0.01 charges, want ~0.03", b.SpentUSD)
	}

	// The fourth crosses the cap.
	_, outcome := b.Charge(Step{USD: 0.03}, epoch.Add(4*time.Second))
	if outcome != OutcomeBudgetExhausted {
		t.Errorf("outcome = %q, want OutcomeBudgetExhausted once the total crossed", outcome)
	}
}

func TestChargeWithNoRepairsAllowedRefusesTheFirstRepair(t *testing.T) {
	// P1's shape: the loop generates once and does not repair.
	b := NewBudget(Caps{MaxRepairDepth: 0, MaxUSD: 0.05, WallClock: 30 * time.Second}, epoch)

	_, outcome := b.Charge(Step{USD: 0.001, EntersRepair: true}, epoch.Add(time.Second))
	if outcome != OutcomeDepthExhausted {
		t.Errorf("outcome = %q, want OutcomeDepthExhausted when no repair is permitted", outcome)
	}
}

func TestRemaining(t *testing.T) {
	b := Budget{Caps: caps(), StartedAt: epoch, SpentUSD: 0.02, RepairDepth: 1}

	usd, wall, repairs := b.Remaining(epoch.Add(10 * time.Second))
	if usd < 0.0299 || usd > 0.0301 {
		t.Errorf("remaining usd = %v, want ~0.03", usd)
	}
	if wall != 20*time.Second {
		t.Errorf("remaining wall = %v, want 20s", wall)
	}
	if repairs != 2 {
		t.Errorf("remaining repairs = %d, want 2", repairs)
	}

	// Past every cap, remaining is zero rather than negative: a negative
	// allowance rendered in the UI reads as a bug.
	usd, wall, repairs = Budget{Caps: caps(), StartedAt: epoch, SpentUSD: 1, RepairDepth: 9}.
		Remaining(epoch.Add(time.Hour))
	if usd != 0 || wall != 0 || repairs != 0 {
		t.Errorf("Remaining() past every cap = (%v, %v, %d), want all zero", usd, wall, repairs)
	}
}
