package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ykzird/astraeus/internal/library"
)

// newTestRepo returns a migrated repository on a fresh database file. Each test
// gets its own file, so tests never share state and can run in parallel.
func newTestRepo(t *testing.T) *Repository {
	t.Helper()

	repo, err := Open(filepath.Join(t.TempDir(), "astraeus-test.db"))
	if err != nil {
		t.Fatalf("opening test database: %v", err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Errorf("closing test database: %v", err)
		}
	})
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatalf("migrating test database: %v", err)
	}
	return repo
}

func mustCreateLibrary(t *testing.T, repo library.Repository, name string, kind library.LibraryKind) *library.Library {
	t.Helper()

	lib := &library.Library{
		ID:        uuid.NewString(),
		Name:      name,
		Path:      filepath.Join(t.TempDir(), name),
		Kind:      kind,
		CreatedAt: time.Now(),
	}
	if err := repo.CreateLibrary(context.Background(), lib); err != nil {
		t.Fatalf("creating library %q: %v", name, err)
	}
	return lib
}

func mustCreateEntity(t *testing.T, repo library.Repository, libraryID string, parentID *string, entityType library.EntityType, name string) *library.MediaEntity {
	t.Helper()

	now := time.Now()
	entity := &library.MediaEntity{
		ID:        uuid.NewString(),
		LibraryID: libraryID,
		ParentID:  parentID,
		Type:      entityType,
		Name:      name,
		Status:    library.StatusIncomplete,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := repo.CreateEntity(context.Background(), entity); err != nil {
		t.Fatalf("creating %s %q: %v", entityType, name, err)
	}
	return entity
}
