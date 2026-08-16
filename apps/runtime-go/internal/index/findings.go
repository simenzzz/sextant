package index

import (
	"context"
	"fmt"
	"sort"
)

// Finding kinds. Each is a contradiction between two indexed facts — never a
// judgement about prose. Anything requiring a model to read intent belongs to
// the claim/verdict layer, not here.
const (
	KindDanglingDocPath   = "dangling-doc-path"
	KindEmptyAdvertised   = "empty-advertised-dir"
	KindMissingScript     = "missing-target-script"
	KindPhaseContradicted = "phase-marker-contradiction"
	KindPhaseUndocumented = "undocumented-shipped-phase"
)

// newFinding builds a finding with every repository-derived string sanitised.
//
// Constructing findings through one function rather than as struct literals is
// what makes that guarantee hold: a new check cannot forget it.
func newFinding(kind, path string, line int, claim, evidence string) Finding {
	return Finding{
		Kind:     kind,
		Path:     sanitizePath(path),
		Line:     line,
		Claim:    sanitize(claim),
		Evidence: sanitize(evidence),
	}
}

// Finding is one contradiction, carrying both sides of it so a reader never has
// to take the tool's word for anything.
type Finding struct {
	Kind     string `json:"kind"`
	Path     string `json:"path"`
	Line     int    `json:"line,omitempty"`
	Claim    string `json:"claim"`
	Evidence string `json:"evidence"`
}

// Findings runs every deterministic check against the index.
//
// Results are sorted by path then line so two runs over an unchanged tree
// produce byte-identical output — a precondition for using this in CI, and for
// diffing one run against the next.
func (s *Store) Findings(ctx context.Context) ([]Finding, error) {
	var all []Finding

	res, err := newResolver(ctx, s)
	if err != nil {
		return nil, err
	}

	checks := []func(context.Context) ([]Finding, error){
		func(c context.Context) ([]Finding, error) { return s.danglingDocPaths(c, res) },
		func(c context.Context) ([]Finding, error) { return s.emptyAdvertisedDirs(c, res) },
		func(c context.Context) ([]Finding, error) { return s.missingTargetScripts(c, res) },
		s.contradictedPhases,
		s.undocumentedPhases,
	}
	for _, check := range checks {
		found, err := check(ctx)
		if err != nil {
			return nil, err
		}
		all = append(all, found...)
	}

	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Path != all[j].Path {
			return all[i].Path < all[j].Path
		}
		if all[i].Line != all[j].Line {
			return all[i].Line < all[j].Line
		}
		return all[i].Kind < all[j].Kind
	})
	return all, nil
}

// docPathRef is one path reference, with the document that made it.
type docPathRef struct {
	path   string
	line   int
	target string
	hint   string
}

// danglingDocPaths finds paths a document points at that name nothing in the
// tree under any reading. See resolver.resolves for what counts as a reading
// and why the bar is set where it is.
func (s *Store) danglingDocPaths(ctx context.Context, res *resolver) ([]Finding, error) {
	refs, err := s.docPathRefs(ctx)
	if err != nil {
		return nil, err
	}

	var siblings map[string]bool
	if s.opts.WorkspaceSiblings {
		siblings = siblingRepos(s.root)
	}

	// Everything still unresolved is asked of git in one batch rather than one
	// process per reference.
	var unresolved []docPathRef
	var candidates []string
	for _, r := range refs {
		// An ambiguous reference is not evidence of anything. Reporting one
		// turned `try/catch` and `ui/Button` into findings on Website, which is
		// how a checker teaches people to ignore it.
		if r.hint == HintAmbiguous {
			continue
		}
		if res.resolves(r.path, r.target) {
			continue
		}
		// A reference into a neighbouring project is not a claim about this
		// repository.
		if siblings[firstSegment(r.target)] {
			continue
		}
		unresolved = append(unresolved, r)
		candidates = append(candidates, r.target)
	}

	ignored, complete := gitIgnored(ctx, s.root, candidates)
	if !complete {
		// A run whose gitignore suppression is incomplete must not read like a
		// clean one; the report says so.
		s.degraded = true
	}

	var out []Finding
	for _, r := range unresolved {
		if ignored[r.target] {
			continue
		}
		out = append(out, newFinding(KindDanglingDocPath, r.path, r.line,
			fmt.Sprintf("documentation points at %q", r.target),
			"no file or directory anywhere in the tree matches that path"))
	}
	return out, nil
}

// emptyAdvertisedDirs finds directories a document points at which exist but
// hold nothing. A documented `eval/` that is a tree of empty folders passes
// every existence check and still delivers none of what the document promises.
func (s *Store) emptyAdvertisedDirs(ctx context.Context, res *resolver) ([]Finding, error) {
	refs, err := s.docPathRefs(ctx)
	if err != nil {
		return nil, err
	}

	var out []Finding
	for _, r := range refs {
		found, ok := res.emptyDirFor(r.path, r.target)
		if !ok {
			continue
		}
		out = append(out, newFinding(KindEmptyAdvertised, r.path, r.line,
			fmt.Sprintf("documentation points at %q", r.target),
			fmt.Sprintf("%q exists but is empty", found)))
	}
	return out, nil
}

func (s *Store) docPathRefs(ctx context.Context) ([]docPathRef, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT path, line, target, hint FROM doc_paths ORDER BY path, line, target`)
	if err != nil {
		return nil, fmt.Errorf("index: reading doc paths: %w", err)
	}
	defer rows.Close()

	var out []docPathRef
	for rows.Next() {
		var r docPathRef
		if err := rows.Scan(&r.path, &r.line, &r.target, &r.hint); err != nil {
			return nil, fmt.Errorf("index: scanning doc path: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// missingTargetScripts finds Make targets whose recipe invokes a script that is
// not in the tree — a target that cannot run, advertised as if it can.
//
// Script extraction happens in Go rather than SQL because it needs the recipe
// parsed, so this check reads the targets and filters in memory.
func (s *Store) missingTargetScripts(ctx context.Context, res *resolver) ([]Finding, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT path, name, line, recipe FROM make_targets ORDER BY path, name`)
	if err != nil {
		return nil, fmt.Errorf("index: reading make targets: %w", err)
	}
	defer rows.Close()

	type target struct {
		path   string
		name   string
		line   int
		recipe string
	}
	var targets []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.path, &t.name, &t.line, &t.recipe); err != nil {
			return nil, fmt.Errorf("index: scanning make target: %w", err)
		}
		targets = append(targets, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("index: reading make targets: %w", err)
	}

	var out []Finding
	for _, t := range targets {
		for _, script := range scriptRefs(t.recipe) {
			// Make runs a recipe with the Makefile's own directory as the
			// working directory, so a bare script path is relative to it, not
			// to the repository root. Both readings are accepted for the same
			// reason as doc paths.
			if res.resolves(t.path, script) {
				continue
			}
			out = append(out, newFinding(KindMissingScript, t.path, t.line,
				fmt.Sprintf("target %q runs %q", t.name, script),
				"that script does not exist, so the target fails immediately"))
		}
	}
	return out, nil
}
