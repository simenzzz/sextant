package trace

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// openTemp gives each test its own database file under t.TempDir(), so tests
// share no state and can run in parallel without a cleanup dance.
func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "trace.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

// A fixed instant, so nothing in these tests depends on the wall clock.
var testStart = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

func TestOpenRejectsEmptyPath(t *testing.T) {
	if _, err := Open("   "); err == nil {
		t.Fatal("Open(blank) = nil error, want a failure")
	}
}

// sql.Open is lazy, so an unusable path only surfaces on Ping. This asserts
// Open fails there rather than handing back a Store that breaks on first use.
func TestOpenFailsFastOnUnusablePath(t *testing.T) {
	dir := t.TempDir() // a directory is not a database file
	if _, err := Open(dir); err == nil {
		t.Fatal("Open(directory) = nil error, want a failure at Ping")
	}
}

func TestLoadEventsForRunWithNoEvents(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if err := s.StartRun(ctx, Run{ID: "run-1", Question: "q", Database: "toy", StartedAt: testStart}); err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	got, err := s.LoadEvents(ctx, "run-1")
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("LoadEvents = %d events, want 0", len(got))
	}
}

// Open applies the schema on every call, so reopening an existing database
// must succeed rather than fail on already-existing tables.
func TestOpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.sqlite")

	first, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := first.StartRun(context.Background(), Run{
		ID: "run-1", Question: "how many?", Database: "toy", StartedAt: testStart,
	}); err != nil {
		t.Fatalf("StartRun: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := Open(path)
	if err != nil {
		t.Fatalf("reopening an existing database: %v", err)
	}
	defer second.Close()

	got, err := second.LoadRun(context.Background(), "run-1")
	if err != nil {
		t.Fatalf("LoadRun after reopen: %v", err)
	}
	if got.Question != "how many?" {
		t.Errorf("Question = %q, want %q", got.Question, "how many?")
	}
}

func TestRunLifecycle(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	if err := s.StartRun(ctx, Run{
		ID: "run-1", Question: "how many orders?", Database: "toy", StartedAt: testStart,
	}); err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	run, err := s.LoadRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if !run.StartedAt.Equal(testStart) {
		t.Errorf("StartedAt = %s, want %s", run.StartedAt, testStart)
	}
	if run.FinishedAt != nil {
		t.Errorf("FinishedAt = %v, want nil for a run in flight", run.FinishedAt)
	}

	end := testStart.Add(2 * time.Second)
	// "budget_exhausted" is a recorded outcome, not an error — the store must
	// treat it exactly like any other ending.
	if err := s.FinishRun(ctx, "run-1", "budget_exhausted", end); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}

	run, err = s.LoadRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("LoadRun after finish: %v", err)
	}
	if run.Outcome != "budget_exhausted" {
		t.Errorf("Outcome = %q, want %q", run.Outcome, "budget_exhausted")
	}
	if run.FinishedAt == nil || !run.FinishedAt.Equal(end) {
		t.Errorf("FinishedAt = %v, want %s", run.FinishedAt, end)
	}
}

func TestLoadRunUnknownID(t *testing.T) {
	s := openTemp(t)
	if _, err := s.LoadRun(context.Background(), "nope"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("LoadRun(unknown) error = %v, want ErrRunNotFound", err)
	}
}

func TestFinishRunUnknownID(t *testing.T) {
	s := openTemp(t)
	err := s.FinishRun(context.Background(), "nope", "answered", testStart)
	if !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("FinishRun(unknown) error = %v, want ErrRunNotFound", err)
	}
}

