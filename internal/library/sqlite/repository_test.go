package sqlite

import (
	"context"
	"errors"
	"github.com/ykzird/astraeus/internal/library"
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

// TestPlaybackProgress covers resumable playback's storage: one position per
// entity, replaced rather than accumulated, absent until the first report.
func TestPlaybackProgress(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	ctx := context.Background()
	lib := mustCreateLibrary(t, repo, "Progress", library.MoviesLibrary)
	entity := mustCreateEntity(t, repo, lib.ID, nil, library.MovieEntity, "Arrival (2016)")

	// Nothing played yet is not an error, and not a row.
	progress, err := repo.GetProgress(ctx, library.DefaultViewerID, entity.ID)
	if err != nil {
		t.Fatalf("GetProgress on an unplayed entity: %v", err)
	}
	if progress != nil {
		t.Fatalf("unplayed entity has progress %+v, want none", progress)
	}

	if err := repo.SaveProgress(ctx, &library.PlaybackProgress{
		EntityID: entity.ID, PositionSeconds: 90, DurationSeconds: 600,
	}); err != nil {
		t.Fatalf("SaveProgress: %v", err)
	}
	progress, err = repo.GetProgress(ctx, library.DefaultViewerID, entity.ID)
	if err != nil {
		t.Fatalf("GetProgress: %v", err)
	}
	if progress == nil || progress.PositionSeconds != 90 || progress.DurationSeconds != 600 {
		t.Fatalf("progress = %+v, want 90 of 600", progress)
	}
	if progress.UpdatedAt.IsZero() {
		t.Error("progress was stored without a timestamp")
	}

	// Reporting again replaces the position: a viewer has one place in a film,
	// not a list of places they have been.
	if err := repo.SaveProgress(ctx, &library.PlaybackProgress{
		EntityID: entity.ID, PositionSeconds: 300, DurationSeconds: 600,
	}); err != nil {
		t.Fatalf("SaveProgress (second report): %v", err)
	}
	progress, err = repo.GetProgress(ctx, library.DefaultViewerID, entity.ID)
	if err != nil {
		t.Fatalf("GetProgress after the second report: %v", err)
	}
	if progress.PositionSeconds != 300 {
		t.Errorf("position = %v, want the latest report 300", progress.PositionSeconds)
	}

	// Starting over forgets it entirely.
	if err := repo.DeleteProgress(ctx, library.DefaultViewerID, entity.ID); err != nil {
		t.Fatalf("DeleteProgress: %v", err)
	}
	if progress, err := repo.GetProgress(ctx, library.DefaultViewerID, entity.ID); err != nil || progress != nil {
		t.Errorf("after DeleteProgress: progress=%+v err=%v, want none", progress, err)
	}
}

// TestPlaybackProgress_GoesWithTheEntity is the reason the table has a cascading
// foreign key: a pruned entity must not leave a position behind pointing at a
// film that no longer exists.
func TestPlaybackProgress_GoesWithTheEntity(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	ctx := context.Background()
	lib := mustCreateLibrary(t, repo, "Progress", library.MoviesLibrary)
	entity := mustCreateEntity(t, repo, lib.ID, nil, library.MovieEntity, "A Film That Gets Deleted")

	if err := repo.SaveProgress(ctx, &library.PlaybackProgress{
		EntityID: entity.ID, PositionSeconds: 60, DurationSeconds: 600,
	}); err != nil {
		t.Fatalf("SaveProgress: %v", err)
	}

	if _, err := repo.db.ExecContext(ctx, `DELETE FROM media_entities WHERE id = ?`, entity.ID); err != nil {
		t.Fatalf("deleting the entity: %v", err)
	}

	var count int
	if err := repo.db.GetContext(ctx, &count, `SELECT COUNT(*) FROM playback_progress`); err != nil {
		t.Fatalf("counting progress rows: %v", err)
	}
	if count != 0 {
		t.Errorf("%d progress rows survived their entity", count)
	}
}

// TestPlaybackProgress_RejectsAnEntityThatDoesNotExist pins the other half of
// the foreign key: progress cannot be invented for something that is not there.
func TestPlaybackProgress_RejectsAnEntityThatDoesNotExist(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	err := repo.SaveProgress(context.Background(), &library.PlaybackProgress{
		EntityID: "00000000-0000-0000-0000-000000000000", PositionSeconds: 1,
	})
	if err == nil {
		t.Error("saving progress for a missing entity should be refused by the foreign key")
	}
}

// TestListProgress covers the continue-watching query: most recently watched
// first, each position travelling with its entity, and nothing that is over.
func TestListProgress(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	ctx := context.Background()
	lib := mustCreateLibrary(t, repo, "Progress", library.MoviesLibrary)

	first := mustCreateEntity(t, repo, lib.ID, nil, library.MovieEntity, "Watched Earlier")
	second := mustCreateEntity(t, repo, lib.ID, nil, library.MovieEntity, "Watched Later")
	finished := mustCreateEntity(t, repo, lib.ID, nil, library.MovieEntity, "Finished")

	if entries, err := repo.ListProgress(ctx, library.DefaultViewerID, 0); err != nil || len(entries) != 0 {
		t.Fatalf("nothing played should list nothing, got %+v err=%v", entries, err)
	}

	if err := repo.SaveProgress(ctx, &library.PlaybackProgress{
		EntityID: first.ID, PositionSeconds: 100, DurationSeconds: 600,
	}); err != nil {
		t.Fatalf("SaveProgress: %v", err)
	}
	// The order is by report time, so a second report has to be later; SQLite's
	// timestamps are stored with sub-second precision, but a test that depends on
	// clock resolution is a flaky test, so the wait is explicit.
	time.Sleep(10 * time.Millisecond)
	if err := repo.SaveProgress(ctx, &library.PlaybackProgress{
		EntityID: second.ID, PositionSeconds: 200, DurationSeconds: 600,
	}); err != nil {
		t.Fatalf("SaveProgress: %v", err)
	}

	// A finished position is excluded even though the row exists, because the
	// listing must apply the same rule the report path clears rows with.
	if _, err := repo.db.ExecContext(ctx,
		`INSERT INTO playback_progress (viewer_id, entity_id, position_seconds, duration_seconds, updated_at)
		 VALUES (?, ?, ?, ?, ?)`,
		library.DefaultViewerID, finished.ID, 599.0, 600.0, formatTime(time.Now())); err != nil {
		t.Fatalf("inserting a finished position: %v", err)
	}

	entries, err := repo.ListProgress(ctx, library.DefaultViewerID, 0)
	if err != nil {
		t.Fatalf("ListProgress: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %+v, want the two unfinished positions", entries)
	}
	if entries[0].Entity.ID != second.ID || entries[1].Entity.ID != first.ID {
		t.Errorf("order = %q then %q, want the most recently watched first",
			entries[0].Entity.Name, entries[1].Entity.Name)
	}
	if entries[0].Entity.Name != "Watched Later" || entries[0].Progress.PositionSeconds != 200 {
		t.Errorf("entry = %+v / %+v, want the entity with its position",
			entries[0].Entity, entries[0].Progress)
	}
	// The entity has to arrive complete: a listing that returned bare ids would
	// make the client fetch each one to render a row.
	if entries[0].Entity.LibraryID != lib.ID || entries[0].Entity.Type != library.MovieEntity {
		t.Errorf("the joined entity is incomplete: %+v", entries[0].Entity)
	}

	limited, err := repo.ListProgress(ctx, library.DefaultViewerID, 1)
	if err != nil {
		t.Fatalf("ListProgress with a limit: %v", err)
	}
	if len(limited) != 1 || limited[0].Entity.ID != second.ID {
		t.Errorf("limited listing = %+v, want just the most recent", limited)
	}
}

// TestPlaybackProgress_IsPerViewer is the storage half of the per-viewer rule:
// two viewers of one film hold two rows, and everything that touches progress
// touches one viewer's.
func TestPlaybackProgress_IsPerViewer(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	ctx := context.Background()
	lib := mustCreateLibrary(t, repo, "Progress", library.MoviesLibrary)
	entity := mustCreateEntity(t, repo, lib.ID, nil, library.MovieEntity, "Watched By Two People")

	const alice, bob = "alice@example.com", "bob@example.com"

	for viewer, position := range map[string]float64{alice: 100, bob: 200} {
		if err := repo.SaveProgress(ctx, &library.PlaybackProgress{
			ViewerID: viewer, EntityID: entity.ID, PositionSeconds: position, DurationSeconds: 600,
		}); err != nil {
			t.Fatalf("SaveProgress for %s: %v", viewer, err)
		}
	}

	// Neither viewer's report moved the other's place.
	for viewer, want := range map[string]float64{alice: 100, bob: 200} {
		progress, err := repo.GetProgress(ctx, viewer, entity.ID)
		if err != nil {
			t.Fatalf("GetProgress for %s: %v", viewer, err)
		}
		if progress == nil || progress.PositionSeconds != want || progress.ViewerID != viewer {
			t.Errorf("%s's progress = %+v, want %v owned by %s", viewer, progress, want, viewer)
		}
	}

	// Re-reporting replaces that viewer's row rather than adding one.
	if err := repo.SaveProgress(ctx, &library.PlaybackProgress{
		ViewerID: alice, EntityID: entity.ID, PositionSeconds: 300, DurationSeconds: 600,
	}); err != nil {
		t.Fatalf("SaveProgress (second report): %v", err)
	}
	if progress, _ := repo.GetProgress(ctx, alice, entity.ID); progress.PositionSeconds != 300 {
		t.Errorf("alice's second report did not replace her first: %+v", progress)
	}
	if progress, _ := repo.GetProgress(ctx, bob, entity.ID); progress.PositionSeconds != 200 {
		t.Errorf("alice's second report moved bob: %+v", progress)
	}

	// The continue-watching listing is one viewer's question.
	entries, err := repo.ListProgress(ctx, alice, 0)
	if err != nil {
		t.Fatalf("ListProgress: %v", err)
	}
	if len(entries) != 1 || entries[0].Progress.PositionSeconds != 300 {
		t.Errorf("alice's listing = %+v, want only her row", entries)
	}

	// Starting over clears the caller's row and leaves the other.
	if err := repo.DeleteProgress(ctx, alice, entity.ID); err != nil {
		t.Fatalf("DeleteProgress: %v", err)
	}
	if progress, _ := repo.GetProgress(ctx, bob, entity.ID); progress == nil {
		t.Error("deleting alice's progress deleted bob's row too")
	}
	if entries, _ := repo.ListProgress(ctx, alice, 0); len(entries) != 0 {
		t.Errorf("alice's listing after starting over = %+v, want none", entries)
	}
}

// TestPlaybackProgress_EmptyViewerIsTheLocalOne pins the naming rule: a caller
// with no identity reads and writes under the named default viewer, so an
// ungated server has a viewer id rather than a nameless row.
func TestPlaybackProgress_EmptyViewerIsTheLocalOne(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	ctx := context.Background()
	lib := mustCreateLibrary(t, repo, "Progress", library.MoviesLibrary)
	entity := mustCreateEntity(t, repo, lib.ID, nil, library.MovieEntity, "A Local Film")

	if err := repo.SaveProgress(ctx, &library.PlaybackProgress{
		EntityID: entity.ID, PositionSeconds: 60, DurationSeconds: 600,
	}); err != nil {
		t.Fatalf("SaveProgress: %v", err)
	}

	progress, err := repo.GetProgress(ctx, library.DefaultViewerID, entity.ID)
	if err != nil {
		t.Fatalf("GetProgress: %v", err)
	}
	if progress == nil || progress.PositionSeconds != 60 {
		t.Fatalf("the default viewer's progress = %+v, want the unnamed report", progress)
	}
	if progress.ViewerID != library.DefaultViewerID {
		t.Errorf("stored viewer = %q, want %q", progress.ViewerID, library.DefaultViewerID)
	}
}

// TestMigrate_WidensProgressToPerViewer is the regression test for the upgrade
// path: a database whose playback_progress predates the viewer column keeps its
// rows, claimed by the default viewer, and gains the wider key.
func TestMigrate_WidensProgressToPerViewer(t *testing.T) {
	t.Parallel()

	db, err := sqlx.Connect("sqlite", DSN(filepath.Join(t.TempDir(), "progress-legacy.db")))
	if err != nil {
		t.Fatalf("connecting to legacy database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// The old table exactly as the previous version created it, with a row.
	db.MustExec(`
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
		CREATE TABLE playback_progress (
			entity_id TEXT PRIMARY KEY,
			position_seconds REAL NOT NULL,
			duration_seconds REAL NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL,
			FOREIGN KEY (entity_id) REFERENCES media_entities(id) ON DELETE CASCADE
		);
	`)
	db.MustExec(
		`INSERT INTO media_entities (id, library_id, type, name, status, created_at, updated_at)
		 VALUES (?, '', ?, ?, ?, ?, ?)`,
		"entity-1", string(library.MovieEntity), "Half Watched",
		string(library.StatusIncomplete), legacyTimestamp, legacyTimestamp)
	db.MustExec(
		`INSERT INTO playback_progress (entity_id, position_seconds, duration_seconds, updated_at)
		 VALUES (?, ?, ?, ?)`,
		"entity-1", 1234.0, 7000.0, legacyTimestamp)

	repo := New(db)
	ctx := context.Background()
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("migrating the pre-viewer database: %v", err)
	}

	// The row survived and belongs to the single pre-viewer viewer.
	progress, err := repo.GetProgress(ctx, library.DefaultViewerID, "entity-1")
	if err != nil {
		t.Fatalf("GetProgress after the rebuild: %v", err)
	}
	if progress == nil || progress.PositionSeconds != 1234 || progress.DurationSeconds != 7000 {
		t.Fatalf("progress after the rebuild = %+v, want the old row kept", progress)
	}

	// And the key is now wide enough for a second viewer beside it.
	if err := repo.SaveProgress(ctx, &library.PlaybackProgress{
		ViewerID: "alice@example.com", EntityID: "entity-1", PositionSeconds: 10, DurationSeconds: 7000,
	}); err != nil {
		t.Fatalf("SaveProgress for a second viewer after the rebuild: %v", err)
	}
	if progress, _ := repo.GetProgress(ctx, library.DefaultViewerID, "entity-1"); progress.PositionSeconds != 1234 {
		t.Errorf("the second viewer's report moved the first's: %+v", progress)
	}

	// The legacy table is gone, not left behind as a second copy.
	var leftover int
	if err := db.GetContext(ctx, &leftover,
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'playback_progress_pre_viewer'`); err != nil {
		t.Fatalf("looking for the pre-rebuild table: %v", err)
	}
	if leftover != 0 {
		t.Error("the pre-viewer table was left behind by the rebuild")
	}

	// Running it again is a no-op rather than a second rebuild.
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("re-running Migrate: %v", err)
	}
	if progress, _ := repo.GetProgress(ctx, library.DefaultViewerID, "entity-1"); progress == nil {
		t.Error("the second Migrate lost the migrated row")
	}
}
