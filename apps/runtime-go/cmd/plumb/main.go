// Command plumb checks whether a repository's documentation still matches the
// repository.
//
// At P2.5 every check is deterministic: the index records facts about the
// working tree and the git history, and a finding is a contradiction between
// two of those facts. No model is involved, nothing is inferred from prose, and
// two runs over an unchanged tree produce identical output.
//
// The claim-extraction and verdict layers that read prose arrive at P6.5 and
// are scored against the corpus this command produces.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/simenzzz/sextant/apps/runtime-go/internal/index"
)

const usage = `plumb — check a repository against its own documentation

usage:
  plumb verify <repo> [--json] [--commits N] [--workspace-siblings] [--db <path>]
  plumb index  <repo> --db <path> [--commits N]

commands:
  verify   index into a temporary database, report contradictions
  index    build a persistent repo.db for querying with plain SQL

flags:
  --db <path>    where to write the index. Required by "index"; optional for
                 "verify", which otherwise uses a temporary file and removes it
  --json         emit findings as JSON rather than text
  --commits N    read only the newest N commits (0 = all, default 0)
  --workspace-siblings
                 ignore references into neighbouring git repositories. Depends
                 on directories outside the tree, so a report using it is not
                 reproducible on another machine. Off by default.

exit status:
  0  no contradictions found
  1  contradictions found
  2  the run itself failed
`

// Exit codes. `verify` distinguishes "found problems" from "could not look",
// so a CI job can fail the build on the first without being fooled by the
// second.
const (
	exitOK       = 0
	exitFindings = 1
	exitError    = 2
)

func main() {
	// Ctrl-C must stop a walk of somebody else's enormous repository, and the
	// git subprocesses are cancellable through the same context.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	code, err := runContext(ctx, os.Args[1:], os.Stdout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "plumb: %v\n", err)
	}
	os.Exit(code)
}

// run is the test entry point; runContext is what main uses.
func run(args []string, out io.Writer) (int, error) {
	return runContext(context.Background(), args, out)
}

func runContext(ctx context.Context, args []string, out io.Writer) (int, error) {
	if len(args) == 0 {
		fmt.Fprint(out, usage)
		return exitError, nil
	}

	cmd := args[0]
	fs := flag.NewFlagSet("plumb "+cmd, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dbPath := fs.String("db", "", "where to write the index")
	asJSON := fs.Bool("json", false, "emit findings as JSON")
	commits := fs.Int("commits", 0, "read only the newest N commits (0 = all)")
	siblings := fs.Bool("workspace-siblings", false,
		"ignore references into neighbouring git repositories (not reproducible; see PLAN.md 5.7)")

	switch cmd {
	case "verify", "index":
	case "-h", "--help", "help":
		fmt.Fprint(out, usage)
		return exitOK, nil
	default:
		return exitError, fmt.Errorf("unknown command %q (try `plumb help`)", cmd)
	}

	if err := fs.Parse(permute(args[1:])); err != nil {
		return exitError, err
	}
	if fs.NArg() != 1 {
		return exitError, errors.New("expected exactly one repository path")
	}

	root, err := filepath.Abs(fs.Arg(0))
	if err != nil {
		return exitError, fmt.Errorf("resolving repository path: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return exitError, fmt.Errorf("reading %s: %w", root, err)
	}
	if !info.IsDir() {
		return exitError, fmt.Errorf("%s is not a directory", root)
	}

	if cmd == "index" {
		// Silently ignoring a flag is a validation gap at a boundary: the user
		// asked for JSON and would have got a human report with exit 0.
		if *asJSON {
			return exitError, errors.New("index does not support --json (did you mean `plumb verify --json`?)")
		}
		return runIndex(ctx, root, *dbPath, *commits, *siblings, out)
	}
	return runVerify(ctx, root, *dbPath, *commits, *asJSON, *siblings, out)
}

// permute moves flags ahead of positional arguments.
//
// Go's flag package stops parsing at the first non-flag argument, so
// `plumb verify /repo --json` would silently treat `--json` as a second
// positional — the exact form this command's own usage text documents. Sorting
// the arguments restores the order the documentation promises.
//
// A flag taking a separate value (`--db path`) keeps that value with it.
func permute(args []string) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		// `--db=path` carries its own value; `--db path` consumes the next
		// argument. Booleans never do, so only the known value-taking flags
		// reach forward.
		if !strings.Contains(a, "=") && takesValue(a) && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}

func takesValue(flag string) bool {
	switch strings.TrimLeft(flag, "-") {
	case "db", "commits":
		return true
	}
	return false
}

func runIndex(ctx context.Context, root, dbPath string, commits int, siblings bool, out io.Writer) (int, error) {
	if dbPath == "" {
		return exitError, errors.New("index requires --db")
	}
	store, stats, err := build(ctx, root, dbPath, commits, siblings)
	if err != nil {
		return exitError, err
	}
	defer store.Close()

	fmt.Fprintf(out, "indexed %s\n", filepath.Base(root))
	printStats(out, stats)
	fmt.Fprintf(out, "\nwrote %s\n", dbPath)
	return exitOK, nil
}

func runVerify(ctx context.Context, root, dbPath string, commits int, asJSON, siblings bool, out io.Writer) (int, error) {
	// A verify with no --db is transient by design: the index is a means to the
	// report, and leaving a database behind in the repository under inspection
	// would make the tool a source of the drift it reports on.
	if dbPath == "" {
		tmp, err := os.MkdirTemp("", "plumb-")
		if err != nil {
			return exitError, fmt.Errorf("creating temporary directory: %w", err)
		}
		defer os.RemoveAll(tmp)
		dbPath = filepath.Join(tmp, "repo.db")
	}

	store, stats, err := build(ctx, root, dbPath, commits, siblings)
	if err != nil {
		return exitError, err
	}
	defer store.Close()

	findings, err := store.Findings(ctx)
	if err != nil {
		return exitError, err
	}

	if asJSON {
		// A nil slice marshals as `null`, which every consumer then has to
		// special-case before it can count or iterate. An empty result is an
		// empty list.
		if findings == nil {
			findings = []index.Finding{}
		}
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{
			"root":     filepath.Base(store.Root()),
			"stats":    stats,
			"findings": findings,
		}); err != nil {
			return exitError, fmt.Errorf("writing JSON: %w", err)
		}
	} else {
		// store.Root() is the resolved path; root is what the user typed. For a
		// symlinked root those differ, and the header must name what was
		// actually scanned.
		printReport(out, store.Root(), stats, findings)
	}

	if store.Degraded() {
		fmt.Fprintln(os.Stderr,
			"plumb: warning: some ignore rules could not be resolved; suppression in this report is incomplete")
	}

	if len(findings) > 0 {
		return exitFindings, nil
	}
	return exitOK, nil
}

