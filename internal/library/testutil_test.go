package library

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

// Test helpers shared by the library package tests.

func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestRepo returns a repository backed by a fresh database file. The file
// lives in t.TempDir so tests never share state.
func newTestRepo(t *testing.T) *SQLiteRepository {
	t.Helper()

	db, err := sqlx.Connect("sqlite", SQLiteDSN(filepath.Join(t.TempDir(), "astraeus-test.db")))
	if err != nil {
		t.Fatalf("connecting to test database: %v", err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("closing test database: %v", err)
		}
	})

	repo := NewSQLiteRepository(db)
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatalf("migrating test database: %v", err)
	}
	return repo
}

func mustCreateLibrary(t *testing.T, repo Repository, name string, kind LibraryKind) *Library {
	t.Helper()

	lib := &Library{
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

func mustCreateEntity(t *testing.T, repo Repository, libraryID string, parentID *string, entityType EntityType, name string) *MediaEntity {
	t.Helper()

	now := time.Now()
	entity := &MediaEntity{
		ID:        uuid.NewString(),
		LibraryID: libraryID,
		ParentID:  parentID,
		Type:      entityType,
		Name:      name,
		Status:    StatusIncomplete,
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := repo.CreateEntity(context.Background(), entity); err != nil {
		t.Fatalf("creating %s %q: %v", entityType, name, err)
	}
	return entity
}
