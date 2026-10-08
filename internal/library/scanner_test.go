package library_test

import (
	"context"
	"github.com/jok/astraeus-media/internal/library"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating directory for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func mustLibraryAt(t *testing.T, repo library.Repository, root string, kind library.LibraryKind) *library.Library {
	t.Helper()

	lib := &library.Library{
		ID:        uuid.NewString(),
		Name:      "Test library.Library",
		Path:      root,
		Kind:      kind,
		CreatedAt: time.Now(),
	}
	if err := repo.CreateLibrary(context.Background(), lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}
	return lib
}

func countObjects(t *testing.T, repo library.Repository, entities []library.MediaEntity) int {
	t.Helper()

	total := 0
	for _, entity := range entities {
		objects, err := repo.GetObjectsByEntity(context.Background(), entity.ID)
		if err != nil {
			t.Fatalf("getting objects for entity %s: %v", entity.ID, err)
		}
		total += len(objects)
	}
	return total
}

func TestPlacementFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		kind           library.LibraryKind
		relPath        string
		absPath        string
		wantContainers []string
		wantLeaf       string
		wantOK         bool
	}{
		{
			name:     "movie in a year-named folder",
			kind:     library.MoviesLibrary,
			relPath:  "Blade Runner 2049 (2017)/Blade Runner 2049 (2017) 1080p.mkv",
			absPath:  "/media/Blade Runner 2049 (2017)/Blade Runner 2049 (2017) 1080p.mkv",
			wantLeaf: "Movie:Blade Runner 2049",
			wantOK:   true,
		},
		{
			name:     "movie file at the library root",
			kind:     library.MoviesLibrary,
			relPath:  "Dune (2021).mp4",
			absPath:  "/media/Dune (2021).mp4",
			wantLeaf: "Movie:Dune",
			wantOK:   true,
		},
		{
			name:     "movie in a folder without a year uses the file name",
			kind:     library.MoviesLibrary,
			relPath:  "Action/Heat (1995).mkv",
			absPath:  "/media/Action/Heat (1995).mkv",
			wantLeaf: "Movie:Heat",
			wantOK:   true,
		},
		{
			name:           "episode in a series and season folder",
			kind:           library.ShowsLibrary,
			relPath:        "Breaking Bad/Season 02/Breaking Bad S02E05 - Breakage.mkv",
			absPath:        "/media/Breaking Bad/Season 02/Breaking Bad S02E05 - Breakage.mkv",
			wantContainers: []string{"Series:Breaking Bad", "Season:Season 2"},
			wantLeaf:       "Episode:S02E05 - Breakage",
			wantOK:         true,
		},
		{
			name:           "episode without a season folder",
			kind:           library.ShowsLibrary,
			relPath:        "The Wire/The.Wire.S03E07.mkv",
			absPath:        "/media/The Wire/The.Wire.S03E07.mkv",
			wantContainers: []string{"Series:The Wire", "Season:Season 3"},
			wantLeaf:       "Episode:S03E07",
			wantOK:         true,
		},
		{
			name:    "episode directly in the library root cannot be placed",
			kind:    library.ShowsLibrary,
			relPath: "S01E01.mkv",
			absPath: "/media/S01E01.mkv",
			wantOK:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			lib := &library.Library{Name: "Test", Kind: tt.kind}
			containers, leaf, ok := library.PlacementFor(lib, tt.relPath, tt.absPath)
			if ok != tt.wantOK {
				t.Fatalf("library.PlacementFor(%q) ok = %v, want %v", tt.relPath, ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}

			var gotContainers []string
			for _, c := range containers {
				gotContainers = append(gotContainers, string(c.Type)+":"+c.Name)
			}
			if len(gotContainers) != len(tt.wantContainers) {
				t.Fatalf("containers = %v, want %v", gotContainers, tt.wantContainers)
			}
			for i := range gotContainers {
				if gotContainers[i] != tt.wantContainers[i] {
					t.Errorf("container[%d] = %q, want %q", i, gotContainers[i], tt.wantContainers[i])
				}
			}
			if got := string(leaf.Type) + ":" + leaf.Name; got != tt.wantLeaf {
				t.Errorf("leaf = %q, want %q", got, tt.wantLeaf)
			}
		})
	}
}

