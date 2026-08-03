package guard

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts/gen"
)

// These tests fail on a panic until Validate is written. That red is the
// worklist, not a broken pipeline — see .claude/CLAUDE.md, "The two-job CI
// gate".

func toyPolicy() Policy {
	return Policy{
		Dialect:           gen.SqlPlanV1DialectSqlite,
		AllowedTables:     []string{"customers", "order_items", "orders", "products"},
		RowLimit:          500,
		StatementTimeout:  10 * time.Second,
		SchemaFingerprint: "9f2b7c1a4e",
	}
}

// summary builds an otherwise-acceptable parse summary, so each test can
// change exactly the one thing it is about.
func summary(mutate ...func(*gen.ParseSummaryV1)) gen.ParseSummaryV1 {
	normalized := "SELECT COUNT(*) FROM orders WHERE status = 'C' LIMIT 500"
	hasLimit := false
	s := gen.ParseSummaryV1{
		Schema:         "parse_summary.v1",
		Ok:             true,
		Dialect:        gen.ParseSummaryV1DialectSqlite,
		StatementCount: 1,
		NodeKinds:      []string{"Select", "From", "Table", "Identifier", "Count", "Where", "EQ", "Column", "Literal", "Limit"},
		Tables:         []string{"orders"},
		Functions:      []string{"count"},
		HasLimit:       &hasLimit,
		NormalizedSql:  &normalized,
		LimitInjected:  true,
	}
	for _, m := range mutate {
		m(&s)
	}
	return s
}

func ptr[T any](v T) *T { return &v }

func TestValidateAcceptsAWellFormedSelect(t *testing.T) {
	plan, err := Validate(summary(), toyPolicy())
	if err != nil {
		t.Fatalf("Validate() rejected a well-formed SELECT: %v", err)
	}

	if plan.Sql != "SELECT COUNT(*) FROM orders WHERE status = 'C' LIMIT 500" {
		// The plan's Sql is what executes. Returning the raw generation would
		// mean the LIMIT the guard believes it enforced is not on the
		// statement that actually runs.
		t.Errorf("plan.Sql = %q, want the normalized statement", plan.Sql)
	}
	if plan.LimitValue != 500 {
		t.Errorf("plan.LimitValue = %d, want 500", plan.LimitValue)
	}
	if !plan.LimitInjected {
		t.Error("plan.LimitInjected = false, but the summary said a limit was added")
	}
	if plan.StatementTimeoutMs != 10_000 {
		t.Errorf("plan.StatementTimeoutMs = %d, want 10000 from the policy",
			plan.StatementTimeoutMs)
	}
	if plan.SchemaFingerprint != "9f2b7c1a4e" {
		t.Errorf("plan.SchemaFingerprint = %q, want the policy's", plan.SchemaFingerprint)
	}
}

func TestValidateProducesAConformingPlan(t *testing.T) {
	plan, err := Validate(summary(), toyPolicy())
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshalling plan: %v", err)
	}
	// The executor takes this document and runs it. A plan that violates its
	// own contract would fail validation at a boundary downstream, after the
	// guard has already said yes.
	if err := contracts.Validate(contracts.SQLPlanV1, raw); err != nil {
		t.Fatalf("sql_plan.v1 validation failed: %v\n%s", err, raw)
	}
}

func TestValidateNeverQuotesTheOffendingSQL(t *testing.T) {
	// The reason travels to a browser. Naming the rule that was broken is
	// useful; echoing model output is a vector.
	const marker = "zzzsecretzzz"
	s := summary(func(s *gen.ParseSummaryV1) {
		s.NodeKinds = []string{"Drop", "Table"}
		s.Tables = []string{marker}
		s.NormalizedSql = ptr("DROP TABLE " + marker)
		s.Error = ptr("failed near " + marker)
	})

	_, err := Validate(s, toyPolicy())
	if err == nil {
		t.Fatal("Validate() accepted a DROP")
	}
	var rejection *Rejection
	if errors.As(err, &rejection) && strings.Contains(rejection.Reason, marker) {
		t.Errorf("rejection reason echoed the statement: %q", rejection.Reason)
	}
}

