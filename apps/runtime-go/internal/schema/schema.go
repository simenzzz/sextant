// Package schema reads a database's shape and renders it for a prompt.
//
// At P1 there is no retriever, so the whole schema goes into the prompt. The
// rendering here is deliberately the format PLAN.md section 5.1 describes for
// a *table document*, because at P5 the retriever embeds one of these per
// table and retrieves over them. Writing the P1 prompt block in the P5
// document format now means P5 changes what is selected, not what a table
// looks like.
//
// The sampled values are the part that earns its cost. A question about
// cancelled orders is unanswerable from column names alone when the column
// stores 'C'; the values are what make the mapping visible, and PLAN.md
// section 11.1 picked the demo corpus partly on that basis.
package schema

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// SampleValuesPerColumn is how many distinct values each column contributes.
//
// Three, from PLAN.md section 5.1. Enough to reveal an encoding ('P','S','C')
// without turning the prompt into a data dump — and the prompt is charged per
// token on every single question.
const SampleValuesPerColumn = 3

// Column is one column of a table.
type Column struct {
	Name string
	// Type is the declared type, as the driver reported it. SQLite allows a
	// column with no declared type, so this can be empty; an absent type is an
	// honest answer where a guessed one would mislead generation.
	Type       string
	PrimaryKey bool
	NotNull    bool
	// Samples are up to SampleValuesPerColumn distinct non-null values.
	Samples []string
}

// ForeignKey is one outbound reference.
type ForeignKey struct {
	Column    string
	RefTable  string
	RefColumn string
}

// Table is one table's document.
type Table struct {
	Name        string
	Columns     []Column
	ForeignKeys []ForeignKey
}

// Schema is every table in a database.
type Schema struct {
	Tables []Table
}

// TableNames returns every table name, in schema order.
//
// This is what the guard checks a statement's tables against: at P1, with no
// retriever, the "retrieved subset" is the whole schema, so a query touching
// anything outside this list is reaching for something the model was never
// shown — sqlite_master, pg_catalog, or a hallucination.
func (s Schema) TableNames() []string {
	out := make([]string, 0, len(s.Tables))
	for _, t := range s.Tables {
		out = append(out, t.Name)
	}
	return out
}

// Card renders the schema as the prompt block.
//
// Stable and fully determined by the schema's contents: the same database
// always renders identically, which is what lets Fingerprint be a fingerprint
// and what lets the prompt cache hit across questions.
func (s Schema) Card() string {
	var b strings.Builder
	for i, t := range s.Tables {
		if i > 0 {
			b.WriteString("\n")
		}
		writeTable(&b, t)
	}
	return b.String()
}

func writeTable(b *strings.Builder, t Table) {
	fmt.Fprintf(b, "TABLE %s\n", t.Name)

	for _, c := range t.Columns {
		fmt.Fprintf(b, "  %s", c.Name)
		if c.Type != "" {
			fmt.Fprintf(b, " %s", c.Type)
		}
		if c.PrimaryKey {
			b.WriteString(" PRIMARY KEY")
		}
		if c.NotNull && !c.PrimaryKey {
			b.WriteString(" NOT NULL")
		}
		if len(c.Samples) > 0 {
			// The line that makes `status = 'C'` reachable from a question
			// about cancelled orders.
			fmt.Fprintf(b, "  -- e.g. %s", strings.Join(quoteAll(c.Samples), ", "))
		}
		b.WriteString("\n")
	}

	for _, fk := range t.ForeignKeys {
		fmt.Fprintf(b, "  FOREIGN KEY %s -> %s.%s\n", fk.Column, fk.RefTable, fk.RefColumn)
	}
}

func quoteAll(values []string) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = fmt.Sprintf("%q", v)
	}
	return out
}

// Fingerprint is a stable hex digest of the schema's structure.
//
// sql_plan.v1 requires one and the semantic cache at P7 is scoped by it: a
// cached SQL against a renamed column is worse than a cache miss, so the
// fingerprint has to change when anything the SQL could depend on changes.
//
// Structure only — names, types, keys, and foreign keys — and deliberately NOT
// the sampled values. Sampling reads whatever rows the database returns first,
// so folding values in would make the fingerprint change on ordinary inserts
// and invalidate a cache that is still perfectly valid. The schema is what SQL
// is written against; the data is not.
func (s Schema) Fingerprint() string {
	var b strings.Builder
	for _, t := range s.Tables {
		fmt.Fprintf(&b, "T:%s\n", t.Name)
		for _, c := range t.Columns {
			fmt.Fprintf(&b, "C:%s|%s|%t|%t\n", c.Name, c.Type, c.PrimaryKey, c.NotNull)
		}
		for _, fk := range t.ForeignKeys {
			fmt.Fprintf(&b, "F:%s|%s|%s\n", fk.Column, fk.RefTable, fk.RefColumn)
		}
	}
	sum := sha256.Sum256([]byte(b.String()))
	// Hex, lowercase, which is what sql_plan.v1's ^[a-f0-9]+$ requires.
	// Truncated to 32 hex characters: 128 bits is far beyond collision risk
	// for a handful of schemas, and the full 64 makes every trace event
	// carrying it noticeably noisier to read.
	return hex.EncodeToString(sum[:])[:32]
}