func TestScanner_ScanLibrary_MoviesIsIdempotent(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	ctx := context.Background()
	root := t.TempDir()

	movieA := filepath.Join(root, "Blade Runner 2049 (2017)", "Blade Runner 2049 (2017).mkv")
	movieB := filepath.Join(root, "Dune (2021).mp4")
	writeFile(t, movieA, "aaaa")
	writeFile(t, movieB, "bb")
	writeFile(t, filepath.Join(root, "notes.txt"), "not media")

	lib := mustLibraryAt(t, repo, root, library.MoviesLibrary)
	scanner := library.NewScanner(repo, newTestLogger())

	first, err := scanner.ScanLibrary(ctx, lib)
	if err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if first.FilesSeen != 2 {
		t.Errorf("files seen = %d, want 2", first.FilesSeen)
	}
	if first.EntitiesCreated != 2 {
		t.Errorf("entities created = %d, want 2", first.EntitiesCreated)
	}
	if first.ObjectsCreated != 2 {
		t.Errorf("objects created = %d, want 2", first.ObjectsCreated)
	}

	// Scanning again must not duplicate anything.
	second, err := scanner.ScanLibrary(ctx, lib)
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if second.EntitiesCreated != 0 || second.ObjectsCreated != 0 {
		t.Errorf("second scan created entities=%d objects=%d, want 0 and 0",
			second.EntitiesCreated, second.ObjectsCreated)
	}
	if second.EntitiesReused != 2 {
		t.Errorf("entities reused = %d, want 2", second.EntitiesReused)
	}

	entities, err := repo.ListEntitiesByLibrary(ctx, lib.ID)
	if err != nil {
		t.Fatalf("listing entities: %v", err)
	}
	if len(entities) != 2 {
		t.Fatalf("got %d entities after two scans, want 2", len(entities))
	}
	if got := countObjects(t, repo, entities); got != 2 {
		t.Fatalf("got %d objects after two scans, want 2", got)
	}

	// A changed file size is picked up without creating a new row.
	writeFile(t, movieB, "bbbbbbbbbb")
	third, err := scanner.ScanLibrary(ctx, lib)
	if err != nil {
		t.Fatalf("third scan: %v", err)
	}
	if third.ObjectsUpdated != 1 || third.ObjectsCreated != 0 {
		t.Errorf("third scan updated=%d created=%d, want 1 and 0", third.ObjectsUpdated, third.ObjectsCreated)
	}

	entities, err = repo.ListEntitiesByLibrary(ctx, lib.ID)
	if err != nil {
		t.Fatalf("listing entities: %v", err)
	}
	if got := countObjects(t, repo, entities); got != 2 {
		t.Errorf("got %d objects after a resize, want 2", got)
	}

	// Entities start out incomplete so the administrator can see what needs
	// metadata.
	for _, entity := range entities {
		if entity.Status != library.StatusIncomplete {
			t.Errorf("entity %q status = %q, want %q", entity.Name, entity.Status, library.StatusIncomplete)
		}
	}
}

