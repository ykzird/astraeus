package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/ykzird/astraeus/internal/library"
)

// TestUpdateEntity_StoresNoMetadataAsNull is L-19's first nit.
//
// The repair migration asks for entities that are Complete with no metadata, and it
// can only see NULL. Entities were written with an empty string instead, so the
// repair matched legacy rows only and the condition it exists for was never met
// again: a Complete entity with no metadata stayed Complete and was never picked up
// by the worker, which is the state the repair exists to undo.
func TestUpdateEntity_StoresNoMetadataAsNull(t *testing.T) {
	t.Parallel()

	repo := newTestRepo(t)
	ctx := context.Background()

	lib := mustCreateLibrary(t, repo, "Films", library.MoviesLibrary)
	entity := &library.MediaEntity{
		ID: "e1", LibraryID: lib.ID, Type: library.MovieEntity, Name: "Dune",
		Identity: "dune", Status: library.StatusComplete,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := repo.CreateEntity(ctx, entity); err != nil {
		t.Fatalf("writing the entity: %v", err)
	}

	// Read the column directly: the domain type cannot tell NULL from ''.
	var metadata any
	if err := repo.db.QueryRowContext(ctx,
		`SELECT metadata FROM media_entities WHERE id = ?`, "e1").Scan(&metadata); err != nil {
		t.Fatalf("reading the metadata column: %v", err)
	}
	if metadata != nil {
		t.Errorf("metadata for an entity with none is %#v, want NULL: a repair that asks "+
			"for NULL cannot see the empty string", metadata)
	}

	// And the repair finds it, which is the point of storing NULL at all.
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("re-migrating: %v", err)
	}
	repaired, err := repo.GetEntity(ctx, "e1")
	if err != nil {
		t.Fatalf("reading the entity back: %v", err)
	}
	if repaired.Status != library.StatusIncomplete {
		t.Errorf("a Complete entity with no metadata is %q after the repair, want "+
			"incomplete so the worker picks it up", repaired.Status)
	}
}

// TestFormatTime_SortsAsText is L-19's second nit.
//
// These columns are ordered as text - "ORDER BY p.updated_at DESC" - and RFC3339Nano
// trims trailing zeros, so the strings were not the same length and did not sort by
// time within a second: ".5" is greater than ".05" as text and less than it as a
// time.
func TestFormatTime_SortsAsText(t *testing.T) {
	t.Parallel()

	base := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	// Three instants inside one second, chosen so the trimmed forms would sort
	// wrongly: .05, .5.
	earlier := base.Add(50 * time.Millisecond)
	later := base.Add(500 * time.Millisecond)

	first := formatTime(earlier)
	second := formatTime(later)

	if len(first) != len(second) {
		t.Errorf("timestamps are not the same width (%d and %d): text ordering cannot "+
			"agree with time ordering across them\n  %s\n  %s",
			len(first), len(second), first, second)
	}
	if !(first < second) {
		t.Errorf("the earlier instant sorts after the later one as text:\n  %s\n  %s",
			first, second)
	}

	// Every instant in a second, so the property holds generally rather than for
	// the pair that was picked.
	previous := formatTime(base)
	for i := 1; i < 1000; i++ {
		current := formatTime(base.Add(time.Duration(i) * time.Millisecond))
		if !(previous < current) {
			t.Fatalf("%s does not sort before %s, so progress updates inside one "+
				"second can come back in the wrong order", previous, current)
		}
		previous = current
	}
}
