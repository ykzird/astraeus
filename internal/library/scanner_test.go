package library

import (
	"context"
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

func mustLibraryAt(t *testing.T, repo Repository, root string, kind LibraryKind) *Library {
	t.Helper()

	lib := &Library{
		ID:        uuid.NewString(),
		Name:      "Test Library",
		Path:      root,
		Kind:      kind,
		CreatedAt: time.Now(),
	}
	if err := repo.CreateLibrary(context.Background(), lib); err != nil {
		t.Fatalf("creating library: %v", err)
	}
	return lib
}

func countObjects(t *testing.T, repo Repository, entities []MediaEntity) int {
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
		kind           LibraryKind
		relPath        string
		absPath        string
		wantContainers []string
		wantLeaf       string
		wantOK         bool
	}{
		{
			name:     "movie in a year-named folder",
			kind:     MoviesLibrary,
			relPath:  "Blade Runner 2049 (2017)/Blade Runner 2049 (2017) 1080p.mkv",
			absPath:  "/media/Blade Runner 2049 (2017)/Blade Runner 2049 (2017) 1080p.mkv",
			wantLeaf: "Movie:Blade Runner 2049",
			wantOK:   true,
		},
		{
			name:     "movie file at the library root",
			kind:     MoviesLibrary,
			relPath:  "Dune (2021).mp4",
			absPath:  "/media/Dune (2021).mp4",
			wantLeaf: "Movie:Dune",
			wantOK:   true,
		},
		{
			name:     "movie in a folder without a year uses the file name",
			kind:     MoviesLibrary,
			relPath:  "Action/Heat (1995).mkv",
			absPath:  "/media/Action/Heat (1995).mkv",
			wantLeaf: "Movie:Heat",
			wantOK:   true,
		},
		{
			name:           "episode in a series and season folder",
			kind:           ShowsLibrary,
			relPath:        "Breaking Bad/Season 02/Breaking Bad S02E05 - Breakage.mkv",
			absPath:        "/media/Breaking Bad/Season 02/Breaking Bad S02E05 - Breakage.mkv",
			wantContainers: []string{"Series:Breaking Bad", "Season:Season 2"},
			wantLeaf:       "Episode:S02E05 - Breakage",
			wantOK:         true,
		},
		{
			name:           "episode without a season folder",
			kind:           ShowsLibrary,
			relPath:        "The Wire/The.Wire.S03E07.mkv",
			absPath:        "/media/The Wire/The.Wire.S03E07.mkv",
			wantContainers: []string{"Series:The Wire", "Season:Season 3"},
			wantLeaf:       "Episode:S03E07",
			wantOK:         true,
		},
		{
			name:    "episode directly in the library root cannot be placed",
			kind:    ShowsLibrary,
			relPath: "S01E01.mkv",
			absPath: "/media/S01E01.mkv",
			wantOK:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			lib := &Library{Name: "Test", Kind: tt.kind}
			containers, leaf, ok := PlacementFor(lib, tt.relPath, tt.absPath)
			if ok != tt.wantOK {
				t.Fatalf("PlacementFor(%q) ok = %v, want %v", tt.relPath, ok, tt.wantOK)
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

	lib := mustLibraryAt(t, repo, root, MoviesLibrary)
	scanner := NewScanner(repo, newTestLogger())

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
		if entity.Status != StatusIncomplete {
			t.Errorf("entity %q status = %q, want %q", entity.Name, entity.Status, StatusIncomplete)
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

	lib := mustLibraryAt(t, repo, root, ShowsLibrary)
	scanner := NewScanner(repo, newTestLogger())

	if _, err := scanner.ScanLibrary(ctx, lib); err != nil {
		t.Fatalf("scan: %v", err)
	}

	entities, err := repo.ListEntitiesByLibrary(ctx, lib.ID)
	if err != nil {
		t.Fatalf("listing entities: %v", err)
	}

	byType := map[EntityType]int{}
	for _, e := range entities {
		byType[e.Type]++
	}
	if byType[SeriesEntity] != 2 {
		t.Errorf("series count = %d, want 2", byType[SeriesEntity])
	}
	if byType[SeasonEntity] != 2 {
		t.Errorf("season count = %d, want 2", byType[SeasonEntity])
	}
	if byType[EpisodeEntity] != 3 {
		t.Errorf("episode count = %d, want 3", byType[EpisodeEntity])
	}
	if got := countObjects(t, repo, entities); got != 3 {
		t.Errorf("object count = %d, want 3", got)
	}

	show, err := repo.FindEntity(ctx, lib.ID, nil, SeriesEntity, "Show")
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
	other, err := repo.FindEntity(ctx, lib.ID, nil, SeriesEntity, "Other Show")
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

	lib := mustLibraryAt(t, repo, root, ShowsLibrary)
	result, err := NewScanner(repo, newTestLogger()).ScanLibrary(ctx, lib)
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
	lib := &Library{
		ID:        uuid.NewString(),
		Name:      "Missing",
		Path:      filepath.Join(t.TempDir(), "does-not-exist"),
		Kind:      MoviesLibrary,
		CreatedAt: time.Now(),
	}

	if _, err := NewScanner(repo, newTestLogger()).ScanLibrary(context.Background(), lib); err == nil {
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

	lib := mustLibraryAt(t, repo, root, MoviesLibrary)
	result, err := NewScanner(repo, newTestLogger()).ScanLibrary(ctx, lib)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if result.FilesSeen != 1 {
		t.Errorf("files seen = %d, want 1 (sample and hidden dirs are ignored)", result.FilesSeen)
	}
}
