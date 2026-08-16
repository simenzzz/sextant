// Package index builds repo.db: a queryable, purely factual picture of a
// repository — its files, its unimplemented stubs, its commits, the paths and
// phase markers its documentation asserts, and its Make targets.
//
// The package decides nothing. It records what is there, so that a later layer
// can ask whether a documented claim survives contact with it. That split is
// deliberate: every row here is reproducible from the working tree with no
// model in the loop, which is what lets the LLM layer above be scored against
// something that cannot itself hallucinate.
//
// SQLite via modernc.org/sqlite — pure Go, no cgo, matching internal/trace so
// the whole binary stays a plain `go build`.
package index

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Store is a handle on a repository index.
type Store struct {
	db   *sql.DB
	root string
	opts Options

	// degraded records that some suppression could not be resolved during the
	// last Findings call. Read through Degraded.
	degraded bool
}

// schema is applied on every Open. Every statement is IF NOT EXISTS, so Open is
// idempotent and re-indexing an existing database needs no migration step.
//
// Natural keys throughout — (path, line) rather than an autoincrement id. An
// index is a projection of a working tree, not an event log: recording the same
// fact twice is a bug, and the primary key is what makes it impossible.
const schema = `
CREATE TABLE IF NOT EXISTS files (
    path  TEXT PRIMARY KEY,
    ext   TEXT    NOT NULL,
    lines INTEGER NOT NULL,
    bytes INTEGER NOT NULL,
    -- 0 when the file was too large or of a kind the scanners do not read. Its
    -- absence from stubs/doc_claims then means "never examined", not "clean".
    scanned INTEGER NOT NULL DEFAULT 0
);

-- Directories are recorded separately from files because emptiness is a fact
-- about a directory that has no file-level equivalent, and it is the fact that
-- catches a documented tree of placeholder folders.
CREATE TABLE IF NOT EXISTS dirs (
    path     TEXT PRIMARY KEY,
    entries  INTEGER NOT NULL,
    -- 1 when the walk deliberately did not descend (node_modules, vendor,
    -- target...). Anything beneath it is unknown rather than absent, so no
    -- check may claim a path under it is missing.
    skipped  INTEGER NOT NULL DEFAULT 0,
    -- 1 when os.ReadDir failed. Distinct from entries = 0: one means "nothing
    -- is here", the other means "I could not look".
    unreadable INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS stubs (
    path   TEXT    NOT NULL,
    line   INTEGER NOT NULL,
    marker TEXT    NOT NULL,
    text   TEXT    NOT NULL,
    PRIMARY KEY (path, line)
);

CREATE TABLE IF NOT EXISTS commits (
    sha          TEXT PRIMARY KEY,
    committed_at TEXT NOT NULL,
    subject      TEXT NOT NULL
);

-- One row per line of documentation that asserts something checkable: a status
-- marker, a phase heading, or both. 'marker' is the normalized vocabulary --
-- the four dialects in use across this workspace collapse to three states here.
CREATE TABLE IF NOT EXISTS doc_claims (
    path   TEXT    NOT NULL,
    line   INTEGER NOT NULL,
    text   TEXT    NOT NULL,
    marker TEXT    NOT NULL,
    phase  TEXT    NOT NULL DEFAULT '',
    PRIMARY KEY (path, line)
);

-- Every line that NAMES a phase, graded or not. Distinct from doc_claims on
-- purpose: "is this phase documented at all?" and "what status does the doc
-- give it?" are different questions, and a phase table with no status column
-- answers the first while contributing nothing to the second.
CREATE TABLE IF NOT EXISTS doc_phases (
    path  TEXT    NOT NULL,
    line  INTEGER NOT NULL,
    phase TEXT    NOT NULL,
    PRIMARY KEY (path, line, phase)
);

-- A filesystem path that a document points at. Recorded whether or not it
-- resolves; resolution is a separate question asked at report time.
CREATE TABLE IF NOT EXISTS doc_paths (
    path   TEXT    NOT NULL,
    line   INTEGER NOT NULL,
    target TEXT    NOT NULL,
    hint   TEXT    NOT NULL DEFAULT 'ambiguous',
    PRIMARY KEY (path, line, target)
);

-- Keyed by (path, name): a repository may carry several Makefiles, and two
-- of them defining 'test' are two different targets. Keying on the name alone
-- would silently drop one -- exactly the class of quiet loss this tool exists
-- to report.
CREATE TABLE IF NOT EXISTS make_targets (
    path   TEXT    NOT NULL,
    name   TEXT    NOT NULL,
    line   INTEGER NOT NULL,
    recipe TEXT    NOT NULL,
    PRIMARY KEY (path, name)
);

CREATE INDEX IF NOT EXISTS idx_stubs_path       ON stubs(path);
CREATE INDEX IF NOT EXISTS idx_doc_claims_phase ON doc_claims(phase);
CREATE INDEX IF NOT EXISTS idx_doc_paths_target ON doc_paths(target);
`

// Open opens (creating if needed) an index database at dbPath, describing the
// repository rooted at root.
//
// Pragmas go in the DSN rather than a follow-up Exec: with a connection pool an
// Exec'd pragma applies only to whichever connection served it, while a DSN
// pragma applies to every connection the pool opens. Same reasoning as
// internal/trace.
func Open(dbPath, root string) (*Store, error) {
	if strings.TrimSpace(dbPath) == "" {
		return nil, fmt.Errorf("index: empty database path")
	}
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("index: empty repository root")
	}

	// filepath.WalkDir lstats its root, so a symlink to a directory is seen as
	// a non-directory and the walk yields nothing — the run then reported "no
	// contradictions found" and exit 0 over a repository it never opened.
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("index: resolving %s: %w", root, err)
	}
	root = resolved

	dsn := "file:" + url.PathEscape(dbPath) +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("index: opening %s: %w", dbPath, err)
	}

	// SQLite serializes writers anyway; a single connection makes that explicit
	// and removes SQLITE_BUSY as a failure mode.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	// sql.Open is lazy and never touches the file. Ping is the fail-fast point
	// where a bad path actually surfaces.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("index: connecting to %s: %w", dbPath, err)
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, root: root}, nil
}

// Degraded reports whether the last Findings call ran with incomplete
// information — currently, a gitignore lookup git refused to answer. The report
// says so rather than presenting a partial run as a whole one.
func (s *Store) Degraded() bool { return s.degraded }

// Root reports the repository root this index describes.
func (s *Store) Root() string { return s.root }

// DB exposes the underlying handle so the report layer can query the index with
// plain SQL. It is read-only by convention: nothing outside this package writes.
func (s *Store) DB() *sql.DB { return s.db }

// Close releases the database handle.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("index: applying schema: %w", err)
	}
	return nil
}

// reset clears every table so that a re-index of the same database reflects the
// working tree rather than the union of every run against it. A stale row that
// silently survives re-indexing is the exact failure this tool exists to catch,
// so the indexer must not be capable of it.
//
// Takes the transaction rather than the pool: clearing and refilling must
// commit or roll back together.
func reset(ctx context.Context, tx *sql.Tx) error {
	const stmt = `
DELETE FROM files;
DELETE FROM dirs;
DELETE FROM stubs;
DELETE FROM commits;
DELETE FROM doc_claims;
DELETE FROM doc_phases;
DELETE FROM doc_paths;
DELETE FROM make_targets;
`
	if _, err := tx.ExecContext(ctx, stmt); err != nil {
		return fmt.Errorf("index: clearing previous index: %w", err)
	}
	return nil
}