func build(ctx context.Context, root, dbPath string, commits int, siblings bool) (*index.Store, index.Stats, error) {
	store, err := index.Open(dbPath, root)
	if err != nil {
		return nil, index.Stats{}, err
	}
	stats, err := store.Build(ctx, index.Options{
		CommitLimit:       commits,
		WorkspaceSiblings: siblings,
	})
	if err != nil {
		store.Close()
		return nil, index.Stats{}, err
	}
	return store, stats, nil
}

func printStats(out io.Writer, s index.Stats) {
	fmt.Fprintf(out,
		"  %d files · %d dirs · %d stub sites · %d commits · %d doc claims · %d phases · %d doc paths · %d make targets\n",
		s.Files, s.Dirs, s.Stubs, s.Commits, s.DocClaims, s.DocPhases, s.DocPaths, s.MakeTargets)

	// A partially readable tree must not print the same line as a complete one.
	// Recording the fact in the index was only half the fix: the default report
	// is what a CI job shows, and it said nothing.
	if s.Unreadable > 0 {
		fmt.Fprintf(out,
			"  warning: %d path(s) could not be read; findings below are incomplete\n",
			s.Unreadable)
	}
}

func printReport(out io.Writer, root string, stats index.Stats, findings []index.Finding) {
	// The basename only: an absolute path in a pull-request comment discloses
	// the runner's layout, and locally it leaks the username.
	fmt.Fprintf(out, "plumb: %s\n", filepath.Base(root))
	printStats(out, stats)
	fmt.Fprintln(out)

	if len(findings) == 0 {
		fmt.Fprintln(out, "no contradictions found")
		return
	}

	for _, f := range findings {
		loc := f.Path
		if f.Line > 0 {
			loc = fmt.Sprintf("%s:%d", f.Path, f.Line)
		}
		fmt.Fprintf(out, "%s\n  [%s] %s\n  evidence: %s\n\n", loc, f.Kind, f.Claim, f.Evidence)
	}
	fmt.Fprintf(out, "%d contradiction(s)\n", len(findings))
}
