package executor

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/clock"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts/gen"
)

var epoch = time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)

func toyPath(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file to resolve the fixture path")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..")
	return filepath.Join(root, "infra", "fixtures", "toy.sqlite")
}

func newToyExecutor(t *testing.T) (*Executor, *clock.Fake) {
	t.Helper()
	db, err := OpenSQLiteReadOnly(toyPath(t))
	if err != nil {
		t.Fatalf("OpenSQLiteReadOnly() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })

	clk := clock.NewFake(epoch)
	e, err := New(db, clk, DefaultMaxResultBytes)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return e, clk
}

func plan(sql string) gen.SqlPlanV1 {
	return gen.SqlPlanV1{
		Schema:             "sql_plan.v1",
		Sql:                sql,
		Dialect:            gen.SqlPlanV1DialectSqlite,
		Tables:             []string{"orders"},
		LimitValue:         500,
		StatementTimeoutMs: 5000,
		SchemaFingerprint:  "9f2b7c1a4e",
	}
}

func TestExecuteReturnsRows(t *testing.T) {
	e, _ := newToyExecutor(t)

	// The question PLAN.md uses as P1's end-to-end check. It is only
	// answerable because the schema card shows that `status` holds 'C'.
	got, err := e.Execute(context.Background(), "r_1",
		plan("SELECT COUNT(*) AS cancelled FROM orders WHERE status = 'C' LIMIT 500"))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if got.RowCount != 1 || len(got.Rows) != 1 {
		t.Fatalf("RowCount = %d, rows = %d, want 1 and 1", got.RowCount, len(got.Rows))
	}
	if len(got.Columns) != 1 || got.Columns[0].Name != "cancelled" {
		t.Fatalf("Columns = %+v, want one named cancelled", got.Columns)
	}
	// The toy fixture has exactly two cancelled orders.
	if n, ok := got.Rows[0][0].(int64); !ok || n != 2 {
		t.Errorf("cancelled orders = %v (%T), want int64(2)", got.Rows[0][0], got.Rows[0][0])
	}
}

func TestExecuteProducesAConformingResultSet(t *testing.T) {
	e, _ := newToyExecutor(t)

	got, err := e.Execute(context.Background(), "r_1",
		plan("SELECT id, status, placed_at FROM orders ORDER BY id LIMIT 500"))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	// The frame goes on the wire and the browser validates it. A result set
	// that violates its own contract would be dropped there and look like a
	// network fault rather than a bug here.
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshalling result: %v", err)
	}
	if err := contracts.Validate(contracts.ResultSetV1, raw); err != nil {
		t.Fatalf("result_set.v1 validation failed: %v\n%s", err, raw)
	}
}

func TestExecutePreservesNullsRatherThanCoercingThem(t *testing.T) {
	e, _ := newToyExecutor(t)

	got, err := e.Execute(context.Background(), "r_1",
		plan("SELECT NULL AS missing, 'x' AS present LIMIT 500"))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	// A null is a real SQL answer. Coercing it to "" would make the rendered
	// table disagree with the query that produced it.
	if got.Rows[0][0] != nil {
		t.Errorf("NULL became %#v, want nil", got.Rows[0][0])
	}
	if got.Rows[0][1] != "x" {
		t.Errorf("text cell = %#v, want \"x\"", got.Rows[0][1])
	}
}

func TestExecuteReturnsTextNotBase64(t *testing.T) {
	e, _ := newToyExecutor(t)

	got, err := e.Execute(context.Background(), "r_1",
		plan("SELECT name FROM customers ORDER BY id LIMIT 1"))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	// The driver hands TEXT back as []byte, which marshals to base64 — a name
	// would arrive in the browser as gibberish.
	name, ok := got.Rows[0][0].(string)
	if !ok {
		t.Fatalf("text cell is %T, want string", got.Rows[0][0])
	}
	if !strings.Contains(name, " ") {
		t.Errorf("name = %q, want a readable value rather than an encoding", name)
	}
}

func TestExecuteRecordsElapsedFromTheInjectedClock(t *testing.T) {
	e, clk := newToyExecutor(t)
	// No sleep: the clock is driven, so a duration assertion is deterministic.
	clk.Advance(0)

	got, err := e.Execute(context.Background(), "r_1", plan("SELECT 1 LIMIT 1"))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got.ElapsedMs == nil {
		t.Fatal("ElapsedMs is nil; the result carries no timing")
	}
	if *got.ElapsedMs != 0 {
		t.Errorf("ElapsedMs = %d, want 0 from a clock that did not advance", *got.ElapsedMs)
	}
}

func TestExecuteTruncatesToTheByteBudgetWithoutUnderstatingTheCount(t *testing.T) {
	db, err := OpenSQLiteReadOnly(toyPath(t))
	if err != nil {
		t.Fatalf("OpenSQLiteReadOnly() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// A budget so small only the first row can fit.
	e, err := New(db, clock.NewFake(epoch), 8)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	got, err := e.Execute(context.Background(), "r_1",
		plan("SELECT id FROM orders ORDER BY id LIMIT 500"))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if !got.Truncated {
		t.Error("Truncated = false on a result that did not fit the budget")
	}
	// row_count is the honest count of what was read. A truncated frame must
	// never understate the result — the UI says "5 rows, showing 1", not "1".
	if got.RowCount != 5 {
		t.Errorf("RowCount = %d, want 5 (every row the executor read)", got.RowCount)
	}
	if len(got.Rows) >= got.RowCount {
		t.Errorf("carried %d rows of %d; the budget did not bound the frame",
			len(got.Rows), got.RowCount)
	}
}

func TestExecuteRefusesAPlanWithoutATimeout(t *testing.T) {
	e, _ := newToyExecutor(t)

	p := plan("SELECT 1 LIMIT 1")
	p.StatementTimeoutMs = 0
	// The injected LIMIT bounds how many rows come back, not how long the
	// database spends deciding that. Only the deadline ends a recursive CTE or
	// a cartesian product, so a plan without one is refused rather than
	// defaulted.
	if _, err := e.Execute(context.Background(), "r_1", p); err == nil {
		t.Fatal("Execute() accepted a plan with no statement timeout")
	}
}

func TestExecuteRefusesAnEmptyStatement(t *testing.T) {
	e, _ := newToyExecutor(t)
	p := plan("")
	if _, err := e.Execute(context.Background(), "r_1", p); err == nil {
		t.Fatal("Execute() accepted a plan carrying no statement")
	}
}

// The invariant that does not depend on the guard being correct: SQL executes
// only on a read-only connection. If the guard ever lets a write through, this
// is what still stands between it and the data.
func TestTheReadOnlyConnectionRefusesEveryWrite(t *testing.T) {
	db, err := OpenSQLiteReadOnly(toyPath(t))
	if err != nil {
		t.Fatalf("OpenSQLiteReadOnly() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })

	writes := []string{
		"DROP TABLE orders",
		"DELETE FROM orders",
		"INSERT INTO orders (id, customer_id, placed_at, status) VALUES (99, 1, '2026-01-01', 'P')",
		"UPDATE orders SET status = 'P'",
		"CREATE TABLE evil (id INTEGER)",
		"ALTER TABLE orders RENAME TO gone",
	}
	for _, stmt := range writes {
		t.Run(stmt, func(t *testing.T) {
			if _, err := db.ExecContext(context.Background(), stmt); err == nil {
				t.Fatalf("the read-only connection executed %q", stmt)
			}
		})
	}

	// And the data is still there afterwards.
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM orders").Scan(&n); err != nil {
		t.Fatalf("counting orders after the write attempts: %v", err)
	}
	if n != 5 {
		t.Errorf("orders count = %d, want the fixture's 5 — a write got through", n)
	}
}

func TestNewRejectsAnUnusableConfiguration(t *testing.T) {
	db, err := OpenSQLiteReadOnly(toyPath(t))
	if err != nil {
		t.Fatalf("OpenSQLiteReadOnly() error = %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if _, err := New(nil, clock.NewFake(epoch), DefaultMaxResultBytes); err == nil {
		t.Error("New() accepted a nil database handle")
	}
	if _, err := New(db, nil, DefaultMaxResultBytes); err == nil {
		t.Error("New() accepted a nil clock")
	}
	// A budget above the browser's ceiling would produce frames the runtime's
	// own client rejects, and the failure would look like a network fault.
	if _, err := New(db, clock.NewFake(epoch), MaxResultBytesCeiling+1); err == nil {
		t.Error("New() accepted a result budget above the browser's ceiling")
	}
	// Zero means "use the default" rather than "carry no rows".
	if e, err := New(db, clock.NewFake(epoch), 0); err != nil || e.limit != DefaultMaxResultBytes {
		t.Errorf("New(.., 0) = (%v, %v), want the default budget applied", e, err)
	}
}

func TestOpenSQLiteReadOnlyFailsFast(t *testing.T) {
	if _, err := OpenSQLiteReadOnly(""); err == nil {
		t.Error("OpenSQLiteReadOnly() accepted an empty path")
	}
	// sql.Open is lazy and never touches the file; Ping is what makes a
	// missing database fail at startup rather than on the first question.
	if _, err := OpenSQLiteReadOnly(filepath.Join(t.TempDir(), "absent.sqlite")); err == nil {
		t.Error("OpenSQLiteReadOnly() succeeded on a database that does not exist")
	}
}

// The browser's MAX_RESULT_FRAME_CHARS must admit the largest frame this
// runtime can emit. Two constants in two languages cannot share a definition,
// so each asserts the same number and names the other.
func TestResultCeilingMatchesTheBrowserConstant(t *testing.T) {
	const browserMaxResultFrameChars = 4 * 1024 * 1024 // apps/web/src/lib/protocol.ts
	if MaxResultBytesCeiling != browserMaxResultFrameChars {
		t.Fatalf("MaxResultBytesCeiling = %d but apps/web/src/lib/protocol.ts's "+
			"MAX_RESULT_FRAME_CHARS is %d — the runtime could emit a frame its own "+
			"client rejects, and it would look like a network fault",
			MaxResultBytesCeiling, browserMaxResultFrameChars)
	}
}
