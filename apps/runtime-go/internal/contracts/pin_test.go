package contracts

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Two files pin go-jsonschema, and they must agree:
//
//   - packages/contracts/codegen/versions.env pins the GENERATOR, the binary
//     that writes internal/contracts/gen/*.go.
//   - apps/runtime-go/go.mod pins the LIBRARY, whose pkg/types.SerializableDate
//     the generated code imports for every `format: date` field.
//
// Nothing else in the repo notices when they diverge, which is what makes this
// worth a test rather than a comment. The contracts drift gate regenerates
// using the pinned binary, so it sees no diff; the build succeeds because
// pkg/types is API-stable across these versions. A Dependabot PR can only move
// go.mod — it cannot edit a shell env file — so the desync arrives green and
// stays invisible until the two versions disagree about how a date serializes.
const (
	versionsEnvPath = "../../../../packages/contracts/codegen/versions.env"
	goModPath       = "../../go.mod"

	moduleKey  = "GO_JSONSCHEMA_MODULE"
	versionKey = "GO_JSONSCHEMA_VERSION"
)

func TestGeneratorPinMatchesLibraryPin(t *testing.T) {
	env := readVersionsEnv(t)

	module := env[moduleKey]
	if module == "" {
		t.Fatalf("%s not found in versions.env — was it renamed?", moduleKey)
	}
	generator := env[versionKey]
	if generator == "" {
		t.Fatalf("%s not found in versions.env — was it renamed?", versionKey)
	}

	library := libraryPin(t, module)

	if generator != library {
		t.Fatalf(`go-jsonschema pins disagree:
  generator %s  (%s in packages/contracts/codegen/versions.env)
  library   %s  (%s in apps/runtime-go/go.mod)

These must move together. The generator writes internal/contracts/gen/*.go and
the library supplies the types that generated code imports, so a mismatch means
the runtime is compiled against a different version than produced its own types.

To bump: raise both, run "make generate-schemas", and commit the regenerated
output in the same change.`, generator, versionKey, library, module)
	}
}

// readVersionsEnv parses versions.env from disk.
//
// The module path is read from the file rather than hardcoded here, so a
// major-version bump that moves the import path (a `/v2` suffix) is caught as
// a mismatch instead of turning this test into a stale third copy of the fact.
func readVersionsEnv(t *testing.T) map[string]string {
	t.Helper()

	f, err := os.Open(versionsEnvPath)
	if err != nil {
		t.Fatalf("opening versions.env: %v", err)
	}
	defer f.Close()

	env, err := parseVersionsEnv(f)
	if err != nil {
		t.Fatalf("reading versions.env: %v", err)
	}
	return env
}

// parseVersionsEnv reads shell-style KEY=value assignments.
//
// versions.env is not a data file — it is `source`d by bash in the Makefile,
// generate.sh, and ci.yml. This parser therefore has to agree with bash, or it
// reports a version that nothing actually installs. Verified against bash:
//
//   - The LAST assignment of a key wins. Returning the first would let an
//     appended override (or a merge-conflict resolution) leave the repo
//     genuinely desynced while this test passes green.
//   - A ` #` comment is stripped, but `v1#x` is not — bash only starts a
//     comment at the beginning of a word.
//   - A quoted value keeps everything inside the quotes, `#` included.
//
// A read error is returned rather than folded into an empty result, so an I/O
// failure is never misreported as "the key is missing".
func parseVersionsEnv(r io.Reader) (map[string]string, error) {
	env := make(map[string]string)

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		env[strings.TrimSpace(key)] = unquoteShellValue(value)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return env, nil
}

// unquoteShellValue applies bash's word rules to the right-hand side.
func unquoteShellValue(value string) string {
	value = strings.TrimSpace(value)

	// A quoted value is taken whole; bash does not look for comments inside it.
	for _, q := range []string{`"`, `'`} {
		if len(value) >= 2 && strings.HasPrefix(value, q) && strings.HasSuffix(value, q) {
			return value[1 : len(value)-1]
		}
	}

	// Unquoted: a comment starts only at the beginning of a word, so strip from
	// a `#` that follows whitespace — never from one inside a token.
	if i := strings.Index(value, " #"); i >= 0 {
		value = value[:i]
	}
	if i := strings.Index(value, "\t#"); i >= 0 {
		value = value[:i]
	}
	return strings.TrimSpace(value)
}

// goModRequire is the slice of `go mod edit -json` this test needs.
type goModRequire struct {
	Require []struct {
		Path    string
		Version string
	}
}

// libraryPin returns the version go.mod requires for module.
//
// Delegating to `go mod edit -json` rather than matching lines by hand: the
// toolchain's own parser distinguishes `require` from `exclude` and `replace`
// blocks, handles both the single-line and parenthesised forms, and ignores
// `// indirect` markers. A regexp over the raw file got the `exclude` case
// wrong by returning the excluded version — silently, and in the direction
// that makes this test pass.
func libraryPin(t *testing.T, module string) string {
	t.Helper()

	out, err := exec.Command("go", "mod", "edit", "-json", goModPath).Output()
	if err != nil {
		t.Fatalf("go mod edit -json %s: %v", goModPath, err)
	}

	var parsed goModRequire
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("parsing go mod edit output: %v", err)
	}

	for _, r := range parsed.Require {
		if r.Path == module {
			return r.Version
		}
	}

	t.Fatalf(`go.mod has no require for %s.

Most likely the module path gained a major-version suffix (a "/v2"), in which
case update %s in versions.env to match. If the generated code genuinely no
longer imports pkg/types, delete this test along with the dependency.`,
		module, moduleKey)
	return ""
}