func TestScanner_ScanLibrary_ShowsBuildsHierarchy(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	ctx := context.Background()
	root := t.TempDir()

	writeFile(t, filepath.Join(root, "Show", "Season 01", "Show.S01E01.mkv"), "1")
	writeFile(t, filepath.Join(root, "Show", "Season 01", "Show.S01E02 - Second.mkv"), "2")
	writeFile(t, filepath.Join(root, "Other Show", "Season 01", "Other.Show.S01E01.mkv"), "3")

	lib := mustLibraryAt(t, repo, root, library.ShowsLibrary)
	scanner := library.NewScanner(repo, newTestLogger())

	if _, err := scanner.ScanLibrary(ctx, lib); err != nil {
		t.Fatalf("scan: %v", err)
	}

	entities, err := repo.ListEntitiesByLibrary(ctx, lib.ID)
	if err != nil {
		t.Fatalf("listing entities: %v", err)
	}

	byType := map[library.EntityType]int{}
	for _, e := range entities {
		byType[e.Type]++
	}
	if byType[library.SeriesEntity] != 2 {
		t.Errorf("series count = %d, want 2", byType[library.SeriesEntity])
	}
	if byType[library.SeasonEntity] != 2 {
		t.Errorf("season count = %d, want 2", byType[library.SeasonEntity])
	}
	if byType[library.EpisodeEntity] != 3 {
		t.Errorf("episode count = %d, want 3", byType[library.EpisodeEntity])
	}
	if got := countObjects(t, repo, entities); got != 3 {
		t.Errorf("object count = %d, want 3", got)
	}

	show, err := repo.FindEntity(ctx, lib.ID, nil, library.SeriesEntity, "Show")
	if err != nil {
		t.Fatalf("finding series: %v", err)
	}
	seasons, err := repo.ListChildren(ctx, show.ID)
	if err != nil {
		t.Fatalf("listing seasons: %v", err)
	}
	if len(seasons) != 1 || seasons[0].Name != "Season 1" {
		t.Fatalf("seasons = %+v, want one Season 1", seasons)
	}
	if seasons[0].ParentID == nil || *seasons[0].ParentID != show.ID {
		t.Errorf("season parent = %v, want %s", seasons[0].ParentID, show.ID)
	}

	episodes, err := repo.ListChildren(ctx, seasons[0].ID)
	if err != nil {
		t.Fatalf("listing episodes: %v", err)
	}
	if len(episodes) != 2 {
		t.Fatalf("got %d episodes, want 2", len(episodes))
	}
	if episodes[0].Name != "S01E01" || episodes[1].Name != "S01E02 - Second" {
		t.Errorf("episode names = %q, %q; want S01E01 and S01E02 - Second",
			episodes[0].Name, episodes[1].Name)
	}
	for _, episode := range episodes {
		if episode.ParentID == nil || *episode.ParentID != seasons[0].ID {
			t.Errorf("episode %q parent = %v, want %s", episode.Name, episode.ParentID, seasons[0].ID)
		}
		if episode.Metadata == nil || episode.Metadata.Extra["season"] != "1" {
			t.Errorf("episode %q is missing its season metadata: %+v", episode.Name, episode.Metadata)
		}
	}

	// The two series each keep their own Season 1.
	other, err := repo.FindEntity(ctx, lib.ID, nil, library.SeriesEntity, "Other Show")
	if err != nil {
		t.Fatalf("finding second series: %v", err)
	}
	otherSeasons, err := repo.ListChildren(ctx, other.ID)
	if err != nil {
		t.Fatalf("listing second series seasons: %v", err)
	}
	if len(otherSeasons) != 1 {
		t.Fatalf("got %d seasons for the second series, want 1", len(otherSeasons))
	}
	if otherSeasons[0].ID == seasons[0].ID {
		t.Error("both series share the same Season 1 entity")
	}
}

func TestScanner_ScanLibrary_WarnsOnUnplaceableFile(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "S01E01.mkv"), "loose")

	lib := mustLibraryAt(t, repo, root, library.ShowsLibrary)
	result, err := library.NewScanner(repo, newTestLogger()).ScanLibrary(ctx, lib)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}

	if result.FilesSeen != 1 {
		t.Errorf("files seen = %d, want 1", result.FilesSeen)
	}
	if len(result.Warnings) != 1 {
		t.Errorf("warnings = %v, want exactly one", result.Warnings)
	}
	entities, err := repo.ListEntitiesByLibrary(ctx, lib.ID)
	if err != nil {
		t.Fatalf("listing entities: %v", err)
	}
	if len(entities) != 0 {
		t.Errorf("got %d entities, want none for an unplaceable file", len(entities))
	}
}

func TestScanner_ScanLibrary_MissingDirectory(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	lib := &library.Library{
		ID:        uuid.NewString(),
		Name:      "Missing",
		Path:      filepath.Join(t.TempDir(), "does-not-exist"),
		Kind:      library.MoviesLibrary,
		CreatedAt: time.Now(),
	}

	if _, err := library.NewScanner(repo, newTestLogger()).ScanLibrary(context.Background(), lib); err == nil {
		t.Fatal("expected an error for a missing library directory")
	}
}

func TestScanner_ScanLibrary_SkipsIgnoredDirectories(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	ctx := context.Background()
	root := t.TempDir()

	writeFile(t, filepath.Join(root, "Dune (2021).mkv"), "d")
	writeFile(t, filepath.Join(root, "Sample", "sample.mkv"), "s")
	writeFile(t, filepath.Join(root, ".hidden", "secret.mkv"), "h")

	lib := mustLibraryAt(t, repo, root, library.MoviesLibrary)
	result, err := library.NewScanner(repo, newTestLogger()).ScanLibrary(ctx, lib)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if result.FilesSeen != 1 {
		t.Errorf("files seen = %d, want 1 (sample and hidden dirs are ignored)", result.FilesSeen)
	}
}

