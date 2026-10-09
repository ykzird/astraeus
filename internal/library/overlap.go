package library

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Overlap describes an existing library whose root overlaps another one.
type Overlap struct {
	// Existing is the library already registered.
	Existing *Library
	// CandidateContains reports which way the overlap runs: true when the
	// candidate root is inside the existing one, false when the existing root is
	// inside the candidate.
	CandidateContains bool
}

// FindOverlap reports the first registered library whose root overlaps root.
//
// Two libraries may not overlap. A media object is identified by its file path,
// and that path is globally unique - so a file reachable from two libraries can
// belong to only one of them. The scanner reconciles the object it finds by
// re-pointing it at whichever library is scanning, which means two overlapping
// roots take turns owning the same files:
//
//   - every scan of either library reports the object as *updated*, forever, so
//     both look busy on every run and neither ever settles;
//   - the entity the file belongs to changes with the scan order, so metadata
//     and a viewer's resume position follow whichever library ran last.
//
// That was L-15 of the 2026-10-09 review. Refusing the overlap is the fix rather
// than teaching the object table to hold one file twice, because the second
// library is almost always a mistake: /srv/media and /srv/media/movies registered
// together mean the same films are in both, and the person doing it wants one
// library or the other.
//
// Paths are compared after resolving symlinks, because /media and /mnt/media-link
// are the same directory and two names for one directory is exactly the case that
// is hard to see. A path that cannot be resolved is compared as written, which
// errs towards reporting an overlap only when the text agrees.
func FindOverlap(existing []Library, root string) (Overlap, bool) {
	candidate := canonicalPath(root)

	for i := range existing {
		lib := &existing[i]
		registered := canonicalPath(lib.Path)
		if registered == "" || candidate == "" {
			continue
		}
		if registered == candidate {
			return Overlap{Existing: lib}, true
		}
		// `containsPath` is true for a path against itself, so the equality case
		// above has to be handled first or the second branch is unreachable and
		// every nested root is reported the same way round.
		switch {
		case containsPath(candidate, registered):
			// The candidate is the outer root: registering it would swallow the
			// library already inside it.
			return Overlap{Existing: lib}, true
		case containsPath(registered, candidate):
			// The candidate is inside the registered root.
			return Overlap{Existing: lib, CandidateContains: true}, true
		}
	}
	return Overlap{}, false
}

// OverlapError renders the refusal an operator sees, so the API and the CLI say
// the same thing.
func (o Overlap) Error() string {
	if o.Existing == nil {
		return "the library root overlaps one that is already registered"
	}
	switch {
	case o.CandidateContains:
		return fmt.Sprintf(
			"the library root is inside %q, which is already registered: a file reachable from two libraries "+
				"can belong to only one of them, so the entity it belongs to would change with the scan order",
			o.Existing.Path)
	default:
		return fmt.Sprintf(
			"%q is inside the library root, so it contains a registered library: register one or the other",
			o.Existing.Path)
	}
}

// containsPath reports whether inner is at or below outer.
//
// It compares the relative path rather than string prefixes, because
// /srv/media-archive is not inside /srv/media even though the text says so.
func containsPath(outer, inner string) bool {
	if outer == "" || inner == "" {
		return false
	}
	rel, err := filepath.Rel(outer, inner)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	// ".." or "../anything" means inner is outside outer.
	return !strings.HasPrefix(rel, "..")
}

// canonicalPath resolves a path as far as it can, for comparison.
//
// EvalSymlinks needs every component to exist, which a path being registered
// often does not yet have - so the deepest existing ancestor is resolved and the
// remainder is appended. That is enough to catch two names for one directory,
// which is the case worth catching.
func canonicalPath(path string) string {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}

	// Walk up until something resolves, then re-attach what was stripped.
	rest := ""
	for current := path; ; {
		parent := filepath.Dir(current)
		if parent == current {
			return path
		}
		rest = filepath.Join(filepath.Base(current), rest)
		if resolved, err := filepath.EvalSymlinks(parent); err == nil {
			return filepath.Join(resolved, rest)
		}
		current = parent
	}
}

// RootExists reports whether a path is a directory, which every library root has
// to be. It is here rather than in the callers so the API and the CLI agree on
// what "a usable root" means.
func RootExists(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("path is not readable: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("path is not a directory")
	}
	return nil
}
