package executor

import (
	"database/sql"
	"fmt"
	"net/url"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// OpenSQLiteReadOnly opens a SQLite database that cannot be written.
//
// Three independent locks, because the AST guard is not the only thing between
// a bad generation and a dropped table:
//
//   - mode=ro makes the driver open the file read-only. A write returns an
//     error from SQLite itself, below anything this codebase controls.
//   - query_only(1) refuses writes at the statement level even on a handle
//     that somehow got write access.
//   - immutable is deliberately NOT set: it would tell SQLite the file cannot
//     change and let it skip locking, which is a correctness bug the moment
//     anything else writes to the same file.
//
// The pragmas ride in the DSN rather than a follow-up Exec because with a
// connection pool an Exec'd pragma applies only to whichever connection served
// it, while a DSN pragma applies to every connection the pool opens. The trace
// store learned the same lesson.
func OpenSQLiteReadOnly(path string) (*sql.DB, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("executor: empty sqlite path")
	}

	dsn := "file:" + url.PathEscape(path) +
		"?mode=ro" +
		"&_pragma=query_only(1)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)"

	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("executor: opening %s: %w", path, err)
	}

	// sql.Open is lazy and never touches the file. Ping is where a missing or
	// unreadable database actually surfaces, so it is the fail-fast point —
	// better at startup than on the first question.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("executor: connecting to %s: %w", path, err)
	}
	return db, nil
}
