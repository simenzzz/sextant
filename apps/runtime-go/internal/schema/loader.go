package schema

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/dbreg"
)

// Loader resolves a database to its introspected schema, once.
//
// Cached because introspection is not free — it runs a PRAGMA per table and a
// sampling query per column — and the schema does not change between questions
// on a demo corpus. Doing it per request would put dozens of round trips in
// front of every answer.
//
// The cache is keyed by slug and never invalidated at P1. That is a real
// limitation and it is the honest place to note it: a schema changed under a
// running server is served stale until restart. P7's semantic cache needs
// fingerprint invalidation anyway, and this should grow the same trigger then
// rather than acquiring a second, different staleness story.
type Loader struct {
	mu     sync.Mutex
	byslug map[string]Schema
	open   map[string]*sql.DB
}

// NewLoader builds an empty loader.
func NewLoader() *Loader {
	return &Loader{byslug: make(map[string]Schema), open: make(map[string]*sql.DB)}
}

// Register makes a database's connection available for introspection.
//
// The handle is the caller's, already opened read-only. This package never
// opens one, so there is no path by which schema reading could reach a
// writable connection.
func (l *Loader) Register(slug string, db *sql.DB) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.open[slug] = db
}

// Load returns a database's schema, introspecting it on first use.
func (l *Loader) Load(ctx context.Context, db dbreg.Database) (Schema, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if cached, ok := l.byslug[db.Slug]; ok {
		return cached, nil
	}

	handle, ok := l.open[db.Slug]
	if !ok {
		return Schema{}, fmt.Errorf("schema: no open connection registered for %q", db.Slug)
	}

	var (
		sch Schema
		err error
	)
	switch db.Dialect {
	case dbreg.DialectSQLite:
		sch, err = IntrospectSQLite(ctx, handle)
	default:
		// Postgres introspection arrives with the dialect adapters at P3.
		// Refusing is better than falling through to the SQLite path, which
		// would query sqlite_master against Postgres and fail confusingly.
		return Schema{}, fmt.Errorf("schema: introspection for dialect %q is not implemented", db.Dialect)
	}
	if err != nil {
		return Schema{}, err
	}

	l.byslug[db.Slug] = sch
	return sch, nil
}
