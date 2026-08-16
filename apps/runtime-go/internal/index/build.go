package index

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Options tunes an index run.
type Options struct {
	// CommitLimit bounds how much history is read. Zero means all of it.
	CommitLimit int

	// WorkspaceSiblings suppresses references whose first segment names a
	// neighbouring git repository. Off by default because it makes the report
	// depend on directories outside the tree — see siblingRepos.
	WorkspaceSiblings bool
}

// Stats summarizes what one index run recorded.
type Stats struct {
	Files       int `json:"files"`
	Dirs        int `json:"dirs"`
	Stubs       int `json:"stubs"`
	Commits     int `json:"commits"`
	DocClaims   int `json:"doc_claims"`
	DocPhases   int `json:"doc_phases"`
	DocPaths    int `json:"doc_paths"`
	MakeTargets int `json:"make_targets"`

	// Unreadable counts paths the walk could not inspect. Surfaced so a report
	// over a partially readable tree says so rather than looking complete.
	Unreadable int `json:"unreadable"`
}

// Build indexes the repository at s.Root() into the store, replacing whatever a
// previous run left behind.
//
// The whole run is one transaction. A half-written index is worse than no index
// — the report layer would read it as a repository that genuinely lacks the
// missing rows and emit confident findings about files it simply never reached.
func (s *Store) Build(ctx context.Context, opts Options) (Stats, error) {
	var stats Stats

	entries, err := walk(s.root)
	if err != nil {
		return stats, fmt.Errorf("index: walking %s: %w", s.root, err)
	}
	// An empty walk means the root was unreadable or is not the directory it
	// appeared to be. Reporting "no contradictions" over it would be a silent
	// pass on a CI gate.
	if len(entries) == 0 {
		return stats, fmt.Errorf("index: %s contains nothing readable", s.root)
	}

	commits, err := readCommits(ctx, s.root, opts.CommitLimit)
	if err != nil {
		return stats, err
	}

	s.opts = opts

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return stats, fmt.Errorf("index: beginning transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once Commit succeeds

	// Clearing happens inside the transaction, not before it. Outside, a run
	// that failed afterwards would leave the index emptied, and the report
	// layer would read that as a repository genuinely missing every row.
	if err := reset(ctx, tx); err != nil {
		return stats, err
	}

	w, err := newWriter(ctx, tx)
	if err != nil {
		return stats, err
	}
	defer w.close()

	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		if e.isDir {
			n, readErr := countEntries(e.abs)
			// An unreadable directory is recorded as unreadable, never as
			// empty, so no check can assert anything about its contents.
			if err := w.dir(e.rel, n, e.skipped, readErr != nil); err != nil {
				return stats, err
			}
			if readErr != nil {
				stats.Unreadable++
			}
			stats.Dirs++
			continue
		}
		n, err := s.indexFile(w, e, &stats)
		if err != nil {
			return stats, err
		}
		stats.Files += n
	}

	for _, c := range commits {
		if err := w.commit(c); err != nil {
			return stats, err
		}
		stats.Commits++
	}

	if err := tx.Commit(); err != nil {
		return stats, fmt.Errorf("index: committing index: %w", err)
	}
	return stats, nil
}

// indexFile records one file and everything derivable from its contents.
// Returns the number of file rows written (0 or 1) so the caller can count.
func (s *Store) indexFile(w *writer, e entry, stats *Stats) (int, error) {
	// A file is examined only when it is small enough and of a kind the
	// scanners understand. Whether that happened is recorded, because "not
	// examined" and "examined and empty" are different facts and a checker that
	// conflates them reports confidently about a file it never opened.
	scanned := e.size > 0 && e.size <= maxFileBytes && isTextExt(e.rel)

	content := ""
	if scanned {
		// Read through a bounded reader rather than trusting e.size: the size
		// was captured during the walk, and a file that grows before it is
		// opened would otherwise be read in full.
		b, err := readBounded(e.abs, maxFileBytes)
		if err != nil {
			// An unreadable file still exists; record it as unexamined rather
			// than as empty.
			scanned = false
			stats.Unreadable++
		} else {
			content = string(b)
		}
	}

	if err := w.file(e.rel, strings.ToLower(filepath.Ext(e.rel)), countLines(content), e.size, scanned); err != nil {
		return 0, err
	}

	if content == "" {
		return 1, nil
	}

	for _, h := range scanStubs(content) {
		if err := w.stub(e.rel, h); err != nil {
			return 0, err
		}
		stats.Stubs++
	}

	if isDocPath(e.rel) && !isScratchDoc(e.rel) {
		for _, c := range scanDocClaims(content) {
			if err := w.docClaim(e.rel, c); err != nil {
				return 0, err
			}
			stats.DocClaims++
		}
		for line, phases := range scanDocPhases(content) {
			for _, ph := range phases {
				if err := w.docPhase(e.rel, line, ph); err != nil {
					return 0, err
				}
				stats.DocPhases++
			}
		}
		for line, targets := range scanDocPaths(content) {
			for _, t := range targets {
				if err := w.docPath(e.rel, line, t); err != nil {
					return 0, err
				}
				stats.DocPaths++
			}
		}
	}

	if base := filepath.Base(e.rel); base == "Makefile" || base == "makefile" {
		for _, t := range scanMakeTargets(content) {
			if err := w.makeTarget(e.rel, t); err != nil {
				return 0, err
			}
			stats.MakeTargets++
		}
	}
	return 1, nil
}

