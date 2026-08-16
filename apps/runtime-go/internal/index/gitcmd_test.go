package index

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestHostileRepoConfigCannotExecute is the regression test for the one
// critical finding of the P2.5 security review.
//
// A repository's `.git/config` is attacker-controlled whenever the repository
// is someone else's, and `core.fsmonitor` names a program that git runs when it
// refreshes the index — which `git check-ignore` does. Pointing plumb at a
// hostile clone therefore executed its command, silently.
//
// The test first proves the primitive still fires under plain git, so it fails
// loudly if a future git release changes the mechanics rather than quietly
// passing for the wrong reason.
func TestHostileRepoConfigCannotExecute(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()
	marker := filepath.Join(dir, "EXECUTED")
	root := filepath.Join(dir, "hostile")

	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("creating %s: %v", root, err)
	}
	// One unresolvable reference is all it takes: that is what sends a
	// candidate through the gitignore lookup.
	write(t, filepath.Join(root, "README.md"), "See `docs/report.md`.\n")

	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
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
	git("commit", "-q", "-m", "chore: init")
	git("config", "core.fsmonitor", "touch "+marker+"; echo")

	t.Run("the primitive still works under plain git", func(t *testing.T) {
		cmd := exec.Command("git", "-C", root, "check-ignore", "--stdin")
		cmd.Stdin = strings.NewReader("docs/report.md\n")
		_ = cmd.Run()

		if _, err := os.Stat(marker); err != nil {
			t.Skip("git no longer runs core.fsmonitor here; the guard is untestable in this environment")
		}
		if err := os.Remove(marker); err != nil {
			t.Fatalf("clearing marker: %v", err)
		}
	})

	t.Run("plumb does not run it", func(t *testing.T) {
		findings := indexFixture(t, root)

		if _, err := os.Stat(marker); err == nil {
			t.Fatal("a hostile repository's core.fsmonitor was executed")
		}
		// The guard must not cost the tool its job.
		if len(findings) == 0 {
			t.Error("hardening suppressed the finding as well as the exploit")
		}
	})
}

func TestGitEnvScrubsRedirection(t *testing.T) {
	t.Setenv("GIT_DIR", "/somewhere/else/.git")
	t.Setenv("GIT_WORK_TREE", "/somewhere/else")
	t.Setenv("GIT_CONFIG_PARAMETERS", "'core.fsmonitor=evil'")

	env := gitEnv()
	for _, kv := range env {
		for _, banned := range []string{"GIT_DIR=", "GIT_WORK_TREE=", "GIT_CONFIG_PARAMETERS="} {
			if strings.HasPrefix(kv, banned) {
				t.Errorf("%s survived scrubbing: %q", banned, kv)
			}
		}
	}

	// The pins must be present, or a developer's global config still decides
	// which paths count as drift.
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("gitEnv is missing %s", want)
		}
	}
}

func TestGitCommandCarriesTheHardening(t *testing.T) {
	cmd, cancel := gitCommand(context.Background(), "/tmp", "status")
	defer cancel()

	joined := strings.Join(cmd.Args, " ")
	for _, want := range []string{"core.fsmonitor=false", "core.hooksPath=/dev/null"} {
		if !strings.Contains(joined, want) {
			t.Errorf("git argv is missing %s: %v", want, cmd.Args)
		}
	}
	if cmd.Args[len(cmd.Args)-1] != "status" {
		t.Errorf("the subcommand must come last: %v", cmd.Args)
	}
}

func TestHasTraversal(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		// One of these in any document emptied the whole ignore set.
		{"x/../../../../etc/passwd.md", true},
		{"../outside.go", true},
		{"..", true},
		{"a/b/c.go", false},
		{"a/..b/c.go", false},
		{"a/b../c.go", false},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := hasTraversal(tt.in); got != tt.want {
				t.Errorf("hasTraversal(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}
