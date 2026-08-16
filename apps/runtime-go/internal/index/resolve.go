package index

import (
	"context"
	"fmt"
	"strings"
)

// resolver answers "does this path reference point at anything real?" against a
// snapshot of the index, held in memory.
//
// Loaded once per report rather than queried per reference: a documentation-
// heavy repository asks this thousands of times, and each SQL form of the
// question is a table scan.
type resolver struct {
	files     map[string]bool
	dirs      map[string]bool
	emptyDirs map[string]bool
	// Directories the walk did not descend into. Their contents are unknown.
	skipped []string

	// bySuffix maps a final path segment to every indexed path ending in it,
	// which is what makes suffix resolution a lookup rather than a scan.
	bySuffix map[string][]string
}

func newResolver(ctx context.Context, s *Store) (*resolver, error) {
	r := &resolver{
		files:     map[string]bool{},
		dirs:      map[string]bool{},
		emptyDirs: map[string]bool{},
		bySuffix:  map[string][]string{},
	}

	add := func(p string) {
		seg := p
		if i := strings.LastIndex(p, "/"); i >= 0 {
			seg = p[i+1:]
		}
		r.bySuffix[seg] = append(r.bySuffix[seg], p)
	}

	rows, err := s.db.QueryContext(ctx, `SELECT path FROM files`)
	if err != nil {
		return nil, fmt.Errorf("index: loading files: %w", err)
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return nil, fmt.Errorf("index: scanning file path: %w", err)
		}
		r.files[p] = true
		add(p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("index: loading files: %w", err)
	}

	drows, err := s.db.QueryContext(ctx, `SELECT path, entries, skipped, unreadable FROM dirs`)
	if err != nil {
		return nil, fmt.Errorf("index: loading dirs: %w", err)
	}
	defer drows.Close()
	for drows.Next() {
		var p string
		var entries int
		var skipped, unreadable bool
		if err := drows.Scan(&p, &entries, &skipped, &unreadable); err != nil {
			return nil, fmt.Errorf("index: scanning dir path: %w", err)
		}
		r.dirs[p] = true
		if skipped || unreadable {
			r.skipped = append(r.skipped, p)
		} else if entries == 0 {
			// A directory we chose not to read is not an empty directory.
			r.emptyDirs[p] = true
		}
		add(p)
	}
	return r, drows.Err()
}

// resolves reports whether target, as written in the document at docPath, names
// anything in the tree.
//
// Three readings are accepted, in increasing generosity:
//
//  1. Repo-root-relative — `packages/contracts/schemas`.
//  2. Relative to the document — `apps/web/CLAUDE.md` naming `src/lib/x.ts`.
//  3. A distinctive suffix of a real path — `docs/DATA_MODEL.md` naming
//     `analytics/router.py` for `services/analytics-python/app/analytics/router.py`.
//
// The third reading is what keeps this check honest. Writing a path by its
// meaningful tail is ordinary documentation practice, not drift, and treating
// it as drift produced 47 findings on the least-drifted repository in the
// workspace. The bar this check enforces is therefore the strict one it can
// actually defend: the reference matches nothing anywhere in the tree.
//
// Precision beyond that — whether the path resolves to the thing the sentence
// is *about* — needs the claim layer, and is deliberately not attempted here.
func (r *resolver) resolves(docPath, target string) bool {
	if target == "" {
		return true
	}
	for _, c := range candidatePaths(docPath, target) {
		if r.files[c] || r.dirs[c] {
			return true
		}
		// Beneath a directory the walk skipped, absence is unknown. Claiming
		// "no such file" there reported committed `vendor/` paths as drift.
		if r.underSkipped(c) {
			return true
		}
	}
	return r.suffixMatch(target)
}

// candidatePaths returns the exact readings of a reference: root-relative
// first, then relative to the referring document.
func candidatePaths(docPath, target string) []string {
	out := []string{target}
	if dir := pathDir(docPath); dir != "" {
		out = append(out, dir+"/"+target)
	}
	return out
}

// pathDir returns the directory part of a repo-relative slash path, or "" when
// the path is at the root.
func pathDir(p string) string {
	i := strings.LastIndex(p, "/")
	if i <= 0 {
		return ""
	}
	return p[:i]
}

// suffixMatch reports whether any indexed path ends in target on a segment
// boundary. The boundary matters: `router.py` must not be satisfied by
// `old_router.py`.
func (r *resolver) suffixMatch(target string) bool {
	seg := target
	if i := strings.LastIndex(target, "/"); i >= 0 {
		seg = target[i+1:]
	}
	for _, p := range r.bySuffix[seg] {
		if p == target || strings.HasSuffix(p, "/"+target) {
			return true
		}
	}
	return false
}

// emptyDirFor returns the tree path of the empty directory a reference names,
// and whether one was found. Only the exact readings are considered: a suffix
// match is too weak a basis for asserting that a specific directory is the one
// the document meant and that it is empty.
func (r *resolver) emptyDirFor(docPath, target string) (string, bool) {
	for _, c := range candidatePaths(docPath, target) {
		if r.emptyDirs[c] {
			return c, true
		}
	}
	return "", false
}

// underSkipped reports whether a path lies inside a directory the walk chose
// not to descend into.
func (r *resolver) underSkipped(p string) bool {
	for _, d := range r.skipped {
		if strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	return false
}
