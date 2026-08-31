package guard

import (
	"errors"
	"testing"
	"time"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts/gen"
)

// The guard refuses a policy that could not produce a conforming plan, and it
// refuses a statement bounded to zero rows. Both are fail-closed paths that the
// adversarial set does not reach, because both are about the runtime's own
// configuration rather than about the generation.

func TestValidateRefusesAPolicyThatCannotProduceAConformingPlan(t *testing.T) {
	// sql_plan.v1 bounds limit_value to 1..10000 and statement_timeout_ms to
	// 100..120000. A policy outside those would make the guard emit a plan
	// that fails its own contract at a boundary downstream — after the guard
	// has already said yes.
	tests := []struct {
		name   string
		mutate func(*Policy)
	}{
		{"no row limit", func(p *Policy) { p.RowLimit = 0 }},
		{"a negative row limit", func(p *Policy) { p.RowLimit = -1 }},
		{"a row limit above what a plan may carry", func(p *Policy) { p.RowLimit = 10_001 }},
		{"no statement timeout", func(p *Policy) { p.StatementTimeout = 0 }},
		{"a statement timeout below the floor", func(p *Policy) { p.StatementTimeout = 99 * time.Millisecond }},
		{"a statement timeout above the ceiling", func(p *Policy) { p.StatementTimeout = 3 * time.Minute }},
		{"no schema fingerprint", func(p *Policy) { p.SchemaFingerprint = "" }},
		{"a blank schema fingerprint", func(p *Policy) { p.SchemaFingerprint = "   " }},
		{"no allowed tables", func(p *Policy) { p.AllowedTables = nil }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy := toyPolicy()
			tt.mutate(&policy)

			plan, err := Validate(summary(), policy)
			if err == nil {
				t.Fatalf("Validate() accepted a policy that cannot produce a conforming plan: %+v", plan)
			}
			if plan.Sql != "" {
				t.Errorf("Validate() returned a statement alongside a rejection: %q", plan.Sql)
			}

			var rejection *Rejection
			if !errors.As(err, &rejection) {
				t.Fatalf("error = %v (%T), want a *Rejection", err, err)
			}
			// A misconfigured runtime is not a bad generation, so it must not
			// reach the P4 repair loop as a prompt about the model's SQL.
			if rejection.Kind != KindInternalError {
				t.Errorf("kind = %q, want %q — this is the runtime's fault, not the model's",
					rejection.Kind, KindInternalError)
			}
		})
	}
}

func TestValidateRefusesAStatementBoundedToZeroRows(t *testing.T) {
	// The parser reports LIMIT 0 as a resolved constant. Raising it to the cap
	// would describe the plan wrongly — the statement returns nothing — and
	// sql_plan.v1 forbids a limit_value of 0, so the honest answer is to
	// refuse.
	_, err := Validate(summary(func(s *gen.ParseSummaryV1) {
		s.HasLimit = ptr(true)
		s.LimitValue = ptr(0)
		s.LimitInjected = false
		s.NormalizedSql = ptr("SELECT id FROM orders LIMIT 0")
	}), toyPolicy())

	if err == nil {
		t.Fatal("Validate() accepted a statement bounded to zero rows")
	}
	var rejection *Rejection
	if !errors.As(err, &rejection) {
		t.Fatalf("error = %v (%T), want a *Rejection", err, err)
	}
	if rejection.Kind != KindGuardRejected {
		t.Errorf("kind = %q, want %q", rejection.Kind, KindGuardRejected)
	}
}

func TestValidateFallsBackWhenTheParserGaveNoReason(t *testing.T) {
	// ok is false and error is absent or blank. The guard still has to say
	// something, and it must still be a syntax_error so the P4 repair loop
	// keys the right prompt.
	for _, tt := range []struct {
		name    string
		failure func(*gen.ParseSummaryV1)
	}{
		{"no reason at all", func(s *gen.ParseSummaryV1) { s.Ok = false; s.Error = nil }},
		{"a blank reason", func(s *gen.ParseSummaryV1) { s.Ok = false; s.Error = ptr("   ") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Validate(summary(tt.failure), toyPolicy())

			var rejection *Rejection
			if !errors.As(err, &rejection) {
				t.Fatalf("error = %v (%T), want a *Rejection", err, err)
			}
			if rejection.Kind != KindSyntaxError {
				t.Errorf("kind = %q, want %q", rejection.Kind, KindSyntaxError)
			}
			if rejection.Reason == "" {
				t.Error("rejection carries no reason")
			}
		})
	}
}

func TestValidateRefusesASummaryThatEstablishesNoStructure(t *testing.T) {
	// parse_summary.v1 permits an empty node_kinds array, and an allowlist
	// walk over an empty list passes vacuously. The guard's primary gate must
	// not be satisfied by the absence of evidence.
	_, err := Validate(summary(func(s *gen.ParseSummaryV1) {
		s.NodeKinds = []string{}
	}), toyPolicy())

	if err == nil {
		t.Fatal("Validate() accepted a summary naming no node kind")
	}
	var rejection *Rejection
	if !errors.As(err, &rejection) {
		t.Fatalf("error = %v (%T), want a *Rejection", err, err)
	}
	if rejection.Kind != KindGuardRejected {
		t.Errorf("kind = %q, want %q", rejection.Kind, KindGuardRejected)
	}
}

func TestValidateRefusesASummaryThatContradictsItself(t *testing.T) {
	// The flags describe what the rewrite did to the statement the guard is
	// about to authorise. A summary whose flags disagree with the limit it
	// reported is a sidecar that is broken or lying, and the guard cannot
	// prove what it would be signing.
	tests := []struct {
		name   string
		mutate func(*gen.ParseSummaryV1)
	}{
		{
			name: "claims it added a limit the statement already had",
			mutate: func(s *gen.ParseSummaryV1) {
				s.HasLimit = ptr(true)
				s.LimitValue = ptr(10)
				s.LimitInjected = true
				s.NormalizedSql = ptr("SELECT id FROM orders LIMIT 10")
			},
		},
		{
			name: "claims it clamped a limit already under the cap",
			mutate: func(s *gen.ParseSummaryV1) {
				s.HasLimit = ptr(true)
				s.LimitValue = ptr(10)
				s.LimitInjected = false
				s.LimitClamped = true
				s.NormalizedSql = ptr("SELECT id FROM orders LIMIT 10")
			},
		},
		{
			name: "reports a limit over the cap but denies clamping it",
			mutate: func(s *gen.ParseSummaryV1) {
				s.HasLimit = ptr(true)
				s.LimitValue = ptr(9999)
				s.LimitInjected = false
				s.LimitClamped = false
				s.NormalizedSql = ptr("SELECT id FROM orders LIMIT 500")
			},
		},
		{
			name: "denies adding a limit to a statement that had none",
			mutate: func(s *gen.ParseSummaryV1) {
				s.HasLimit = ptr(false)
				s.LimitInjected = false
				s.NormalizedSql = ptr("SELECT id FROM orders LIMIT 500")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Validate(summary(tt.mutate), toyPolicy())
			if err == nil {
				t.Fatalf("Validate() accepted a self-contradictory summary: %+v", plan)
			}
			var rejection *Rejection
			if !errors.As(err, &rejection) {
				t.Fatalf("error = %v (%T), want a *Rejection", err, err)
			}
			// A broken sidecar is the runtime's fault, so it must not reach
			// the P4 repair loop as a prompt about the model's SQL.
			if rejection.Kind != KindInternalError {
				t.Errorf("kind = %q, want %q", rejection.Kind, KindInternalError)
			}
		})
	}
}
