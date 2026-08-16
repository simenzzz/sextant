// Tests for the source-side scanners: stub markers, Makefile targets, and the
// commit-subject reader.
package index

import (
	"testing"
)

func TestScanStubs(t *testing.T) {
	content := `package x

func A() { panic("TODO(you): implement A") }
func B() { panic("TODO: later") }
func C() { return 1 }
`
	got := scanStubs(content)
	if len(got) != 2 {
		t.Fatalf("got %d stubs, want 2: %+v", len(got), got)
	}
	// The house marker must win over the generic panic form, so the site is
	// counted once and labelled as the contract's.
	if got[0].marker != "todo-you" || got[0].line != 3 {
		t.Errorf("first stub = %+v, want todo-you on line 3", got[0])
	}
	if got[1].marker != "panic-todo" || got[1].line != 4 {
		t.Errorf("second stub = %+v, want panic-todo on line 4", got[1])
	}
}

func TestScanStubsLanguages(t *testing.T) {
	tests := []struct {
		name   string
		line   string
		marker string
	}{
		{"rust unimplemented", "    unimplemented!()", "unimplemented"},
		{"rust todo macro", "    todo!(\"later\")", "todo-macro"},
		{"python", "    raise NotImplementedError('soon')", "not-implemented"},
		{"typescript", `  throw new Error("Not implemented");`, "throw-not-implemented"},
		{"ordinary comment is not a stub", "// TODO: tidy this up", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scanStubs(tt.line)
			if tt.marker == "" {
				if len(got) != 0 {
					t.Fatalf("got %+v, want no stubs", got)
				}
				return
			}
			if len(got) != 1 || got[0].marker != tt.marker {
				t.Fatalf("got %+v, want one %s", got, tt.marker)
			}
		})
	}
}

func TestScanMakeTargets(t *testing.T) {
	content := "\n" +
		".PHONY: eval\n" +
		"eval: ## run the harness\n" +
		"\tpython3 eval/run.py --all\n" +
		"\n" +
		"VERSION := 1.0\n" +
		"build:\n" +
		"\tgo build ./...\n"

	got := scanMakeTargets(content)
	if len(got) != 2 {
		t.Fatalf("got %d targets, want 2: %+v", len(got), got)
	}
	if got[0].name != "eval" || got[0].line != 3 {
		t.Errorf("first target = %+v, want eval on line 3", got[0])
	}
	// A `:=` assignment is not a target.
	if got[1].name != "build" {
		t.Errorf("second target = %+v, want build", got[1])
	}
}

