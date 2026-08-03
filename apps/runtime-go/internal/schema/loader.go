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
	// once serialises introspection PER DATABASE rather than globally.
	//
	// Holding one mutex across the whole read meant a cold cache serialised
	// every concurrent first question — including questions against a
	// different database — behind a PRAGMA per table and a sampling SELECT per
	// column. sync.Once also collapses a thundering herd into one read and
	// gives every waiter the same result.
	once map[string]*sync.Once
}

// NewLoader builds an empty loader.
func NewLoader() *Loader {
	return &Loader{
		byslug: make(map[string]Schema),
		open:   make(map[string]*sql.DB),
		once:   make(map[string]*sync.Once),
	}
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
	if cached, ok := l.byslug[db.Slug]; ok {
		l.mu.Unlock()
		return cached, nil
	}
	handle, registered := l.open[db.Slug]
	gate, ok := l.once[db.Slug]
	if !ok {
		gate = new(sync.Once)
		l.once[db.Slug] = gate
	}
	l.mu.Unlock()

	if !registered {
		return Schema{}, fmt.Errorf("schema: no open connection registered for %q", db.Slug)
	}
	if db.Dialect != dbreg.DialectSQLite {
		// Postgres introspection arrives with the dialect adapters at P3.
		// Refusing is better than falling through to the SQLite path, which
		// would query sqlite_master against Postgres and fail confusingly.
		return Schema{}, fmt.Errorf("schema: introspection for dialect %q is not implemented", db.Dialect)
	}

	// Introspection runs OUTSIDE the mutex, so a slow read on one database
	// does not block questions against another.
	var readErr error
	gate.Do(func() {
		sch, err := IntrospectSQLite(ctx, handle)
		if err != nil {
			readErr = err
			// A failed read must not be remembered as success, and the next
			// caller should get to try again rather than inherit this one's
			// cancelled context.
			l.mu.Lock()
			delete(l.once, db.Slug)
			l.mu.Unlock()
			return
		}
		l.mu.Lock()
		l.byslug[db.Slug] = sch
		l.mu.Unlock()
	})
	if readErr != nil {
		return Schema{}, readErr
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	sch, ok := l.byslug[db.Slug]
	if !ok {
		// Another caller's introspection failed while this one waited on the
		// Once. Report rather than return a zero schema, which the guard would
		// read as "no tables are allowed".
		return Schema{}, fmt.Errorf("schema: introspecting %q failed", db.Slug)
	}
	return sch, nil
}
