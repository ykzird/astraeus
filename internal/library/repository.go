package library

import (
	"context"
)

// Repository defines the interface for interacting with the media library
// storage. Implementations must be safe for concurrent use.
type Repository interface {
	// Migrate brings the underlying schema up to date and is idempotent.
	Migrate(ctx context.Context) error

	// Library operations.
	CreateLibrary(ctx context.Context, lib *Library) error
	GetLibrary(ctx context.Context, id string) (*Library, error)
	GetLibraryByPath(ctx context.Context, path string) (*Library, error)
	ListLibraries(ctx context.Context) ([]Library, error)
	DeleteLibrary(ctx context.Context, id string) error

	// MediaEntity operations.
	CreateEntity(ctx context.Context, entity *MediaEntity) error
	UpdateEntity(ctx context.Context, entity *MediaEntity) error
	GetEntity(ctx context.Context, id string) (*MediaEntity, error)
	// FindEntity looks up a single entity by its natural key within a library.
	// A nil parentID matches top-level entities.
	FindEntity(ctx context.Context, libraryID string, parentID *string, entityType EntityType, name string) (*MediaEntity, error)
	ListEntities(ctx context.Context) ([]MediaEntity, error)
	ListEntitiesByLibrary(ctx context.Context, libraryID string) ([]MediaEntity, error)
	ListChildren(ctx context.Context, parentID string) ([]MediaEntity, error)

	// MediaObject operations.
	CreateObject(ctx context.Context, obj *MediaObject) error
	UpdateObject(ctx context.Context, obj *MediaObject) error
	GetObject(ctx context.Context, id string) (*MediaObject, error)
	GetObjectByPath(ctx context.Context, filePath string) (*MediaObject, error)
	GetObjectsByEntity(ctx context.Context, entityID string) ([]MediaObject, error)

	// PruneLibrary removes objects whose files are not in keepPaths, and any
	// entity left with neither objects nor children. It is how a file deleted
	// from disk stops being a ghost entry in every listing.
	PruneLibrary(ctx context.Context, libraryID string, keepPaths map[string]bool) (PruneResult, error)

	// WithTx runs fn inside a transaction. The Repository handed to fn is bound
	// to that transaction; returning an error rolls the whole thing back.
	WithTx(ctx context.Context, fn func(tx Repository) error) error
}
