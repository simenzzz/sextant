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

// The adversarial fixture set from PLAN.md section 5.2. Every one must be
// refused, and each is expressed as the summary the sidecar actually produces
// for it — verified against sqlglot when the parse endpoint was written.
func TestValidateRefusesTheAdversarialSet(t *testing.T) {
	tests := []struct {
		name      string
		statement string // documentation: the SQL this summary came from
		summary   gen.ParseSummaryV1
		wantKind  string
	}{
		{
			name:      "DROP TABLE",
			statement: "DROP TABLE users",
			summary: summary(func(s *gen.ParseSummaryV1) {
				s.NodeKinds = []string{"Drop", "Table", "Identifier"}
				s.Tables = []string{"users"}
				s.Functions = []string{}
				s.NormalizedSql = nil
			}),
			wantKind: KindGuardRejected,
		},
		{
			name:      "stacked query",
			statement: "SELECT 1; DELETE FROM users",
			summary: summary(func(s *gen.ParseSummaryV1) {
				s.StatementCount = 2
				s.NodeKinds = []string{"Select", "Literal", "Delete", "Table", "Identifier"}
				s.Tables = []string{"users"}
				s.NormalizedSql = nil
			}),
			wantKind: KindGuardRejected,
		},
		{
			name:      "DELETE",
			statement: "DELETE FROM orders",
			summary: summary(func(s *gen.ParseSummaryV1) {
				s.NodeKinds = []string{"Delete", "Table", "Identifier"}
				s.NormalizedSql = nil
			}),
			wantKind: KindGuardRejected,
		},
		{
			name:      "INSERT",
			statement: "INSERT INTO orders (id) VALUES (1)",
			summary: summary(func(s *gen.ParseSummaryV1) {
				s.NodeKinds = []string{"Insert", "Table", "Identifier", "Values", "Literal"}
				s.NormalizedSql = nil
			}),
			wantKind: KindGuardRejected,
		},
		{
			name:      "UPDATE",
			statement: "UPDATE orders SET status = 'P'",
			summary: summary(func(s *gen.ParseSummaryV1) {
				s.NodeKinds = []string{"Update", "Table", "Identifier", "EQ", "Column", "Literal"}
				s.NormalizedSql = nil
			}),
			wantKind: KindGuardRejected,
		},
		{
			name:      "ATTACH DATABASE",
			statement: "ATTACH DATABASE 'evil.db' AS evil",
			summary: summary(func(s *gen.ParseSummaryV1) {
				s.NodeKinds = []string{"Attach", "Alias", "Identifier", "Literal"}
				s.Tables = []string{}
				s.NormalizedSql = nil
			}),
			wantKind: KindGuardRejected,
		},
		{
			name:      "PRAGMA",
			statement: "PRAGMA table_info(orders)",
			summary: summary(func(s *gen.ParseSummaryV1) {
				s.NodeKinds = []string{"Pragma", "Column", "EQ", "Identifier", "Var"}
				s.Tables = []string{}
				s.NormalizedSql = nil
			}),
			wantKind: KindGuardRejected,
		},
		{
			name:      "COPY",
			statement: "COPY orders FROM '/tmp/x'",
			summary: summary(func(s *gen.ParseSummaryV1) {
				s.NodeKinds = []string{"Copy", "Credentials", "Table", "Identifier", "Literal"}
				s.NormalizedSql = nil
			}),
			wantKind: KindGuardRejected,
		},
		{
			name:      "CREATE TABLE",
			statement: "CREATE TABLE evil (id INTEGER)",
			summary: summary(func(s *gen.ParseSummaryV1) {
				s.NodeKinds = []string{"Create", "Schema", "Table", "Identifier", "ColumnDef"}
				s.Tables = []string{"evil"}
				s.NormalizedSql = nil
			}),
			wantKind: KindGuardRejected,
		},
		{
			// The reason parse_summary.v1 carries `functions` at all. This
			// arrives as node kind "Anonymous", indistinguishable from any
			// other unknown call — only the name gives it away.
			name:      "pg_read_file",
			statement: "SELECT pg_read_file('/etc/passwd') FROM orders",
			summary: summary(func(s *gen.ParseSummaryV1) {
				s.NodeKinds = []string{"Select", "From", "Table", "Identifier", "Anonymous", "Literal", "Limit"}
				s.Functions = []string{"pg_read_file"}
			}),
			wantKind: KindGuardRejected,
		},
		{
			name:      "readfile",
			statement: "SELECT readfile('/etc/passwd') FROM orders",
			summary: summary(func(s *gen.ParseSummaryV1) {
				s.NodeKinds = []string{"Select", "From", "Table", "Identifier", "Anonymous", "Literal", "Limit"}
				s.Functions = []string{"readfile"}
			}),
			wantKind: KindGuardRejected,
		},
		{
			name:      "writefile",
			statement: "SELECT writefile('/tmp/x', 'y') FROM orders",
			summary: summary(func(s *gen.ParseSummaryV1) {
				s.NodeKinds = []string{"Select", "From", "Table", "Identifier", "Anonymous", "Literal", "Limit"}
				s.Functions = []string{"writefile"}
			}),
			wantKind: KindGuardRejected,
		},
		{
			// The guard proves the statement stays inside what generation was
			// shown. A table outside it is a bug or an injection.
			name:      "table outside the retrieved subset",
			statement: "SELECT * FROM sqlite_master LIMIT 500",
			summary: summary(func(s *gen.ParseSummaryV1) {
				s.Tables = []string{"sqlite_master"}
				s.Functions = []string{}
			}),
			wantKind: KindUnknownTable,
		},
		{
			name:      "one known table and one unknown",
			statement: "SELECT * FROM orders JOIN secrets ON 1=1 LIMIT 500",
			summary: summary(func(s *gen.ParseSummaryV1) {
				s.Tables = []string{"orders", "secrets"}
				s.NodeKinds = append(s.NodeKinds, "Join")
				s.Functions = []string{}
			}),
			wantKind: KindUnknownTable,
		},
		{
			// sql_plan.v1 requires at least one table, and a question answered
			// without touching the database is not an answer about the data.
			name:      "no tables at all",
			statement: "SELECT 1 LIMIT 500",
			summary: summary(func(s *gen.ParseSummaryV1) {
				s.Tables = []string{}
				s.Functions = []string{}
				s.NodeKinds = []string{"Select", "Literal", "Limit"}
			}),
			wantKind: KindUnknownTable,
		},
		{
			name:      "unparseable generation",
			statement: "here is your SQL: SELECT FROM WHERE",
			summary: summary(func(s *gen.ParseSummaryV1) {
				s.Ok = false
				s.StatementCount = 0
				s.NodeKinds = []string{}
				s.Tables = []string{}
				s.Functions = []string{}
				s.NormalizedSql = nil
				s.Error = ptr("input is not a valid statement (ParseError)")
			}),
			wantKind: KindSyntaxError,
		},
		{
			// A summary parsed as one dialect proves nothing about another,
			// and the executor will run it against the policy's database.
			name:      "dialect mismatch",
			statement: "SELECT COUNT(*) FROM orders LIMIT 500",
			summary: summary(func(s *gen.ParseSummaryV1) {
				s.Dialect = gen.ParseSummaryV1DialectPostgres
			}),
			wantKind: KindGuardRejected,
		},
		{
			// Fail closed: without a normalized statement there is nothing the
			// guard can prove carries a bound.
			name:      "no normalized statement to execute",
			statement: "SELECT COUNT(*) FROM orders",
			summary: summary(func(s *gen.ParseSummaryV1) {
				s.NormalizedSql = nil
			}),
			wantKind: KindGuardRejected,
		},
		{
			name:      "empty normalized statement",
			statement: "SELECT COUNT(*) FROM orders",
			summary: summary(func(s *gen.ParseSummaryV1) {
				s.NormalizedSql = ptr("")
			}),
			wantKind: KindGuardRejected,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := Validate(tt.summary, toyPolicy())

			if err == nil {
				t.Fatalf("Validate() ACCEPTED %q — it would have executed", tt.statement)
			}
			// A caller must not have to inspect the plan to know it was
			// refused; a zero plan alongside the error is the contract.
			if plan.Sql != "" {
				t.Errorf("Validate() returned a statement alongside a rejection: %q", plan.Sql)
			}

			var rejection *Rejection
			if !errors.As(err, &rejection) {
				t.Fatalf("Validate() error = %v (%T), want a *Rejection so P4 can key a repair on the kind", err, err)
			}
			if rejection.Kind != tt.wantKind {
				t.Errorf("rejection kind = %q, want %q", rejection.Kind, tt.wantKind)
			}
			if rejection.Reason == "" {
				t.Error("rejection carries no reason")
			}
		})
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
		"Grant", "Anonymous",
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
