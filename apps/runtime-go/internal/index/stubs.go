package index

import (
	"strings"
)

// stubMarker is one recognized way a language says "this is not implemented".
type stubMarker struct {
	name  string
	match func(line string) bool
}

// stubMarkers covers what this workspace actually produces. Deliberately not a
// general "TODO" scanner: a `// TODO: tidy this up` is a note beside working
// code, while these markers mean the body does not exist. Conflating them would
// bury the signal under every aspirational comment in the tree.
var stubMarkers = []stubMarker{
	{
		// The house contract's own marker, in any language. Checked first so a
		// TODO(you) panic is labelled as the contract's rather than Go's.
		name:  "todo-you",
		match: func(l string) bool { return strings.Contains(l, "TODO(you)") },
	},
	{
		name: "panic-todo",
		match: func(l string) bool {
			return strings.Contains(l, `panic("TODO`) || strings.Contains(l, "panic(`TODO")
		},
	},
	{
		name:  "unimplemented",
		match: func(l string) bool { return strings.Contains(l, "unimplemented!") },
	},
	{
		name:  "todo-macro",
		match: func(l string) bool { return strings.Contains(l, "todo!(") },
	},
	{
		name: "not-implemented",
		match: func(l string) bool {
			return strings.Contains(l, "NotImplementedError")
		},
	},
	{
		name: "throw-not-implemented",
		match: func(l string) bool {
			return strings.Contains(l, "new Error(\"Not implemented") ||
				strings.Contains(l, "new Error('Not implemented")
		},
	},
}

// stubHit is one detected unimplemented site.
type stubHit struct {
	line   int
	marker string
	text   string
}

// scanStubs finds unimplemented sites in one file's contents.
//
// The first matching marker wins, so a `panic("TODO(you): …")` is recorded once
// as todo-you rather than twice. Recording it twice would inflate every count
// the report layer draws conclusions from.
func scanStubs(content string) []stubHit {
	var hits []stubHit
	for i, line := range strings.Split(content, "\n") {
		for _, m := range stubMarkers {
			if !m.match(line) {
				continue
			}
			hits = append(hits, stubHit{
				line:   i + 1,
				marker: m.name,
				text:   trimForStorage(line),
			})
			break
		}
	}
	return hits
}

// maxStoredText bounds a stored excerpt. Long enough to identify the site when
// a human reads the report, short enough that a minified line cannot bloat the
// database.
const maxStoredText = 240

func trimForStorage(s string) string {
	return capLength(sanitize(strings.TrimSpace(s)))
}

// capLength truncates to maxStoredText on a rune boundary.
func capLength(s string) string {
	if len(s) <= maxStoredText {
		return s
	}
	// Cut on a rune boundary so the stored excerpt stays valid UTF-8.
	cut := maxStoredText
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

// sanitizePath scrubs a value that will be printed as a location.
//
// Stricter than sanitize: newlines go too. A directory whose name embeds one
// let a repository forge a "plumb:" header and a fake "no contradictions found"
// line inside a real report — harmless to the exit code, which is what a gate
// reads, but not to the human reading the log or the pull-request comment.
func sanitizePath(s string) string {
	return strings.ReplaceAll(sanitize(s), "\n", "")
}

// sanitize removes control characters from text that came out of a repository.
//
// Findings are printed to a terminal and, via the Action, into a pull-request
// comment. A file or a documentation line is attacker-controlled input when the
// repository is someone else's, and an ANSI escape sequence embedded in either
// would otherwise be rendered rather than shown.
//
// Newlines survive: a stored Makefile recipe is several lines joined by them,
// and dropping them would run `python3 eval/run.py` into the next command and
// change what the script scanner reads. A newline is not an escape sequence, so
// keeping it costs nothing. Tabs become spaces; everything else below 0x20,
// plus DEL and the C1 range, is dropped.
func sanitize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteByte('\n')
		case r == '\t':
			b.WriteByte(' ')
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			// dropped
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			// Unicode bidi overrides. Not control bytes, but they reorder
			// rendered text in a terminal and on GitHub, which is the same
			// deception the C0 range is dropped for.
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