// writer holds the prepared statements for one index run. Preparing once and
// reusing turns a repository-sized run from tens of thousands of statement
// compilations into one apiece.
type writer struct {
	files   *sql.Stmt
	dirs    *sql.Stmt
	stubs   *sql.Stmt
	commits *sql.Stmt
	claims  *sql.Stmt
	phases  *sql.Stmt
	paths   *sql.Stmt
	targets *sql.Stmt
}

func newWriter(ctx context.Context, tx *sql.Tx) (*writer, error) {
	prep := func(q string) (*sql.Stmt, error) { return tx.PrepareContext(ctx, q) }
	w := &writer{}
	var err error

	// INSERT OR REPLACE rather than INSERT: a case-insensitive filesystem can
	// present the same path twice, and a duplicate must not abort the run.
	if w.files, err = prep(`INSERT OR REPLACE INTO files (path, ext, lines, bytes, scanned) VALUES (?,?,?,?,?)`); err != nil {
		return nil, wrapPrepare(err)
	}
	if w.dirs, err = prep(`INSERT OR REPLACE INTO dirs (path, entries, skipped, unreadable) VALUES (?,?,?,?)`); err != nil {
		return nil, wrapPrepare(err)
	}
	if w.stubs, err = prep(`INSERT OR REPLACE INTO stubs (path, line, marker, text) VALUES (?,?,?,?)`); err != nil {
		return nil, wrapPrepare(err)
	}
	if w.commits, err = prep(`INSERT OR REPLACE INTO commits (sha, committed_at, subject) VALUES (?,?,?)`); err != nil {
		return nil, wrapPrepare(err)
	}
	if w.claims, err = prep(`INSERT OR REPLACE INTO doc_claims (path, line, text, marker, phase) VALUES (?,?,?,?,?)`); err != nil {
		return nil, wrapPrepare(err)
	}
	if w.phases, err = prep(`INSERT OR REPLACE INTO doc_phases (path, line, phase) VALUES (?,?,?)`); err != nil {
		return nil, wrapPrepare(err)
	}
	if w.paths, err = prep(`INSERT OR REPLACE INTO doc_paths (path, line, target, hint) VALUES (?,?,?,?)`); err != nil {
		return nil, wrapPrepare(err)
	}
	if w.targets, err = prep(`INSERT OR REPLACE INTO make_targets (path, name, line, recipe) VALUES (?,?,?,?)`); err != nil {
		return nil, wrapPrepare(err)
	}
	return w, nil
}

func wrapPrepare(err error) error { return fmt.Errorf("index: preparing statement: %w", err) }

func (w *writer) close() {
	for _, s := range []*sql.Stmt{w.files, w.dirs, w.stubs, w.commits, w.claims, w.phases, w.paths, w.targets} {
		if s != nil {
			s.Close()
		}
	}
}

func (w *writer) file(path, ext string, lines int, bytes int64, scanned bool) error {
	_, err := w.files.Exec(path, ext, lines, bytes, scanned)
	return wrapWrite("file", path, err)
}

// readBounded reads at most limit bytes from path.
func readBounded(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit))
}

// countLines counts lines the way a text editor does: a trailing newline
// terminates the last line rather than starting an empty one. `strings.Count+1`
// recorded every POSIX-conformant file as one line too long.
func countLines(content string) int {
	if content == "" {
		return 0
	}
	n := strings.Count(content, "\n")
	if !strings.HasSuffix(content, "\n") {
		n++
	}
	return n
}

func (w *writer) dir(path string, entries int, skipped, unreadable bool) error {
	_, err := w.dirs.Exec(path, entries, skipped, unreadable)
	return wrapWrite("dir", path, err)
}

func (w *writer) stub(path string, h stubHit) error {
	_, err := w.stubs.Exec(path, h.line, h.marker, h.text)
	return wrapWrite("stub", path, err)
}

func (w *writer) commit(c commitRec) error {
	_, err := w.commits.Exec(c.sha, c.committedAt, c.subject)
	return wrapWrite("commit", c.sha, err)
}

func (w *writer) docClaim(path string, c docClaim) error {
	_, err := w.claims.Exec(path, c.line, c.text, c.marker, c.phase)
	return wrapWrite("doc claim", path, err)
}

func (w *writer) docPhase(path string, line int, phase string) error {
	_, err := w.phases.Exec(path, line, phase)
	return wrapWrite("doc phase", path, err)
}

func (w *writer) docPath(path string, line int, ref docPathRefRaw) error {
	_, err := w.paths.Exec(path, line, ref.target, ref.hint)
	return wrapWrite("doc path", path, err)
}

func (w *writer) makeTarget(path string, t makeTarget) error {
	_, err := w.targets.Exec(path, t.name, t.line, t.recipe)
	return wrapWrite("make target", path+":"+t.name, err)
}

func wrapWrite(kind, subject string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("index: recording %s %q: %w", kind, subject, err)
}
