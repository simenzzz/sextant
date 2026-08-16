package index

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIgnoreLookupSurvivesARefusedCandidate is the regression test for the
// second security review's remaining MEDIUM.
//
// `git check-ignore --stdin` aborts the ENTIRE batch on the first path it
// refuses and prints nothing. Filtering `..` closed one way to trigger that,
// but not the general case: a path through a symlinked directory draws
// "fatal: pathspec is beyond a symbolic link", exit 128. One such reference in
// one document silently emptied the ignore set for the whole repository, so
// every gitignored artifact path then reported as drift.
func TestIgnoreLookupSurvivesARefusedCandidate(t *testing.T) {
	root := fixtureRepo(t, map[string]string{
		".gitignore":   "buildout/\n",
		"real/keep.md": "placeholder\n",
		// buildout/report.md is gitignored, so it must stay suppressed.
		"README.md": "Report at `buildout/report.md`.\n",
	}, []string{"chore: init"})

	// A symlinked directory, and a reference through it. git refuses the second
	// but must not be allowed to take the first down with it.
	if err := os.Symlink("real", filepath.Join(root, "esc")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	readme := filepath.Join(root, "README.md")
	if err := os.WriteFile(readme,
		[]byte("Report at `buildout/report.md`.\nAlso `esc/x.md`.\n"), 0o644); err != nil {
		t.Fatalf("rewriting README: %v", err)
	}

	findings := indexFixture(t, root)

	for _, f := range findings {
		if strings.Contains(f.Claim, "buildout/report.md") {
			t.Errorf("a gitignored path was reported after a refused candidate: %+v", f)
		}
	}
}

func TestGitIgnoredReportsIncompleteness(t *testing.T) {
	root := fixtureRepo(t, map[string]string{
		".gitignore": "buildout/\n",
		"README.md":  "# Fixture\n",
	}, []string{"chore: init"})

	// A well-formed batch is answered completely.
	got, complete := gitIgnored(context.Background(), root, []string{"buildout/report.md"})
	if !complete {
		t.Error("a well-formed batch should report completeness")
	}
	if !got["buildout/report.md"] {
		t.Errorf("gitignored path not recognised: %v", got)
	}

	// No candidates is trivially complete, not a failure.
	if _, complete := gitIgnored(context.Background(), root, nil); !complete {
		t.Error("an empty batch should report completeness")
	}
}

func TestCappedBufferBoundsMemory(t *testing.T) {
	// A commit subject has no length limit. One 200 MB subject reached 906 MB of
	// RSS through an unbounded buffer, from a repository whose packed objects
	// were 386 KiB.
	c := &cappedBuffer{limit: 16}

	n, err := c.Write([]byte("0123456789"))
	if err != nil || n != 10 {
		t.Fatalf("Write = (%d, %v), want (10, nil)", n, err)
	}
	// A short write must report the full length so the child never sees EPIPE.
	n, err = c.Write([]byte("abcdefghijklmnop"))
	if err != nil || n != 16 {
		t.Fatalf("Write = (%d, %v), want (16, nil)", n, err)
	}
	if got := c.buf.Len(); got != 16 {
		t.Errorf("buffered %d bytes, want the 16-byte cap", got)
	}
	if !c.truncated {
		t.Error("truncation was not recorded")
	}
	if got := c.buf.String(); got != "0123456789abcdef" {
		t.Errorf("buffered %q, want the first 16 bytes", got)
	}

	// Writes past the cap keep succeeding rather than erroring.
	if n, err := c.Write([]byte("more")); err != nil || n != 4 {
		t.Errorf("Write past the cap = (%d, %v), want (4, nil)", n, err)
	}
}

func TestGitEnvDropsConfigPayloadVars(t *testing.T) {
	// Inert today only because GIT_CONFIG_COUNT is dropped and git gates on it.
	// Leaving the payload loaded next to the trigger is not a defence.
	t.Setenv("GIT_CONFIG_KEY_0", "core.fsmonitor")
	t.Setenv("GIT_CONFIG_VALUE_0", "touch /tmp/pwned")

	for _, kv := range gitEnv() {
		if strings.HasPrefix(kv, "GIT_CONFIG_KEY_") || strings.HasPrefix(kv, "GIT_CONFIG_VALUE_") {
			t.Errorf("config payload survived scrubbing: %q", kv)
		}
	}
}
