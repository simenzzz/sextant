package pricing

import (
	"math"
	"testing"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/config"
)

func TestCost(t *testing.T) {
	tests := []struct {
		name      string
		model     string
		usage     Usage
		wantUSD   float64
		wantKnown bool
	}{
		{
			name:  "haiku at a million in and out",
			model: "claude-haiku-4-5", usage: Usage{TokensIn: 1_000_000, TokensOut: 1_000_000},
			wantUSD: 6.00, wantKnown: true,
		},
		{
			name:  "haiku at a realistic step",
			model: "claude-haiku-4-5", usage: Usage{TokensIn: 3_000, TokensOut: 150},
			wantUSD: 0.003 + 0.00075, wantKnown: true,
		},
		{
			name:  "the dated snapshot prices the same as the alias",
			model: "claude-haiku-4-5-20251001", usage: Usage{TokensIn: 1_000_000, TokensOut: 1_000_000},
			wantUSD: 6.00, wantKnown: true,
		},
		{
			name:  "sonnet 5 at its standard rate",
			model: "claude-sonnet-5", usage: Usage{TokensIn: 1_000_000, TokensOut: 1_000_000},
			wantUSD: 18.00, wantKnown: true,
		},
		{
			name:  "a free step still has a known price",
			model: "claude-haiku-4-5", usage: Usage{TokensIn: 0, TokensOut: 0},
			wantUSD: 0, wantKnown: true,
		},
		{
			// An unpriced model must not read as free. The ledger's cost_known
			// flag exists for this: a model the table has never heard of is far
			// more likely to be an expensive new one than a free one.
			name:  "an unpriced model is unknown, not free",
			model: "some-model-shipped-after-this-table", usage: Usage{TokensIn: 5_000, TokensOut: 500},
			wantUSD: 0, wantKnown: false,
		},
		{
			name:  "an empty model id is unknown",
			model: "", usage: Usage{TokensIn: 1, TokensOut: 1},
			wantUSD: 0, wantKnown: false,
		},
		{
			// A negative count cannot come from a conforming provider, and a
			// negative charge would credit the run's budget and let it run past
			// its cap.
			name:  "negative counts are floored rather than credited",
			model: "claude-haiku-4-5", usage: Usage{TokensIn: -1_000_000, TokensOut: 1_000_000},
			wantUSD: 5.00, wantKnown: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			usd, known := Cost(tt.model, tt.usage)

			if known != tt.wantKnown {
				t.Fatalf("Cost(%q) known = %v, want %v", tt.model, known, tt.wantKnown)
			}
			// Float comparison with a tolerance well below a cent: these are
			// dollar figures, and exact equality on binary floats would make
			// the test about IEEE 754 rather than about the price table.
			if math.Abs(usd-tt.wantUSD) > 1e-9 {
				t.Errorf("Cost(%q, %+v) = %.10f, want %.10f",
					tt.model, tt.usage, usd, tt.wantUSD)
			}
		})
	}
}

func TestCostIsNeverNegative(t *testing.T) {
	for _, model := range Models() {
		usd, known := Cost(model, Usage{TokensIn: -5, TokensOut: -5,
			CacheReadTokens: -5, CacheWriteTokens: -5})
		if !known {
			t.Errorf("Cost(%q) reported unknown for a model in the table", model)
		}
		if usd < 0 {
			t.Errorf("Cost(%q, -5, -5) = %v, want a non-negative charge", model, usd)
		}
	}
}

// The configured models must be priced, or every run's cost is unknown from
// the first request. This is the check that catches a model rename in config
// that nobody mirrored here.
func TestConfiguredDefaultModelsArePriced(t *testing.T) {
	for _, model := range []string{config.DefaultCheapModel, config.DefaultStrongModel} {
		if _, ok := Lookup(model); !ok {
			t.Errorf("config default model %q has no price table entry — "+
				"every step on it would record an unknown cost", model)
		}
	}
}

func TestEveryRateIsUsableAndSelfConsistent(t *testing.T) {
	for model, rate := range table {
		t.Run(model, func(t *testing.T) {
			if rate.Model != model {
				t.Errorf("entry keyed %q carries Model %q — a copy-paste slip that "+
					"would misattribute the rate in any diagnostic that prints it",
					model, rate.Model)
			}
			if rate.InputUSDPerMTok <= 0 || rate.OutputUSDPerMTok <= 0 {
				t.Errorf("rate %+v has a non-positive price; a zero would silently "+
					"make every step on this model free", rate)
			}
			// True of every Claude model and a cheap guard against a
			// transposed pair, which would otherwise look plausible.
			if rate.OutputUSDPerMTok < rate.InputUSDPerMTok {
				t.Errorf("rate %+v prices output below input — likely transposed", rate)
			}
		})
	}
}

func TestCheckedOnIsAParseableDate(t *testing.T) {
	// price_table_date carries `format: date` in cost_ledger.v1 and formats are
	// asserted, so a malformed constant here would fail contract validation on
	// every ledger the runtime emits.
	if _, err := CheckedOnDate(); err != nil {
		t.Fatalf("CheckedOn = %q is not a valid ISO 8601 date: %v", CheckedOn, err)
	}
}

func TestCostPricesTheThreeInputClassesAtTheirOwnRates(t *testing.T) {
	// The reason provider.Usage keeps them apart. Summing them into one figure
	// overstates reads by 10x and understates writes by 25%, and leaves the
	// dollar amount uncheckable by anyone reading the ledger.
	const million = 1_000_000

	standard, _ := Cost("claude-haiku-4-5", Usage{TokensIn: million})
	read, _ := Cost("claude-haiku-4-5", Usage{CacheReadTokens: million})
	write, _ := Cost("claude-haiku-4-5", Usage{CacheWriteTokens: million})

	if math.Abs(standard-1.00) > 1e-9 {
		t.Errorf("standard input = %v, want 1.00", standard)
	}
	if math.Abs(read-0.10) > 1e-9 {
		t.Errorf("cache read = %v, want 0.10 (a tenth of standard)", read)
	}
	if math.Abs(write-1.25) > 1e-9 {
		t.Errorf("cache write = %v, want 1.25 (a premium over standard)", write)
	}
	// And the ordering that makes caching worth doing at all.
	if !(read < standard && standard < write) {
		t.Errorf("rates are not read < standard < write: %v, %v, %v", read, standard, write)
	}
}

func TestCostSumsTheClassesRatherThanPickingOne(t *testing.T) {
	usd, known := Cost("claude-haiku-4-5", Usage{
		TokensIn: 1000, CacheReadTokens: 4000, CacheWriteTokens: 2000, TokensOut: 500,
	})
	if !known {
		t.Fatal("Cost() reported unknown for a priced model")
	}
	// 1000*1.00 + 4000*0.10 + 2000*1.25 + 500*5.00 per million.
	want := (1000*1.00 + 4000*0.10 + 2000*1.25 + 500*5.00) / 1_000_000
	if math.Abs(usd-want) > 1e-12 {
		t.Errorf("Cost() = %.12f, want %.12f", usd, want)
	}
}

func TestAMostlyCachedStepCostsFarLessThanAnUncachedOne(t *testing.T) {
	// The property the whole feature exists for, stated as a test rather than
	// assumed from the multipliers.
	uncached, _ := Cost("claude-haiku-4-5", Usage{TokensIn: 10_000, TokensOut: 100})
	cached, _ := Cost("claude-haiku-4-5", Usage{CacheReadTokens: 10_000, TokensOut: 100})

	if cached >= uncached {
		t.Errorf("a cache-read step (%v) did not cost less than an uncached one (%v)", cached, uncached)
	}
}
