package index

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// skipDirs are never descended into. Every entry is either a package manager's
// cache, a build output, or a tool's scratch space — directories whose contents
// are derived rather than authored, and which would otherwise dominate every
// count in the index.
//
// This list is the single biggest lever on false positives: `loom` carries a
// Rust `target/` with more files than the rest of the workspace combined.
var skipDirs = map[string]bool{
	".git":          true,
	"node_modules":  true,
	"target":        true,
	"dist":          true,
	"build":         true,
	".venv":         true,
	"venv":          true,
	"__pycache__":   true,
	".mypy_cache":   true,
	".ruff_cache":   true,
	".pytest_cache": true,
	"site-packages": true,
	".next":         true,
	"coverage":      true,
	".terraform":    true,
	"vendor":        true,
}

// maxFileBytes bounds what the line-oriented scanners will read. A file above
// it is still recorded in `files` — its existence is a fact — but is not
// scanned for stubs. Committed fixtures and minified bundles routinely run to
// megabytes on one line, and scanning them buys nothing.
const maxFileBytes = 2 << 20 // 2 MiB

// maxEntries caps how much tree the indexer will hold in memory at once.
// Measured at roughly 72 MB of RSS for 200,000 entries, so this bounds the walk
// at a few hundred megabytes rather than leaving it open-ended.
const maxEntries = 400_000

// entry is one walked path, relative to the repository root.
type entry struct {
	rel     string
	abs     string
	isDir   bool
	skipped bool
	size    int64
}

// walk collects every file and directory under root, skipping the derived
// directories above. Paths are returned repo-relative with forward slashes, so
// an index built on Windows and one built on Linux compare equal.
//
// Symlinks are recorded but never followed: following them can escape the
// repository entirely, and a link's target is a fact about somewhere else.
func walk(root string) ([]entry, error) {
	var out []entry

	err := filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		// Enforced here rather than on the finished slice: checking afterwards
		// means the memory the cap exists to bound has already been allocated.
		if len(out) >= maxEntries {
			return errTooManyEntries
		}
		if err != nil {
			// An unreadable directory is a fact about the tree, not a reason to
			// abandon the index. Skip it and keep going.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		rel, relErr := filepath.Rel(root, abs)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}

		if d.IsDir() {
			if skipDirs[d.Name()] {
				// Recorded, then not descended into. Its contents are derived
				// and would dominate every count, but the directory itself is a
				// fact: without the row, a documented `vendor/x/y.go` resolves
				// nowhere and is reported as missing when it is committed and
				// present.
				out = append(out, entry{rel: rel, abs: abs, isDir: true, skipped: true})
				return filepath.SkipDir
			}
			out = append(out, entry{rel: rel, abs: abs, isDir: true})
			return nil
		}

		// Symlinks report as irregular; record the name, never traverse.
		if !d.Type().IsRegular() {
			out = append(out, entry{rel: rel, abs: abs})
			return nil
		}

		info, infoErr := d.Info()
		if infoErr != nil {
			return nil
		}
		out = append(out, entry{rel: rel, abs: abs, size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// errTooManyEntries stops a walk that has exceeded maxEntries.
var errTooManyEntries = fmt.Errorf(
	"tree holds more than %d files; narrow the path or raise maxEntries", maxEntries)

// countEntries reports how many direct children a directory has, after the skip
// list is applied. A directory holding only `node_modules` reads as empty here,
// which is the honest answer to "does this advertised directory contain
// anything authored?".
//
// The error is returned rather than folded into a zero. "I could not read this"
// and "this is empty" must never render as the same sentence: an EACCES
// directory was being reported as "exists but is empty" about a directory with
// files in it.
func countEntries(abs string) (int, error) {
	items, err := os.ReadDir(abs)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, it := range items {
		if it.IsDir() && skipDirs[it.Name()] {
			continue
		}
		n++
	}
	return n, nil
}

// isTextExt reports whether a path is worth scanning line by line. An allowlist
// rather than a binary sniff: the scanners look for source-language stub
// markers, and an allowlist keeps a committed `.bin` fixture from ever reaching
// them.
func isTextExt(rel string) bool {
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".go", ".rs", ".py", ".ts", ".tsx", ".js", ".jsx", ".java", ".kt",
		".rb", ".c", ".h", ".cc", ".cpp", ".hpp", ".cs", ".swift", ".scala",
		".sh", ".bash", ".sql", ".md", ".yml", ".yaml", ".toml", ".json":
		return true
	}
	// Extensionless files that are conventionally text and worth scanning.
	switch filepath.Base(rel) {
	case "Makefile", "makefile", "Dockerfile", "Justfile":
		return true
	}
	return false
}

// isDocPath reports whether a path is documentation — the surface whose claims
// the tool exists to check.
func isDocPath(rel string) bool {
	if strings.ToLower(filepath.Ext(rel)) != ".md" {
		return false
	}
	return true
}
