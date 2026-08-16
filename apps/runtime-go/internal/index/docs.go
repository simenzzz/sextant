package index

import (
	"path"
	"regexp"
	"strings"
)

// Normalized status vocabulary. Five projects in this workspace use four
// incompatible dialects — emoji, checkboxes, and two bare words — so every
// dialect is folded onto these three states before anything reasons about it.
const (
	MarkerDone       = "done"
	MarkerInProgress = "in-progress"
	MarkerNotStarted = "not-started"
)

// docClaim is one line of documentation that asserts a checkable status.
type docClaim struct {
	line   int
	text   string
	marker string
	phase  string
}

var (
	// Phase and milestone identifiers: "Phase 3", "P0", "M5a", "Milestone 2".
	// The trailing letter on M5a is deliberate — tideway numbers milestones
	// that way, and dropping it would merge M5a into M5.
	//
	// The bare P-form is capital-only and capped at one digit precisely so that
	// "p50 / p95 latency" is not read as phases 50 and 95. A roadmap does not
	// number phases past 9 in the bare form; the word form still takes two.
	phaseRe = regexp.MustCompile(`(?i)\b(?:phase|milestone)\s*([0-9]{1,2}[a-z]?)\b|\b([PM][0-9][a-z]?)\b`)

	// todoContractRe distinguishes the stub contract from a status word. The
	// open paren is the whole signal: "TODO(you)" names a code marker, a bare
	// "TODO" in a status column grades the row.
	todoContractRe = regexp.MustCompile(`\bTODO\s*\(`)

	// A markdown task checkbox at the start of a list item.
	checkedRe   = regexp.MustCompile(`^\s*[-*+]\s*\[[xX]\]`)
	uncheckedRe = regexp.MustCompile(`^\s*[-*+]\s*\[\s\]`)

	// Bare-word states, as whole words so "INCOMPLETE" never reads as
	// "COMPLETE" and a sentence about "work done here" is not a claim.
	completeWordRe = regexp.MustCompile(`\b(?:COMPLETE|COMPLETED|DONE|SHIPPED)\b`)
	todoWordRe     = regexp.MustCompile(`\b(?:NOT STARTED|NOT-STARTED|TODO|PLANNED|PENDING)\b`)
	wipWordRe      = regexp.MustCompile(`\b(?:IN PROGRESS|IN-PROGRESS|WIP|PARTIAL)\b`)
)

// classifyMarker folds one documentation line onto the normalized vocabulary,
// returning "" when the line asserts no status at all.
//
// Emoji are checked before words because a heading like "Phase 0 — Skeleton 🚧"
// carries both a marker and prose, and the marker is the authoritative half.
// Order among the emoji matters too: a line carrying both ✅ and ⬜ is a legend
// rather than a claim, and is rejected below.
func classifyMarker(line string) string {
	done := strings.Contains(line, "✅")
	wip := strings.Contains(line, "🚧")
	todo := strings.Contains(line, "⬜")

	// A legend line ("✅ done · 🚧 in progress · ⬜ not started") defines the
	// vocabulary rather than using it. Every roadmap in this workspace opens
	// with one, and reading it as a claim would produce a finding in each.
	if boolCount(done, wip, todo) > 1 {
		return ""
	}
	switch {
	case done:
		return MarkerDone
	case wip:
		return MarkerInProgress
	case todo:
		return MarkerNotStarted
	}

	switch {
	case checkedRe.MatchString(line):
		return MarkerDone
	case uncheckedRe.MatchString(line):
		return MarkerNotStarted
	}

	// Bare words are the weakest dialect and the easiest to misread, so they
	// are consulted last and only in upper case — prose says "done", a status
	// column says "DONE".
	switch {
	case wipWordRe.MatchString(line):
		return MarkerInProgress
	case completeWordRe.MatchString(line):
		return MarkerDone
	case todoWordRe.MatchString(line):
		// "TODO(you)" is this workspace's stub contract, not a roadmap status.
		// Without this, PLAN.md's own P6.5 row — which mentions the contract in
		// prose — graded phase 6 as not-started, and the first
		// "feat: Phase 6 ..." commit would have turned plumb-check red on this
		// repository's own roadmap.
		if todoContractRe.MatchString(line) {
			return ""
		}
		return MarkerNotStarted
	}
	return ""
}

