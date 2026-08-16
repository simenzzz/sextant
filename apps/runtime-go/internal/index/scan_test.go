// Tests for the documentation scanners: status markers, phase identifiers, and
// path references — the three vocabularies the index normalises.
package index

import (
	"testing"
)

func TestClassifyMarker(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{"emoji done", "## Phase 2 — Parse stock ✅", MarkerDone},
		{"emoji wip", "## Phase 0 — Skeleton 🚧", MarkerInProgress},
		{"emoji not started", "## Phase 7 — Deploy ⬜", MarkerNotStarted},

		// Every roadmap in the workspace opens with a legend. Reading one as a
		// claim would produce a finding in each.
		{"legend line is not a claim", "Status markers: ⬜ not started · 🚧 in progress · ✅ done", ""},
		{"two markers is not a claim", "✅ done, ⬜ pending", ""},

		{"checked box", "- [x] `go mod init` the project", MarkerDone},
		{"unchecked box", "- [ ] **Milestone (live):** run the notifier", MarkerNotStarted},
		{"star bullet checked", "* [X] done item", MarkerDone},

		{"bare word complete", "| Phase 3 | COMPLETE |", MarkerDone},
		{"bare word done", "Status: DONE", MarkerDone},
		{"bare word in progress", "Phase 4: IN PROGRESS", MarkerInProgress},

		// "INCOMPLETE" contains "COMPLETE"; word boundaries must reject it.
		{"incomplete is not complete", "Phase 5 is INCOMPLETE", ""},
		// Lower-case prose is not a status column.
		{"prose done is not a claim", "the work is done here", ""},
		{"plain prose", "This paragraph asserts nothing.", ""},

		// A line carrying both a WIP word and a done word resolves to WIP,
		// because the weaker signal must not override the active one.
		{"wip wins over complete", "IN PROGRESS — not yet COMPLETE", MarkerInProgress},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyMarker(tt.line); got != tt.want {
				t.Errorf("classifyMarker(%q) = %q, want %q", tt.line, got, tt.want)
			}
		})
	}
}

func TestExtractPhase(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{"word form", "## Phase 0 — Skeleton", "0"},
		{"word form two digits", "Phase 10 shipped", "10"},
		{"lower case", "phase 3 notes", "3"},
		{"bare P form", "**P0** | 1 | Scaffold", "0"},
		{"bare M form", "M5a in progress", "5a"},
		{"milestone word", "Milestone 2 complete", "2"},
		{"suffixed milestone keeps letter", "Phase 5a", "5a"},
		{"no phase", "General notes about the project", ""},
		{"version is not a phase", "release 1.2.3", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractPhase(tt.line); got != tt.want {
				t.Errorf("extractPhase(%q) = %q, want %q", tt.line, got, tt.want)
			}
		})
	}
}

func TestScanDocClaimsSkipsFencedBlocks(t *testing.T) {
	content := "# Roadmap\n" +
		"## Phase 1 ✅\n" +
		"```\n" +
		"## Phase 2 ✅\n" + // illustrative, inside a fence
		"```\n" +
		"## Phase 3 🚧\n"

	got := scanDocClaims(content)
	if len(got) != 2 {
		t.Fatalf("got %d claims, want 2: %+v", len(got), got)
	}
	if got[0].phase != "1" || got[0].marker != MarkerDone {
		t.Errorf("first claim = %+v, want phase 1 done", got[0])
	}
	if got[1].phase != "3" || got[1].marker != MarkerInProgress {
		t.Errorf("second claim = %+v, want phase 3 in-progress", got[1])
	}
	if got[1].line != 6 {
		t.Errorf("second claim line = %d, want 6", got[1].line)
	}
}

func TestLooksLikePath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"file with extension", "eval/run.py", true},
		{"nested path", "packages/contracts/schemas", true},
		{"bare dotfile", ".env", false},
		{"extension only", "config.ts", true},
		{"url rejected", "https://example.com/a", false},
		{"flag rejected", "--json", false},
		{"absolute rejected", "/usr/bin/env", false},
		{"parent traversal rejected", "../other/file.go", false},
		{"ellipsis rejected", "engine-rust/.../generated", false},
		{"bare word rejected", "SELECT", false},
		{"single word no slash no ext", "make", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looksLikePath(tt.in); got != tt.want {
				t.Errorf("looksLikePath(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestHintFor(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"eval/run.py", HintFile},
		{"eval/", HintDir},
		{"src/gen/", HintDir},
		// The class that made Website unusable: prose containing a slash.
		{"try/catch", HintAmbiguous},
		{"ui/Button", HintAmbiguous},
		{"packages/contracts", HintAmbiguous},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := hintFor(tt.in); got != tt.want {
				t.Errorf("hintFor(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
