package index

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// isScratchDoc reports whether a markdown file is a working log rather than a
// statement about the repository.
//
// `.claude/plans/` is a per-session scratch log by convention in this
// workspace: entries are dated, superseded rather than corrected, and never
// meant to describe the tree as it stands. Reading them as claims produced 236
// findings on learningproj, which carries 105 of them — noise that would bury
// every real finding in the same report.
func isScratchDoc(rel string) bool {
	return strings.Contains(rel, ".claude/plans/") ||
		strings.HasPrefix(rel, "plans/") ||
		strings.Contains(rel, "/archive/") ||
		strings.HasPrefix(rel, "archive/")
}

// gitIgnored returns the subset of candidates that git would ignore.
//
// A documented path that is gitignored is a build artifact or a run output:
// `web/playwright-report/` is where the test reporter writes, and its absence
// from a clean tree is correct rather than drift. Asking git is better than
// re-implementing .gitignore semantics, which include negation, precedence, and
// nested ignore files.
//
// A repository without git, or a git that fails, yields an empty set: the
// checks then run as if nothing were ignored, which is the conservative
// direction — it can only add findings a human will judge, never hide one.
// The bool reports whether every candidate was classified. False means the
// suppression in this report is incomplete.
func gitIgnored(ctx context.Context, root string, candidates []string) (map[string]bool, bool) {
	out := map[string]bool{}
	if len(candidates) == 0 {
		return out, true
	}

	// A candidate escaping the repository aborts the WHOLE batch: git exits 128
	// on the first such path and prints nothing, silently emptying the ignore
	// set. One crafted reference in one document was enough to disable this
	// filter everywhere. looksLikePath rejects a leading "../", but not an
	// embedded one such as "x/../../../etc/passwd.md".
	safe := candidates[:0:0]
	for _, c := range candidates {
		if hasTraversal(c) {
			continue
		}
		safe = append(safe, c)
	}
	if len(safe) == 0 {
		return out, true
	}
	candidates = safe

	// Chunked, because `git check-ignore --stdin` aborts the ENTIRE batch on
	// the first path it refuses and prints nothing. `..` is filtered above, but
	// it is not the only refusable form — a path through a symlinked directory
	// draws "beyond a symbolic link", exit 128 — and one such candidate
	// silently emptied the ignore set for the whole repository.
	//
	// Splitting on failure isolates the offender in O(log n) extra passes
	// instead of losing every suppression.
	// Returned rather than swallowed: a failed lookup and a repository with no
	// ignore rules produced identical output, which is what made the original
	// defect invisible.
	return out, lookupChunk(ctx, root, candidates, out)
}

// lookupChunk fills found with the ignored subset of candidates, splitting on
// failure. Reports whether every candidate was successfully classified.
func lookupChunk(ctx context.Context, root string, candidates []string, found map[string]bool) bool {
	if len(candidates) == 0 {
		return true
	}

	if runIgnoreQuery(ctx, root, candidates, found) {
		return true
	}
	if len(candidates) == 1 {
		// This single candidate is the one git refuses. Treat it as not
		// ignored — the conservative direction, since it can only add a
		// finding a human will judge, never hide one.
		return false
	}

	mid := len(candidates) / 2
	left := lookupChunk(ctx, root, candidates[:mid], found)
	right := lookupChunk(ctx, root, candidates[mid:], found)
	return left && right
}

// runIgnoreQuery asks git about one batch, reporting whether it answered.
func runIgnoreQuery(ctx context.Context, root string, candidates []string, found map[string]bool) bool {
	// Each candidate is probed twice, bare and with a trailing slash.
	//
	// A gitignore pattern ending in "/" matches directories only, and
	// check-ignore decides whether a path is a directory by looking at the
	// filesystem. These paths are exactly the ones that do not exist, so the
	// bare form can never match a directory rule: ishtirak's `.gitignore` lists
	// `playwright-report/`, and `web/playwright-report` was still reported as
	// drift until the slashed form was asked about too.
	probe := make([]string, 0, len(candidates)*2)
	for _, c := range candidates {
		probe = append(probe, c, c+"/")
	}

	cmd, cancel := gitCommand(ctx, root, "check-ignore", "--stdin")
	defer cancel()
	cmd.Stdin = strings.NewReader(strings.Join(probe, "\n") + "\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Exit 1 means "nothing matched" and is a normal answer. Anything above
	// that means git refused the batch and printed nothing usable.
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 {
			return false
		}
	}

	for _, line := range strings.Split(stdout.String(), "\n") {
		p := strings.TrimSpace(line)
		if p == "" {
			continue
		}
		found[strings.TrimSuffix(p, "/")] = true
	}
	return true
}

// hasTraversal reports whether any segment of a slash path is "..".
func hasTraversal(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// siblingRepos returns the names of git repositories sitting alongside this one.
//
// A reference like `loom/apps/crawler-go/internal/fetch/clock.go` inside
// sextant's PLAN.md points at a different project in the same workspace, not at
// a missing file in this one.
//
// **Off by default, and deliberately.** The answer depends on directories
// outside the tree being examined, so enabling it makes a report a function of
// its environment: a developer with 34 neighbouring folders and a CI runner
// with one would disagree about the same commit. That breaks the determinism
// this tool promises and would make `plumb-check` an unreliable gate. It is
// available for interactive use in a multi-repo workspace, where the noise it
// removes is worth more than reproducibility.
//
// Even when enabled, a neighbour must contain a `.git` directory. Without that
// test, an unrelated `docs/` or `eval/` folder next door silently suppressed
// genuine findings.
func siblingRepos(root string) map[string]bool {
	parent := filepath.Dir(root)
	if parent == root {
		return nil
	}
	items, err := os.ReadDir(parent)
	if err != nil {
		return nil
	}
	self := filepath.Base(root)
	out := map[string]bool{}
	for _, it := range items {
		if !it.IsDir() || it.Name() == self {
			continue
		}
		if _, err := os.Stat(filepath.Join(parent, it.Name(), ".git")); err != nil {
			continue
		}
		out[it.Name()] = true
	}
	return out
}

// firstSegment returns the leading path component of a slash path.
func firstSegment(p string) string {
	if i := strings.Index(p, "/"); i >= 0 {
		return p[:i]
	}
	return p
}