// A guard test that passes vacuously is worse than no guard, so the parsing is
// exercised against the shapes these files can legitimately take. Every
// expectation below was checked against bash and against the Go toolchain, not
// assumed.
func TestParseVersionsEnv(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "GO_JSONSCHEMA_VERSION=v0.23.1\n", "v0.23.1"},
		{"quoted", `GO_JSONSCHEMA_VERSION="v0.23.1"` + "\n", "v0.23.1"},
		{"single quoted", "GO_JSONSCHEMA_VERSION='v0.23.1'\n", "v0.23.1"},
		{"trailing whitespace", "GO_JSONSCHEMA_VERSION=v0.23.1   \n", "v0.23.1"},
		{"CRLF line endings", "GO_JSONSCHEMA_VERSION=v0.23.1\r\n", "v0.23.1"},
		{
			// bash sources this file and takes the last assignment. Returning
			// the first would let an appended override sit in the repo while
			// this test reports agreement.
			name: "the last assignment wins, as bash does",
			in:   "GO_JSONSCHEMA_VERSION=v0.23.1\nGO_JSONSCHEMA_VERSION=v0.24.0\n",
			want: "v0.24.0",
		},
		{
			name: "a trailing comment is stripped, as bash does",
			in:   "GO_JSONSCHEMA_VERSION=v0.23.1 # bumped 2026-08\n",
			want: "v0.23.1",
		},
		{
			// bash starts a comment only at the beginning of a word.
			name: "a hash inside the token is kept, as bash does",
			in:   "GO_JSONSCHEMA_VERSION=v0.23.1#notacomment\n",
			want: "v0.23.1#notacomment",
		},
		{
			name: "a commented-out line must not win",
			in:   "GO_JSONSCHEMA_VERSION=v0.23.1\n#GO_JSONSCHEMA_VERSION=v9.9.9\n",
			want: "v0.23.1",
		},
		{
			name: "the MODULE key must not be mistaken for VERSION",
			in:   "GO_JSONSCHEMA_MODULE=github.com/atombender/go-jsonschema\nGO_JSONSCHEMA_VERSION=v0.23.1\n",
			want: "v0.23.1",
		},
		{"absent", "DATAMODEL_CODEGEN_VERSION=0.68.1\n", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env, err := parseVersionsEnv(strings.NewReader(tt.in))
			if err != nil {
				t.Fatalf("parseVersionsEnv: %v", err)
			}
			if got := env[versionKey]; got != tt.want {
				t.Errorf("parsed %q, want %q", got, tt.want)
			}
		})
	}
}

// A read failure must be reported as one, never collapsed into "key absent" —
// the house rule is that errors are not silently swallowed, and the two have
// completely different remedies.
func TestParseVersionsEnvReportsReadErrors(t *testing.T) {
	want := fmt.Errorf("disk fell over")
	if _, err := parseVersionsEnv(failingReader{err: want}); err == nil {
		t.Fatal("parseVersionsEnv() = nil error on a failing reader")
	}
}

type failingReader struct{ err error }

func (f failingReader) Read([]byte) (int, error) { return 0, f.err }

// libraryPin delegates to the Go toolchain, so this asserts the delegation
// handles the go.mod forms that a regexp got wrong.
func TestLibraryPinAcrossGoModForms(t *testing.T) {
	const module = "github.com/atombender/go-jsonschema"

	tests := []struct {
		name string
		mod  string
		want string
	}{
		{
			name: "single-line require",
			mod:  "module e\n\ngo 1.25.3\n\nrequire " + module + " v0.23.1\n",
			want: "v0.23.1",
		},
		{
			name: "require block",
			mod:  "module e\n\ngo 1.25.3\n\nrequire (\n\t" + module + " v0.23.1\n)\n",
			want: "v0.23.1",
		},
		{
			name: "indirect marker",
			mod:  "module e\n\ngo 1.25.3\n\nrequire (\n\t" + module + " v0.23.1 // indirect\n)\n",
			want: "v0.23.1",
		},
		{
			// The case the hand-rolled regexp got wrong: it returned the
			// EXCLUDED version, which is both silent and wrong.
			name: "an exclude block must not shadow the require",
			mod: "module e\n\ngo 1.25.3\n\nexclude (\n\t" + module + " v0.22.0\n)\n\n" +
				"require (\n\t" + module + " v0.23.1\n)\n",
			want: "v0.23.1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "go.mod")
			if err := os.WriteFile(path, []byte(tt.mod), 0o600); err != nil {
				t.Fatalf("writing temp go.mod: %v", err)
			}

			out, err := exec.Command("go", "mod", "edit", "-json", path).Output()
			if err != nil {
				t.Fatalf("go mod edit -json: %v", err)
			}
			var parsed goModRequire
			if err := json.Unmarshal(out, &parsed); err != nil {
				t.Fatalf("parsing output: %v", err)
			}

			got := ""
			for _, r := range parsed.Require {
				if r.Path == module {
					got = r.Version
				}
			}
			if got != tt.want {
				t.Errorf("resolved %q, want %q", got, tt.want)
			}
		})
	}
}
