package index

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindingsDetectsEachContradiction(t *testing.T) {
	root := fixtureRepo(t, map[string]string{
		"README.md": "# Fixture\n" +
			"The harness lives in `eval/run.py` and writes to `eval/results/`.\n",
		"Makefile": "eval:\n\tpython3 eval/run.py --all\n",
		"ROADMAP.md": "Markers: ✅ done · 🚧 in progress · ⬜ not started\n" +
			"\n" +
			"## Phase 0 — Skeleton 🚧\n" +
			"## Phase 1 — Fetch ✅\n",
		"eval/results/.keep": "",
		"src/main.go":        "package main\n\nfunc main() { panic(\"TODO(you): implement\") }\n",
	}, []string{
		"chore: scaffold",
		"feat: Phase 0 skeleton",   // contradicts the 🚧 marker
		"feat: Phase 2 fetch loop", // documented nowhere
	})

	// eval/results must be genuinely empty on disk; the .keep above only exists
	// so git tracks the directory, so remove it after the commit.
	if err := os.Remove(filepath.Join(root, "eval", "results", ".keep")); err != nil {
		t.Fatalf("removing placeholder: %v", err)
	}

	findings := indexFixture(t, root)

	t.Run("missing target script", func(t *testing.T) {
		got := findingsOfKind(findings, KindMissingScript)
		if len(got) != 1 {
			t.Fatalf("got %d, want 1: %+v", len(got), got)
		}
		if got[0].Path != "Makefile" {
			t.Errorf("path = %q, want Makefile", got[0].Path)
		}
	})

	t.Run("empty advertised dir", func(t *testing.T) {
		got := findingsOfKind(findings, KindEmptyAdvertised)
		if len(got) != 1 {
			t.Fatalf("got %d, want 1: %+v", len(got), got)
		}
		if got[0].Path != "README.md" {
			t.Errorf("path = %q, want README.md", got[0].Path)
		}
	})

	t.Run("phase marker contradicted by a delivery commit", func(t *testing.T) {
		got := findingsOfKind(findings, KindPhaseContradicted)
		if len(got) != 1 {
			t.Fatalf("got %d, want 1: %+v", len(got), got)
		}
		if got[0].Line != 3 {
			t.Errorf("line = %d, want 3 (the 🚧 heading)", got[0].Line)
		}
	})

	t.Run("shipped phase absent from the roadmap", func(t *testing.T) {
		got := findingsOfKind(findings, KindPhaseUndocumented)
		if len(got) != 1 {
			t.Fatalf("got %d, want 1: %+v", len(got), got)
		}
	})

	t.Run("a done marker is never contradicted", func(t *testing.T) {
		for _, f := range findingsOfKind(findings, KindPhaseContradicted) {
			if f.Line == 4 {
				t.Errorf("phase 1 is marked ✅ and must not be reported: %+v", f)
			}
		}
	})
}

func TestFindingsCleanRepoIsSilent(t *testing.T) {
	root := fixtureRepo(t, map[string]string{
		"README.md":   "# Clean\n\nEntry point is `src/main.go`.\n",
		"src/main.go": "package main\n\nfunc main() {}\n",
		"ROADMAP.md":  "## Phase 0 — Skeleton ✅\n",
	}, []string{"feat: Phase 0 skeleton"})

	if got := indexFixture(t, root); len(got) != 0 {
		t.Fatalf("clean repository produced findings: %+v", got)
	}
}

func TestDocPathResolutionReadings(t *testing.T) {
	root := fixtureRepo(t, map[string]string{
		// Root-relative.
		"README.md": "See `apps/web/src/app.ts` and `docs/GUIDE.md`.\n",
		// Relative to the document that names it.
		"apps/web/CLAUDE.md": "The entry point is `src/app.ts`.\n",
		// A distinctive suffix of a real path.
		"docs/GUIDE.md": "Config lives in `web/src/app.ts`.\n",
		// Genuinely absent.
		"docs/BROKEN.md": "Look in `does/not/exist.ts`.\n",

		"apps/web/src/app.ts": "export {}\n",
	}, []string{"chore: init"})

	got := findingsOfKind(indexFixture(t, root), KindDanglingDocPath)
	if len(got) != 1 {
		t.Fatalf("got %d dangling findings, want 1: %+v", len(got), got)
	}
	if got[0].Path != "docs/BROKEN.md" {
		t.Errorf("path = %q, want docs/BROKEN.md", got[0].Path)
	}
}

