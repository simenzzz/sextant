// Package trace persists agent runs: the event stream the UI renders live,
// and the cost ledger that turns it into dollars.
//
// SQLite via modernc.org/sqlite — pure Go, no cgo, so the build stays a plain
// `go build` and CI needs no C toolchain.
package trace

import (
	"database/sql"
	"fmt"
	"net/url"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Store is a handle on the trace database.
type Store struct {
	db *sql.DB
}

// schema is applied on every Open. Every statement is IF NOT EXISTS, so Open
// is idempotent and a fresh clone needs no migration step.
//
// Events are keyed by (run_id, step) rather than an autoincrement id: step is
// already the monotonic index the wire protocol carries, so the primary key
// doubles as the guarantee that a step is never recorded twice.
const schema = `
CREATE TABLE IF NOT EXISTS runs (
    id          TEXT PRIMARY KEY,
    question    TEXT NOT NULL,
    database    TEXT NOT NULL,
    started_at  TEXT NOT NULL,
    finished_at TEXT,
    outcome     TEXT
);

CREATE TABLE IF NOT EXISTS trace_events (
    run_id     TEXT    NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step       INTEGER NOT NULL,
    type       TEXT    NOT NULL,
    elapsed_ms INTEGER NOT NULL,
    payload    TEXT    NOT NULL,
    PRIMARY KEY (run_id, step)
);

CREATE TABLE IF NOT EXISTS cost_entries (
    run_id       TEXT    NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    step         INTEGER NOT NULL,
    model        TEXT    NOT NULL,
    tokens_in    INTEGER NOT NULL,
    tokens_out   INTEGER NOT NULL,
    usd          REAL    NOT NULL,
    ms           INTEGER NOT NULL,
    cache_hit    INTEGER NOT NULL DEFAULT 0,
    escalated    INTEGER NOT NULL DEFAULT 0,
    repair_depth INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (run_id, step)
);

CREATE INDEX IF NOT EXISTS idx_runs_started_at ON runs(started_at);
`

// Open opens (creating if needed) the trace database at path.
//
// Pragmas go in the DSN rather than a follow-up Exec: with a connection pool,
// an Exec'd pragma applies to whichever connection happened to serve it, while
// a DSN pragma applies to every connection the pool opens.
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("trace: empty database path")
	}

	dsn := "file:" + url.PathEscape(path) +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("trace: opening %s: %w", path, err)
	}

	// SQLite serializes writers anyway. Holding the pool to a single
	// connection makes that explicit and removes SQLITE_BUSY as a failure
	// mode under concurrent runs.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	// sql.Open is lazy — it does not touch the file. Ping is where a bad path
	// or an unreadable file actually surfaces, so it is the fail-fast point.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("trace: connecting to %s: %w", path, err)
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close releases the database handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("trace: applying schema: %w", err)
	}
	return nil
}
