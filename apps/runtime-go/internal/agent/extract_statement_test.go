package agent

import (
	"errors"
	"testing"
)

// ExtractSQL refuses a candidate that does not open a statement.
//
// This is what stops an unfenced refusal — "I am not sure how to answer that"
// — from travelling to the parser as though it were SQL. The rule is narrow on
// purpose: only SELECT and WITH open a statement this system executes, so the
// keyword test costs nothing in recall and cannot admit prose.

func TestExtractSQLAcceptsEveryStatementFormItExecutes(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "a plain select",
			raw:  "SELECT 1 FROM orders",
			want: "SELECT 1 FROM orders",
		},
		{
			name: "a common table expression",
			raw:  "WITH recent AS (SELECT * FROM orders) SELECT COUNT(*) FROM recent",
			want: "WITH recent AS (SELECT * FROM orders) SELECT COUNT(*) FROM recent",
		},
		{
			// Models are inconsistent about case and the keyword test must not
			// depend on it.
			name: "lower case",
			raw:  "select count(*) from orders",
			want: "select count(*) from orders",
		},
		{
			// The keyword ends at a paren as well as at a space, so a
			// parenthesised sub-select is not mistaken for an unknown word.
			name: "a select whose keyword is followed by a paren",
			raw:  "SELECT(1) FROM orders",
			want: "SELECT(1) FROM orders",
		},
		{
			// Models routinely open a generation with a comment naming what
			// the query does. Judging that line as the first word would refuse
			// the statement under it.
			name: "a leading line comment",
			raw:  "-- Count of cancelled orders\nSELECT COUNT(*) FROM orders",
			want: "-- Count of cancelled orders\nSELECT COUNT(*) FROM orders",
		},
		{
			name: "a leading block comment",
			raw:  "/* cancelled orders */ SELECT COUNT(*) FROM orders",
			want: "/* cancelled orders */ SELECT COUNT(*) FROM orders",
		},
		{
			name: "several leading comments",
			raw:  "-- one\n/* two */\n-- three\nSELECT 1 FROM orders",
			want: "-- one\n/* two */\n-- three\nSELECT 1 FROM orders",
		},
		{
			// A parenthesised query opens with a paren rather than a keyword.
			name: "a parenthesised union",
			raw:  "(SELECT id FROM orders) UNION (SELECT id FROM customers)",
			want: "(SELECT id FROM orders) UNION (SELECT id FROM customers)",
		},
		{
			name: "a fenced statement is judged on its contents",
			raw:  "```sql\nWITH t AS (SELECT 1) SELECT * FROM t\n```",
			want: "WITH t AS (SELECT 1) SELECT * FROM t",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExtractSQL(tt.raw)
			if err != nil {
				t.Fatalf("ExtractSQL() error = %v, want the statement", err)
			}
			if got != tt.want {
				t.Errorf("ExtractSQL() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractSQLRefusesOutputThatIsNotAStatement(t *testing.T) {
	// Every one of these would otherwise reach the parser, which costs a
	// network round trip to be told what the first word already said.
	tests := []struct {
		name string
		raw  string
	}{
		{name: "a refusal", raw: "I'm not sure how to answer that."},
		{name: "an explanation with no query", raw: "The orders table stores status as a single letter."},
		{name: "a question back", raw: "Which date range did you mean?"},
		{name: "prose inside a fence", raw: "```sql\nI cannot answer this question.\n```"},
		{name: "a statement this system does not execute", raw: "DROP TABLE orders"},
		{name: "a comment and nothing else", raw: "-- I could not work this one out"},
		{name: "an unterminated block comment", raw: "/* thinking about it"},
		{name: "a comment followed by prose", raw: "-- note\nI cannot answer this."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExtractSQL(tt.raw)
			if err == nil {
				t.Fatalf("ExtractSQL() returned %q, want ErrNoSQL", got)
			}
			if !errors.Is(err, ErrNoSQL) {
				t.Errorf("error = %v, want ErrNoSQL so the loop can classify it", err)
			}
		})
	}
}
