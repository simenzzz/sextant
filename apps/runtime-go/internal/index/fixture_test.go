package index

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// fixtureRepo writes a throwaway repository and returns its root.
//
// Real git history rather than a fake: the phase checks read commit subjects,
// and the gitignore exclusion shells out to `git check-ignore`. Faking either
// would test the fake. The repository is local, offline, and thrown away with
// the test's temp dir.
func fixtureRepo(t *testing.T, files map[string]string, commitSubjects []string) string {
	t.Helper()
	root := t.TempDir()

	for rel, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(abs), err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("writing %s: %v", abs, err)
		}
	}

	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		// A contributor's global git config must not change what the test sees.
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	git("init", "-q", "-b", "main")
	git("add", "-A")
	for _, subject := range commitSubjects {
		git("commit", "-q", "--allow-empty", "-m", subject)
	}
	return root
}

// indexFixture builds an index over root and returns its findings.
func indexFixture(t *testing.T, root string) []Finding {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "repo.db"), root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	if _, err := store.Build(context.Background(), Options{}); err != nil {
		t.Fatalf("Build: %v", err)
	}
	findings, err := store.Findings(context.Background())
	if err != nil {
		t.Fatalf("Findings: %v", err)
	}
	return findings
}

func findingsOfKind(findings []Finding, kind string) []Finding {
	var out []Finding
	for _, f := range findings {
		if f.Kind == kind {
			out = append(out, f)
		}
	}
	return out
}