// TestScanner_ScanLibrary_CountsDistinctEntities pins the accounting: a series
// and season shared by many episodes are single entities, not one per lookup.
func TestScanner_ScanLibrary_CountsDistinctEntities(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Show", "Season 01", "Show.S01E01.mkv"), "1")
	writeFile(t, filepath.Join(root, "Show", "Season 01", "Show.S01E02.mkv"), "2")
	writeFile(t, filepath.Join(root, "Show", "Season 01", "Show.S01E03.mkv"), "3")

	lib := mustLibraryAt(t, repo, root, library.ShowsLibrary)
	scanner := library.NewScanner(repo, newTestLogger())

	// One series plus one season plus three episodes. The containers are looked
	// up three times each, but they are two entities.
	first, err := scanner.ScanLibrary(ctx, lib)
	if err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if first.FilesSeen != 3 {
		t.Errorf("files seen = %d, want 3", first.FilesSeen)
	}
	if first.EntitiesCreated != 5 {
		t.Errorf("entities created = %d, want 5 (1 series + 1 season + 3 episodes)", first.EntitiesCreated)
	}
	if first.EntitiesReused != 0 {
		t.Errorf("entities reused = %d, want 0 (nothing existed before this scan)", first.EntitiesReused)
	}

	second, err := scanner.ScanLibrary(ctx, lib)
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if second.EntitiesCreated != 0 {
		t.Errorf("entities created = %d on a repeat scan, want 0", second.EntitiesCreated)
	}
	if second.EntitiesReused != 5 {
		t.Errorf("entities reused = %d on a repeat scan, want 5 (nine lookups, five entities)",
			second.EntitiesReused)
	}

	// A new episode is the only new entity; the containers it shares are still
	// one reuse each.
	writeFile(t, filepath.Join(root, "Show", "Season 01", "Show.S01E04.mkv"), "4")
	third, err := scanner.ScanLibrary(ctx, lib)
	if err != nil {
		t.Fatalf("third scan: %v", err)
	}
	if third.EntitiesCreated != 1 {
		t.Errorf("entities created = %d, want 1 (the new episode)", third.EntitiesCreated)
	}
	if third.EntitiesReused != 5 {
		t.Errorf("entities reused = %d, want 5", third.EntitiesReused)
	}
}

// TestScanner_PrunesFilesThatAreGone covers ghost entries: a file removed from
// disk must stop being listed, and entities left holding nothing must go with
// it, upwards through the hierarchy.
func TestScanner_PrunesFilesThatAreGone(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	seasonDir := filepath.Join(root, "Astraeus Show", "Season 1")
	first := filepath.Join(seasonDir, "S01E01 - Pilot.mkv")
	second := filepath.Join(seasonDir, "S01E02 - Descent.mkv")
	writeFile(t, first, "video")
	writeFile(t, second, "video")

	repo := newTestRepo(t)
	lib := mustLibraryAt(t, repo, root, library.ShowsLibrary)
	scanner := library.NewScanner(repo, newTestLogger())

	initial, err := scanner.ScanLibrary(context.Background(), lib)
	if err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if initial.FilesSeen != 2 {
		t.Fatalf("files seen = %d, want 2", initial.FilesSeen)
	}
	if initial.ObjectsPruned != 0 || initial.EntitiesPruned != 0 {
		t.Errorf("a first scan must not prune anything: %+v", initial)
	}

	// Remove one episode and rescan.
	if err := os.Remove(second); err != nil {
		t.Fatalf("removing the file: %v", err)
	}
	after, err := scanner.ScanLibrary(context.Background(), lib)
	if err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if after.ObjectsPruned != 1 {
		t.Errorf("objects pruned = %d, want 1", after.ObjectsPruned)
	}

	entities, err := repo.ListEntitiesByLibrary(context.Background(), lib.ID)
	if err != nil {
		t.Fatalf("listing entities: %v", err)
	}
	for _, entity := range entities {
		if entity.Name == "S01E02 - Descent" {
			t.Error("the episode for a deleted file is still listed")
		}
	}
	if !hasEntityNamed(entities, "Astraeus Show") {
		t.Error("the series was pruned even though an episode remains")
	}

	// Remove the last file: the whole hierarchy should go.
	if err := os.Remove(first); err != nil {
		t.Fatalf("removing the last file: %v", err)
	}
	// The library now looks empty, and that is the ambiguous case - so a prune
	// here is deliberately refused. Deleting the files AND an entity keeps the
	// scan non-empty while still leaving something to prune.
	writeFile(t, filepath.Join(root, "Astraeus Show", "Season 1", "S01E03 - New.mkv"), "video")
	if _, err := scanner.ScanLibrary(context.Background(), lib); err != nil {
		t.Fatalf("third scan: %v", err)
	}

	entities, err = repo.ListEntitiesByLibrary(context.Background(), lib.ID)
	if err != nil {
		t.Fatalf("listing entities: %v", err)
	}
	if hasEntityNamed(entities, "S01E01 - Pilot") {
		t.Error("the deleted episode survived a scan that saw other files")
	}
	if !hasEntityNamed(entities, "S01E03 - New") {
		t.Error("the new episode was not created")
	}
}

