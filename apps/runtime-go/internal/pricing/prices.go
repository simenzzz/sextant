// Package pricing turns provider-reported token counts into dollars.
//
// The invariant this exists to serve: the price table lives in versioned
// config with the date it was last checked, and a price is never written
// inline at a use site. A stale table is then visible — CheckedOn is on every
// entry and travels into cost_ledger.v1 as price_table_date — rather than
// being a number nobody can date.
//
// The second invariant: the ledger records provider-reported token counts,
// never estimates. This package has no tokenizer and no heuristic. A model it
// does not know prices at "unknown", and callers record that rather than
// guessing.
package pricing

import "time"

// CheckedOn is the date the rates below were last verified against
// Anthropic's published pricing.
//
// It is the value that goes into cost_ledger.v1's price_table_date, so every
// dollar figure the eval reports carries the date its rates were checked. Bump
// it only after actually re-reading the pricing page.
const CheckedOn = "2026-08-03"

// Rate is what one model costs, per million tokens.
//
// Per million rather than per token because that is the unit the pricing page
// publishes: storing it in the same unit as the source means transcription
// errors are visible when the two are read side by side, and the division
// happens once, here.
type Rate struct {
	// Model is the exact API model id these rates apply to.
	Model string
	// InputUSDPerMTok is dollars per million input tokens.
	InputUSDPerMTok float64
	// OutputUSDPerMTok is dollars per million output tokens.
	OutputUSDPerMTok float64
	// Note records anything about this rate that a reader needs in order to
	// trust it — a promotional period, a tier restriction, a caveat.
	Note string
}

// usdPerMTok is the divisor turning a per-million rate into a per-token one.
const usdPerMTok = 1_000_000

// table is the price table.
//
// Sonnet 5 is listed at its standard rate although an introductory rate of
// $2/$10 is in force until 2026-08-31. That is deliberate and not an
// oversight: the strong tier is not called until the uncertainty router lands
// at P6, which PLAN.md section 7 puts in weeks 10-12 — after the promotion
// ends. Recording the promotional rate would mean shipping a table that
// silently understates cost from the first day it is actually used, and
// understating is the direction that matters, because cost per correct answer
// is a headline number in the README.
var table = map[string]Rate{
	"claude-haiku-4-5": {
		Model:            "claude-haiku-4-5",
		InputUSDPerMTok:  1.00,
		OutputUSDPerMTok: 5.00,
	},
	// The dated snapshot of the same model. Both ids are live and either may
	// be configured, so both are priced — a configured id with no entry would
	// otherwise make every step's cost unknown.
	"claude-haiku-4-5-20251001": {
		Model:            "claude-haiku-4-5-20251001",
		InputUSDPerMTok:  1.00,
		OutputUSDPerMTok: 5.00,
		Note:             "dated snapshot of claude-haiku-4-5",
	},
	"claude-sonnet-5": {
		Model:            "claude-sonnet-5",
		InputUSDPerMTok:  3.00,
		OutputUSDPerMTok: 15.00,
		Note:             "standard rate; an introductory $2/$10 applies through 2026-08-31",
	},
	"claude-opus-5": {
		Model:            "claude-opus-5",
		InputUSDPerMTok:  5.00,
		OutputUSDPerMTok: 25.00,
	},
}

// Cost returns the dollars a step cost, and whether the price is known.
//
// known is false for a model with no entry. Callers must record that as an
// unknown cost — cost_ledger.v1's cost_known flag exists for exactly this —
// rather than treating the zero as free. A model the table has never heard of
// is far more likely to be an expensive new one than a free one.
func Cost(model string, tokensIn, tokensOut int) (usd float64, known bool) {
	rate, ok := table[model]
	if !ok {
		return 0, false
	}
	// Negative counts cannot come from a conforming provider and must not
	// produce a negative charge, which would credit a run's budget and let it
	// run past its cap.
	if tokensIn < 0 {
		tokensIn = 0
	}
	if tokensOut < 0 {
		tokensOut = 0
	}
	in := float64(tokensIn) * rate.InputUSDPerMTok / usdPerMTok
	out := float64(tokensOut) * rate.OutputUSDPerMTok / usdPerMTok
	return in + out, true
}

// Lookup returns the rate for a model.
func Lookup(model string) (Rate, bool) {
	rate, ok := table[model]
	return rate, ok
}

// Models returns every priced model id. Order is not guaranteed.
func Models() []string {
	out := make([]string, 0, len(table))
	for m := range table {
		out = append(out, m)
	}
	return out
}

// CheckedOnDate parses CheckedOn, so a malformed constant fails a test rather
// than producing a cost_ledger.v1 that violates its own `format: date`.
func CheckedOnDate() (time.Time, error) {
	return time.Parse(time.DateOnly, CheckedOn)
}