func TestEventsRoundTripInStepOrder(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if err := s.StartRun(ctx, Run{ID: "run-1", Question: "q", Database: "toy", StartedAt: testStart}); err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	// Inserted out of order on purpose: LoadEvents must sort by step, not by
	// insertion, or a replayed trace shows the timeline scrambled.
	for _, ev := range []Event{
		{Step: 2, Type: "executed", ElapsedMS: 900, Payload: []byte(`{"row_count":4}`)},
		{Step: 0, Type: "run_started", ElapsedMS: 0, Payload: []byte(`{}`)},
		{Step: 1, Type: "generated", ElapsedMS: 400, Payload: []byte(`{"sql":"SELECT 1"}`)},
	} {
		if err := s.AppendEvent(ctx, "run-1", ev); err != nil {
			t.Fatalf("AppendEvent %d: %v", ev.Step, err)
		}
	}

	got, err := s.LoadEvents(ctx, "run-1")
	if err != nil {
		t.Fatalf("LoadEvents: %v", err)
	}
	wantTypes := []string{"run_started", "generated", "executed"}
	if len(got) != len(wantTypes) {
		t.Fatalf("LoadEvents returned %d events, want %d", len(got), len(wantTypes))
	}
	for i, want := range wantTypes {
		if got[i].Step != i {
			t.Errorf("event %d has step %d, want %d", i, got[i].Step, i)
		}
		if got[i].Type != want {
			t.Errorf("event %d type = %q, want %q", i, got[i].Type, want)
		}
	}
	if string(got[1].Payload) != `{"sql":"SELECT 1"}` {
		t.Errorf("payload = %q, want the bytes exactly as they went on the wire", got[1].Payload)
	}
}

// A step is the monotonic wire index; recording one twice would double an
// entry in the timeline, so the store rejects it rather than accepting it.
func TestDuplicateStepIsRejected(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if err := s.StartRun(ctx, Run{ID: "run-1", Question: "q", Database: "toy", StartedAt: testStart}); err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	ev := Event{Step: 0, Type: "run_started", Payload: []byte(`{}`)}
	if err := s.AppendEvent(ctx, "run-1", ev); err != nil {
		t.Fatalf("first AppendEvent: %v", err)
	}
	if err := s.AppendEvent(ctx, "run-1", ev); err == nil {
		t.Fatal("second AppendEvent with the same step = nil error, want a uniqueness failure")
	}
}

// foreign_keys(1) is set in the DSN; without it this insert would silently
// create an orphan ledger entry attributable to no run.
func TestCostEntryRequiresAnExistingRun(t *testing.T) {
	s := openTemp(t)
	err := s.AppendCost(context.Background(), "ghost-run", CostEntry{
		Step: 0, Model: "m", TokensIn: 1, TokensOut: 1, USD: 0.01, MS: 5,
	})
	if err == nil {
		t.Fatal("AppendCost for an unknown run = nil error, want a foreign-key failure")
	}
}

func TestTotalUSD(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if err := s.StartRun(ctx, Run{ID: "run-1", Question: "q", Database: "toy", StartedAt: testStart}); err != nil {
		t.Fatalf("StartRun: %v", err)
	}

	// A run with no ledger entries costs nothing — a real answer (full cache
	// hit), not a missing one, so this must be 0 and not an error.
	total, err := s.TotalUSD(ctx, "run-1")
	if err != nil {
		t.Fatalf("TotalUSD on an empty ledger: %v", err)
	}
	if total != 0 {
		t.Errorf("TotalUSD = %g, want 0", total)
	}

	for _, e := range []CostEntry{
		{Step: 0, Model: "cheap", TokensIn: 100, TokensOut: 10, USD: 0.002, MS: 300},
		{Step: 1, Model: "strong", TokensIn: 200, TokensOut: 20, USD: 0.010, MS: 800, Escalated: true},
	} {
		if err := s.AppendCost(ctx, "run-1", e); err != nil {
			t.Fatalf("AppendCost step %d: %v", e.Step, err)
		}
	}

	total, err = s.TotalUSD(ctx, "run-1")
	if err != nil {
		t.Fatalf("TotalUSD: %v", err)
	}
	if want := 0.012; total < want-1e-9 || total > want+1e-9 {
		t.Errorf("TotalUSD = %g, want %g", total, want)
	}
}
