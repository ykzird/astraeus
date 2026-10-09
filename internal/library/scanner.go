package library

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/ykzird/astraeus/internal/library/naming"
)

// ScanResult summarises what a scan actually did. It is returned to callers
// (CLI and API) so a scan can be reported without guessing.
type ScanResult struct {
	FilesSeen       int `json:"files_seen"`
	EntitiesCreated int `json:"entities_created"`
	EntitiesReused  int `json:"entities_reused"`
	ObjectsCreated  int `json:"objects_created"`
	ObjectsUpdated  int `json:"objects_updated"`
	ObjectsPruned   int `json:"objects_pruned"`
	EntitiesPruned  int `json:"entities_pruned"`
	// Warnings are paths the scan could not read. They mean its view of the disk
	// may be incomplete, so a prune is refused while any remain.
	Warnings []string `json:"warnings,omitempty"`
	// Notices are files the scan read but could not place in the library
	// hierarchy - a multi-episode name, an unrecognised pattern, a sample file.
	// The file is visible and simply not catalogued, so a notice does not mean
	// the disk view is incomplete and does not block a prune (L-7 of the
	// 2026-10-09 review).
	Notices []string `json:"notices,omitempty"`
}

// Scanner walks a library directory and persists what it finds. It is
// idempotent: re-scanning an unchanged tree creates no new rows.
type Scanner struct {
	repo   Repository
	logger *slog.Logger
	now    func() time.Time
}

// NewScanner creates a Scanner.
func NewScanner(repo Repository, logger *slog.Logger) *Scanner {
	if logger == nil {
		logger = slog.Default()
	}
	return &Scanner{repo: repo, logger: logger, now: time.Now}
}

// entitySpec describes one entity that a file implies in the hierarchy.
type entitySpec struct {
	Type EntityType
	Name string
	Meta *MetadataSet
}

// ScanLibrary walks lib.Path and reconciles the database with the files found.
func (s *Scanner) ScanLibrary(ctx context.Context, lib *Library) (ScanResult, error) {
	var result ScanResult

	root, err := filepath.Abs(lib.Path)
	if err != nil {
		return result, fmt.Errorf("resolving library path %q: %w", lib.Path, err)
	}

	// The root is resolved to a real path before anything walks it.
	//
	// os.Stat follows a symbolic link but filepath.WalkDir does not: it lstats
	// the root, sees something that is not a directory, and stops. So a library
	// registered as a symlink - the ordinary shape in a Docker or NAS setup,
	// where /media is a link to the real volume - reported zero files and said
	// nothing about why (L-5 of the 2026-10-09 review).
	resolved, resolveErr := filepath.EvalSymlinks(root)
	if resolveErr != nil {
		// An unresolvable root is a real error, not a link to be ignored: the
		// path exists for Stat but cannot be followed.
		return result, fmt.Errorf("resolving library path %q: %w", lib.Path, resolveErr)
	}
	root = resolved

	if info, statErr := os.Stat(root); statErr != nil {
		return result, fmt.Errorf("library path %q: %w", lib.Path, statErr)
	} else if !info.IsDir() {
		return result, fmt.Errorf("library path %q is not a directory", lib.Path)
	}

	// If the root is still a link, EvalSymlinks left it alone and the walk is
	// about to see a non-directory and stop. Saying so beats reporting an empty
	// library with no explanation.
	if info, lstatErr := os.Lstat(root); lstatErr == nil && info.Mode()&os.ModeSymlink != 0 {
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("%s is a symbolic link that could not be resolved, so it was not walked", lib.Path))
		s.logger.WarnContext(ctx, "library root is an unresolved symbolic link", "path", lib.Path)
	}

	// tally accumulates the distinct entities this pass touches.
	tally := newEntityTally()
	// seenPaths is what still exists on disk, and therefore what a prune is
	// allowed to keep.
	seenPaths := make(map[string]bool)

	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			// An unreadable subtree should not abort the whole scan.
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s: %v", path, err))
			s.logger.WarnContext(ctx, "skipping unreadable path", "path", path, "error", err)
			if entry != nil && entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if path != root && naming.IsIgnored(entry.Name()) {
				return fs.SkipDir
			}
			return nil
		}
		if naming.IsIgnored(entry.Name()) || !naming.IsVideoFile(path) {
			return nil
		}

		result.FilesSeen++
		seenPaths[path] = true
		if err := s.ingestFile(ctx, lib, path, root, &result, tally); err != nil {
			return err
		}
		return nil
	})
	if walkErr != nil {
		return result, fmt.Errorf("scanning %s: %w", root, walkErr)
	}

	// Report distinct entities rather than lookups: a season shared by twenty
	// episodes is one reused entity, not twenty.
	tally.apply(&result)

	pruned, err := s.pruneMissing(ctx, lib, seenPaths, result)
	if err != nil {
		return result, err
	}
	result.ObjectsPruned = pruned.ObjectsPruned
	result.EntitiesPruned = pruned.EntitiesPruned

	s.logger.InfoContext(ctx, "scan complete",
		"library", lib.Name,
		"files", result.FilesSeen,
		"entities_created", result.EntitiesCreated,
		"entities_reused", result.EntitiesReused,
		"objects_created", result.ObjectsCreated,
		"warnings", len(result.Warnings), "notices", len(result.Notices))
	return result, nil
}

