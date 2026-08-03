package schema

import (
	"context"
	"database/sql"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// toyDB opens the committed fixture read-only.
//
// The whole suite runs against infra/fixtures/toy.sqlite, so no test needs a
// network, a credential, or a download — and the schema assertions below are
// about a database that is checked in and cannot drift underneath them.
func toyDB(t *testing.T) *sql.DB {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file to resolve the fixture path")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..")
	path := filepath.Join(root, "infra", "fixtures", "toy.sqlite")

	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=query_only(1)")
	if err != nil {
		t.Fatalf("opening the toy fixture: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Fatalf("connecting to %s: %v", path, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func introspectToy(t *testing.T) Schema {
	t.Helper()
	s, err := IntrospectSQLite(context.Background(), toyDB(t))
	if err != nil {
		t.Fatalf("IntrospectSQLite() error = %v", err)
	}
	return s
}

func TestIntrospectFindsEveryUserTable(t *testing.T) {
	got := introspectToy(t).TableNames()
	want := []string{"customers", "order_items", "orders", "products"}

	if len(got) != len(want) {
		t.Fatalf("TableNames() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			// Sorted, not natural order: the card rendering and the
			// fingerprint both depend on this being identical every time.
			t.Fatalf("TableNames() = %v, want %v (sorted)", got, want)
		}
	}
}

func TestIntrospectExcludesSQLiteInternals(t *testing.T) {
	for _, name := range introspectToy(t).TableNames() {
		if strings.HasPrefix(name, "sqlite_") {
			t.Errorf("internal table %q was introspected; it would then be in the "+
				"guard's allowed table set and a query could reach it", name)
		}
	}
}

func tableByName(t *testing.T, s Schema, name string) Table {
	t.Helper()
	for _, tbl := range s.Tables {
		if tbl.Name == name {
			return tbl
		}
	}
	t.Fatalf("table %q not found", name)
	return Table{}
}

func columnByName(t *testing.T, tbl Table, name string) Column {
	t.Helper()
	for _, c := range tbl.Columns {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("column %q not found on %q", name, tbl.Name)
	return Column{}
}

func TestIntrospectReadsColumnsAndKeys(t *testing.T) {
	orders := tableByName(t, introspectToy(t), "orders")

	id := columnByName(t, orders, "id")
	if !id.PrimaryKey {
		t.Error("orders.id was not reported as a primary key")
	}
	status := columnByName(t, orders, "status")
	if status.Type != "TEXT" {
		t.Errorf("orders.status type = %q, want TEXT", status.Type)
	}
	if !status.NotNull {
		t.Error("orders.status was not reported NOT NULL")
	}
}

func TestIntrospectSamplesTheValuesThatMakeAColumnLegible(t *testing.T) {
	// The whole argument for sampling. `status` stores 'C', not 'cancelled',
	// so a question about cancelled orders is unanswerable from column names
	// alone — these values are the only thing that makes the mapping visible
	// to generation.
	status := columnByName(t, tableByName(t, introspectToy(t), "orders"), "status")

	if len(status.Samples) == 0 {
		t.Fatal("orders.status has no sampled values; " +
			"a question about cancelled orders is then unanswerable from the card")
	}
	if len(status.Samples) > SampleValuesPerColumn {
		t.Errorf("orders.status sampled %d values, want at most %d",
			len(status.Samples), SampleValuesPerColumn)
	}
	for _, v := range status.Samples {
		if v != "C" && v != "P" && v != "S" {
			t.Errorf("sampled %q, want one of the encoded status values", v)
		}
	}
}

func TestIntrospectReadsForeignKeys(t *testing.T) {
	// order_items is the bridge table: the case pure similarity retrieval
	// misses and foreign-key expansion catches at P5. Its edges have to be
	// visible in the card for that to be possible at all.
	items := tableByName(t, introspectToy(t), "order_items")

	if len(items.ForeignKeys) != 2 {
		t.Fatalf("order_items has %d foreign keys, want 2", len(items.ForeignKeys))
	}
	targets := map[string]string{}
	for _, fk := range items.ForeignKeys {
		targets[fk.Column] = fk.RefTable
	}
	if targets["order_id"] != "orders" {
		t.Errorf("order_items.order_id -> %q, want orders", targets["order_id"])
	}
	if targets["product_id"] != "products" {
		t.Errorf("order_items.product_id -> %q, want products", targets["product_id"])
	}
}

func TestCardIsRenderableAndCarriesWhatGenerationNeeds(t *testing.T) {
	card := introspectToy(t).Card()

	for _, want := range []string{
		"TABLE orders", "TABLE order_items", "TABLE customers", "TABLE products",
		"FOREIGN KEY order_id -> orders.id",
		"status",
	} {
		if !strings.Contains(card, want) {
			t.Errorf("card is missing %q\n---\n%s", want, card)
		}
	}
	// The encoded value has to appear literally, or generation cannot write
	// `status = 'C'`.
	if !strings.Contains(card, `"C"`) {
		t.Errorf("card does not show the encoded status values\n---\n%s", card)
	}
}

func TestCardIsStableAcrossReads(t *testing.T) {
	// The prompt cache and the fingerprint both depend on this. A card that
	// reordered between reads would produce a different prompt for the same
	// database and quietly cost a cache write on every question.
	first := introspectToy(t).Card()
	second := introspectToy(t).Card()

	if first != second {
		t.Error("Card() differed between two reads of the same database")
	}
}

func TestFingerprintIsStableAndSchemaShaped(t *testing.T) {
	s := introspectToy(t)
	fp := s.Fingerprint()

	if fp != s.Fingerprint() {
		t.Error("Fingerprint() is not stable across calls")
	}
	// sql_plan.v1 requires ^[a-f0-9]+$; an uppercase digest would fail the
	// contract at the guard.
	for _, r := range fp {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("Fingerprint() = %q contains %q, want lowercase hex", fp, r)
		}
	}
	if len(fp) != 32 {
		t.Errorf("Fingerprint() length = %d, want 32", len(fp))
	}
}

func TestFingerprintChangesWithStructureButNotWithData(t *testing.T) {
	base := Schema{Tables: []Table{{
		Name:    "orders",
		Columns: []Column{{Name: "id", Type: "INTEGER", PrimaryKey: true}},
	}}}

	// Sampling reads whatever rows come back first, so folding values into the
	// fingerprint would invalidate the P7 cache on ordinary inserts — against a
	// schema that has not changed at all.
	withSamples := Schema{Tables: []Table{{
		Name: "orders",
		Columns: []Column{{
			Name: "id", Type: "INTEGER", PrimaryKey: true,
			Samples: []string{"1", "2", "3"},
		}},
	}}}
	if base.Fingerprint() != withSamples.Fingerprint() {
		t.Error("Fingerprint() changed when only the sampled data changed; " +
			"the P7 cache would be invalidated by an ordinary insert")
	}

	// A renamed column must change it. A cached SQL against a renamed column
	// is worse than a cache miss.
	renamed := Schema{Tables: []Table{{
		Name:    "orders",
		Columns: []Column{{Name: "order_id", Type: "INTEGER", PrimaryKey: true}},
	}}}
	if base.Fingerprint() == renamed.Fingerprint() {
		t.Error("Fingerprint() did not change when a column was renamed")
	}

	retyped := Schema{Tables: []Table{{
		Name:    "orders",
		Columns: []Column{{Name: "id", Type: "TEXT", PrimaryKey: true}},
	}}}
	if base.Fingerprint() == retyped.Fingerprint() {
		t.Error("Fingerprint() did not change when a column type changed")
	}
}

func TestQuoteIdent(t *testing.T) {
	// Identifiers cannot be bind parameters, so PRAGMA and the sample query
	// build them into the statement. A table named `order` or `my"table` must
	// not change the shape of the statement around it.
	tests := []struct{ in, want string }{
		{"orders", `"orders"`},
		{"order", `"order"`},
		{`my"table`, `"my""table"`},
		{`a"; DROP TABLE users; --`, `"a""; DROP TABLE users; --"`},
	}
	for _, tt := range tests {
		if got := quoteIdent(tt.in); got != tt.want {
			t.Errorf("quoteIdent(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestTruncateSplitsOnRunesNotBytes(t *testing.T) {
	// Slicing UTF-8 at a byte offset can split a codepoint and put a
	// replacement character into the prompt. The corpus PLAN.md section 11.1
	// chose stores Portuguese category names, so this is a real case.
	long := strings.Repeat("é", maxSampleChars+10)
	got := truncate(long, maxSampleChars)

	if strings.Contains(got, "�") {
		t.Errorf("truncate() produced a replacement character: %q", got)
	}
	if r := []rune(got); len(r) != maxSampleChars+1 { // +1 for the ellipsis
		t.Errorf("truncate() returned %d runes, want %d", len(r), maxSampleChars+1)
	}
	if short := truncate("abc", maxSampleChars); short != "abc" {
		t.Errorf("truncate() altered a short value: %q", short)
	}
}
