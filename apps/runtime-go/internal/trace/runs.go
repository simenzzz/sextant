package trace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ErrRunNotFound is returned when a run id has no record.
var ErrRunNotFound = errors.New("trace: run not found")

// Run is one question the agent was asked. Outcome is empty until the run
// finishes; a run that ended by hitting a cap records that cap as its outcome,
// because a capped run is a recorded result and not an error.
type Run struct {
	ID       string
	Question string
	Database string
	// Evidence is the external knowledge submitted with the question. Stored
	// because the run starts on the SSE connect, not on the POST — so the row
	// is the only thing carrying the question across the two requests.
	Evidence   string
	StartedAt  time.Time
	FinishedAt *time.Time
	Outcome    string
}

// Event is one persisted trace event. Payload is the marshalled
// trace_event.v1 document exactly as it went over the wire, so a replayed
// trace and a live one are the same bytes.
type Event struct {
	Step      int
	Type      string
	ElapsedMS int
	Payload   []byte
}

// CostEntry is one row of the cost ledger, matching cost_ledger.v1's entry.
type CostEntry struct {
	Step        int
	Model       string
	TokensIn    int
	TokensOut   int
	USD         float64
	MS          int
	CacheHit    bool
	Escalated   bool
	RepairDepth int
}

// StartRun records a run before any work happens, so a run that crashes
// mid-flight still leaves a trace to inspect.
func (s *Store) StartRun(ctx context.Context, run Run) error {
	if run.ID == "" {
		return fmt.Errorf("trace: StartRun requires a run id")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO runs (id, question, database, evidence, started_at) VALUES (?, ?, ?, ?, ?)`,
		run.ID, run.Question, run.Database, run.Evidence,
		run.StartedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("trace: recording run %s: %w", run.ID, err)
	}
	return nil
}

// OutcomeRunning marks a run that has been claimed and is in flight.
//
// Not a trace_event.v1 type: it never reaches the wire. It exists so that
// "has this run already been started?" is a question the database answers
// atomically, rather than one two concurrent requests can both answer "no".
const OutcomeRunning = "running"

// ClaimRun marks an unstarted run as in flight, reporting whether the caller
// won the claim.
//
// The whole point is the WHERE clause. Reading the outcome and then writing it
// is two statements with a gap between them, and two requests that both read
// an empty outcome will both start a loop — two provider bills for one
// question. A single conditional UPDATE has no gap: exactly one caller sees a
// row affected.
//
// A double-clicked link or a proxy retry is enough to hit this; it does not
// take an attacker.
func (s *Store) ClaimRun(ctx context.Context, runID string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET outcome = ? WHERE id = ? AND (outcome IS NULL OR outcome = '')`,
		OutcomeRunning, runID)
	if err != nil {
		return false, fmt.Errorf("trace: claiming run %s: %w", runID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// "The driver could not tell us" must not become "you won the claim",
		// which is the reading that starts a second paid loop.
		return false, fmt.Errorf("trace: confirming claim of run %s: %w", runID, err)
	}
	return n == 1, nil
}

// FinishRun stamps a run's end and its outcome.
func (s *Store) FinishRun(ctx context.Context, runID, outcome string, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET finished_at = ?, outcome = ? WHERE id = ?`,
		at.UTC().Format(time.RFC3339Nano), outcome, runID)
	if err != nil {
		return fmt.Errorf("trace: finishing run %s: %w", runID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// Swallowing this would turn "the driver could not tell us" into
		// "the run was found and updated", which is the opposite claim.
		return fmt.Errorf("trace: confirming update of run %s: %w", runID, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrRunNotFound, runID)
	}
	return nil
}

// AppendEvent persists one trace event.
//
// The (run_id, step) primary key makes a duplicate step a database error
// rather than a silently doubled entry in the timeline.
func (s *Store) AppendEvent(ctx context.Context, runID string, ev Event) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO trace_events (run_id, step, type, elapsed_ms, payload) VALUES (?, ?, ?, ?, ?)`,
		runID, ev.Step, ev.Type, ev.ElapsedMS, string(ev.Payload))
	if err != nil {
		return fmt.Errorf("trace: appending event %d to run %s: %w", ev.Step, runID, err)
	}
	return nil
}

// AppendCost persists one cost ledger entry.
func (s *Store) AppendCost(ctx context.Context, runID string, e CostEntry) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO cost_entries
		   (run_id, step, model, tokens_in, tokens_out, usd, ms, cache_hit, escalated, repair_depth)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		runID, e.Step, e.Model, e.TokensIn, e.TokensOut, e.USD, e.MS,
		e.CacheHit, e.Escalated, e.RepairDepth)
	if err != nil {
		return fmt.Errorf("trace: appending cost for step %d of run %s: %w", e.Step, runID, err)
	}
	return nil
}

// LoadRun returns a run's metadata.
func (s *Store) LoadRun(ctx context.Context, runID string) (Run, error) {
	var (
		run        Run
		started    string
		finished   sql.NullString
		outcomeCol sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, question, database, evidence, started_at, finished_at, outcome FROM runs WHERE id = ?`,
		runID,
	).Scan(&run.ID, &run.Question, &run.Database, &run.Evidence, &started, &finished, &outcomeCol)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, fmt.Errorf("%w: %s", ErrRunNotFound, runID)
	}
	if err != nil {
		return Run{}, fmt.Errorf("trace: loading run %s: %w", runID, err)
	}

	if run.StartedAt, err = time.Parse(time.RFC3339Nano, started); err != nil {
		return Run{}, fmt.Errorf("trace: run %s has an unparseable started_at %q: %w", runID, started, err)
	}
	if finished.Valid {
		t, err := time.Parse(time.RFC3339Nano, finished.String)
		if err != nil {
			return Run{}, fmt.Errorf("trace: run %s has an unparseable finished_at %q: %w", runID, finished.String, err)
		}
		run.FinishedAt = &t
	}
	run.Outcome = outcomeCol.String
	return run, nil
}

// LoadEvents returns a run's events in step order.
func (s *Store) LoadEvents(ctx context.Context, runID string) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT step, type, elapsed_ms, payload FROM trace_events WHERE run_id = ? ORDER BY step`, runID)
	if err != nil {
		return nil, fmt.Errorf("trace: loading events for run %s: %w", runID, err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var (
			ev      Event
			payload string
		)
		if err := rows.Scan(&ev.Step, &ev.Type, &ev.ElapsedMS, &payload); err != nil {
			return nil, fmt.Errorf("trace: scanning event for run %s: %w", runID, err)
		}
		ev.Payload = []byte(payload)
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("trace: iterating events for run %s: %w", runID, err)
	}
	return out, nil
}

// TotalUSD sums a run's cost ledger. A run with no entries costs nothing,
// which is a real answer (a full cache hit) and not a missing one.
func (s *Store) TotalUSD(ctx context.Context, runID string) (float64, error) {
	var total sql.NullFloat64
	err := s.db.QueryRowContext(ctx,
		`SELECT SUM(usd) FROM cost_entries WHERE run_id = ?`, runID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("trace: totalling cost for run %s: %w", runID, err)
	}
	return total.Float64, nil
}
