package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"

	"github.com/jok/astraeus-media/internal/library"
)

// sqlxExecutor is satisfied by both *sqlx.DB and *sqlx.Tx, which lets the same
// repository code run inside or outside a transaction.
type sqlxExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	NamedExecContext(ctx context.Context, query string, arg any) (sql.Result, error)
	GetContext(ctx context.Context, dest any, query string, args ...any) error
	SelectContext(ctx context.Context, dest any, query string, args ...any) error
}

// A compile-time guarantee that this adapter still satisfies the port it exists
// to implement. library.Repository itself is checked at the composition root.
var _ library.Repository = (*Repository)(nil)

// Repository implements library.Repository on SQLite.
type Repository struct {
	db *sqlx.DB
	tx *sqlx.Tx
}

// New wraps an existing connection.
func New(db *sqlx.DB) *Repository {
	return &Repository{db: db}
}

// Open connects to the database at path with the pragmas this project needs and
// returns a repository on it. This is the constructor callers want.
func Open(path string) (*Repository, error) {
	db, err := sqlx.Connect("sqlite", DSN(path))
	if err != nil {
		return nil, fmt.Errorf("opening database %s: %w", path, err)
	}
	return New(db), nil
}

// Close releases the underlying connection pool.
func (r *Repository) Close() error {
	if r.db == nil {
		return nil
	}
	return r.db.Close()
}

// exec returns the transaction when one is active, otherwise the pool.
func (r *Repository) exec() sqlxExecutor {
	if r.tx != nil {
		return r.tx
	}
	return r.db
}

// WithTx runs fn inside a transaction.
func (r *Repository) WithTx(ctx context.Context, fn func(tx library.Repository) error) error {
	if r.tx != nil {
		// Already inside a transaction; reuse it rather than nesting.
		return fn(r)
	}

	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	if err := fn(&Repository{db: r.db, tx: tx}); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
		}
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// ---- schema ----------------------------------------------------------------

