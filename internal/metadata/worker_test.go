package metadata

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ykzird/astraeus/internal/library"
)

// memoryStore is a Store in memory. The worker only needs to list entities and
// write one back, so the metadata tests need no database at all - which is the
// point of keeping the port narrow.
type memoryStore struct {
	entities []library.MediaEntity
	listErr  error
	saveErr  error
	saved    int
	// pruneOnSave names an entity that is removed as it is written, which is what
	// a concurrent scan does to an entity between the list and the write.
	pruneOnSave string
}

func (m *memoryStore) ListEntities(context.Context) ([]library.MediaEntity, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.entities, nil
}

func (m *memoryStore) UpdateEntity(_ context.Context, entity *library.MediaEntity) error {
	if m.saveErr != nil {
		return m.saveErr
	}
	if m.pruneOnSave != "" && entity.ID == m.pruneOnSave {
		for i := range m.entities {
			if m.entities[i].ID == entity.ID {
				m.entities = append(m.entities[:i], m.entities[i+1:]...)
				break
			}
		}
		return library.ErrNotFound
	}
	for i := range m.entities {
		if m.entities[i].ID == entity.ID {
			m.entities[i] = *entity
			m.saved++
			return nil
		}
	}
	return library.ErrNotFound
}

// entity returns the stored copy, so assertions see what the worker wrote.
func (m *memoryStore) entity(id string) library.MediaEntity {
	for _, entity := range m.entities {
		if entity.ID == id {
			return entity
		}
	}
	return library.MediaEntity{}
}

func newEntity(id string, entityType library.EntityType, name string) library.MediaEntity {
	return library.MediaEntity{
		ID:     id,
		Type:   entityType,
		Name:   name,
		Status: library.StatusIncomplete,
	}
}

func TestWorker_EnrichOnce(t *testing.T) {
	t.Parallel()

	store := &memoryStore{entities: []library.MediaEntity{
		newEntity("movie-1", library.MovieEntity, "Dune"),
		newEntity("episode-1", library.EpisodeEntity, "S01E01"),
	}}
	worker := NewWorker(store, NewMock(), time.Hour, nil)

	result, err := worker.EnrichOnce(context.Background())
	if err != nil {
		t.Fatalf("EnrichOnce: %v", err)
	}
	if result.Processed != 2 || result.Enriched != 2 || result.Failed != 0 {
		t.Errorf("result = %+v, want 2 processed, 2 enriched, 0 failed", result)
	}

	for _, id := range []string{"movie-1", "episode-1"} {
		got := store.entity(id)
		if got.Status != library.StatusComplete {
			t.Errorf("entity %s status = %q, want %q", id, got.Status, library.StatusComplete)
		}
		if got.Metadata == nil {
			t.Errorf("entity %s has no metadata after enrichment", id)
		}
	}

	// A second pass has nothing left to do.
	second, err := worker.EnrichOnce(context.Background())
	if err != nil {
		t.Fatalf("second EnrichOnce: %v", err)
	}
	if second.Processed != 0 {
		t.Errorf("processed = %d on a second pass, want 0", second.Processed)
	}
}

func TestWorker_ProviderFailureLeavesEntityIncomplete(t *testing.T) {
	t.Parallel()

	store := &memoryStore{entities: []library.MediaEntity{
		newEntity("movie-1", library.MovieEntity, "Mystery Film"),
	}}
	worker := NewWorker(store, &stubProvider{name: "broken", err: errors.New("upstream down")}, time.Hour, nil)

	result, err := worker.EnrichOnce(context.Background())
	if err != nil {
		t.Fatalf("EnrichOnce: %v", err)
	}
	if result.Failed != 1 || result.Enriched != 0 {
		t.Errorf("result = %+v, want 1 failed and 0 enriched", result)
	}

	got := store.entity("movie-1")
	if got.Status != library.StatusIncomplete {
		t.Errorf("status = %q, want %q so the administrator can see it", got.Status, library.StatusIncomplete)
	}
	if got.Metadata != nil {
		t.Errorf("metadata = %+v, want nil", got.Metadata)
	}
}

