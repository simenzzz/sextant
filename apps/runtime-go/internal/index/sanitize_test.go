package index

import "testing"

func TestSanitize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		// A finding travels to a terminal and into a pull-request comment, so a
		// hostile repository must not be able to emit escape sequences through
		// a filename or a documentation line.
		{"ansi escape dropped", "before\x1b[31mred\x1b[0m", "before[31mred[0m"},
		{"carriage return dropped", "line\roverwrite", "lineoverwrite"},
		{"bell dropped", "ding\a", "ding"},
		{"del dropped", "a\x7fb", "ab"},
		{"c1 range dropped", "ab", "ab"},
		{"tab becomes a space", "a\tb", "a b"},

		// Recipes are several lines joined by newlines; losing them would run
		// one command into the next before the script scanner sees it.
		{"newline preserved", "python3 a.py\necho done", "python3 a.py\necho done"},
		{"unicode preserved", "café — ✅", "café — ✅"},
		{"empty", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitize(tt.in); got != tt.want {
				t.Errorf("sanitize(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestNewFindingSanitisesEveryField(t *testing.T) {
	// The escape rides in on the path, so this pins the constructor rather than
	// any single check's formatting.
	f := newFinding("kind", "docs/\x1b[31mevil.md", 3, "claim\a", "evidence\r")

	if f.Path != "docs/[31mevil.md" {
		t.Errorf("Path = %q, want the escape stripped", f.Path)
	}
	if f.Claim != "claim" {
		t.Errorf("Claim = %q, want %q", f.Claim, "claim")
	}
	if f.Evidence != "evidence" {
		t.Errorf("Evidence = %q, want %q", f.Evidence, "evidence")
	}
	if f.Line != 3 {
		t.Errorf("Line = %d, want 3", f.Line)
	}
}

func TestMakeTargetRecipeSurvivesSanitisation(t *testing.T) {
	// Regression: sanitize once dropped newlines, which joined a recipe's
	// commands into one line and changed what scriptRefs matched.
	content := "eval:\n\tpython3 eval/run.py\n\techo done\n"

	targets := scanMakeTargets(content)
	if len(targets) != 1 {
		t.Fatalf("got %d targets, want 1", len(targets))
	}
	refs := scriptRefs(targets[0].recipe)
	if len(refs) != 1 || refs[0] != "eval/run.py" {
		t.Errorf("scriptRefs = %v, want [eval/run.py]", refs)
	}
}