// TestScanner_DoesNotPruneWhenTheScanIsBlind is the guard that matters most. An
// unmounted drive or an unreadable tree must never be mistaken for a user
// deleting everything: silently emptying a library is far worse than leaving a
// ghost entry behind.
func TestScanner_DoesNotPruneWhenTheScanIsBlind(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Astraeus Show", "Season 1", "S01E01 - Pilot.mkv"), "video")

	repo := newTestRepo(t)
	lib := mustLibraryAt(t, repo, root, library.ShowsLibrary)
	scanner := library.NewScanner(repo, newTestLogger())

	if _, err := scanner.ScanLibrary(context.Background(), lib); err != nil {
		t.Fatalf("first scan: %v", err)
	}

	before, err := repo.ListEntitiesByLibrary(context.Background(), lib.ID)
	if err != nil {
		t.Fatalf("listing entities: %v", err)
	}
	if len(before) == 0 {
		t.Fatal("the first scan created nothing, so this test proves nothing")
	}

	// Every file disappears at once - the shape of an unmounted drive.
	if err := os.RemoveAll(filepath.Join(root, "Astraeus Show")); err != nil {
		t.Fatalf("emptying the library: %v", err)
	}

	result, err := scanner.ScanLibrary(context.Background(), lib)
	if err != nil {
		t.Fatalf("scan of the emptied path: %v", err)
	}
	if result.FilesSeen != 0 {
		t.Fatalf("files seen = %d, want 0", result.FilesSeen)
	}
	if result.ObjectsPruned != 0 || result.EntitiesPruned != 0 {
		t.Errorf("a blind scan pruned data: %+v", result)
	}

	after, err := repo.ListEntitiesByLibrary(context.Background(), lib.ID)
	if err != nil {
		t.Fatalf("listing entities: %v", err)
	}
	if len(after) != len(before) {
		t.Errorf("entities went from %d to %d: an empty scan must not delete a library",
			len(before), len(after))
	}
}

// TestDeleteLibrary_WithHierarchyAndForeignKeys covers a path that only became
// reachable when foreign keys were switched on: objects reference entities and
// entities reference their parent, so a deletion order that removes a parent
// first would now fail outright.
func TestDeleteLibrary_WithHierarchyAndForeignKeys(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	ctx := context.Background()
	lib := mustCreateLibrary(t, repo, "Hierarchy", library.ShowsLibrary)

	series := mustCreateEntity(t, repo, lib.ID, nil, library.SeriesEntity, "Show")
	season := mustCreateEntity(t, repo, lib.ID, &series.ID, library.SeasonEntity, "Season 1")
	episode := mustCreateEntity(t, repo, lib.ID, &season.ID, library.EpisodeEntity, "Episode 1")

	if err := repo.CreateObject(ctx, &library.MediaObject{
		ID:            uuid.NewString(),
		MediaEntityID: episode.ID,
		FilePath:      filepath.Join(t.TempDir(), "s01e01.mkv"),
		Size:          10,
		MimeType:      "video/x-matroska",
		CreatedAt:     time.Now(),
	}); err != nil {
		t.Fatalf("creating object: %v", err)
	}

	if err := repo.DeleteLibrary(ctx, lib.ID); err != nil {
		t.Fatalf("deleting a hierarchical library: %v", err)
	}
}

// hasEntityNamed reports whether any entity carries this name.
func hasEntityNamed(entities []library.MediaEntity, name string) bool {
	for _, entity := range entities {
		if entity.Name == name {
			return true
		}
	}
	return false
}