// Migrate creates the schema if needed, upgrades databases written by earlier
// versions, and backfills the data those upgrades depend on. It is idempotent.
func (r *Repository) Migrate(ctx context.Context) error {
	baseline := []string{
		`CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS libraries (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			path TEXT NOT NULL,
			kind TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS media_entities (
			id TEXT PRIMARY KEY,
			library_id TEXT NOT NULL DEFAULT '',
			parent_id TEXT,
			type TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			metadata TEXT,
			FOREIGN KEY (parent_id) REFERENCES media_entities(id),
			FOREIGN KEY (library_id) REFERENCES libraries(id)
		)`,
		`CREATE TABLE IF NOT EXISTS media_objects (
			id TEXT PRIMARY KEY,
			media_entity_id TEXT NOT NULL,
			file_path TEXT NOT NULL,
			size INTEGER NOT NULL,
			mime_type TEXT NOT NULL,
			created_at TEXT NOT NULL,
			FOREIGN KEY (media_entity_id) REFERENCES media_entities(id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_media_entities_parent_id ON media_entities(parent_id)`,
		`CREATE INDEX IF NOT EXISTS idx_media_objects_entity_id ON media_objects(media_entity_id)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_libraries_path ON libraries(path)`,
	}
	for _, stmt := range baseline {
		if _, err := r.exec().ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migration statement failed: %w", err)
		}
	}

	// Databases created before libraries existed are missing these columns.
	for _, col := range []struct{ name, ddl string }{
		{"library_id", `ALTER TABLE media_entities ADD COLUMN library_id TEXT NOT NULL DEFAULT ''`},
		{"name", `ALTER TABLE media_entities ADD COLUMN name TEXT NOT NULL DEFAULT ''`},
	} {
		if err := r.ensureColumn(ctx, "media_entities", col.name, col.ddl); err != nil {
			return err
		}
	}

	// Entities from before the name column need a name before the identity
	// index below can be enforced.
	if err := r.backfillEntityNames(ctx); err != nil {
		return err
	}

	// The old scanner marked every entity Complete regardless of whether a
	// MetadataSet was attached, which contradicts the domain rule that an
	// entity is Complete only once it has metadata. Reset those so the
	// metadata worker will pick them up.
	if _, err := r.exec().ExecContext(ctx,
		`UPDATE media_entities SET status = ? WHERE status = ? AND metadata IS NULL`,
		string(library.StatusIncomplete), string(library.StatusComplete),
	); err != nil {
		return fmt.Errorf("repairing entity statuses: %w", err)
	}

	// Collapse any duplicate rows for the same file written by older scanners
	// that inserted unconditionally on every run.
	if _, err := r.exec().ExecContext(ctx,
		`DELETE FROM media_objects WHERE rowid NOT IN (SELECT MIN(rowid) FROM media_objects GROUP BY file_path)`,
	); err != nil {
		return fmt.Errorf("deduplicating media objects: %w", err)
	}

	// Identity index: a library holds at most one entity of a given type and
	// name under a given parent. COALESCE keeps top-level (NULL parent) rows
	// subject to the same rule, which plain unique indexes do not do in SQLite.
	post := []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_media_objects_path ON media_objects(file_path)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_media_entities_identity
			ON media_entities(library_id, COALESCE(parent_id, ''), type, name)`,
		`CREATE INDEX IF NOT EXISTS idx_media_entities_library ON media_entities(library_id)`,
	}
	for _, stmt := range post {
		if _, err := r.exec().ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migration statement failed: %w", err)
		}
	}

	_, err := r.exec().ExecContext(ctx,
		`INSERT OR IGNORE INTO schema_migrations (version, applied_at) VALUES (1, ?)`, formatTime(time.Now()))
	if err != nil {
		return fmt.Errorf("recording migration: %w", err)
	}
	return nil
}

// ensureColumn adds a column when it does not already exist. SQLite has no
// ADD COLUMN IF NOT EXISTS, so the table definition is inspected first.
func (r *Repository) ensureColumn(ctx context.Context, table, column, ddl string) error {
	var columns []struct {
		Name string `db:"name"`
	}
	if err := r.exec().SelectContext(ctx, &columns, `SELECT name FROM pragma_table_info(?)`, table); err != nil {
		return fmt.Errorf("inspecting %s: %w", table, err)
	}
	for _, c := range columns {
		if c.Name == column {
			return nil
		}
	}
	if _, err := r.exec().ExecContext(ctx, ddl); err != nil {
		return fmt.Errorf("adding %s.%s: %w", table, column, err)
	}
	return nil
}

// backfillEntityNames gives a name to entities created before the name column
// existed, deriving it from the file that represents them.
func (r *Repository) backfillEntityNames(ctx context.Context) error {
	var rows []struct {
		ID   string             `db:"id"`
		Type library.EntityType `db:"type"`
	}
	if err := r.exec().SelectContext(ctx, &rows, `SELECT id, type FROM media_entities WHERE name = ''`); err != nil {
		return fmt.Errorf("finding unnamed entities: %w", err)
	}

	for _, row := range rows {
		name := ""
		var filePath string
		err := r.exec().GetContext(ctx, &filePath,
			`SELECT file_path FROM media_objects WHERE media_entity_id = ? LIMIT 1`, row.ID)
		switch {
		case err == nil && filePath != "":
			name = library.TitleFromPath(filePath)
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("looking up object for entity %s: %w", row.ID, err)
		}
		if name == "" {
			name = fmt.Sprintf("%s %s", row.Type, row.ID)
		}
		if _, err := r.exec().ExecContext(ctx,
			`UPDATE media_entities SET name = ? WHERE id = ?`, name, row.ID); err != nil {
			return fmt.Errorf("backfilling name for entity %s: %w", row.ID, err)
		}
	}
	return nil
}

// ---- libraries -------------------------------------------------------------

func (r *Repository) CreateLibrary(ctx context.Context, lib *library.Library) error {
	_, err := r.exec().NamedExecContext(ctx,
		`INSERT INTO libraries (id, name, path, kind, created_at)
		 VALUES (:id, :name, :path, :kind, :created_at)`,
		struct {
			ID        string `db:"id"`
			Name      string `db:"name"`
			Path      string `db:"path"`
			Kind      string `db:"kind"`
			CreatedAt string `db:"created_at"`
		}{lib.ID, lib.Name, lib.Path, string(lib.Kind), formatTime(lib.CreatedAt)})
	if err != nil {
		return fmt.Errorf("creating library: %w", err)
	}
	return nil
}

func (r *Repository) GetLibrary(ctx context.Context, id string) (*library.Library, error) {
	var row libraryRow
	err := r.exec().GetContext(ctx, &row,
		`SELECT id, name, path, kind, created_at FROM libraries WHERE id = ?`, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("library %s: %w", id, library.ErrNotFound)
		}
		return nil, fmt.Errorf("getting library %s: %w", id, err)
	}
	return row.toLibrary()
}

func (r *Repository) GetLibraryByPath(ctx context.Context, path string) (*library.Library, error) {
	var row libraryRow
	err := r.exec().GetContext(ctx, &row,
		`SELECT id, name, path, kind, created_at FROM libraries WHERE path = ?`, path)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("library at %s: %w", path, library.ErrNotFound)
		}
		return nil, fmt.Errorf("getting library at %s: %w", path, err)
	}
	return row.toLibrary()
}

func (r *Repository) ListLibraries(ctx context.Context) ([]library.Library, error) {
	var rows []libraryRow
	if err := r.exec().SelectContext(ctx, &rows,
		`SELECT id, name, path, kind, created_at FROM libraries ORDER BY name`); err != nil {
		return nil, fmt.Errorf("listing libraries: %w", err)
	}
	libs := make([]library.Library, 0, len(rows))
	for _, row := range rows {
		lib, err := row.toLibrary()
		if err != nil {
			return nil, err
		}
		libs = append(libs, *lib)
	}
	return libs, nil
}

// DeleteLibrary removes a library together with its entities and objects.
// PruneResult reports what a prune removed.
// PruneLibrary removes objects for files this pass did not see, then any entity
// left with neither objects nor children, working upwards so an emptied season
// is removed and then an emptied series.
//
// Deletion order matters now that foreign keys are enforced: objects reference
// entities, and entities reference their parent, so the children have to go
// before the parents.
func (r *Repository) PruneLibrary(ctx context.Context, libraryID string, keepPaths map[string]bool) (library.PruneResult, error) {
	var result library.PruneResult

	err := r.WithTx(ctx, func(tx library.Repository) error {
		inner := tx.(*Repository)

		type row struct {
			ID       string `db:"id"`
			FilePath string `db:"file_path"`
		}
		var rows []row
		if err := inner.exec().SelectContext(ctx, &rows,
			`SELECT o.id, o.file_path FROM media_objects o
			 JOIN media_entities e ON e.id = o.media_entity_id
			 WHERE e.library_id = ?`, libraryID); err != nil {
			return fmt.Errorf("listing objects for library %s: %w", libraryID, err)
		}

		for _, candidate := range rows {
			if keepPaths[candidate.FilePath] {
				continue
			}
			if _, err := inner.exec().ExecContext(ctx,
				`DELETE FROM media_objects WHERE id = ?`, candidate.ID); err != nil {
				return fmt.Errorf("pruning object %s: %w", candidate.ID, err)
			}
			result.ObjectsPruned++
		}

		// Childless and objectless entities, repeatedly, so removing a season
		// can make its series prunable on the next pass.
		for {
			res, err := inner.exec().ExecContext(ctx, `
				DELETE FROM media_entities
				WHERE library_id = ?
				  AND id NOT IN (SELECT media_entity_id FROM media_objects)
				  AND id NOT IN (SELECT parent_id FROM media_entities WHERE parent_id IS NOT NULL)`,
				libraryID)
			if err != nil {
				return fmt.Errorf("pruning entities for library %s: %w", libraryID, err)
			}
			affected, err := res.RowsAffected()
			if err != nil {
				return fmt.Errorf("counting pruned entities: %w", err)
			}
			result.EntitiesPruned += int(affected)
			if affected == 0 {
				break
			}
		}

		return nil
	})
	if err != nil {
		return library.PruneResult{}, err
	}
	return result, nil
}

func (r *Repository) DeleteLibrary(ctx context.Context, id string) error {
	return r.WithTx(ctx, func(tx library.Repository) error {
		if err := tx.(*Repository).deleteLibraryRows(ctx, id); err != nil {
			return err
		}
		return nil
	})
}

func (r *Repository) deleteLibraryRows(ctx context.Context, id string) error {
	if _, err := r.exec().ExecContext(ctx,
		`DELETE FROM media_objects WHERE media_entity_id IN (SELECT id FROM media_entities WHERE library_id = ?)`, id); err != nil {
		return fmt.Errorf("deleting objects for library %s: %w", id, err)
	}
	if _, err := r.exec().ExecContext(ctx, `DELETE FROM media_entities WHERE library_id = ?`, id); err != nil {
		return fmt.Errorf("deleting entities for library %s: %w", id, err)
	}
	res, err := r.exec().ExecContext(ctx, `DELETE FROM libraries WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("deleting library %s: %w", id, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("library %s: %w", id, library.ErrNotFound)
	}
	return nil
}