func TestGitignoredPathsAreNotDrift(t *testing.T) {
	// The directory pattern is the case that matters: `git check-ignore` can
	// only apply a trailing-slash rule when it can tell the path is a
	// directory, which it cannot for a path that does not exist.
	root := fixtureRepo(t, map[string]string{
		".gitignore": "playwright-report/\n",
		"README.md":  "The report lands in `web/playwright-report/`.\n",
	}, []string{"chore: init"})

	if got := findingsOfKind(indexFixture(t, root), KindDanglingDocPath); len(got) != 0 {
		t.Fatalf("gitignored output directory reported as drift: %+v", got)
	}
}

func TestScratchPlansAreNotClaims(t *testing.T) {
	root := fixtureRepo(t, map[string]string{
		".claude/plans/old.md": "Superseded: see `does/not/exist.ts`.\n",
		"README.md":            "# Fixture\n",
	}, []string{"chore: init"})

	if got := indexFixture(t, root); len(got) != 0 {
		t.Fatalf("a working log was read as documentation: %+v", got)
	}
}

func TestBuildIsIdempotent(t *testing.T) {
	root := fixtureRepo(t, map[string]string{
		"README.md":  "See `does/not/exist.ts`.\n",
		"ROADMAP.md": "## Phase 0 ✅\n",
	}, []string{"chore: init"})

	dbPath := filepath.Join(t.TempDir(), "repo.db")
	store, err := Open(dbPath, root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	first, err := store.Build(context.Background(), Options{})
	if err != nil {
		t.Fatalf("first Build: %v", err)
	}
	second, err := store.Build(context.Background(), Options{})
	if err != nil {
		t.Fatalf("second Build: %v", err)
	}

	// Re-indexing must replace, not accumulate. A stale row surviving a rebuild
	// is precisely the failure this tool reports on, so the indexer must not be
	// capable of it.
	if first != second {
		t.Errorf("re-index changed the stats: %+v then %+v", first, second)
	}

	f1, err := store.Findings(context.Background())
	if err != nil {
		t.Fatalf("Findings: %v", err)
	}
	f2, err := store.Findings(context.Background())
	if err != nil {
		t.Fatalf("Findings: %v", err)
	}
	if len(f1) != len(f2) {
		t.Errorf("findings not stable across calls: %d then %d", len(f1), len(f2))
	}
}

func TestOpenRejectsEmptyArguments(t *testing.T) {
	if _, err := Open("", "/tmp"); err == nil {
		t.Error("Open with an empty database path must fail")
	}
	if _, err := Open(filepath.Join(t.TempDir(), "x.db"), ""); err == nil {
		t.Error("Open with an empty root must fail")
	}
}

func TestBuildWithoutGitHistory(t *testing.T) {
	// A tree with no git history is a normal thing to index; it simply offers
	// no commit-derived findings.
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("# Bare\n"), 0o644); err != nil {
		t.Fatalf("writing README: %v", err)
	}

	store, err := Open(filepath.Join(t.TempDir(), "repo.db"), root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	stats, err := store.Build(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Build on a non-repository must succeed: %v", err)
	}
	if stats.Commits != 0 {
		t.Errorf("commits = %d, want 0", stats.Commits)
	}
}

func TestWalkSkipsDerivedDirectories(t *testing.T) {
	root := fixtureRepo(t, map[string]string{
		"src/main.go":               "package main\n",
		"node_modules/pkg/index.js": "module.exports = {}\n",
		"target/debug/build.rs":     "fn main() {}\n",
	}, []string{"chore: init"})

	entries, err := walk(root)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	seen := map[string]entry{}
	for _, e := range entries {
		seen[e.rel] = e
	}

	// The directory itself is recorded — a documented path beneath it must
	// resolve rather than be reported missing — but its contents are not.
	for _, d := range []string{"node_modules", "target"} {
		e, ok := seen[d]
		if !ok {
			t.Errorf("walk did not record the skipped directory %s", d)
			continue
		}
		if !e.skipped {
			t.Errorf("%s was recorded but not marked skipped", d)
		}
	}
	for rel := range seen {
		if strings.HasPrefix(rel, "node_modules/") || strings.HasPrefix(rel, "target/") {
			t.Errorf("walk descended into a derived directory: %s", rel)
		}
	}
}

func TestPathsUnderSkippedDirectoriesAreNotReportedMissing(t *testing.T) {
	// A committed vendor tree is present but deliberately unscanned. Absence
	// there is unknown, and a checker must not assert it.
	root := fixtureRepo(t, map[string]string{
		"README.md":                "See `vendor/github.com/x/x.go` and `gone/absent.go`.\n",
		"vendor/github.com/x/x.go": "package x\n",
	}, []string{"chore: init"})

	got := findingsOfKind(indexFixture(t, root), KindDanglingDocPath)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want only the genuinely absent one: %+v", len(got), got)
	}
	if !strings.Contains(got[0].Claim, "gone/absent.go") {
		t.Errorf("wrong finding: %+v", got[0])
	}
}