func TestValidateSettlesTheLimit(t *testing.T) {
	tests := []struct {
		name         string
		mutate       func(*gen.ParseSummaryV1)
		wantLimit    int
		wantInjected bool
		wantClamped  bool
	}{
		{
			name: "injected when the generation had none",
			mutate: func(s *gen.ParseSummaryV1) {
				s.HasLimit = ptr(false)
				s.LimitInjected = true
				s.NormalizedSql = ptr("SELECT id FROM orders LIMIT 500")
			},
			wantLimit: 500, wantInjected: true,
		},
		{
			name: "clamped when the generation asked for more than the cap",
			mutate: func(s *gen.ParseSummaryV1) {
				s.HasLimit = ptr(true)
				s.LimitValue = ptr(9999)
				s.LimitInjected = false
				s.LimitClamped = true
				s.NormalizedSql = ptr("SELECT id FROM orders LIMIT 500")
			},
			wantLimit: 500, wantClamped: true,
		},
		{
			// The generation's own smaller limit is respected. Raising it to
			// the cap would return more rows than the query asked for.
			name: "left alone when the generation asked for less",
			mutate: func(s *gen.ParseSummaryV1) {
				s.HasLimit = ptr(true)
				s.LimitValue = ptr(10)
				s.LimitInjected = false
				s.LimitClamped = false
				s.NormalizedSql = ptr("SELECT id FROM orders LIMIT 10")
			},
			wantLimit: 10,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Validate(summary(tt.mutate), toyPolicy())
			if err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if plan.LimitValue != tt.wantLimit {
				t.Errorf("plan.LimitValue = %d, want %d", plan.LimitValue, tt.wantLimit)
			}
			if plan.LimitInjected != tt.wantInjected {
				t.Errorf("plan.LimitInjected = %v, want %v", plan.LimitInjected, tt.wantInjected)
			}
			if plan.LimitClamped != tt.wantClamped {
				t.Errorf("plan.LimitClamped = %v, want %v", plan.LimitClamped, tt.wantClamped)
			}
		})
	}
}

func TestValidateTreatsANonConstantLimitAsNoBound(t *testing.T) {
	// The parser reports HasLimit true but cannot resolve the value. A limit
	// nothing can evaluate before execution is not a bound, so the guard must
	// not trust it as one.
	plan, err := Validate(summary(func(s *gen.ParseSummaryV1) {
		s.HasLimit = ptr(true)
		s.LimitValue = nil
		s.LimitInjected = true
		s.NormalizedSql = ptr("SELECT id FROM orders LIMIT 500")
	}), toyPolicy())
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if plan.LimitValue > 500 {
		t.Errorf("plan.LimitValue = %d, want the cap applied over an unresolvable limit",
			plan.LimitValue)
	}
}

func TestValidateComparesTableNamesCaseInsensitively(t *testing.T) {
	// The sidecar lowercases; the policy's list comes from schema
	// introspection and preserves the database's casing. A case-sensitive
	// comparison would reject every legitimate query on a schema whose tables
	// are not already lowercase.
	policy := toyPolicy()
	policy.AllowedTables = []string{"Orders", "Customers"}

	if _, err := Validate(summary(), policy); err != nil {
		t.Errorf("Validate() rejected `orders` against an allowlist containing `Orders`: %v", err)
	}
}

func TestAllowlistsRefuseTheThingsTheyMustRefuse(t *testing.T) {
	// A sqlglot upgrade that renames or adds a node kind must cause a
	// rejection rather than a silent pass. These assertions are what turn that
	// from a hope into a test.
	for _, kind := range []string{
		"Drop", "Delete", "Insert", "Update", "Create", "Alter",
		"Attach", "Detach", "Pragma", "Copy", "Command", "Transaction",
		"Grant",
		// "Anonymous" is deliberately NOT in this list. It was, until the
		// corpus in corpus_test.go showed that printf and julianday arrive as
		// that kind alongside readfile and load_extension — so refusing it
		// would refuse ordinary SQL. The function allowlist is what separates
		// them, which is what TestTheFunctionAllowlistIsWhatCatchesFileFunctions
		// pins down.
	} {
		if AllowedNodeKinds[kind] {
			t.Errorf("AllowedNodeKinds permits %q; a statement containing it would execute", kind)
		}
	}
	for _, fn := range []string{
		"pg_read_file", "readfile", "writefile", "lo_import", "lo_export",
		"pg_ls_dir", "load_extension", "system",
	} {
		if AllowedFunctions[fn] {
			t.Errorf("AllowedFunctions permits %q; PLAN.md section 5.2 requires it refused", fn)
		}
	}
	// And it must still admit an ordinary analytical query, or the guard is
	// safe and useless.
	for _, kind := range []string{"Select", "From", "Where", "Join", "Group", "Order", "Limit", "Count"} {
		if !AllowedNodeKinds[kind] {
			t.Errorf("AllowedNodeKinds refuses %q; an ordinary question could not be answered", kind)
		}
	}
}