// entityTally counts the distinct entities a scan touched. Containers are
// looked up once per file, so counting lookups would report the same series
// once for every episode it holds.
type entityTally struct {
	created map[string]bool
	reused  map[string]bool
}

func newEntityTally() *entityTally {
	return &entityTally{created: make(map[string]bool), reused: make(map[string]bool)}
}

func (t *entityTally) markCreated(id string) {
	t.created[id] = true
	// An entity this scan created is not also a reuse, even though a later
	// file in the same scan will legitimately find it.
	delete(t.reused, id)
}

func (t *entityTally) markReused(id string) {
	if t.created[id] {
		return
	}
	t.reused[id] = true
}

func (t *entityTally) apply(result *ScanResult) {
	result.EntitiesCreated = len(t.created)
	result.EntitiesReused = len(t.reused)
}

// pruneMissing removes records for files that are no longer on disk.
//
// This deletes data, so it refuses to act whenever the scan's view of the disk
// might be incomplete: a warning means a subtree could not be read, and an empty
// result against a library that previously had media usually means the mount is
// not up rather than that the user deleted everything. Getting this wrong
// silently empties a library, which is far worse than leaving a ghost entry.
func (s *Scanner) pruneMissing(ctx context.Context, lib *Library, seenPaths map[string]bool, result ScanResult) (PruneResult, error) {
	if len(result.Warnings) > 0 {
		s.logger.WarnContext(ctx, "skipping prune: the scan could not read every path",
			"library", lib.Name, "warnings", len(result.Warnings))
		return PruneResult{}, nil
	}
	if len(result.Notices) > 0 {
		// A notice is not a reason to refuse. It is logged so an operator can
		// see what was not catalogued.
		s.logger.InfoContext(ctx, "pruning despite files that could not be classified",
			"library", lib.Name, "notices", len(result.Notices))
	}

	if result.FilesSeen == 0 {
		existing, err := s.repo.ListEntitiesByLibrary(ctx, lib.ID)
		if err != nil {
			return PruneResult{}, fmt.Errorf("checking library %s before pruning: %w", lib.ID, err)
		}
		if len(existing) > 0 {
			s.logger.WarnContext(ctx, "skipping prune: no files were visible but the library is not empty",
				"library", lib.Name)
			return PruneResult{}, nil
		}
	}

	return s.repo.PruneLibrary(ctx, lib.ID, seenPaths)
}

