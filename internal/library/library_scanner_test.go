package library_test

import (
	"context"
	"github.com/jok/astraeus-media/internal/library"
	"path/filepath"
	"testing"
	"time"
)

func TestScanScheduler_ScanAll(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	ctx := context.Background()

	moviesRoot := t.TempDir()
	writeFile(t, filepath.Join(moviesRoot, "Dune (2021).mkv"), "dune")
	showsRoot := t.TempDir()
	writeFile(t, filepath.Join(showsRoot, "Show", "Season 01", "Show.S01E01.mkv"), "ep")

	mustLibraryAt(t, repo, moviesRoot, library.MoviesLibrary)
	mustLibraryAt(t, repo, showsRoot, library.ShowsLibrary)

	scheduler := library.NewScanScheduler(repo, library.NewScanner(repo, newTestLogger()), time.Hour, newTestLogger())

	outcomes, err := scheduler.ScanAll(ctx)
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}
	if len(outcomes) != 2 {
		t.Fatalf("got %d outcomes, want 2", len(outcomes))
	}

	totalFiles := 0
	for _, outcome := range outcomes {
		if outcome.Error != "" {
			t.Errorf("library %q failed: %s", outcome.LibraryName, outcome.Error)
		}
		totalFiles += outcome.Result.FilesSeen
	}
	if totalFiles != 2 {
		t.Errorf("files seen across libraries = %d, want 2", totalFiles)
	}

	// A second pass must be idempotent.
	outcomes, err = scheduler.ScanAll(ctx)
	if err != nil {
		t.Fatalf("second ScanAll: %v", err)
	}
	for _, outcome := range outcomes {
		if outcome.Result.EntitiesCreated != 0 || outcome.Result.ObjectsCreated != 0 {
			t.Errorf("library %q created entities=%d objects=%d on a repeat pass, want 0 and 0",
				outcome.LibraryName, outcome.Result.EntitiesCreated, outcome.Result.ObjectsCreated)
		}
	}
}

func TestScanScheduler_ScansNewFilesOnTheNextPass(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	ctx := context.Background()
	root := t.TempDir()

	writeFile(t, filepath.Join(root, "Dune (2021).mkv"), "dune")
	mustLibraryAt(t, repo, root, library.MoviesLibrary)

	scheduler := library.NewScanScheduler(repo, library.NewScanner(repo, newTestLogger()), time.Hour, newTestLogger())
	if _, err := scheduler.ScanAll(ctx); err != nil {
		t.Fatalf("first ScanAll: %v", err)
	}

	// A file that appears later is picked up by the next pass, with no manual
	// intervention.
	writeFile(t, filepath.Join(root, "Arrival (2016).mkv"), "arrival")

	outcomes, err := scheduler.ScanAll(ctx)
	if err != nil {
		t.Fatalf("second ScanAll: %v", err)
	}
	if len(outcomes) != 1 {
		t.Fatalf("got %d outcomes, want 1", len(outcomes))
	}
	if outcomes[0].Result.EntitiesCreated != 1 {
		t.Errorf("entities created = %d, want 1", outcomes[0].Result.EntitiesCreated)
	}
}

func TestScanScheduler_RecordsPerLibraryFailures(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	ctx := context.Background()

	goodRoot := t.TempDir()
	writeFile(t, filepath.Join(goodRoot, "Dune (2021).mkv"), "dune")
	mustLibraryAt(t, repo, goodRoot, library.MoviesLibrary)

	// A library whose directory has since disappeared.
	missing := &library.Library{
		ID:        "missing-library",
		Name:      "Missing",
		Path:      filepath.Join(t.TempDir(), "gone"),
		Kind:      library.MoviesLibrary,
		CreatedAt: time.Now(),
	}
	if err := repo.CreateLibrary(ctx, missing); err != nil {
		t.Fatalf("creating library: %v", err)
	}

	scheduler := library.NewScanScheduler(repo, library.NewScanner(repo, newTestLogger()), time.Hour, newTestLogger())
	outcomes, err := scheduler.ScanAll(ctx)
	if err != nil {
		t.Fatalf("ScanAll: %v", err)
	}

	var failures, successes int
	for _, outcome := range outcomes {
		if outcome.Error != "" {
			failures++
		} else {
			successes++
		}
	}
	if failures != 1 {
		t.Errorf("failures = %d, want 1", failures)
	}
	if successes != 1 {
		t.Errorf("successes = %d, want 1 (one bad library must not stop the others)", successes)
	}
}

func TestScanScheduler_StartStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	scheduler := library.NewScanScheduler(repo, library.NewScanner(repo, newTestLogger()), 10*time.Millisecond, newTestLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		scheduler.Start(ctx)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after the context was cancelled")
	}
}

func TestScanScheduler_DisabledIntervalReturnsImmediately(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	scheduler := library.NewScanScheduler(repo, library.NewScanner(repo, newTestLogger()), 0, newTestLogger())

	done := make(chan struct{})
	go func() {
		defer close(done)
		scheduler.Start(context.Background())
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a disabled scheduler should not block")
	}
	if scheduler.Interval() != 0 {
		t.Errorf("interval = %v, want 0", scheduler.Interval())
	}
}
