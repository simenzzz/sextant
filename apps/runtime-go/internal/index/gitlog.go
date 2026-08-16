package index

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// commitRec is one commit, reduced to what a claim can be checked against.
type commitRec struct {
	sha         string
	committedAt string
	subject     string
}

// defaultCommitLimit bounds history when the caller asks for "all of it".
const defaultCommitLimit = 20_000

// maxGitOutput bounds the BYTES of git log output, which defaultCommitLimit
// does not: a commit subject has no length limit, and one 200 MB subject took
// 906 MB of RSS through an unbounded buffer — from a repository whose packed
// objects were 386 KiB. Twenty thousand commits of ordinary subjects come to a
// few megabytes, so this is generous by three orders of magnitude.
const maxGitOutput = 16 << 20 // 16 MiB

// cappedBuffer collects up to limit bytes and silently discards the rest.
//
// Discarding rather than erroring keeps the child writing to a pipe that still
// accepts data: failing the write would hand git an EPIPE and turn a bounded
// read into a spurious "git is broken" error.
type cappedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if remain := c.limit - c.buf.Len(); remain > 0 {
		if len(p) <= remain {
			return c.buf.Write(p)
		}
		c.buf.Write(p[:remain])
	}
	c.truncated = true
	return len(p), nil
}

// gitFieldSep is an ASCII unit separator. Commit subjects routinely contain
// colons, pipes, and tabs — every obvious delimiter — so the format string uses
// a byte that cannot appear in a subject line.
const gitFieldSep = "\x1f"

// readCommits returns the repository's commits, newest first.
//
// A directory that is not a git repository is not an error: plenty of trees
// worth indexing have no history, and the report layer simply has no
// commit-derived findings to offer for them.
func readCommits(ctx context.Context, root string, limit int) ([]commitRec, error) {
	// `git -C dir log` searches upward, so indexing a plain subdirectory would
	// otherwise report the enclosing repository's commits and compare this
	// tree's docs against another tree's delivery history.
	if !isRepoRoot(ctx, root) {
		return nil, nil
	}

	args := []string{
		"log",
		"--no-merges",
		"--date=short",
		"--format=%H" + gitFieldSep + "%cd" + gitFieldSep + "%s",
	}
	if limit <= 0 {
		// Unbounded by default meant a 300,000-commit history was buffered
		// whole. Phase checks only ever need the delivery commits, and a
		// repository with more than this many is well past the point where one
		// more matters.
		limit = defaultCommitLimit
	}
	args = append(args, fmt.Sprintf("-%d", limit))

	cmd, cancel := gitCommand(ctx, root, args...)
	defer cancel()
	stdout := &cappedBuffer{limit: maxGitOutput}
	stderr := &cappedBuffer{limit: 8 << 10}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		// Distinguish "no history here" from "git is broken". The former is a
		// normal state; the latter should surface.
		msg := stderr.buf.String()
		if strings.Contains(msg, "not a git repository") ||
			strings.Contains(msg, "does not have any commits") {
			return nil, nil
		}
		// git's stderr is repository-controlled text on its way to a terminal
		// and possibly a pull-request comment; it goes through the same
		// scrubber as everything else derived from the tree.
		return nil, fmt.Errorf("index: reading git log: %w: %s", err, sanitize(strings.TrimSpace(msg)))
	}

	var out []commitRec
	// A truncated read loses the oldest commits, which are the least likely to
	// be the delivery commits the phase checks look for. The subjects that did
	// arrive are still whole records, so they are still usable.
	for _, line := range strings.Split(stdout.buf.String(), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, gitFieldSep, 3)
		if len(parts) != 3 {
			continue
		}
		out = append(out, commitRec{
			sha:         parts[0],
			committedAt: parts[1],
			subject:     trimForStorage(parts[2]),
		})
	}
	return out, nil
}

// commitPhases returns the set of phase identifiers a commit subject claims to
// have shipped.
//
// Only subjects that read as delivery are considered. "feat: Phase 3 state
// persistence" is a claim that Phase 3 landed; "docs: plan Phase 7" is not, and
// counting it would invent contradictions against roadmaps that are simply
// looking ahead.
func commitPhases(subject string) []string {
	if !isDeliveryCommit(subject) {
		return nil
	}
	phase := extractPhase(subject)
	if phase == "" {
		return nil
	}
	return []string{phase}
}

// deliveryPrefixes are the conventional-commit types that assert working code.
// `docs`, `chore`, and `ci` are excluded on purpose.
var deliveryPrefixes = []string{"feat", "fix", "perf", "refactor"}

func isDeliveryCommit(subject string) bool {
	lower := strings.ToLower(strings.TrimSpace(subject))
	for _, p := range deliveryPrefixes {
		// Match "feat:" and "feat(scope):" but not "feature-flag".
		if strings.HasPrefix(lower, p+":") || strings.HasPrefix(lower, p+"(") {
			return true
		}
	}
	return false
}

// isRepoRoot reports whether root is itself the top of a work tree.
func isRepoRoot(ctx context.Context, root string) bool {
	cmd, cancel := gitCommand(ctx, root, "rev-parse", "--show-toplevel")
	defer cancel()
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return false
	}

	top, err := filepath.EvalSymlinks(strings.TrimSpace(stdout.String()))
	if err != nil {
		return false
	}
	// The caller has already made root absolute; resolve both so a symlinked
	// temp dir (macOS /var, and t.TempDir everywhere) compares equal.
	here, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	return filepath.Clean(top) == filepath.Clean(here)
}
