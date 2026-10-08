package library

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrNotFound is returned when a requested record does not exist. Callers
// should test for it with errors.Is rather than string matching.
var ErrNotFound = errors.New("not found")

// EntityType enumerates the kinds of MediaEntity in the library hierarchy.
type EntityType string

const (
	SeriesEntity  EntityType = "Series"
	SeasonEntity  EntityType = "Season"
	EpisodeEntity EntityType = "Episode"
	MovieEntity   EntityType = "Movie"
)

// IsContainer reports whether the type may hold child entities.
func (t EntityType) IsContainer() bool {
	return t == SeriesEntity || t == SeasonEntity
}

// IsValid reports whether the type is one of the known entity types.
func (t EntityType) IsValid() bool {
	switch t {
	case SeriesEntity, SeasonEntity, EpisodeEntity, MovieEntity:
		return true
	default:
		return false
	}
}

// EntityStatus tracks whether a MetadataSet has been attached to an entity.
type EntityStatus string

const (
	// StatusIncomplete means the entity still needs metadata; it is surfaced to
	// the administrator as an outstanding item.
	StatusIncomplete EntityStatus = "Incomplete"
	// StatusComplete means a MetadataSet has been successfully attached.
	StatusComplete EntityStatus = "Complete"
)

// LibraryKind describes how a library's directory tree is interpreted.
type LibraryKind string

const (
	// MoviesLibrary expects one movie per file (optionally in its own folder).
	MoviesLibrary LibraryKind = "movies"
	// ShowsLibrary expects Series/Season directories holding episode files.
	ShowsLibrary LibraryKind = "shows"
)

// ParseLibraryKind validates a user-supplied library kind.
func ParseLibraryKind(s string) (LibraryKind, error) {
	switch LibraryKind(strings.ToLower(strings.TrimSpace(s))) {
	case MoviesLibrary:
		return MoviesLibrary, nil
	case ShowsLibrary:
		return ShowsLibrary, nil
	default:
		return "", fmt.Errorf("invalid library kind %q: want %q or %q", s, MoviesLibrary, ShowsLibrary)
	}
}

// Library is a logical collection of MediaEntity objects rooted at a directory.
type Library struct {
	ID        string      `json:"id" db:"id"`
	Name      string      `json:"name" db:"name"`
	Path      string      `json:"path" db:"path"`
	Kind      LibraryKind `json:"kind" db:"kind"`
	CreatedAt time.Time   `json:"created_at" db:"created_at"`
}

// MediaEntity is the logical representation of a piece of content. A
// ContainerEntity (Series, Season) may hold children; a LeafEntity (Episode,
// Movie) is the final node in a hierarchy and is what plays.
type MediaEntity struct {
	ID        string       `json:"id" db:"id"`
	LibraryID string       `json:"library_id" db:"library_id"`
	ParentID  *string      `json:"parent_id,omitempty" db:"parent_id"`
	Type      EntityType   `json:"type" db:"type"`
	Name      string       `json:"name" db:"name"`
	Status    EntityStatus `json:"status" db:"status"`
	CreatedAt time.Time    `json:"created_at" db:"created_at"`
	UpdatedAt time.Time    `json:"updated_at" db:"updated_at"`

	// Metadata is the descriptive set attached once a provider succeeds. It is
	// nil while the entity is Incomplete.
	Metadata *MetadataSet `json:"metadata,omitempty" db:"-"`
}

// DisplayTitle returns the best human-readable title available: the metadata
// title when present, otherwise the name derived from the filesystem.
func (e *MediaEntity) DisplayTitle() string {
	if e.Metadata != nil && e.Metadata.Title != "" {
		return e.Metadata.Title
	}
	return e.Name
}

// MediaObject is the physical file(s) on disk that represent a LeafEntity.
type MediaObject struct {
	ID            string    `json:"id" db:"id"`
	MediaEntityID string    `json:"media_entity_id" db:"media_entity_id"`
	FilePath      string    `json:"file_path" db:"file_path"`
	Size          int64     `json:"size" db:"size"`
	MimeType      string    `json:"mime_type" db:"mime_type"`
	CreatedAt     time.Time `json:"created_at" db:"created_at"`
}

// MetadataSet is a collection of descriptive attributes associated with a
// MediaEntity. Provider records which provider produced it.
type MetadataSet struct {
	Title        string            `json:"title"`
	Description  string            `json:"description"`
	PosterPath   string            `json:"poster_path,omitempty"`
	BackdropPath string            `json:"backdrop_path,omitempty"`
	Provider     string            `json:"provider,omitempty"`
	Extra        map[string]string `json:"extra,omitempty"`
}