type libraryRow struct {
	ID        string `db:"id"`
	Name      string `db:"name"`
	Path      string `db:"path"`
	Kind      string `db:"kind"`
	CreatedAt string `db:"created_at"`
}

func (row libraryRow) toLibrary() (*library.Library, error) {
	createdAt, err := parseTime(row.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("library %s: parsing created_at: %w", row.ID, err)
	}
	return &library.Library{
		ID:        row.ID,
		Name:      row.Name,
		Path:      row.Path,
		Kind:      library.LibraryKind(row.Kind),
		CreatedAt: createdAt,
	}, nil
}

// ---- media entities --------------------------------------------------------

// entityColumns is the shared SELECT list for media_entities.
const entityColumns = `id, library_id, parent_id, type, name, status, created_at, updated_at, metadata`

type entityRow struct {
	ID        string               `db:"id"`
	LibraryID string               `db:"library_id"`
	ParentID  *string              `db:"parent_id"`
	Type      library.EntityType   `db:"type"`
	Name      string               `db:"name"`
	Status    library.EntityStatus `db:"status"`
	CreatedAt string               `db:"created_at"`
	UpdatedAt string               `db:"updated_at"`
	// Metadata is NULL for entities that have never been enriched.
	Metadata *string `db:"metadata"`
}

