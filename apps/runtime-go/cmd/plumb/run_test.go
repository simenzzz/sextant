package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fixtureRepo writes a throwaway git repository and returns its root.
func fixtureRepo(t *testing.T, files map[string]string, subjects []string) string {
	t.Helper()
	root := t.TempDir()

	for rel, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(abs), err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatalf("writing %s: %v", abs, err)
		}
	}

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
	for _, s := range subjects {
		git("commit", "-q", "--allow-empty", "-m", s)
	}
	return root
}

func cleanRepo(t *testing.T) string {
	t.Helper()
	return fixtureRepo(t, map[string]string{
		"README.md":   "# Clean\n\nEntry point is `src/main.go`.\n",
		"src/main.go": "package main\n\nfunc main() {}\n",
	}, []string{"chore: init"})
}

func driftedRepo(t *testing.T) string {
	t.Helper()
	return fixtureRepo(t, map[string]string{
		"README.md": "# Drifted\n\nSee `does/not/exist.ts`.\n",
	}, []string{"chore: init"})
}

func TestVerifyExitCodes(t *testing.T) {
	tests := []struct {
		name string
		root func(*testing.T) string
		want int
	}{
		// The distinction a CI job depends on: "found problems" must not be
		// confusable with "could not look".
		{"clean repository", cleanRepo, exitOK},
		{"drifted repository", driftedRepo, exitFindings},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			code, err := run([]string{"verify", tt.root(t)}, &out)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if code != tt.want {
				t.Errorf("exit = %d, want %d\noutput:\n%s", code, tt.want, out.String())
			}
		})
	}
}

func TestVerifyJSONShape(t *testing.T) {
	var out bytes.Buffer
	if _, err := run([]string{"verify", driftedRepo(t), "--json"}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}

	var got struct {
		Root     string `json:"root"`
		Findings []struct {
			Kind     string `json:"kind"`
			Path     string `json:"path"`
			Claim    string `json:"claim"`
			Evidence string `json:"evidence"`
		} `json:"findings"`
		Stats map[string]int `json:"stats"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out.String())
	}
	if len(got.Findings) != 1 {
		t.Fatalf("got %d findings, want 1", len(got.Findings))
	}
	// Every finding carries both halves of the contradiction, so a reader never
	// has to take the tool's word for it.
	f := got.Findings[0]
	if f.Kind == "" || f.Path == "" || f.Claim == "" || f.Evidence == "" {
		t.Errorf("finding is missing a field: %+v", f)
	}
	if got.Stats["files"] == 0 {
		t.Errorf("stats reported no files: %v", got.Stats)
	}
}

func TestVerifyJSONEmptyIsList(t *testing.T) {
	var out bytes.Buffer
	if _, err := run([]string{"verify", "--json", cleanRepo(t)}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}
	// A nil slice would marshal as `null`, forcing every consumer to
	// special-case it before counting or iterating.
	if !strings.Contains(out.String(), `"findings": []`) {
		t.Errorf("empty findings must marshal as [], got:\n%s", out.String())
	}
}

func TestIndexWritesADatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "repo.db")

	var out bytes.Buffer
	code, err := run([]string{"index", cleanRepo(t), "--db", dbPath}, &out)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != exitOK {
		t.Fatalf("exit = %d, want %d: %s", code, exitOK, out.String())
	}

	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("index did not write %s: %v", dbPath, err)
	}
	if info.Size() == 0 {
		t.Error("index wrote an empty database")
	}
}

func TestVerifyLeavesNoDatabaseBehind(t *testing.T) {
	root := cleanRepo(t)

	var out bytes.Buffer
	if _, err := run([]string{"verify", root}, &out); err != nil {
		t.Fatalf("run: %v", err)
	}

	// A checker that litters the tree it inspects becomes a source of the drift
	// it reports on.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading %s: %v", root, err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".db") || e.Name() == ".plumb" {
			t.Errorf("verify left %s behind in the repository", e.Name())
		}
	}
}

func TestHelpIsNotAnError(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			var out bytes.Buffer
			code, err := run([]string{arg}, &out)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if code != exitOK {
				t.Errorf("exit = %d, want %d", code, exitOK)
			}
			if !strings.Contains(out.String(), "usage:") {
				t.Errorf("help printed no usage text:\n%s", out.String())
			}
		})
	}
}

func TestVerifyIsDeterministic(t *testing.T) {
	root := driftedRepo(t)

	var first, second bytes.Buffer
	if _, err := run([]string{"verify", "--json", root}, &first); err != nil {
		t.Fatalf("first run: %v", err)
	}
	if _, err := run([]string{"verify", "--json", root}, &second); err != nil {
		t.Fatalf("second run: %v", err)
	}
	// Byte-identical output across runs is what lets this gate CI and lets one
	// run be diffed against the next.
	if first.String() != second.String() {
		t.Error("two runs over an unchanged tree produced different output")
	}
}
