package sqlite

import (
	"context"
	"errors"
	"github.com/jok/astraeus-media/internal/library"
	"path/filepath"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"
)

// legacySchema is the schema the previous version of this project created. It
// has no libraries table, no name or library_id columns, and its timestamps
// were written using Go's default time formatting.
const legacySchema = `
CREATE TABLE media_entities (
	id TEXT PRIMARY KEY,
	parent_id TEXT,
	type TEXT NOT NULL,
	status TEXT NOT NULL,
	created_at DATETIME NOT NULL,
	updated_at DATETIME NOT NULL,
	metadata TEXT,
	FOREIGN KEY (parent_id) REFERENCES media_entities(id)
);
CREATE TABLE media_objects (
	id TEXT PRIMARY KEY,
	media_entity_id TEXT NOT NULL,
	file_path TEXT NOT NULL,
	size INTEGER NOT NULL,
	mime_type TEXT NOT NULL,
	created_at DATETIME NOT NULL
);
`

const legacyTimestamp = "2026-10-07 16:23:13.732481079 +0200 CEST m=+0.100313790"

// TestMigrate_UpgradesLegacyDatabase is the regression test for the crash that
// stopped the previous session: listing entities failed with
// `converting NULL to string is unsupported` because the metadata column is
// NULL for rows the scanner wrote.
func TestMigrate_UpgradesLegacyDatabase(t *testing.T) {
	t.Parallel()

	db, err := sqlx.Connect("sqlite", DSN(filepath.Join(t.TempDir(), "legacy.db")))
	if err != nil {
		t.Fatalf("connecting to legacy database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	db.MustExec(legacySchema)
	db.MustExec(
		`INSERT INTO media_entities (id, parent_id, type, status, created_at, updated_at, metadata)
		 VALUES (?, NULL, ?, ?, ?, ?, NULL)`,
		"entity-1", string(library.MovieEntity), string(library.StatusComplete), legacyTimestamp, legacyTimestamp)
	db.MustExec(
		`INSERT INTO media_objects (id, media_entity_id, file_path, size, mime_type, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"object-1", "entity-1", "/media/test_media/episode1.mkv", 0, "video/x-matroska", legacyTimestamp)

	repo := New(db)
	ctx := context.Background()

	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("migrating legacy database: %v", err)
	}

	entities, err := repo.ListEntities(ctx)
	if err != nil {
		t.Fatalf("listing entities after migration: %v", err)
	}
	if len(entities) != 1 {
		t.Fatalf("got %d entities, want 1", len(entities))
	}

	got := entities[0]
	if got.Name != "episode1" {
		t.Errorf("backfilled name = %q, want %q", got.Name, "episode1")
	}
	if got.Metadata != nil {
		t.Errorf("metadata = %+v, want nil for a never-enriched entity", got.Metadata)
	}
	if got.Status != library.StatusIncomplete {
		t.Errorf("status = %q, want %q: an entity without metadata is not complete",
			got.Status, library.StatusIncomplete)
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at was not parsed from the legacy timestamp format")
	}

	// Migrate must be safe to run repeatedly.
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
}

func TestListEntities_EntityWithoutMetadata(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	ctx := context.Background()
	lib := mustCreateLibrary(t, repo, "Movies", library.MoviesLibrary)
	created := mustCreateEntity(t, repo, lib.ID, nil, library.MovieEntity, "Dune")

	entities, err := repo.ListEntities(ctx)
	if err != nil {
		t.Fatalf("listing entities: %v", err)
	}
	if len(entities) != 1 {
		t.Fatalf("got %d entities, want 1", len(entities))
	}
	if entities[0].ID != created.ID {
		t.Errorf("entity id = %q, want %q", entities[0].ID, created.ID)
	}
	if entities[0].Metadata != nil {
		t.Errorf("metadata = %+v, want nil", entities[0].Metadata)
	}
}

func TestEntity_RoundTripsMetadata(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	ctx := context.Background()
	lib := mustCreateLibrary(t, repo, "Movies", library.MoviesLibrary)
	entity := mustCreateEntity(t, repo, lib.ID, nil, library.MovieEntity, "Dune")

	want := &library.MetadataSet{
		Title:        "Dune",
		Description:  "A duke's son leads desert warriors.",
		PosterPath:   "/poster.jpg",
		BackdropPath: "/backdrop.jpg",
		Provider:     "tmdb",
		Extra:        map[string]string{"year": "2021"},
	}
	entity.Metadata = want
	entity.Status = library.StatusComplete
	entity.UpdatedAt = time.Now().Truncate(time.Second)
	if err := repo.UpdateEntity(ctx, entity); err != nil {
		t.Fatalf("updating entity: %v", err)
	}

	got, err := repo.GetEntity(ctx, entity.ID)
	if err != nil {
		t.Fatalf("getting entity: %v", err)
	}
	if got.Metadata == nil {
		t.Fatal("metadata is nil after round trip")
	}
	if got.Metadata.Title != want.Title || got.Metadata.Provider != want.Provider {
		t.Errorf("metadata = %+v, want %+v", got.Metadata, want)
	}
	if got.Metadata.Extra["year"] != "2021" {
		t.Errorf("metadata extra year = %q, want %q", got.Metadata.Extra["year"], "2021")
	}
	if got.Status != library.StatusComplete {
		t.Errorf("status = %q, want %q", got.Status, library.StatusComplete)
	}
}

func TestGetEntity_NotFound(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)

	_, err := repo.GetEntity(context.Background(), "does-not-exist")
	if !errors.Is(err, library.ErrNotFound) {
		t.Fatalf("error = %v, want library.ErrNotFound", err)
	}
}

func TestUpdateEntity_NotFound(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	entity := &library.MediaEntity{
		ID:        "missing",
		Type:      library.MovieEntity,
		Name:      "Nope",
		Status:    library.StatusComplete,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}

	err := repo.UpdateEntity(context.Background(), entity)
	if !errors.Is(err, library.ErrNotFound) {
		t.Fatalf("error = %v, want library.ErrNotFound", err)
	}
}

// TestFindEntity_ScopesSeasonsToTheirSeries answers the design question that
// kept "Season 1" entities from being conflated across series: identity is
// scoped by library, parent and type, not by name alone.
func TestFindEntity_ScopesSeasonsToTheirSeries(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	ctx := context.Background()
	lib := mustCreateLibrary(t, repo, "Shows", library.ShowsLibrary)

	breakingBad := mustCreateEntity(t, repo, lib.ID, nil, library.SeriesEntity, "Breaking Bad")
	betterCallSaul := mustCreateEntity(t, repo, lib.ID, nil, library.SeriesEntity, "Better Call Saul")

	bbSeason := mustCreateEntity(t, repo, lib.ID, &breakingBad.ID, library.SeasonEntity, "Season 1")
	bcsSeason := mustCreateEntity(t, repo, lib.ID, &betterCallSaul.ID, library.SeasonEntity, "Season 1")

	gotBB, err := repo.FindEntity(ctx, lib.ID, &breakingBad.ID, library.SeasonEntity, "Season 1")
	if err != nil {
		t.Fatalf("finding Breaking Bad season: %v", err)
	}
	gotBCS, err := repo.FindEntity(ctx, lib.ID, &betterCallSaul.ID, library.SeasonEntity, "Season 1")
	if err != nil {
		t.Fatalf("finding Better Call Saul season: %v", err)
	}

	if gotBB.ID != bbSeason.ID {
		t.Errorf("Breaking Bad season id = %q, want %q", gotBB.ID, bbSeason.ID)
	}
	if gotBCS.ID != bcsSeason.ID {
		t.Errorf("Better Call Saul season id = %q, want %q", gotBCS.ID, bcsSeason.ID)
	}
	if gotBB.ID == gotBCS.ID {
		t.Fatal("two series' Season 1 resolved to the same entity")
	}
}

func TestFindEntity_TopLevelUsesNilParent(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	ctx := context.Background()
	lib := mustCreateLibrary(t, repo, "Shows", library.ShowsLibrary)
	series := mustCreateEntity(t, repo, lib.ID, nil, library.SeriesEntity, "The Wire")

	got, err := repo.FindEntity(ctx, lib.ID, nil, library.SeriesEntity, "The Wire")
	if err != nil {
		t.Fatalf("finding top-level series: %v", err)
	}
	if got.ID != series.ID {
		t.Errorf("entity id = %q, want %q", got.ID, series.ID)
	}
	if got.ParentID != nil {
		t.Errorf("parent id = %v, want nil", *got.ParentID)
	}

	_, err = repo.FindEntity(ctx, lib.ID, nil, library.SeriesEntity, "Missing Show")
	if !errors.Is(err, library.ErrNotFound) {
		t.Fatalf("error = %v, want library.ErrNotFound", err)
	}
}

func TestListChildren(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	ctx := context.Background()
	lib := mustCreateLibrary(t, repo, "Shows", library.ShowsLibrary)
	series := mustCreateEntity(t, repo, lib.ID, nil, library.SeriesEntity, "The Wire")
	mustCreateEntity(t, repo, lib.ID, &series.ID, library.SeasonEntity, "Season 2")
	mustCreateEntity(t, repo, lib.ID, &series.ID, library.SeasonEntity, "Season 1")

	children, err := repo.ListChildren(ctx, series.ID)
	if err != nil {
		t.Fatalf("listing children: %v", err)
	}
	if len(children) != 2 {
		t.Fatalf("got %d children, want 2", len(children))
	}
	if children[0].Name != "Season 1" || children[1].Name != "Season 2" {
		t.Errorf("children = %q, %q; want sorted Season 1, Season 2", children[0].Name, children[1].Name)
	}
}

func TestWithTx_RollsBackOnError(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	ctx := context.Background()
	lib := mustCreateLibrary(t, repo, "Movies", library.MoviesLibrary)

	sentinel := errors.New("boom")
	err := repo.WithTx(ctx, func(tx library.Repository) error {
		if err := tx.CreateEntity(ctx, &library.MediaEntity{
			ID:        "rolled-back",
			LibraryID: lib.ID,
			Type:      library.MovieEntity,
			Name:      "Rolled Back",
			Status:    library.StatusIncomplete,
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want %v", err, sentinel)
	}

	_, err = repo.GetEntity(ctx, "rolled-back")
	if !errors.Is(err, library.ErrNotFound) {
		t.Fatalf("entity survived a rolled-back transaction: %v", err)
	}
}

func TestLibraryLifecycle(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	ctx := context.Background()
	lib := mustCreateLibrary(t, repo, "Movies", library.MoviesLibrary)

	byID, err := repo.GetLibrary(ctx, lib.ID)
	if err != nil {
		t.Fatalf("getting library by id: %v", err)
	}
	if byID.Name != "Movies" || byID.Kind != library.MoviesLibrary {
		t.Errorf("library = %+v, want name Movies kind movies", byID)
	}

	byPath, err := repo.GetLibraryByPath(ctx, lib.Path)
	if err != nil {
		t.Fatalf("getting library by path: %v", err)
	}
	if byPath.ID != lib.ID {
		t.Errorf("library id = %q, want %q", byPath.ID, lib.ID)
	}

	all, err := repo.ListLibraries(ctx)
	if err != nil {
		t.Fatalf("listing libraries: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("got %d libraries, want 1", len(all))
	}

	// Deleting a library removes its entities and objects too.
	series := mustCreateEntity(t, repo, lib.ID, nil, library.MovieEntity, "Dune")
	if err := repo.CreateObject(ctx, &library.MediaObject{
		ID: "obj-1", MediaEntityID: series.ID, FilePath: "/media/dune.mkv",
		Size: 10, MimeType: "video/x-matroska", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("creating object: %v", err)
	}

	if err := repo.DeleteLibrary(ctx, lib.ID); err != nil {
		t.Fatalf("deleting library: %v", err)
	}

	if _, err := repo.GetLibrary(ctx, lib.ID); !errors.Is(err, library.ErrNotFound) {
		t.Errorf("library still present after delete: %v", err)
	}
	if _, err := repo.GetEntity(ctx, series.ID); !errors.Is(err, library.ErrNotFound) {
		t.Errorf("entity still present after library delete: %v", err)
	}
	if _, err := repo.GetObjectByPath(ctx, "/media/dune.mkv"); !errors.Is(err, library.ErrNotFound) {
		t.Errorf("object still present after library delete: %v", err)
	}
}

func TestGetObjectByPath_NotFound(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)

	_, err := repo.GetObjectByPath(context.Background(), "/nothing/here.mkv")
	if !errors.Is(err, library.ErrNotFound) {
		t.Fatalf("error = %v, want library.ErrNotFound", err)
	}
}

func TestParseTime_LegacyFormats(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "empty", input: ""},
		{name: "rfc3339", input: "2026-10-07T16:23:13.732481079Z"},
		{name: "legacy monotonic suffix", input: legacyTimestamp},
		{name: "space separated", input: "2026-10-07 16:23:13"},
		{name: "garbage", input: "not a time", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseTime(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseTime(%q) = %v, want error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTime(%q): %v", tt.input, err)
			}
			if tt.input != "" && got.IsZero() {
				t.Errorf("parseTime(%q) returned the zero time", tt.input)
			}
		})
	}
}