func (row entityRow) toEntity() (*library.MediaEntity, error) {
	createdAt, err := parseTime(row.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("entity %s: parsing created_at: %w", row.ID, err)
	}
	updatedAt, err := parseTime(row.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("entity %s: parsing updated_at: %w", row.ID, err)
	}

	// The earliest prototype wrote an empty string rather than NULL for
	// top-level entities; normalise that here so callers only ever deal with
	// nil meaning "no parent".
	parentID := row.ParentID
	if parentID != nil && *parentID == "" {
		parentID = nil
	}

	entity := &library.MediaEntity{
		ID:        row.ID,
		LibraryID: row.LibraryID,
		ParentID:  parentID,
		Type:      row.Type,
		Name:      row.Name,
		Status:    row.Status,
		CreatedAt: createdAt,
		UpdatedAt: updatedAt,
	}

	if row.Metadata != nil && *row.Metadata != "" {
		var meta library.MetadataSet
		if err := json.Unmarshal([]byte(*row.Metadata), &meta); err != nil {
			return nil, fmt.Errorf("entity %s: unmarshalling metadata: %w", row.ID, err)
		}
		entity.Metadata = &meta
	}
	return entity, nil
}

type entityWriteParams struct {
	ID        string               `db:"id"`
	LibraryID string               `db:"library_id"`
	ParentID  *string              `db:"parent_id"`
	Type      library.EntityType   `db:"type"`
	Name      string               `db:"name"`
	Status    library.EntityStatus `db:"status"`
	CreatedAt string               `db:"created_at"`
	UpdatedAt string               `db:"updated_at"`
	Metadata  string               `db:"metadata"`
}

func entityParams(e *library.MediaEntity) (entityWriteParams, error) {
	metadata, err := marshalMetadata(e.Metadata)
	if err != nil {
		return entityWriteParams{}, err
	}
	return entityWriteParams{
		ID:        e.ID,
		LibraryID: e.LibraryID,
		ParentID:  e.ParentID,
		Type:      e.Type,
		Name:      e.Name,
		Status:    e.Status,
		CreatedAt: formatTime(e.CreatedAt),
		UpdatedAt: formatTime(e.UpdatedAt),
		Metadata:  metadata,
	}, nil
}

func (r *Repository) CreateEntity(ctx context.Context, entity *library.MediaEntity) error {
	params, err := entityParams(entity)
	if err != nil {
		return err
	}
	_, err = r.exec().NamedExecContext(ctx,
		`INSERT INTO media_entities (id, library_id, parent_id, type, name, status, created_at, updated_at, metadata)
		 VALUES (:id, :library_id, :parent_id, :type, :name, :status, :created_at, :updated_at, :metadata)`, params)
	if err != nil {
		return fmt.Errorf("creating entity %q: %w", entity.Name, err)
	}
	return nil
}

