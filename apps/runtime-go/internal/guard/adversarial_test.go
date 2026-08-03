package guard

import (
	"errors"
	"testing"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts/gen"
)

// The adversarial half of the guard suite. Helpers live in guard_test.go;
// split out to keep both files under the 400-line rule.

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
