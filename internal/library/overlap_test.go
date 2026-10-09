package library_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ykzird/astraeus/internal/library"
)

// TestFindOverlap_RefusesNestedRoots is the regression test for L-15.
//
// A media object is identified by its file path and that path is globally unique,
// so a file reachable from two libraries can belong to only one of them. The
// scanner reconciles the object it finds by re-pointing it at whichever library is
// scanning, so two overlapping roots took turns owning the same files: every scan
// of either reported the object as updated forever, and the entity the file
// belonged to changed with the scan order - taking its metadata and the viewer's
// resume position with it.
//
// The overlap is refused rather than modelled, because the second library is
// almost always a mistake.
func TestFindOverlap_RefusesNestedRoots(t *testing.T) {
	t.Parallel()

	// A real tree, because the comparison resolves symlinks.
	root := t.TempDir()
	media := filepath.Join(root, "media")
	movies := filepath.Join(media, "movies")
	if err := os.MkdirAll(movies, 0o755); err != nil {
		t.Fatalf("creating the tree: %v", err)
	}
	// A sibling whose name shares a prefix, which a string comparison would
	// mistake for a nested path.
	archive := filepath.Join(root, "media-archive")
	if err := os.MkdirAll(archive, 0o755); err != nil {
		t.Fatalf("creating the sibling: %v", err)
	}

	registered := []library.Library{{ID: "one", Name: "Media", Path: media}}

	tests := []struct {
		name         string
		candidate    string
		wantOverlap  bool
		wantContains bool
	}{
		{
			name:        "the same root",
			candidate:   media,
			wantOverlap: true,
		},
		{
			name:         "a root inside a registered one",
			candidate:    movies,
			wantOverlap:  true,
			wantContains: true,
		},
		{
			name:        "a root that contains a registered one",
			candidate:   root,
			wantOverlap: true,
		},
		{
			name:      "a sibling sharing a name prefix",
			candidate: archive,
		},
		{
			name:      "an unrelated directory",
			candidate: t.TempDir(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			overlap, found := library.FindOverlap(registered, tt.candidate)
			if found != tt.wantOverlap {
				t.Fatalf("FindOverlap(%s) found = %v, want %v", tt.candidate, found, tt.wantOverlap)
			}
			if !found {
				return
			}
			if overlap.Existing == nil || overlap.Existing.ID != "one" {
				t.Errorf("the overlap names %+v, want the registered library", overlap.Existing)
			}
			if overlap.CandidateContains != tt.wantContains {
				t.Errorf("CandidateContains = %v, want %v", overlap.CandidateContains, tt.wantContains)
			}
			// The refusal has to say which library and why, or an operator is left
			// to work out which of their roots is at fault.
			if !contains(overlap.Error(), media) {
				t.Errorf("the refusal does not name the registered root: %s", overlap.Error())
			}
		})
	}
}

// TestFindOverlap_FollowsNamesForOneDirectory covers the case that is hardest to
// see: two paths that resolve to the same directory.
func TestFindOverlap_FollowsNamesForOneDirectory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("creating the directory: %v", err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}

	registered := []library.Library{{ID: "one", Name: "Real", Path: real}}
	if _, found := library.FindOverlap(registered, link); !found {
		t.Error("a symlink to a registered root was accepted as a separate library, " +
			"so the same files would be owned by two libraries under two names")
	}

	// And the other way round: the registered path is the link.
	registered = []library.Library{{ID: "one", Name: "Link", Path: link}}
	if _, found := library.FindOverlap(registered, real); !found {
		t.Error("a registered symlink did not overlap the directory it points at")
	}
}

// TestFindOverlap_AllowsEveryRootWhenNoneShareA tree of unrelated roots is the
// normal case and must not be refused.
func TestFindOverlap_AllowsEveryRootWhenNoneShare(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	var registered []library.Library
	for _, name := range []string{"films", "shows", "music"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatalf("creating %s: %v", name, err)
		}
		registered = append(registered, library.Library{ID: name, Name: name, Path: path})
	}
	// A fourth that shares no prefix with any of them.
	other := filepath.Join(root, "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatalf("creating other: %v", err)
	}
	if overlap, found := library.FindOverlap(registered, other); found {
		t.Errorf("an unrelated root was refused because of %+v", overlap.Existing)
	}
}

// TestRootExists pins what a usable root is, since the API and the CLI both ask.
func TestRootExists(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := library.RootExists(dir); err != nil {
		t.Errorf("a directory was refused: %v", err)
	}

	file := filepath.Join(dir, "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("writing the file: %v", err)
	}
	if err := library.RootExists(file); err == nil {
		t.Error("a file was accepted as a library root")
	}
	if err := library.RootExists(filepath.Join(dir, "missing")); err == nil {
		t.Error("a path that does not exist was accepted as a library root")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
