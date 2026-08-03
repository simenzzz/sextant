package agent

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/clock"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/contracts/gen"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/executor"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/provider"
	"github.com/simenzzz/sextant/apps/runtime-go/internal/schema"
)

// These tests fail on a panic until Run is written.

// fakeParser returns a canned summary without a sidecar.
type fakeParser struct {
	summary gen.ParseSummaryV1
	err     error
	calls   int
}

func (f *fakeParser) Parse(_ context.Context, _ string, _ gen.SqlPlanV1Dialect, _ int) (gen.ParseSummaryV1, error) {
	f.calls++
	if f.err != nil {
		return gen.ParseSummaryV1{}, f.err
	}
	return f.summary, nil
}

func okSummary(sql string, tables ...string) gen.ParseSummaryV1 {
	if len(tables) == 0 {
		tables = []string{"orders"}
	}
	hasLimit := false
	return gen.ParseSummaryV1{
		Schema:         "parse_summary.v1",
		Ok:             true,
		Dialect:        gen.ParseSummaryV1DialectSqlite,
		StatementCount: 1,
		NodeKinds:      []string{"Select", "From", "Table", "Identifier", "Count", "Limit"},
		Tables:         tables,
		Functions:      []string{"count"},
		HasLimit:       &hasLimit,
		NormalizedSql:  &sql,
		LimitInjected:  true,
	}
}

func toyExecutor(t *testing.T, clk clock.Clock) *executor.Executor {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..")
	db, err := executor.OpenSQLiteReadOnly(filepath.Join(root, "infra", "fixtures", "toy.sqlite"))
	if err != nil {
		t.Fatalf("opening the toy fixture: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	e, err := executor.New(db, clk, executor.DefaultMaxResultBytes)
	if err != nil {
		t.Fatalf("executor.New() error = %v", err)
	}
	return e
}

func toySchema(t *testing.T, clk clock.Clock) schema.Schema {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test file")
	}
	root := filepath.Join(filepath.Dir(thisFile), "..", "..", "..", "..")
	db, err := executor.OpenSQLiteReadOnly(filepath.Join(root, "infra", "fixtures", "toy.sqlite"))
	if err != nil {
		t.Fatalf("opening the toy fixture: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	s, err := schema.IntrospectSQLite(context.Background(), db)
	if err != nil {
		t.Fatalf("introspecting the toy fixture: %v", err)
	}
	return s
}

// recorder collects the trace a run emitted.
type recorder struct{ events []gen.TraceEventV1 }

func (r *recorder) emit(ev gen.TraceEventV1) { r.events = append(r.events, ev) }

func (r *recorder) types() []string {
	out := make([]string, len(r.events))
	for i, ev := range r.events {
		out[i] = string(ev.Type)
	}
	return out
}

func (r *recorder) has(t string) bool {
	for _, ev := range r.events {
		if string(ev.Type) == t {
			return true
		}
	}
	return false
}

func (r *recorder) find(t string) (gen.TraceEventV1, bool) {
	for _, ev := range r.events {
		if string(ev.Type) == t {
			return ev, true
		}
	}
	return gen.TraceEventV1{}, false
}

const cancelledSQL = "SELECT COUNT(*) AS cancelled FROM orders WHERE status = 'C' LIMIT 500"

type harness struct {
	agent    *Agent
	provider *provider.FakeProvider
	parser   *fakeParser
	clock    *clock.Fake
	input    RunInput
	rec      *recorder
}

func newHarness(t *testing.T, mutate ...func(*harness)) *harness {
	t.Helper()
	clk := clock.NewFake(epoch)

	h := &harness{
		provider: &provider.FakeProvider{
			Deltas: []string{"```sql\n", cancelledSQL, "\n```"},
			Usage:  &provider.Usage{TokensIn: 400, TokensOut: 25},
		},
		parser: &fakeParser{summary: okSummary(cancelledSQL)},
		clock:  clk,
		rec:    &recorder{},
	}
	for _, m := range mutate {
		m(h)
	}

	a, err := New(Deps{
		Provider:   h.provider,
		Parser:     h.parser,
		Executor:   toyExecutor(t, clk),
		Clock:      clk,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		CheapModel: "claude-haiku-4-5",
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	h.agent = a
	h.input = RunInput{
		RunID:            "r_test",
		Question:         "how many cancelled orders are there?",
		Schema:           toySchema(t, clk),
		Dialect:          gen.SqlPlanV1DialectSqlite,
		Caps:             Caps{MaxRepairDepth: 0, MaxUSD: 0.05, WallClock: 30 * time.Second},
		RowLimit:         500,
		StatementTimeout: 10 * time.Second,
	}
	return h
}
