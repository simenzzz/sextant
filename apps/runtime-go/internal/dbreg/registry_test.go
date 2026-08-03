package dbreg

import (
	"errors"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name      string
		spec      string
		wantSlugs []string
		wantErr   bool
	}{
		{name: "empty spec configures nothing", spec: "", wantSlugs: nil},
		{name: "whitespace only", spec: "   ", wantSlugs: nil},
		{
			name: "one sqlite database", spec: "toy=sqlite:infra/fixtures/toy.sqlite",
			wantSlugs: []string{"toy"},
		},
		{
			name:      "two databases, sorted on read",
			spec:      "demo=postgres://u:p@host/db,toy=sqlite:t.sqlite",
			wantSlugs: []string{"demo", "toy"},
		},
		{name: "surrounding whitespace tolerated", spec: " toy = sqlite:t.sqlite ", wantSlugs: []string{"toy"}},
		{name: "trailing comma tolerated", spec: "toy=sqlite:t.sqlite,", wantSlugs: []string{"toy"}},

		{name: "no equals", spec: "toy-sqlite:t.sqlite", wantErr: true},
		{name: "no dialect prefix", spec: "toy=t.sqlite", wantErr: true},
		{name: "unsupported dialect", spec: "toy=mysql:t.sqlite", wantErr: true},
		{name: "empty dsn", spec: "toy=sqlite:", wantErr: true},
		{name: "empty slug", spec: "=sqlite:t.sqlite", wantErr: true},
		// The slug pattern mirrors question_request.v1 exactly. A slug that
		// only exists in config would be one the contract rejects at the
		// boundary — configured but permanently unreachable.
		{name: "slug with uppercase", spec: "Toy=sqlite:t.sqlite", wantErr: true},
		{name: "slug starting with a dash", spec: "-toy=sqlite:t.sqlite", wantErr: true},
		{name: "slug with a path separator", spec: "../toy=sqlite:t.sqlite", wantErr: true},
		{name: "slug with a semicolon", spec: "toy;drop=sqlite:t.sqlite", wantErr: true},
		// Last-wins would make which database gets queried depend on string
		// order in an environment variable.
		{name: "duplicate slug", spec: "toy=sqlite:a.sqlite,toy=sqlite:b.sqlite", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg, err := Parse(tt.spec)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q) = nil error, want a refusal", tt.spec)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) error = %v", tt.spec, err)
			}
			got := reg.Slugs()
			if len(got) != len(tt.wantSlugs) {
				t.Fatalf("Slugs() = %v, want %v", got, tt.wantSlugs)
			}
			for i := range got {
				if got[i] != tt.wantSlugs[i] {
					t.Fatalf("Slugs() = %v, want %v", got, tt.wantSlugs)
				}
			}
		})
	}
}

func TestParseKeepsDialectAndDSN(t *testing.T) {
	reg, err := Parse("toy=sqlite:infra/fixtures/toy.sqlite")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	db, err := reg.Lookup("toy")
	if err != nil {
		t.Fatalf("Lookup() error = %v", err)
	}
	if db.Dialect != DialectSQLite {
		t.Errorf("Dialect = %q, want %q", db.Dialect, DialectSQLite)
	}
	if db.DSN != "infra/fixtures/toy.sqlite" {
		t.Errorf("DSN = %q, want the path after the dialect prefix", db.DSN)
	}
	if db.Slug != "toy" {
		t.Errorf("Slug = %q, want %q", db.Slug, "toy")
	}
}

func TestLookupRejectsEverythingUnconfigured(t *testing.T) {
	reg, err := Parse("toy=sqlite:t.sqlite")
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	for _, slug := range []string{
		"", "other", "TOY",
		// The whole reason the slug is a registry key and never a path: none
		// of these can reach a filesystem, because none of them is a key.
		"../../etc/passwd", "toy/../../secret", "toy; DROP TABLE users",
	} {
		if _, err := reg.Lookup(slug); err == nil {
			t.Errorf("Lookup(%q) succeeded; every unconfigured slug must be refused", slug)
		}
	}
}

func TestUnknownDatabaseErrorDoesNotEchoTheSlug(t *testing.T) {
	// The slug came from the request. Reflecting untrusted input into a
	// response is how a message becomes a vector; the value stays in the log.
	reg, _ := Parse("")
	_, err := reg.Lookup("<script>alert(1)</script>")

	var unknown ErrUnknownDatabase
	if !errors.As(err, &unknown) {
		t.Fatalf("Lookup() error = %v, want ErrUnknownDatabase", err)
	}
	if strings.Contains(unknown.SafeMessage(), "script") {
		t.Errorf("SafeMessage() = %q, want it not to echo the requested slug", unknown.SafeMessage())
	}
	// Error() is the server-side half and should name it, for the log.
	if !strings.Contains(unknown.Error(), "script") {
		t.Errorf("Error() = %q, want the slug present for diagnosis", unknown.Error())
	}
}

func TestEmptyRegistryPermitsNothing(t *testing.T) {
	// The safe reading of an unset variable, matching the origin allowlist:
	// the runtime refuses to serve questions rather than querying something it
	// guessed at.
	reg, err := Parse("")
	if err != nil {
		t.Fatalf("Parse(\"\") error = %v", err)
	}
	if reg.Len() != 0 {
		t.Errorf("Len() = %d, want 0", reg.Len())
	}
	if _, err := reg.Lookup("toy"); err == nil {
		t.Error("an empty registry resolved a slug")
	}
}

func TestNilRegistryIsSafe(t *testing.T) {
	// A zero-value Registry reaching a lookup would otherwise panic on the
	// request path rather than returning a rejection.
	var reg *Registry
	if _, err := reg.Lookup("toy"); err == nil {
		t.Error("nil registry resolved a slug")
	}
	if reg.Len() != 0 || reg.Slugs() != nil {
		t.Error("nil registry reported contents")
	}
}
