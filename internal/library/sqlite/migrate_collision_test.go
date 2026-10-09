package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/jmoiron/sqlx"

	"github.com/ykzird/astraeus/internal/library"
)

// duplicateNameLegacySchema is the entities table as an older version wrote it: no identity
// column, and every legacy row sharing library_id=” with a NULL parent.
const duplicateNameLegacySchema = `
	CREATE TABLE media_entities (
		id TEXT PRIMARY KEY,
		library_id TEXT NOT NULL DEFAULT '',
		parent_id TEXT,
		type TEXT NOT NULL,
		name TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		metadata TEXT
	);
	CREATE TABLE media_objects (
		id TEXT PRIMARY KEY,
		media_entity_id TEXT NOT NULL,
		file_path TEXT NOT NULL,
		size INTEGER NOT NULL,
		mime_type TEXT NOT NULL,
		created_at TEXT NOT NULL,
		FOREIGN KEY (media_entity_id) REFERENCES media_entities(id)
	);
`

const duplicateNameTimestamp = "2024-01-01T00:00:00Z"

// TestMigrate_SurvivesDuplicateEntityNames is the regression test for L-9.
//
// A legacy database has no library_id and no parent, so two files that share a
// base name - /a/Film.mkv and /b/Film.mkv - became two rows that are identical
// under the identity index. Migrate ran every step before the index and then
// failed on it with "UNIQUE constraint failed", so the process exited on a
// database it had already half-changed.
func TestMigrate_SurvivesDuplicateEntityNames(t *testing.T) {
	t.Parallel()

	db, err := sqlx.Connect("sqlite", DSN(filepath.Join(t.TempDir(), "duplicate-names.db")))
	if err != nil {
		t.Fatalf("connecting to the legacy database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(duplicateNameLegacySchema); err != nil {
		t.Fatalf("creating the legacy schema: %v", err)
	}

	// Two entities with the same name and no distinguishing parent or library,
	// each with a file behind it. This is the shape that collided.
	for i, path := range []string{"/media/a/Film.mkv", "/media/b/Film.mkv"} {
		id := "entity-" + string(rune('1'+i))
		if _, err := db.Exec(
			`INSERT INTO media_entities (id, library_id, type, name, status, created_at, updated_at)
			 VALUES (?, '', ?, ?, ?, ?, ?)`,
			id, string(library.MovieEntity), "Film", string(library.StatusIncomplete),
			duplicateNameTimestamp, duplicateNameTimestamp); err != nil {
			t.Fatalf("inserting entity %s: %v", id, err)
		}
		if _, err := db.Exec(
			`INSERT INTO media_objects (id, media_entity_id, file_path, size, mime_type, created_at)
			 VALUES (?, ?, ?, 100, 'video/x-matroska', ?)`,
			"object-"+string(rune('1'+i)), id, path, duplicateNameTimestamp); err != nil {
			t.Fatalf("inserting object for %s: %v", id, err)
		}
	}

	repo := New(db)
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatalf("migrating a database with duplicate entity names failed, which stops the "+
			"server from starting on a database it has partly changed: %v", err)
	}

	// Both entities survived. Merging them would have thrown a film away.
	var entities int
	if err := db.Get(&entities, `SELECT COUNT(*) FROM media_entities`); err != nil {
		t.Fatalf("counting entities: %v", err)
	}
	if entities != 2 {
		t.Errorf("got %d entities after the migration, want 2: a duplicate name is not "+
			"a duplicate entity", entities)
	}

	// And the identity index is in force, with distinct identities.
	var identities int
	if err := db.Get(&identities, `SELECT COUNT(DISTINCT identity) FROM media_entities`); err != nil {
		t.Fatalf("counting identities: %v", err)
	}
	if identities != 2 {
		t.Errorf("got %d distinct identities, want 2: the migration has to disambiguate "+
			"the rows it cannot merge", identities)
	}
}

// TestMigrate_IsAtomic covers the other half of L-9: a migration step that fails
// must leave the database as it was rather than half-changed.
//
// The failure is provoked by a trigger that rejects one of the migration's own
// statements, which is a stand-in for any real failure - a constraint, a full
// disk, a kill. The assertion is that the schema is unchanged afterwards.
func TestMigrate_IsAtomic(t *testing.T) {
	t.Parallel()

	db, err := sqlx.Connect("sqlite", DSN(filepath.Join(t.TempDir(), "atomic.db")))
	if err != nil {
		t.Fatalf("connecting: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.Exec(duplicateNameLegacySchema); err != nil {
		t.Fatalf("creating the legacy schema: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO media_entities (id, library_id, type, name, status, created_at, updated_at)
		 VALUES ('entity-1', '', ?, 'Half Watched', ?, ?, ?)`,
		string(library.MovieEntity), string(library.StatusIncomplete),
		duplicateNameTimestamp, duplicateNameTimestamp); err != nil {
		t.Fatalf("inserting the legacy row: %v", err)
	}

	// A status the repair step would change, and a trigger that refuses the
	// change. The repair runs late in the migration, so a non-transactional
	// Migrate has already committed everything before it.
	if _, err := db.Exec(
		`CREATE TRIGGER refuse_status_repair BEFORE UPDATE ON media_entities
		 BEGIN SELECT RAISE(ABORT, 'the migration is not allowed to finish'); END`); err != nil {
		t.Fatalf("creating the trigger: %v", err)
	}

	repo := New(db)
	if err := repo.Migrate(context.Background()); err == nil {
		t.Fatal("the migration should have failed against the trigger")
	}

	// Whatever it did, it must not have left the schema half-built.
	var columns int
	if err := db.Get(&columns,
		`SELECT COUNT(*) FROM pragma_table_info('media_entities') WHERE name = 'identity'`); err != nil {
		t.Fatalf("reading the schema: %v", err)
	}
	if columns != 0 {
		t.Error("the migration added a column and then failed, leaving the database " +
			"half-changed; a migration has to be all or nothing")
	}
}
