package index

import (
	"regexp"
	"strings"
)

// makeTarget is one target and the recipe it runs.
type makeTarget struct {
	name   string
	line   int
	recipe string
}

// targetRe matches a target definition: a name at column zero followed by a
// colon. Pattern rules (`%.o:`) and variable assignments (`FOO := bar`) are
// excluded — the former is not a runnable entry point and the latter is not a
// target at all.
var targetRe = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*)\s*:(?:[^=]|$)`)

// scanMakeTargets parses a Makefile into targets and their recipe bodies.
//
// Recipe lines are tab-indented by Make's own rules, which makes the body
// unambiguous to collect without implementing Make itself. `.PHONY` and other
// dot-directives are skipped: they declare properties of targets rather than
// being targets.
func scanMakeTargets(content string) []makeTarget {
	var out []makeTarget
	lines := strings.Split(content, "\n")

	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.HasPrefix(line, ".") || strings.HasPrefix(line, "\t") {
			continue
		}
		m := targetRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]

		var recipe []string
		for j := i + 1; j < len(lines); j++ {
			next := lines[j]
			if strings.HasPrefix(next, "\t") {
				recipe = append(recipe, strings.TrimSpace(next))
				continue
			}
			// A blank line inside a recipe is legal and does not end it; any
			// other unindented line does.
			if strings.TrimSpace(next) == "" {
				continue
			}
			break
		}
		out = append(out, makeTarget{
			name: name,
			line: i + 1,
			// Length-capped but NOT control-stripped: scriptRefs reads this
			// back, and scrubbing first turned "python3 \x1b[31mfoo.py" into
			// something the interpreter pattern no longer matched — a way to
			// hide an advertised script from the check. The recipe is never
			// printed; the target name and script that are both go through
			// newFinding.
			recipe: capLength(strings.Join(recipe, "\n")),
		})
	}
	return out
}

// interpreterRe matches an invocation whose next bare argument is a script this
// repository is expected to contain.
var interpreterRe = regexp.MustCompile(`\b(?:python3?|node|bash|sh|ruby|deno|tsx|ts-node)\s+([A-Za-z0-9_./-]+)`)

// scriptRefs extracts the script paths a recipe invokes.
//
// Only interpreter invocations are considered. A recipe line like `go test
// ./...` names a package pattern rather than a file, and `make -C dir` names a
// directory Make resolves itself — neither is a claim that a specific file
// exists, so neither is recorded.
func scriptRefs(recipe string) []string {
	seen := map[string]bool{}
	var out []string

	for _, m := range interpreterRe.FindAllStringSubmatch(recipe, -1) {
		cand := m[1]
		// A flag caught by the greedy character class is not a script.
		if strings.HasPrefix(cand, "-") {
			continue
		}
		// Require an extension. `python -m mod` and `node --version` would
		// otherwise register as missing files.
		if !pathExtRe.MatchString(cand) {
			continue
		}
		cand = strings.TrimPrefix(cand, "./")
		if seen[cand] {
			continue
		}
		seen[cand] = true
		out = append(out, cand)
	}
	return out
}