func (r *Repository) UpdateEntity(ctx context.Context, entity *library.MediaEntity) error {
	params, err := entityParams(entity)
	if err != nil {
		return err
	}
	res, err := r.exec().NamedExecContext(ctx,
		`UPDATE media_entities
		 SET library_id = :library_id, parent_id = :parent_id, type = :type, name = :name,
		     status = :status, updated_at = :updated_at, metadata = :metadata
		 WHERE id = :id`, params)
	if err != nil {
		return fmt.Errorf("updating entity %s: %w", entity.ID, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("entity %s: %w", entity.ID, library.ErrNotFound)
	}
	return nil
}

func (r *Repository) GetEntity(ctx context.Context, id string) (*library.MediaEntity, error) {
	var row entityRow
	err := r.exec().GetContext(ctx, &row,
		`SELECT `+entityColumns+` FROM media_entities WHERE id = ?`, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("entity %s: %w", id, library.ErrNotFound)
		}
		return nil, fmt.Errorf("getting entity %s: %w", id, err)
	}
	return row.toEntity()
}

func (r *Repository) FindEntity(ctx context.Context, libraryID string, parentID *string, entityType library.EntityType, name string) (*library.MediaEntity, error) {
	var row entityRow
	// `IS` compares NULL-safely, so a nil parentID matches top-level entities.
	err := r.exec().GetContext(ctx, &row,
		`SELECT `+entityColumns+` FROM media_entities
		 WHERE library_id = ? AND parent_id IS ? AND type = ? AND name = ?`,
		libraryID, parentID, entityType, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("entity %q: %w", name, library.ErrNotFound)
		}
		return nil, fmt.Errorf("finding entity %q: %w", name, err)
	}
	return row.toEntity()
}

func (r *Repository) ListEntities(ctx context.Context) ([]library.MediaEntity, error) {
	var rows []entityRow
	if err := r.exec().SelectContext(ctx, &rows,
		`SELECT `+entityColumns+` FROM media_entities ORDER BY type, name`); err != nil {
		return nil, fmt.Errorf("listing entities: %w", err)
	}
	return entitiesFromRows(rows)
}

func (r *Repository) ListEntitiesByLibrary(ctx context.Context, libraryID string) ([]library.MediaEntity, error) {
	var rows []entityRow
	if err := r.exec().SelectContext(ctx, &rows,
		`SELECT `+entityColumns+` FROM media_entities WHERE library_id = ? ORDER BY type, name`, libraryID); err != nil {
		return nil, fmt.Errorf("listing entities for library %s: %w", libraryID, err)
	}
	return entitiesFromRows(rows)
}

func (r *Repository) ListChildren(ctx context.Context, parentID string) ([]library.MediaEntity, error) {
	var rows []entityRow
	if err := r.exec().SelectContext(ctx, &rows,
		`SELECT `+entityColumns+` FROM media_entities WHERE parent_id = ? ORDER BY type, name`, parentID); err != nil {
		return nil, fmt.Errorf("listing children of %s: %w", parentID, err)
	}
	return entitiesFromRows(rows)
}

func entitiesFromRows(rows []entityRow) ([]library.MediaEntity, error) {
	entities := make([]library.MediaEntity, 0, len(rows))
	for _, row := range rows {
		entity, err := row.toEntity()
		if err != nil {
			return nil, err
		}
		entities = append(entities, *entity)
	}
	return entities, nil
}

// ---- media objects ---------------------------------------------------------

type objectRow struct {
	ID            string `db:"id"`
	MediaEntityID string `db:"media_entity_id"`
	FilePath      string `db:"file_path"`
	Size          int64  `db:"size"`
	MimeType      string `db:"mime_type"`
	CreatedAt     string `db:"created_at"`
}

func (row objectRow) toObject() (*library.MediaObject, error) {
	createdAt, err := parseTime(row.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("object %s: parsing created_at: %w", row.ID, err)
	}
	return &library.MediaObject{
		ID:            row.ID,
		MediaEntityID: row.MediaEntityID,
		FilePath:      row.FilePath,
		Size:          row.Size,
		MimeType:      row.MimeType,
		CreatedAt:     createdAt,
	}, nil
}

