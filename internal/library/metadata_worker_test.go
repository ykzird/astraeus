package library

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestMetadataWorker_EnrichOnce(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	ctx := context.Background()
	lib := mustCreateLibrary(t, repo, "Movies", MoviesLibrary)
	movie := mustCreateEntity(t, repo, lib.ID, nil, MovieEntity, "Dune")
	episode := mustCreateEntity(t, repo, lib.ID, nil, EpisodeEntity, "S01E01")

	worker := NewMetadataWorker(repo, NewMockProvider(), time.Hour, newTestLogger())

	result, err := worker.EnrichOnce(ctx)
	if err != nil {
		t.Fatalf("EnrichOnce: %v", err)
	}
	if result.Processed != 2 {
		t.Errorf("processed = %d, want 2", result.Processed)
	}
	if result.Enriched != 2 {
		t.Errorf("enriched = %d, want 2", result.Enriched)
	}
	if result.Failed != 0 {
		t.Errorf("failed = %d, want 0", result.Failed)
	}

	for _, id := range []string{movie.ID, episode.ID} {
		got, err := repo.GetEntity(ctx, id)
		if err != nil {
			t.Fatalf("getting entity %s: %v", id, err)
		}
		if got.Status != StatusComplete {
			t.Errorf("entity %q status = %q, want %q", got.Name, got.Status, StatusComplete)
		}
		if got.Metadata == nil {
			t.Errorf("entity %q has no metadata after enrichment", got.Name)
		}
	}

	// A second pass has nothing left to do.
	result, err = worker.EnrichOnce(ctx)
	if err != nil {
		t.Fatalf("second EnrichOnce: %v", err)
	}
	if result.Processed != 0 {
		t.Errorf("processed = %d on a second pass, want 0", result.Processed)
	}
}

func TestMetadataWorker_ProviderFailureLeavesEntityIncomplete(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	ctx := context.Background()
	lib := mustCreateLibrary(t, repo, "Movies", MoviesLibrary)
	movie := mustCreateEntity(t, repo, lib.ID, nil, MovieEntity, "Mystery Film")

	worker := NewMetadataWorker(repo, &stubProvider{name: "broken", err: errors.New("upstream down")}, time.Hour, newTestLogger())

	result, err := worker.EnrichOnce(ctx)
	if err != nil {
		t.Fatalf("EnrichOnce: %v", err)
	}
	if result.Failed != 1 {
		t.Errorf("failed = %d, want 1", result.Failed)
	}
	if result.Enriched != 0 {
		t.Errorf("enriched = %d, want 0", result.Enriched)
	}

	got, err := repo.GetEntity(ctx, movie.ID)
	if err != nil {
		t.Fatalf("getting entity: %v", err)
	}
	if got.Status != StatusIncomplete {
		t.Errorf("status = %q, want %q so the administrator can see it", got.Status, StatusIncomplete)
	}
	if got.Metadata != nil {
		t.Errorf("metadata = %+v, want nil", got.Metadata)
	}
}

func TestMetadataWorker_ContinuesAfterOneFailure(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	ctx := context.Background()
	lib := mustCreateLibrary(t, repo, "Movies", MoviesLibrary)
	mustCreateEntity(t, repo, lib.ID, nil, MovieEntity, "First")
	mustCreateEntity(t, repo, lib.ID, nil, MovieEntity, "Second")

	// A provider that fails for the first lookup and succeeds afterwards.
	flaky := &flakyProvider{failures: 1, provider: NewMockProvider()}
	worker := NewMetadataWorker(repo, flaky, time.Hour, newTestLogger())

	result, err := worker.EnrichOnce(ctx)
	if err != nil {
		t.Fatalf("EnrichOnce: %v", err)
	}
	if result.Failed != 1 || result.Enriched != 1 {
		t.Errorf("failed = %d enriched = %d, want 1 and 1", result.Failed, result.Enriched)
	}
}

func TestMetadataWorker_StartStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	worker := NewMetadataWorker(repo, NewMockProvider(), 10*time.Millisecond, newTestLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		worker.Start(ctx)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after the context was cancelled")
	}
}

type flakyProvider struct {
	failures int
	calls    int
	provider MetadataProvider
}

func (f *flakyProvider) Name() string { return "flaky" }

func (f *flakyProvider) FetchMetadata(ctx context.Context, entity *MediaEntity) (*MetadataSet, error) {
	f.calls++
	if f.calls <= f.failures {
		return nil, errors.New("transient failure")
	}
	return f.provider.FetchMetadata(ctx, entity)
}
