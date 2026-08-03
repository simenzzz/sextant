// Package dbreg maps the database slug on a question request to a real
// connection.
//
// The indirection is the point. question_request.v1's `database` field is
// attacker-influenced — it arrives from a browser — and it must never reach a
// filesystem path or a SQL string. It is a key into this registry and nothing
// else: an unknown slug is a rejection, not a lookup that happens to fail.
package dbreg

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Dialect is a SQL dialect the runtime can execute against.
type Dialect string

const (
	DialectSQLite   Dialect = "sqlite"
	DialectPostgres Dialect = "postgres"
)

// Mirrors sql_plan.v1's dialect enum. A dialect outside it would produce a
// plan that fails its own contract at the guard.
func (d Dialect) valid() bool {
	return d == DialectSQLite || d == DialectPostgres
}

// slugPattern mirrors question_request.v1's `database` pattern exactly.
//
// Enforced here as well as at the HTTP boundary because this is the layer that
// would otherwise let a slug from some other path — an eval fixture, a config
// typo — name something the contract would have rejected.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Database is one configured, queryable database.
type Database struct {
	// Slug is the identifier a question request names.
	Slug string
	// Dialect decides which executor and which parse dialect apply.
	Dialect Dialect
	// DSN is the driver-specific connection string. It never leaves the
	// server: an error mentioning it would hand a client the database path or,
	// on Postgres, the credentials embedded in it.
	DSN string
}

// Registry is an immutable set of configured databases.
type Registry struct {
	byslug map[string]Database
}

// ErrUnknownDatabase is returned for a slug with no configured database.
type ErrUnknownDatabase struct{ Slug string }

func (e ErrUnknownDatabase) Error() string {
	return fmt.Sprintf("dbreg: no database configured for %q", e.Slug)
}

// SafeMessage is what may be shown to a client.
//
// It deliberately does not echo the slug: that value came from the request, and
// reflecting untrusted input into a response is how a message becomes a vector.
// The slug is in the server log via Error().
func (e ErrUnknownDatabase) SafeMessage() string {
	return "no such database"
}

// Parse builds a registry from the SEXTANT_DATABASES form:
//
//	slug=dialect:dsn,slug=dialect:dsn
//
// for example:
//
//	toy=sqlite:infra/fixtures/toy.sqlite
//
// An empty spec yields an empty registry, which answers every question with
// "no such database". That is the safe reading of an unset variable — the same
// rule the origin allowlist follows — and it is why the runtime refuses to
// serve questions rather than silently querying something it guessed at.
func Parse(spec string) (*Registry, error) {
	out := make(map[string]Database)

	for _, entry := range strings.Split(spec, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}

		slug, rest, ok := strings.Cut(entry, "=")
		if !ok {
			return nil, fmt.Errorf("dbreg: entry %q is not slug=dialect:dsn", entry)
		}
		slug = strings.TrimSpace(slug)
		if !slugPattern.MatchString(slug) {
			return nil, fmt.Errorf(
				"dbreg: %q is not a usable database slug (want %s, matching question_request.v1)",
				slug, slugPattern)
		}
		if _, dup := out[slug]; dup {
			// Last-wins would make which database gets queried depend on
			// string order in an environment variable.
			return nil, fmt.Errorf("dbreg: database %q is configured twice", slug)
		}

		rawDialect, dsn, ok := strings.Cut(strings.TrimSpace(rest), ":")
		if !ok {
			return nil, fmt.Errorf("dbreg: database %q has no dialect prefix (want sqlite: or postgres:)", slug)
		}
		dialect := Dialect(strings.TrimSpace(rawDialect))
		if !dialect.valid() {
			return nil, fmt.Errorf("dbreg: database %q has unsupported dialect %q (want %s or %s)",
				slug, dialect, DialectSQLite, DialectPostgres)
		}
		if strings.TrimSpace(dsn) == "" {
			return nil, fmt.Errorf("dbreg: database %q has an empty connection string", slug)
		}

		out[slug] = Database{Slug: slug, Dialect: dialect, DSN: strings.TrimSpace(dsn)}
	}

	return &Registry{byslug: out}, nil
}

// Lookup resolves a slug.
func (r *Registry) Lookup(slug string) (Database, error) {
	if r == nil {
		return Database{}, ErrUnknownDatabase{Slug: slug}
	}
	db, ok := r.byslug[slug]
	if !ok {
		return Database{}, ErrUnknownDatabase{Slug: slug}
	}
	return db, nil
}

// Slugs returns every configured slug, sorted. Safe to show a client: these
// are names the operator chose, not anything a request supplied.
func (r *Registry) Slugs() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.byslug))
	for slug := range r.byslug {
		out = append(out, slug)
	}
	sort.Strings(out)
	return out
}

// Len reports how many databases are configured.
func (r *Registry) Len() int {
	if r == nil {
		return 0
	}
	return len(r.byslug)
}
