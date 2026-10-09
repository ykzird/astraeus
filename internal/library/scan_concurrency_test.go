package library_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ykzird/astraeus/internal/library"
	"github.com/ykzird/astraeus/internal/library/sqlite"
)

// TestScanLibrary_RunsAlongsideAnotherWriter is the regression test for L-4.
//
// A scan writes inside a transaction that reads first - it lists what it knows
// and then updates it. With BEGIN DEFERRED that transaction takes a read lock
// and upgrades to a write lock later, and if another connection committed in
// between, SQLite cannot grant the upgrade: it returns SQLITE_BUSY_SNAPSHOT at
// once, and busy_timeout is never consulted. The result was a scan failing with
// "database is locked (517)" after zero to two files whenever the metadata
// worker was writing at the same time.
//
// Two connections are used deliberately. One Repository pools connections, and a
// single pool serialises enough of this to hide the problem; the review's
// failure needs a second connection committing while the scan's transaction is
// open, which is what a second Repository on the same file provides.
func TestScanLibrary_RunsAlongsideAnotherWriter(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "astraeus.db")

	open := func() *sqlite.Repository {
		repo, err := sqlite.Open(dbPath)
		if err != nil {
			t.Fatalf("opening %s: %v", dbPath, err)
		}
		if err := repo.Migrate(context.Background()); err != nil {
			t.Fatalf("migrating %s: %v", dbPath, err)
		}
		t.Cleanup(func() { _ = repo.Close() })
		return repo
	}

	scannerRepo := open()
	writerRepo := open()

	// A library with enough files that the scan holds its transaction open long
	// enough for the writer to commit inside it.
	media := filepath.Join(dir, "media")
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatalf("creating the media directory: %v", err)
	}
	const files = 400
	for i := 0; i < files; i++ {
		name := filepath.Join(media, fmt.Sprintf("Film %03d (2020).mp4", i))
		if err := os.WriteFile(name, []byte("not really a film"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	ctx := context.Background()
	lib := &library.Library{
		ID:        "library-1",
		Name:      "Movies",
		Path:      media,
		Kind:      library.MoviesLibrary,
		CreatedAt: time.Now(),
	}
	if err := scannerRepo.CreateLibrary(ctx, lib); err != nil {
		t.Fatalf("creating the library: %v", err)
	}
	// One entity for the writer to update while the scan runs.
	if err := writerRepo.CreateEntity(ctx, &library.MediaEntity{
		ID: "entity-1", LibraryID: lib.ID, Type: library.MovieEntity,
		Name: "Existing", Status: library.StatusIncomplete,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("creating the entity: %v", err)
	}

	// The writer loop stands in for the metadata worker: a read-modify-write
	// cycle committing repeatedly while the scan is in progress.
	stop := make(chan struct{})
	var (
		wg         sync.WaitGroup
		writerErrs []error
		mu         sync.Mutex
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}

			entity, err := writerRepo.GetEntity(ctx, "entity-1")
			if err != nil {
				mu.Lock()
				writerErrs = append(writerErrs, err)
				mu.Unlock()
				return
			}
			entity.Name = fmt.Sprintf("Existing %d", i)
			if err := writerRepo.UpdateEntity(ctx, entity); err != nil {
				mu.Lock()
				writerErrs = append(writerErrs, err)
				mu.Unlock()
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	scanner := library.NewScanner(scannerRepo, newTestLogger())
	result, scanErr := scanner.ScanLibrary(ctx, lib)
	close(stop)
	wg.Wait()

	if scanErr != nil {
		t.Errorf("a scan alongside another writer failed: %v\n"+
			"This is the SQLITE_BUSY_SNAPSHOT failure: a deferred transaction could not "+
			"upgrade to a write lock because the other connection committed first", scanErr)
	}
	for _, err := range writerErrs {
		// The writer may legitimately see SQLITE_BUSY if it exhausted its own
		// busy handler, but a snapshot failure is the bug.
		if strings.Contains(err.Error(), "database is locked") {
			t.Errorf("the writer failed with %v", err)
		}
	}
	if scanErr == nil && result.FilesSeen != files {
		t.Errorf("the scan saw %d files, want %d", result.FilesSeen, files)
	}
}

// TestDSN_RequestsImmediateTransactions pins the setting itself, so a
// refactor of the DSN cannot quietly drop the fix.
func TestDSN_RequestsImmediateTransactions(t *testing.T) {
	t.Parallel()

	dsn := sqlite.DSN(filepath.Join(t.TempDir(), "astraeus.db"))
	if !strings.Contains(dsn, "_txlock=immediate") {
		t.Errorf("the DSN does not start transactions immediately, so a scan and a "+
			"writer can deadlock on a lock upgrade (L-4):\n%s", dsn)
	}
}
