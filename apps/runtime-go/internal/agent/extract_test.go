package agent

import (
	"errors"
	"strings"
	"testing"
)

// These tests fail on a panic until ExtractSQL is written.

func TestExtractSQL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "bare statement, no decoration",
			raw:  "SELECT COUNT(*) FROM orders WHERE status = 'C'",
			want: "SELECT COUNT(*) FROM orders WHERE status = 'C'",
		},
		{
			name: "surrounding whitespace trimmed",
			raw:  "\n\n  SELECT 1 FROM orders  \n\n",
			want: "SELECT 1 FROM orders",
		},
		{
			name: "sql-tagged fence",
			raw:  "```sql\nSELECT COUNT(*) FROM orders\n```",
			want: "SELECT COUNT(*) FROM orders",
		},
		{
			name: "untagged fence",
			raw:  "```\nSELECT COUNT(*) FROM orders\n```",
			want: "SELECT COUNT(*) FROM orders",
		},
		{
			name: "prose before the fence",
			raw: "To count cancelled orders, note that status stores 'C':\n\n" +
				"```sql\nSELECT COUNT(*) FROM orders WHERE status = 'C'\n```",
			want: "SELECT COUNT(*) FROM orders WHERE status = 'C'",
		},
		{
			name: "prose on both sides",
			raw: "Here is the query:\n```sql\nSELECT 1 FROM orders\n```\n" +
				"This counts every order in the table.",
			want: "SELECT 1 FROM orders",
		},
		{
			// A model that shows its working writes the draft first and the
			// answer last. Taking the first block would systematically pick
			// the attempt the model itself rejected.
			name: "several blocks takes the last",
			raw: "First attempt:\n```sql\nSELECT * FROM order\n```\n" +
				"That table does not exist. Corrected:\n" +
				"```sql\nSELECT * FROM orders LIMIT 10\n```",
			want: "SELECT * FROM orders LIMIT 10",
		},
		{
			// A generation cut off by a token cap opens a fence and never
			// closes it. The statement is very likely complete even when the
			// fence is not, so discarding it would throw away a good answer.
			name: "unterminated fence",
			raw:  "Here you go:\n```sql\nSELECT COUNT(*) FROM orders",
			want: "SELECT COUNT(*) FROM orders",
		},
		{
			name: "multi-line statement keeps its shape",
			raw: "```sql\nSELECT c.name, COUNT(*)\nFROM customers c\n" +
				"JOIN orders o ON o.customer_id = c.id\nGROUP BY c.name\n```",
			want: "SELECT c.name, COUNT(*)\nFROM customers c\n" +
				"JOIN orders o ON o.customer_id = c.id\nGROUP BY c.name",
		},
		{
			name: "trailing semicolon is preserved",
			raw:  "```sql\nSELECT 1 FROM orders;\n```",
			want: "SELECT 1 FROM orders;",
		},
		{
			// A bare fence's first line may still be the language tag.
			name: "language tag on its own line inside a bare fence",
			raw:  "```\nsql\nSELECT 1 FROM orders\n```",
			want: "SELECT 1 FROM orders",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ExtractSQL(tt.raw)
			if err != nil {
				t.Fatalf("ExtractSQL() error = %v, want the statement", err)
			}
			if got != tt.want {
				t.Errorf("ExtractSQL() =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestExtractSQLRefusesRatherThanGuessing(t *testing.T) {
	// An unparseable candidate is something the parser reports cleanly and the
	// guard rejects as a syntax_error. That is a far better outcome than a
	// statement this function invented.
	tests := []struct {
		name string
		raw  string
	}{
		{name: "empty output", raw: ""},
		{name: "whitespace only", raw: "   \n\t\n  "},
		{name: "empty fence", raw: "```sql\n```"},
		{name: "fence containing only whitespace", raw: "```sql\n   \n```"},
		{name: "beyond the contract's length", raw: strings.Repeat("a", MaxGeneratedSQLChars+1)},
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

func TestExtractSQLNeverFabricates(t *testing.T) {
	// This function may only cut, never add or reorder. A statement it
	// invented would be validated and executed as though the model had
	// written it.
	inputs := []string{
		"SELECT 1 FROM orders",
		"```sql\nSELECT 1 FROM orders\n```",
		"prose\n```sql\nSELECT 1 FROM orders\n```\nmore prose",
		"```sql\nSELECT 1 FROM orders",
	}
	for _, raw := range inputs {
		got, err := ExtractSQL(raw)
		if err != nil {
			continue
		}
		if !strings.Contains(raw, got) {
			t.Errorf("ExtractSQL(%q) = %q, which is not a substring of the input", raw, got)
		}
	}
}

func TestExtractSQLIsDeterministic(t *testing.T) {
	// P2's record/replay cannot reproduce a run whose extraction varies.
	const raw = "one:\n```sql\nSELECT 1 FROM orders\n```\ntwo:\n```sql\nSELECT 2 FROM orders\n```"

	first, err1 := ExtractSQL(raw)
	second, err2 := ExtractSQL(raw)
	if (err1 == nil) != (err2 == nil) || first != second {
		t.Errorf("ExtractSQL() is not deterministic: (%q,%v) then (%q,%v)",
			first, err1, second, err2)
	}
}

func TestExtractSQLIsTotal(t *testing.T) {
	// Whatever the model produced, this returns a value and an error and never
	// panics.
	for _, raw := range []string{
		"", "```", "``````", "```sql", "\x00\x01\x02",
		strings.Repeat("`", 100),
		"SELECT 1 -- ```sql nested fence in a comment",
		"```sql\n```sql\nSELECT 1 FROM orders\n```\n```",
	} {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("ExtractSQL(%q) panicked: %v", raw, r)
				}
			}()
			_, _ = ExtractSQL(raw)
		}()
	}
}