func TestSubdirectoryDoesNotInheritParentHistory(t *testing.T) {
	root := fixtureRepo(t, map[string]string{
		"sub/ROADMAP.md": "## Phase 3 \u2705\n",
	}, []string{"feat: Phase 9 something the parent shipped"})

	store, err := Open(filepath.Join(t.TempDir(), "repo.db"), filepath.Join(root, "sub"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	stats, err := store.Build(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	// `git -C sub log` would happily report the parent's commits.
	if stats.Commits != 0 {
		t.Errorf("commits = %d, want 0 — a subdirectory has no history of its own", stats.Commits)
	}
}

// TestBuildFailureLeavesTheIndexQueryable covers the pre-transaction failure
// path only: a cancelled context stops Build inside readCommits, before any
// write happens. It does NOT exercise the clear-inside-the-transaction
// ordering — TestResetParticipatesInTheTransaction does that.
func TestBuildFailureLeavesTheIndexQueryable(t *testing.T) {
	root := fixtureRepo(t, map[string]string{
		"README.md": "See `does/not/exist.ts`.\n",
	}, []string{"chore: init"})

	store, err := Open(filepath.Join(t.TempDir(), "repo.db"), root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	if _, err := store.Build(context.Background(), Options{}); err != nil {
		t.Fatalf("first Build: %v", err)
	}
	before, err := store.Findings(context.Background())
	if err != nil {
		t.Fatalf("Findings: %v", err)
	}
	if len(before) == 0 {
		t.Fatal("fixture should produce a finding")
	}

	// A build that cannot start must leave the previous index intact.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := store.Build(cancelled, Options{}); err == nil {
		t.Fatal("Build with a cancelled context should fail")
	}

	after, err := store.Findings(context.Background())
	if err != nil {
		t.Fatalf("Findings after failed build: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("failed build changed the index: %d findings before, %d after", len(before), len(after))
	}
}

// TestResetParticipatesInTheTransaction pins the property that clearing the
// index is part of the same transaction as refilling it.
//
// Reset used to run on the connection pool before the transaction opened, so
// any later failure left the index emptied and the report layer read it as a
// repository genuinely containing nothing.
//
// The guarantee here is structural as much as behavioural: reset takes a
// *sql.Tx, so a regression to pool-based clearing cannot compile against this
// test. The rollback assertion then confirms the transaction actually governs
// the delete. A cancelled-context test cannot substitute — Build fails earlier,
// inside readCommits, and never reaches the clear at all.
func TestResetParticipatesInTheTransaction(t *testing.T) {
	root := fixtureRepo(t, map[string]string{
		"README.md": "See `does/not/exist.ts`.\n",
	}, []string{"chore: init"})

	store, err := Open(filepath.Join(t.TempDir(), "repo.db"), root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	if _, err := store.Build(context.Background(), Options{}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	before := countRows(t, store, "files")
	if before == 0 {
		t.Fatal("fixture indexed no files")
	}

	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if err := reset(context.Background(), tx); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if after := countRows(t, store, "files"); after != before {
		t.Errorf("rolled-back reset still cleared the index: %d files before, %d after", before, after)
	}
}

func countRows(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	// The table name is a compile-time constant from the caller, never input.
	if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatalf("counting %s: %v", table, err)
	}
	return n
}