func TestScriptRefs(t *testing.T) {
	tests := []struct {
		name   string
		recipe string
		want   []string
	}{
		{"python script", "python3 eval/run.py --smoke", []string{"eval/run.py"}},
		{"node script", "node scripts/build.js", []string{"scripts/build.js"}},
		{"leading dot slash stripped", "bash ./infra/smoke.sh", []string{"infra/smoke.sh"}},
		// A package pattern is not a file, and neither is a module flag.
		{"go packages ignored", "go test ./...", nil},
		{"python module ignored", "python3 -m pytest", nil},
		{"duplicates collapsed", "python3 a/b.py && python3 a/b.py", []string{"a/b.py"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scriptRefs(tt.recipe)
			if len(got) != len(tt.want) {
				t.Fatalf("scriptRefs(%q) = %v, want %v", tt.recipe, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("scriptRefs(%q)[%d] = %q, want %q", tt.recipe, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestCommitPhases(t *testing.T) {
	tests := []struct {
		name    string
		subject string
		want    string
	}{
		{"feat delivers", "feat: Phase 3 state persistence & change detection", "3"},
		{"feat with scope", "feat(runtime): Phase 6 vault", "6"},
		{"fix delivers", "fix: Phase 2 parser", "2"},
		// Planning and documentation are not delivery; counting them would
		// invent contradictions against roadmaps that are looking ahead.
		{"docs does not deliver", "docs: plan Phase 7", ""},
		{"chore does not deliver", "chore: scaffold Phase 9", ""},
		{"ci does not deliver", "ci: Phase 4 workflow", ""},
		{"no phase named", "feat: add the notifier", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := commitPhases(tt.subject)
			if tt.want == "" {
				if len(got) != 0 {
					t.Fatalf("commitPhases(%q) = %v, want none", tt.subject, got)
				}
				return
			}
			if len(got) != 1 || got[0] != tt.want {
				t.Fatalf("commitPhases(%q) = %v, want [%s]", tt.subject, got, tt.want)
			}
		})
	}
}

func TestIsScratchDoc(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{".claude/plans/some-plan.md", true},
		{"plans/older.md", true},
		{"docs/archive/old-spec.md", true},
		{"README.md", false},
		{"docs/ROADMAP.md", false},
		{".claude/CLAUDE.md", false},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := isScratchDoc(tt.in); got != tt.want {
				t.Errorf("isScratchDoc(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestTrimForStorageKeepsValidUTF8(t *testing.T) {
	// A multi-byte rune straddling the cut point must not be split in half.
	long := ""
	for len(long) < maxStoredText+10 {
		long += "é"
	}
	got := trimForStorage(long)
	for i, r := range got {
		if r == '�' {
			t.Fatalf("trimForStorage produced an invalid rune at byte %d", i)
		}
	}
}

func TestTodoContractIsNotAStatus(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		// The bug this pins was live in PLAN.md: a P6.5 row mentioning the stub
		// contract in prose graded phase 6 as not-started, so the first
		// "feat: Phase 6 ..." commit would have contradicted it.
		{"contract marker in prose", "| **P6.5** | Blocked on P1: an open `TODO(you)`. |", ""},
		{"contract with a space", "see TODO (you) about phase 4", ""},
		{"named contract", "the TODO(sami) note", ""},
		// A bare TODO in a status column is still a status.
		{"bare todo is a status", "| Phase 6 | TODO |", MarkerNotStarted},
		{"todo colon is a status", "Phase 6: TODO", MarkerNotStarted},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyMarker(tt.line); got != tt.want {
				t.Errorf("classifyMarker(%q) = %q, want %q", tt.line, got, tt.want)
			}
		})
	}
}

func TestExtractPhasesFindsEveryPhaseOnALine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want []string
	}{
		// Recording only the first left P4 looking undocumented.
		{"slash pair", "| `internal/agent/loop.go` | Run | P1/P4 |", []string{"1", "4"}},
		{"prose pair", "Phases: P1 and P2 are planned.", []string{"1", "2"}},
		{"deduplicated", "P3 then P3 again", []string{"3"}},
		{"word form two digits", "Phase 10 shipped", []string{"10"}},
		{"milestone letter kept", "M5a in progress", []string{"5a"}},
		// Latency percentiles are not phases.
		{"percentiles ignored", "p50 / p95 latency", nil},
		{"none", "General notes", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractPhases(tt.line)
			if len(got) != len(tt.want) {
				t.Fatalf("extractPhases(%q) = %v, want %v", tt.line, got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("extractPhases(%q)[%d] = %q, want %q", tt.line, i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestCleanTarget(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"./scripts/foo.sh", "scripts/foo.sh"},
		// TrimPrefix stripped one "./" and turned this into an absolute path.
		{".//x.md", "x.md"},
		{"./", ""},
		{"eval/results/", "eval/results"},
		{"a/b.go", "a/b.go"},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := cleanTarget(tt.in); got != tt.want {
				t.Errorf("cleanTarget(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSanitizeDropsBidiOverrides(t *testing.T) {
	// These reorder rendered text in a terminal and on GitHub — the same
	// deception the C0 range is dropped for.
	in := "safe‮gnisrever‬ end"
	got := sanitize(in)
	for _, r := range got {
		if (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			t.Fatalf("bidi override survived: %q", got)
		}
	}
}