func TestWorker_ContinuesAfterOneFailure(t *testing.T) {
	t.Parallel()

	store := &memoryStore{entities: []library.MediaEntity{
		newEntity("movie-1", library.MovieEntity, "First"),
		newEntity("movie-2", library.MovieEntity, "Second"),
	}}

	// A provider that fails for the first lookup and succeeds afterwards.
	flaky := &flakyProvider{failures: 1, provider: NewMock()}
	worker := NewWorker(store, flaky, time.Hour, nil)

	result, err := worker.EnrichOnce(context.Background())
	if err != nil {
		t.Fatalf("EnrichOnce: %v", err)
	}
	if result.Failed != 1 || result.Enriched != 1 {
		t.Errorf("failed = %d enriched = %d, want 1 and 1", result.Failed, result.Enriched)
	}
}

func TestWorker_StartStopsOnContextCancel(t *testing.T) {
	t.Parallel()

	worker := NewWorker(&memoryStore{}, NewMock(), 10*time.Millisecond, nil)

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

// TestWorker_PropagatesStoreErrors keeps the worker honest about failures that
// are not the provider's fault: a store that cannot be read has to surface,
// not look like a pass with nothing to do.
func TestWorker_PropagatesStoreErrors(t *testing.T) {
	t.Parallel()

	worker := NewWorker(&memoryStore{listErr: errors.New("database is gone")}, NewMock(), time.Hour, nil)
	if _, err := worker.EnrichOnce(context.Background()); err == nil {
		t.Error("a failing ListEntities should be reported")
	}

	store := &memoryStore{
		entities: []library.MediaEntity{newEntity("movie-1", library.MovieEntity, "Dune")},
		saveErr:  errors.New("disk full"),
	}
	worker = NewWorker(store, NewMock(), time.Hour, nil)
	if _, err := worker.EnrichOnce(context.Background()); err == nil {
		t.Error("a failing UpdateEntity should be reported")
	}
}

type flakyProvider struct {
	failures int
	calls    int
	provider Provider
}

func (f *flakyProvider) Name() string { return "flaky" }

func (f *flakyProvider) FetchMetadata(ctx context.Context, entity *library.MediaEntity) (*library.MetadataSet, error) {
	f.calls++
	if f.calls <= f.failures {
		return nil, errors.New("transient failure")
	}
	return f.provider.FetchMetadata(ctx, entity)
}

// TestWorker_KeepsGoingWhenAnEntityIsPruned is the regression test for L-18's
// first item.
//
// A scan running concurrently can remove an entity between the list a pass read
// and the write it makes, and UpdateEntity answers ErrNotFound for it. Aborting
// the whole pass because one entity of many went away means the rest are not
// enriched - and the next pass starts again from the beginning of a list that has
// changed again.
func TestWorker_KeepsGoingWhenAnEntityIsPruned(t *testing.T) {
	t.Parallel()

	store := &memoryStore{entities: []library.MediaEntity{
		newEntity("gone", library.MovieEntity, "Deleted Mid-Pass"),
		newEntity("kept", library.MovieEntity, "Still Here"),
	}}
	store.pruneOnSave = "gone"
	provider := &stubProvider{meta: &library.MetadataSet{Title: "Found"}}

	worker := NewWorker(store, provider, time.Hour, nil)
	result, err := worker.EnrichOnce(context.Background())
	if err != nil {
		t.Fatalf("a pruned entity failed the whole pass: %v", err)
	}

	if result.Skipped != 1 {
		t.Errorf("skipped = %d, want 1", result.Skipped)
	}
	if result.Enriched != 1 {
		t.Errorf("enriched = %d, want 1: the entity that still exists was not enriched "+
			"because another one had gone", result.Enriched)
	}
	if got := store.entity("kept"); got.Status != library.StatusComplete {
		t.Errorf("the surviving entity is %q, want it enriched", got.Status)
	}
}

// TestWorker_StopsOnARealStoreFailure guards the other direction: a store error
// that is not "this entity is gone" must still stop the pass, because continuing
// means trying every remaining entity against a database that is not answering.
func TestWorker_StopsOnARealStoreFailure(t *testing.T) {
	t.Parallel()

	store := &memoryStore{entities: []library.MediaEntity{
		newEntity("one", library.MovieEntity, "One"),
		newEntity("two", library.MovieEntity, "Two"),
	}}
	store.saveErr = errors.New("database is locked")
	provider := &stubProvider{meta: &library.MetadataSet{Title: "Found"}}

	worker := NewWorker(store, provider, time.Hour, nil)
	if _, err := worker.EnrichOnce(context.Background()); err == nil {
		t.Fatal("a store failure was reported as a completed pass")
	}
	if store.saved != 0 {
		t.Errorf("the store recorded %d saves despite failing on the first", store.saved)
	}
}
