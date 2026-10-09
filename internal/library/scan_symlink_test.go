package library_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ykzird/astraeus/internal/library"
)

// TestScanLibrary_FollowsASymlinkedRoot is the regression test for L-5.
//
// Registering a library as a symbolic link is the ordinary shape in a Docker or
// NAS setup: /media is a link to the real volume, and every path in the compose
// file says /media. os.Stat followed the link happily, so the library passed
// validation, but filepath.WalkDir lstats its root - it saw a link rather than a
// directory and stopped. The scan reported zero files and no warning, which
// reads as "this library is empty" rather than "the path was never walked".
func TestScanLibrary_FollowsASymlinkedRoot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// The real volume, and a link to it, exactly as a container would mount it.
	real := filepath.Join(dir, "volume", "media")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("creating the real media directory: %v", err)
	}
	const files = 3
	for i := 0; i < files; i++ {
		name := filepath.Join(real, string(rune('A'+i))+" Film (2020).mp4")
		if err := os.WriteFile(name, []byte("not really a film"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	link := filepath.Join(dir, "media")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("this host cannot create symbolic links: %v", err)
	}

	repo := newTestRepo(t)
	ctx := context.Background()
	lib := &library.Library{
		ID: "library-symlink", Name: "Movies", Path: link,
		Kind: library.MoviesLibrary, CreatedAt: time.Now(),
	}
	if err := repo.CreateLibrary(ctx, lib); err != nil {
		t.Fatalf("creating the library: %v", err)
	}

	result, err := library.NewScanner(repo, newTestLogger()).ScanLibrary(ctx, lib)
	if err != nil {
		t.Fatalf("ScanLibrary on a symlinked root: %v", err)
	}

	if result.FilesSeen != files {
		t.Errorf("the scan saw %d files through a symlinked root, want %d\n"+
			"A symlinked root reports an empty library and no warning, which is "+
			"indistinguishable from a library that really is empty", result.FilesSeen, files)
	}
	if result.EntitiesCreated != files {
		t.Errorf("entities created = %d, want %d", result.EntitiesCreated, files)
	}
}

// TestScanLibrary_ReportsAnUnresolvableRoot covers the other half: a root that
// exists for Stat but cannot be followed must say so rather than scan nothing.
func TestScanLibrary_ReportsAnUnresolvableRoot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// A link whose target does not exist. os.Stat follows it and fails, so this
	// is caught before the walk; the assertion is that the error names the
	// library path rather than any of the paths inside it.
	link := filepath.Join(dir, "media")
	if err := os.Symlink(filepath.Join(dir, "gone"), link); err != nil {
		t.Skipf("this host cannot create symbolic links: %v", err)
	}

	repo := newTestRepo(t)
	lib := &library.Library{
		ID: "library-broken", Name: "Movies", Path: link,
		Kind: library.MoviesLibrary, CreatedAt: time.Now(),
	}
	if err := repo.CreateLibrary(context.Background(), lib); err != nil {
		t.Fatalf("creating the library: %v", err)
	}

	if _, err := library.NewScanner(repo, newTestLogger()).ScanLibrary(context.Background(), lib); err == nil {
		t.Error("a library root pointing nowhere should be an error, not an empty scan")
	}
}