func boolCount(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

// extractPhase returns the normalized phase identifier a line names ("3", "0",
// "5a"), or "" when it names none. Normalization drops the P/M prefix and the
// word so that "Phase 3", "P3", and "phase 3" all key the same.
func extractPhase(line string) string {
	if ps := extractPhases(line); len(ps) > 0 {
		return ps[0]
	}
	return ""
}

// extractPhases returns every phase identifier a line names, in order and
// deduplicated.
//
// Separate from extractPhase because the two callers want different things: a
// doc_claims row or a commit subject grades exactly one phase, while "is this
// phase documented anywhere?" must see all of them.
func extractPhases(line string) []string {
	matches := phaseRe.FindAllStringSubmatch(line, -1)
	if matches == nil {
		return nil
	}

	seen := map[string]bool{}
	var out []string
	for _, m := range matches {
		var p string
		if m[1] != "" {
			p = strings.ToLower(m[1])
		} else {
			// m[2] is the bare form: strip the leading P or M.
			p = strings.ToLower(strings.TrimLeft(m[2], "PMpm"))
		}
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// scanDocClaims extracts every status-asserting line from one markdown file.
//
// Fenced code blocks are skipped: a shell transcript or a sample roadmap inside
// ``` is illustrative, not an assertion the repository is making.
func scanDocClaims(content string) []docClaim {
	var out []docClaim
	inFence := false

	for i, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		marker := classifyMarker(line)
		if marker == "" {
			continue
		}
		out = append(out, docClaim{
			line:   i + 1,
			text:   trimForStorage(line),
			marker: marker,
			phase:  extractPhase(line),
		})
	}
	return out
}

// scanDocPhases returns every line that names a phase, whatever its status.
//
// Separate from scanDocClaims because a roadmap row can name a phase without
// grading it — this repository's own P0-P9 table has no status column — and
// treating "ungraded" as "undocumented" reported PLAN.md as missing an entry
// that is plainly there.
func scanDocPhases(content string) map[int][]string {
	out := map[int][]string{}
	inFence := false

	for i, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		// Every phase on the line, not just the first. A row reading
		// "| P1/P4 |" documents both, and recording only P1 left P4 looking
		// undocumented — the exact false positive doc_phases exists to remove.
		if ps := extractPhases(line); len(ps) > 0 {
			out[i+1] = append(out[i+1], ps...)
		}
	}
	return out
}

// backtickPathRe finds a code span that looks like a filesystem path: it either
// contains a slash or ends in a known source extension. Requiring one of those
// is what keeps `SELECT` and `make` out of the path table.
var backtickPathRe = regexp.MustCompile("`([A-Za-z0-9_./@-]+)`")

var pathExtRe = regexp.MustCompile(`\.(go|rs|py|ts|tsx|js|jsx|java|sh|sql|md|ya?ml|toml|json|lock|tf|proto)$`)

// Reference hints. A reference that names a file ("eval/run.py") or is written
// as a directory ("eval/") asserts something specific. A bare two-segment span
// asserts far less: `try/catch`, `ui/Button`, and `and/or` are all prose, and
// no lexical rule separates them from `packages/contracts`.
//
// The hint is recorded rather than filtered at scan time so that the empty-
// directory check can still use ambiguous references, which it can do safely
// because it resolves them exactly and requires the directory to exist.
const (
	HintFile      = "file"
	HintDir       = "dir"
	HintAmbiguous = "ambiguous"
)

// docPathRefRaw is one extracted reference before it reaches the database.
type docPathRefRaw struct {
	target string
	hint   string
}

// scanDocPaths extracts the filesystem paths a document points at.
//
// Only code spans are considered. Prose mentions a directory in passing all the
// time ("the eval harness"), while a backticked `eval/run.py` is the document
// making a specific, checkable assertion about the tree.
func scanDocPaths(content string) map[int][]docPathRefRaw {
	out := map[int][]docPathRefRaw{}
	inFence := false

	for i, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		for _, m := range backtickPathRe.FindAllStringSubmatch(line, -1) {
			cand := m[1]
			if !looksLikePath(cand) {
				continue
			}
			target := cleanTarget(cand)
			if target == "" {
				continue
			}
			out[i+1] = append(out[i+1], docPathRefRaw{
				// "./scripts/foo.sh" and "scripts/foo.sh" name the same file;
				// scriptRefs already normalises this and scanDocPaths must
				// agree, or ordinary "run `./x.sh`" prose reads as drift.
				// path.Clean rather than TrimPrefix: ".//x.md" would otherwise
				// become "/x.md" and read as an absolute path.
				target: target,
				hint:   hintFor(cand),
			})
		}
	}
	return out
}

// looksLikePath decides whether a code span is asserting a repository path.
//
// Conservative by construction: this feeds a finding that says "your docs point
// at something that is not there", and a false positive there is worse than a
// miss. Anything ambiguous is dropped.
func looksLikePath(s string) bool {
	if s == "" || len(s) > 200 {
		return false
	}
	// URLs, flags, and shell fragments are not repository paths.
	if strings.Contains(s, "://") || strings.HasPrefix(s, "-") || strings.HasPrefix(s, "@") {
		return false
	}
	// An elision in prose — `engine-rust/.../generated` — names a shape rather
	// than a path, and no such file was ever meant to exist.
	if strings.Contains(s, "...") {
		return false
	}
	// Absolute paths point outside the repository; a claim about /usr/bin is
	// not a claim about this tree.
	if strings.HasPrefix(s, "/") {
		return false
	}
	// A parent traversal anywhere makes this not a claim about a path in this
	// repository. Checked on segments, not as a prefix: "x/../../etc/passwd.md"
	// escapes just as surely as "../x", and it additionally used to abort the
	// whole gitignore lookup.
	if hasTraversal(s) {
		return false
	}
	hasSlash := strings.Contains(s, "/")
	hasExt := pathExtRe.MatchString(s)
	if !hasSlash && !hasExt {
		return false
	}
	return true
}

// hintFor classifies how specific a reference is. Trailing slash beats
// extension: `src/gen/` is a directory even though the span has no dot.
func hintFor(raw string) string {
	if strings.HasSuffix(raw, "/") {
		return HintDir
	}
	if pathExtRe.MatchString(raw) {
		return HintFile
	}
	return HintAmbiguous
}

// cleanTarget normalises a reference to the form the index stores paths in.
// Returns "" for anything that normalises to nothing meaningful.
func cleanTarget(raw string) string {
	c := path.Clean(strings.TrimSuffix(raw, "/"))
	if c == "." || c == "/" {
		return ""
	}
	return c
}
