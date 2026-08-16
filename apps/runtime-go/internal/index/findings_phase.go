// Phase checks: the two findings derived from comparing what a roadmap grades
// against what the commit history says shipped.
//
// Split from findings.go by check family rather than by size — these are the
// only checks that read `commits`, and the only ones whose vocabulary problem
// (four incompatible marker dialects) is solved in the index rather than here.
package index

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// contradictedPhases finds a roadmap line marking a phase as unfinished when a
// delivery commit says that phase shipped.
//
// This is the check a regex cannot do: the four marker dialects are already
// normalized in the index, so the comparison is between two facts rather than
// between two spellings.
func (s *Store) contradictedPhases(ctx context.Context) ([]Finding, error) {
	claims, err := s.phaseClaims(ctx)
	if err != nil {
		return nil, err
	}
	shipped, err := s.shippedPhases(ctx)
	if err != nil {
		return nil, err
	}

	// Grouped by (document, phase). A roadmap grades one phase on a heading and
	// again on each checkbox beneath it, and emitting a finding per line buried
	// the report under identical evidence.
	type group struct {
		path   string
		phase  string
		marker string
		first  int
		lines  []int
	}
	var order []string
	groups := map[string]*group{}

	for _, c := range claims {
		if c.marker == MarkerDone {
			continue
		}
		if _, ok := shipped[c.phase]; !ok {
			continue
		}
		key := c.path + "\x00" + c.phase
		g, seen := groups[key]
		if !seen {
			g = &group{path: c.path, phase: c.phase, marker: c.marker, first: c.line}
			groups[key] = g
			order = append(order, key)
		}
		g.lines = append(g.lines, c.line)
	}

	var out []Finding
	for _, key := range order {
		g := groups[key]
		out = append(out, newFinding(KindPhaseContradicted, g.path, g.first,
			fmt.Sprintf("phase %s marked %s%s", g.phase, g.marker, alsoOnLines(g.lines)),
			fmt.Sprintf("commit %q reports it delivered", shipped[g.phase])))
	}
	return out, nil
}

// undocumentedPhases finds phases that shipped but appear in no roadmap at all
// — the drift that is invisible to any check keyed on documented lines, because
// the documentation simply stops.
func (s *Store) undocumentedPhases(ctx context.Context) ([]Finding, error) {
	documented, roadmap, err := s.documentedPhases(ctx)
	if err != nil {
		return nil, err
	}
	if len(documented) == 0 {
		// No roadmap at all is not drift; it is a repository without one.
		return nil, nil
	}
	shipped, err := s.shippedPhases(ctx)
	if err != nil {
		return nil, err
	}

	var phases []string
	for p := range shipped {
		if !documented[p] {
			phases = append(phases, p)
		}
	}
	sort.Strings(phases)

	var out []Finding
	for _, p := range phases {
		out = append(out, newFinding(KindPhaseUndocumented, roadmap, 0,
			fmt.Sprintf("no roadmap entry for phase %s", p),
			fmt.Sprintf("commit %q reports it delivered", shipped[p])))
	}
	return out, nil
}

type phaseClaim struct {
	path   string
	line   int
	phase  string
	marker string
}

// phaseClaims returns every documented status line that names a phase.
func (s *Store) phaseClaims(ctx context.Context) ([]phaseClaim, error) {
	const q = `
SELECT path, line, phase, marker
FROM doc_claims
WHERE phase <> ''
ORDER BY path, line`

	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("index: reading phase claims: %w", err)
	}
	defer rows.Close()

	var out []phaseClaim
	for rows.Next() {
		var c phaseClaim
		if err := rows.Scan(&c.path, &c.line, &c.phase, &c.marker); err != nil {
			return nil, fmt.Errorf("index: scanning phase claim: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// documentedPhases returns every phase the documentation NAMES, graded or not,
// and the document that names the most of them.
//
// Reading grades here instead was a false-positive source: this repository's
// P0-P9 table has no status column, so every phase in it looked undocumented.
func (s *Store) documentedPhases(ctx context.Context) (map[string]bool, string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT path, phase FROM doc_phases ORDER BY path, line`)
	if err != nil {
		return nil, "", fmt.Errorf("index: reading documented phases: %w", err)
	}
	defer rows.Close()

	out := map[string]bool{}
	perDoc := map[string]int{}
	for rows.Next() {
		var path, phase string
		if err := rows.Scan(&path, &phase); err != nil {
			return nil, "", fmt.Errorf("index: scanning documented phase: %w", err)
		}
		out[phase] = true
		perDoc[path]++
	}
	if err := rows.Err(); err != nil {
		return nil, "", fmt.Errorf("index: reading documented phases: %w", err)
	}

	// Attribute the finding to whichever document tracks the most phases rather
	// than to whichever sorts first alphabetically, so it lands on the roadmap
	// instead of on an unrelated file.
	best, bestN := "", -1
	for path, n := range perDoc {
		if n > bestN || (n == bestN && path < best) {
			best, bestN = path, n
		}
	}
	return out, best, nil
}

// shippedPhases maps a phase identifier to the subject of the commit claiming
// it shipped. When several commits name the same phase the earliest wins — that
// is the one that delivered it, and later ones are follow-ups.
func (s *Store) shippedPhases(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT subject FROM commits ORDER BY committed_at ASC, sha ASC`)
	if err != nil {
		return nil, fmt.Errorf("index: reading commits: %w", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var subject string
		if err := rows.Scan(&subject); err != nil {
			return nil, fmt.Errorf("index: scanning commit: %w", err)
		}
		for _, p := range commitPhases(subject) {
			if _, seen := out[p]; !seen {
				out[p] = subject
			}
		}
	}
	return out, rows.Err()
}

// alsoOnLines renders the remaining line numbers of a grouped finding, so the
// report stays one line per problem without losing where else it appears.
func alsoOnLines(lines []int) string {
	if len(lines) < 2 {
		return ""
	}
	rest := make([]string, 0, len(lines)-1)
	for _, l := range lines[1:] {
		rest = append(rest, strconv.Itoa(l))
	}
	return " (also on line " + strings.Join(rest, ", ") + ")"
}