type objectWriteParams struct {
	ID            string `db:"id"`
	MediaEntityID string `db:"media_entity_id"`
	FilePath      string `db:"file_path"`
	Size          int64  `db:"size"`
	MimeType      string `db:"mime_type"`
	CreatedAt     string `db:"created_at"`
}

const objectColumns = `id, media_entity_id, file_path, size, mime_type, created_at`

func (r *Repository) CreateObject(ctx context.Context, obj *library.MediaObject) error {
	_, err := r.exec().NamedExecContext(ctx,
		`INSERT INTO media_objects (id, media_entity_id, file_path, size, mime_type, created_at)
		 VALUES (:id, :media_entity_id, :file_path, :size, :mime_type, :created_at)`,
		objectWriteParams{obj.ID, obj.MediaEntityID, obj.FilePath, obj.Size, obj.MimeType, formatTime(obj.CreatedAt)})
	if err != nil {
		return fmt.Errorf("creating object for %s: %w", obj.FilePath, err)
	}
	return nil
}

func (r *Repository) UpdateObject(ctx context.Context, obj *library.MediaObject) error {
	res, err := r.exec().NamedExecContext(ctx,
		`UPDATE media_objects
		 SET media_entity_id = :media_entity_id, file_path = :file_path, size = :size, mime_type = :mime_type
		 WHERE id = :id`,
		objectWriteParams{obj.ID, obj.MediaEntityID, obj.FilePath, obj.Size, obj.MimeType, formatTime(obj.CreatedAt)})
	if err != nil {
		return fmt.Errorf("updating object %s: %w", obj.ID, err)
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return fmt.Errorf("object %s: %w", obj.ID, library.ErrNotFound)
	}
	return nil
}

func (r *Repository) GetObject(ctx context.Context, id string) (*library.MediaObject, error) {
	var row objectRow
	err := r.exec().GetContext(ctx, &row,
		`SELECT `+objectColumns+` FROM media_objects WHERE id = ?`, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("object %s: %w", id, library.ErrNotFound)
		}
		return nil, fmt.Errorf("getting object %s: %w", id, err)
	}
	return row.toObject()
}

func (r *Repository) GetObjectByPath(ctx context.Context, filePath string) (*library.MediaObject, error) {
	var row objectRow
	err := r.exec().GetContext(ctx, &row,
		`SELECT `+objectColumns+` FROM media_objects WHERE file_path = ?`, filePath)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("object %s: %w", filePath, library.ErrNotFound)
		}
		return nil, fmt.Errorf("getting object %s: %w", filePath, err)
	}
	return row.toObject()
}

func (r *Repository) GetObjectsByEntity(ctx context.Context, entityID string) ([]library.MediaObject, error) {
	var rows []objectRow
	if err := r.exec().SelectContext(ctx, &rows,
		`SELECT `+objectColumns+` FROM media_objects WHERE media_entity_id = ? ORDER BY file_path`, entityID); err != nil {
		return nil, fmt.Errorf("getting objects for entity %s: %w", entityID, err)
	}
	objects := make([]library.MediaObject, 0, len(rows))
	for _, row := range rows {
		obj, err := row.toObject()
		if err != nil {
			return nil, err
		}
		objects = append(objects, *obj)
	}
	return objects, nil
}

// ---- helpers ---------------------------------------------------------------

func marshalMetadata(meta *library.MetadataSet) (string, error) {
	if meta == nil {
		return "", nil
	}
	raw, err := json.Marshal(meta)
	if err != nil {
		return "", fmt.Errorf("marshalling metadata: %w", err)
	}
	return string(raw), nil
}

// timeLayouts are tried in order when reading timestamps back. The last two
// exist to read rows written by earlier versions of this project.
var timeLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999999 -0700 MST",
	"2006-01-02 15:04:05",
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	// Legacy rows recorded the monotonic clock suffix from time.Time.String.
	if idx := strings.Index(s, " m="); idx != -1 {
		s = s[:idx]
	}
	var lastErr error
	for _, layout := range timeLayouts {
		t, err := time.Parse(layout, s)
		if err == nil {
			return t, nil
		}
		lastErr = err
	}
	return time.Time{}, fmt.Errorf("unrecognised timestamp %q: %w", s, lastErr)
}