// ingestFile reconciles one media file with the database inside a transaction,
// so a failure part-way cannot leave an episode without its series or season.
func (s *Scanner) ingestFile(ctx context.Context, lib *Library, absPath, root string, result *ScanResult, tally *entityTally) error {
	info, err := os.Stat(absPath)
	if err != nil {
		result.Warnings = append(result.Warnings, fmt.Sprintf("%s: %v", absPath, err))
		return nil
	}

	rel, err := filepath.Rel(root, absPath)
	if err != nil {
		rel = filepath.Base(absPath)
	}

	containers, leaf, ok := PlacementFor(lib, rel, absPath)
	if !ok {
		// A notice rather than a warning: the file was read, and it is on disk
		// where the scan can see it. Failing to classify it says nothing about
		// whether the scan's view of the library is complete, and treating it as
		// though it did meant one unplaceable file - a multi-episode name, say -
		// disabled pruning for its whole library forever. Deleting an episode
		// then left its entity in place for good (L-7).
		msg := fmt.Sprintf("%s: could not be placed in the library hierarchy; skipped", rel)
		result.Notices = append(result.Notices, msg)
		s.logger.WarnContext(ctx, "skipping unplaceable file", "file", rel, "library_kind", lib.Kind)
		return nil
	}

	return s.repo.WithTx(ctx, func(tx Repository) error {
		var parentID *string

		for _, spec := range containers {
			entity, err := tx.FindEntity(ctx, lib.ID, parentID, spec.Type, spec.Name)
			if err != nil {
				if !errors.Is(err, ErrNotFound) {
					return err
				}
				entity = s.newEntity(lib.ID, parentID, spec)
				if err := tx.CreateEntity(ctx, entity); err != nil {
					return err
				}
				tally.markCreated(entity.ID)
			} else {
				tally.markReused(entity.ID)
			}
			parentID = &entity.ID
		}

		leafEntity, err := tx.FindEntity(ctx, lib.ID, parentID, leaf.Type, leaf.Name)
		if err != nil {
			if !errors.Is(err, ErrNotFound) {
				return err
			}
			leafEntity = s.newEntity(lib.ID, parentID, leaf)
			if err := tx.CreateEntity(ctx, leafEntity); err != nil {
				return err
			}
			tally.markCreated(leafEntity.ID)
		} else {
			tally.markReused(leafEntity.ID)
		}

		existing, err := tx.GetObjectByPath(ctx, absPath)
		switch {
		case err == nil:
			if existing.Size != info.Size() || existing.MediaEntityID != leafEntity.ID {
				existing.Size = info.Size()
				existing.MediaEntityID = leafEntity.ID
				if err := tx.UpdateObject(ctx, existing); err != nil {
					return err
				}
				result.ObjectsUpdated++
			}
		case errors.Is(err, ErrNotFound):
			obj := &MediaObject{
				ID:            uuid.NewString(),
				MediaEntityID: leafEntity.ID,
				FilePath:      absPath,
				Size:          info.Size(),
				MimeType:      naming.MimeTypeForExt(filepath.Ext(absPath)),
				CreatedAt:     s.now(),
			}
			if err := tx.CreateObject(ctx, obj); err != nil {
				return err
			}
			result.ObjectsCreated++
		default:
			return err
		}

		return nil
	})
}

func (s *Scanner) newEntity(libraryID string, parentID *string, spec entitySpec) *MediaEntity {
	now := s.now()
	return &MediaEntity{
		ID:        uuid.NewString(),
		LibraryID: libraryID,
		ParentID:  parentID,
		Type:      spec.Type,
		Name:      spec.Name,
		Status:    StatusIncomplete,
		CreatedAt: now,
		UpdatedAt: now,
		Metadata:  spec.Meta,
	}
}

// PlacementFor maps a file to the entities it implies: the containers that
// must exist above it, and the leaf entity that represents it.
//
// It is exported so the layout rules can be exercised directly in tests.
func PlacementFor(lib *Library, relPath, absPath string) (containers []entitySpec, leaf entitySpec, ok bool) {
	switch lib.Kind {
	case MoviesLibrary:
		leaf, ok = moviePlacement(relPath, absPath)
		return nil, leaf, ok
	case ShowsLibrary:
		return showPlacement(relPath, absPath)
	default:
		return nil, entitySpec{}, false
	}
}

func moviePlacement(relPath, absPath string) (entitySpec, bool) {
	// The file name is usually the most precise title, but a containing folder
	// that carries a release year ("Blade Runner 2049 (2017)/...1080p.mkv") is
	// a stronger signal, so it wins in that case only.
	parts := naming.SplitPath(relPath)
	source := filepath.Base(absPath)
	if len(parts) >= 2 {
		if dir := parts[len(parts)-2]; dir != "" {
			if _, year := naming.ParseMovieName(dir); year != 0 {
				source = dir
			}
		}
	}

	title, year := naming.ParseMovieName(source)
	if title == "" {
		title = naming.TitleFromPath(absPath)
	}
	if title == "" {
		return entitySpec{}, false
	}

	spec := entitySpec{Type: MovieEntity, Name: title}
	if year != 0 {
		spec.Meta = &MetadataSet{Extra: map[string]string{"year": fmt.Sprint(year)}}
	}
	return spec, true
}

func showPlacement(relPath, absPath string) ([]entitySpec, entitySpec, bool) {
	info, ok := naming.ParseEpisodePath(relPath)
	if !ok || info.Series == "" {
		return nil, entitySpec{}, false
	}

	containers := []entitySpec{{Type: SeriesEntity, Name: info.Series}}
	if info.Season > 0 {
		containers = append(containers, entitySpec{Type: SeasonEntity, Name: naming.SeasonDirName(info.Season)})
	}

	name := fmt.Sprintf("S%02dE%02d", info.Season, info.Episode)
	if info.Title != "" {
		name += " - " + info.Title
	}

	meta := &MetadataSet{Extra: map[string]string{
		"season":  fmt.Sprint(info.Season),
		"episode": fmt.Sprint(info.Episode),
	}}
	return containers, entitySpec{Type: EpisodeEntity, Name: name, Meta: meta}, true
}
