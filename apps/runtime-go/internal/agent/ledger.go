package agent

import (
	"github.com/atombender/go-jsonschema/pkg/types"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts/gen"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/pricing"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/provider"
)

// Ledger accumulates what a run spent.
//
// Mechanical bookkeeping, deliberately separate from the budget: the Budget
// decides whether a run may continue, the Ledger records what it cost. Keeping
// them apart means the accounting stays complete even on a run that ended at a
// cap, which is precisely the run whose cost most needs recording.
type Ledger struct {
	runID   string
	entries []gen.Entry
}

// NewLedger starts a ledger for a run.
func NewLedger(runID string) *Ledger {
	return &Ledger{runID: runID}
}

// Record adds one step.
//
// usage is nil when the provider closed without reporting it. That step's cost
// is then UNKNOWN, not zero, and the entry says so via cost_known — the
// invariant is that the runtime records provider-reported counts and never
// estimates, so a step nobody reported is one the ledger declines to price.
func (l *Ledger) Record(step int, model string, usage *provider.Usage, ms int, repairDepth int, escalated bool) gen.Entry {
	entry := gen.Entry{
		Step:        step,
		Model:       model,
		Ms:          ms,
		RepairDepth: repairDepth,
		Escalated:   escalated,
		CostKnown:   true,
	}

	if usage == nil {
		entry.CostKnown = false
		l.entries = append(l.entries, entry)
		return entry
	}

	entry.TokensIn = usage.TokensIn
	entry.TokensOut = usage.TokensOut
	usd, known := pricing.Cost(model, usage.TokensIn, usage.TokensOut)
	entry.Usd = usd
	entry.CostKnown = known

	l.entries = append(l.entries, entry)
	return entry
}

// RecordCacheHit adds a step the semantic cache served, at no provider cost.
//
// Present at P1 although the cache lands at P7, because the alternative is a
// ledger shape that changes when the cache arrives — and a metric whose
// definition moved is a metric whose history cannot be compared.
func (l *Ledger) RecordCacheHit(step int, ms int) gen.Entry {
	entry := gen.Entry{
		Step: step, Model: "cache", Ms: ms,
		CacheHit: true, CostKnown: true,
	}
	l.entries = append(l.entries, entry)
	return entry
}

// SpentUSD is what the ledger has priced so far.
//
// Steps with an unknown cost contribute nothing, which makes this a LOWER
// BOUND whenever any exist. The budget is enforced against it regardless: a
// bound that undercounts can overspend, but the alternative is enforcing a cap
// against an estimate, and the invariant forbids estimating.
func (l *Ledger) SpentUSD() float64 {
	var total float64
	for _, e := range l.entries {
		total += e.Usd
	}
	return total
}

// Document renders the cost_ledger.v1 that goes on the wire.
func (l *Ledger) Document() gen.CostLedgerV1 {
	totals := gen.Totals{}
	for _, e := range l.entries {
		totals.TokensIn += e.TokensIn
		totals.TokensOut += e.TokensOut
		totals.Usd += e.Usd
		totals.Ms += e.Ms
		if e.CacheHit {
			totals.CacheHits++
		} else {
			totals.ProviderCalls++
		}
		if !e.CostKnown {
			// Above zero, the totals are a lower bound rather than the cost of
			// the run, and anything deriving cost per correct answer has to
			// say so.
			totals.StepsCostUnknown++
		}
	}

	entries := l.entries
	if entries == nil {
		// The contract wants an array. A nil slice marshals to null, which
		// fails validation on a run that made no paid call — a full cache hit,
		// or a run that ended at the deadline before generating.
		entries = []gen.Entry{}
	}

	return gen.CostLedgerV1{
		Schema:         "cost_ledger.v1",
		RunId:          l.runID,
		PriceTableDate: mustPriceTableDate(),
		Entries:        entries,
		Totals:         totals,
	}
}

// Entries returns a copy of the recorded entries.
func (l *Ledger) Entries() []gen.Entry {
	out := make([]gen.Entry, len(l.entries))
	copy(out, l.entries)
	return out
}

// mustPriceTableDate converts the pricing package's CheckedOn constant into the
// contract's date type.
//
// It panics on a malformed constant, which is the right response to a build
// whose price table cannot state when it was checked: the alternative is
// emitting a ledger that fails its own contract on every run. pricing's own
// test parses the same constant, so this fails there first.
func mustPriceTableDate() types.SerializableDate {
	d, err := pricing.CheckedOnDate()
	if err != nil {
		panic("agent: pricing.CheckedOn is not a valid date: " + err.Error())
	}
	return types.SerializableDate{Time: d}
}
