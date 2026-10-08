package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"

	"github.com/jok/astraeus-media/internal/images"
	"github.com/jok/astraeus-media/internal/library"
	"github.com/jok/astraeus-media/internal/metadata"
	"github.com/jok/astraeus-media/internal/observability"
	"github.com/jok/astraeus-media/internal/streaming"
)

type testEnv struct {
	server *Server
	repo   library.Repository
}

// envOption customises the dependencies of a test server.
type envOption func(*Deps)

func withProber(prober streaming.Prober) envOption {
	return func(deps *Deps) { deps.Prober = prober }
}

func withStreams(streams StreamManager) envOption {
	return func(deps *Deps) { deps.Streams = streams }
}

func withServerCapability(capability streaming.ServerCapability) envOption {
	return func(deps *Deps) { deps.Server = capability }
}

func withWebDir(dir string) envOption {
	return func(deps *Deps) { deps.WebDir = dir }
}

func withImages(proxy *images.Proxy) envOption {
	return func(deps *Deps) { deps.Images = proxy }
}

func withSubtitles(converter SubtitleConverter) envOption {
	return func(deps *Deps) { deps.Subtitles = converter }
}

func newTestEnv(t *testing.T, opts ...envOption) *testEnv {
	t.Helper()

	db, err := sqlx.Connect("sqlite", library.SQLiteDSN(filepath.Join(t.TempDir(), "api-test.db")))
	if err != nil {
		t.Fatalf("connecting to test database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	repo := library.NewSQLiteRepository(db)
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatalf("migrating test database: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	scanner := library.NewScanner(repo, logger)
	deps := Deps{
		Repository: repo,
		Scanner:    scanner,
		Scheduler:  library.NewScanScheduler(repo, scanner, 0, logger),
		Worker:     metadata.NewWorker(repo, metadata.NewMock(), time.Hour, logger),
		Metrics:    observability.New(),
		Logger:     logger,
		// A running server probes this at startup, so software encoders are
		// always present where ffmpeg is. Negotiation is server-aware now: a
		// test env that declares no encoders is correctly told a transcode
		// cannot be delivered. Tests that care about a specific layout
		// override this with withServerCapability.
		Server: streaming.ServerCapability{
			FFmpegAvailable:  true,
			FFprobeAvailable: true,
			VideoEncoders:    []string{"libx264", "libx265"},
			AudioEncoders:    []string{"aac"},
			HLS:              true,
		},
	}
	for _, opt := range opts {
		opt(&deps)
	}
	return &testEnv{server: NewServer(deps), repo: repo}
}

func (e *testEnv) do(t *testing.T, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	e.server.Handler().ServeHTTP(recorder, req)
	return recorder
}

func decodeBody[T any](t *testing.T, recorder *httptest.ResponseRecorder) T {
	t.Helper()

	var payload T
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decoding response %q: %v", recorder.Body.String(), err)
	}
	return payload
}

func createLibrary(t *testing.T, env *testEnv, name, path, kind string) library.Library {
	t.Helper()

	recorder := env.do(t, http.MethodPost, "/api/libraries",
		`{"name":"`+name+`","path":"`+path+`","kind":"`+kind+`"}`)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("creating library: status %d body %s", recorder.Code, recorder.Body.String())
	}
	return decodeBody[library.Library](t, recorder)
}

func writeMediaFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("creating directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing media file: %v", err)
	}
}

func TestHealth(t *testing.T) {
	t.Parallel()

	recorder := newTestEnv(t).do(t, http.MethodGet, "/api/health", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	body := decodeBody[map[string]string](t, recorder)
	if body["status"] != "ok" {
		t.Errorf("status = %q, want ok", body["status"])
	}
}

func TestCreateLibrary_Validation(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)
	validDir := t.TempDir()
	missingDir := filepath.Join(t.TempDir(), "nope")

	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{name: "missing name", body: `{"path":"` + validDir + `","kind":"movies"}`, wantStatus: http.StatusBadRequest},
		{name: "missing path", body: `{"name":"Movies","kind":"movies"}`, wantStatus: http.StatusBadRequest},
		{name: "unknown kind", body: `{"name":"Movies","path":"` + validDir + `","kind":"music"}`, wantStatus: http.StatusBadRequest},
		{name: "path does not exist", body: `{"name":"Movies","path":"` + missingDir + `","kind":"movies"}`, wantStatus: http.StatusBadRequest},
		{name: "unknown field", body: `{"name":"Movies","path":"` + validDir + `","kind":"movies","extra":1}`, wantStatus: http.StatusBadRequest},
		{name: "malformed json", body: `{"name":`, wantStatus: http.StatusBadRequest},
		{name: "empty body", body: "", wantStatus: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			recorder := env.do(t, http.MethodPost, "/api/libraries", tt.body)
			if recorder.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (body %s)", recorder.Code, tt.wantStatus, recorder.Body.String())
			}
		})
	}
}

func TestCreateLibrary_RejectsDuplicatePath(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)
	dir := t.TempDir()
	createLibrary(t, env, "Movies", dir, "movies")

	recorder := env.do(t, http.MethodPost, "/api/libraries",
		`{"name":"Movies again","path":"`+dir+`","kind":"movies"}`)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", recorder.Code, recorder.Body.String())
	}
}

func TestLibraryLifecycle(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)
	dir := t.TempDir()
	created := createLibrary(t, env, "Movies", dir, "movies")

	recorder := env.do(t, http.MethodGet, "/api/libraries", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("listing libraries: status %d", recorder.Code)
	}
	libraries := decodeBody[[]library.Library](t, recorder)
	if len(libraries) != 1 || libraries[0].ID != created.ID {
		t.Fatalf("libraries = %+v, want the created library", libraries)
	}

	recorder = env.do(t, http.MethodGet, "/api/libraries/"+created.ID, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("getting library: status %d", recorder.Code)
	}
	got := decodeBody[library.Library](t, recorder)
	if got.Kind != library.MoviesLibrary {
		t.Errorf("kind = %q, want %q", got.Kind, library.MoviesLibrary)
	}

	recorder = env.do(t, http.MethodDelete, "/api/libraries/"+created.ID, "")
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("deleting library: status %d", recorder.Code)
	}

	recorder = env.do(t, http.MethodGet, "/api/libraries/"+created.ID, "")
	if recorder.Code != http.StatusNotFound {
		t.Errorf("getting a deleted library: status = %d, want 404", recorder.Code)
	}
}

func TestScanAndBrowseLibrary(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)
	root := t.TempDir()
	writeMediaFile(t, filepath.Join(root, "Dune (2021).mkv"), "dune")
	writeMediaFile(t, filepath.Join(root, "Arrival (2016).mp4"), "arrival")

	lib := createLibrary(t, env, "Movies", root, "movies")

	recorder := env.do(t, http.MethodPost, "/api/libraries/"+lib.ID+"/scan", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("scanning: status %d body %s", recorder.Code, recorder.Body.String())
	}
	result := decodeBody[library.ScanResult](t, recorder)
	if result.FilesSeen != 2 || result.EntitiesCreated != 2 {
		t.Errorf("scan result = %+v, want 2 files and 2 entities", result)
	}

	// Scanning again is idempotent over HTTP too.
	recorder = env.do(t, http.MethodPost, "/api/libraries/"+lib.ID+"/scan", "")
	second := decodeBody[library.ScanResult](t, recorder)
	if second.EntitiesCreated != 0 || second.ObjectsCreated != 0 {
		t.Errorf("second scan created entities=%d objects=%d, want 0 and 0",
			second.EntitiesCreated, second.ObjectsCreated)
	}

	recorder = env.do(t, http.MethodGet, "/api/libraries/"+lib.ID+"/entities", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("listing entities: status %d", recorder.Code)
	}
	entities := decodeBody[[]library.MediaEntity](t, recorder)
	if len(entities) != 2 {
		t.Fatalf("got %d entities, want 2", len(entities))
	}

	// Entities are incomplete until metadata is attached, and the status
	// filter exposes exactly that view.
	recorder = env.do(t, http.MethodGet, "/api/libraries/"+lib.ID+"/entities?status=Incomplete", "")
	incomplete := decodeBody[[]library.MediaEntity](t, recorder)
	if len(incomplete) != 2 {
		t.Errorf("incomplete entities = %d, want 2", len(incomplete))
	}

	// Entities are listed in a stable order; find the one backed by the .mkv.
	var dune library.MediaEntity
	for _, entity := range entities {
		if entity.Name == "Dune" {
			dune = entity
		}
	}
	if dune.ID == "" {
		t.Fatalf("no entity named Dune in %+v", entities)
	}

	recorder = env.do(t, http.MethodGet, "/api/entities/"+dune.ID, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("getting entity: status %d", recorder.Code)
	}
	detail := decodeBody[entityDetail](t, recorder)
	if detail.Entity.ID != dune.ID {
		t.Errorf("entity id = %q, want %q", detail.Entity.ID, dune.ID)
	}
	if len(detail.Objects) != 1 {
		t.Errorf("objects = %d, want 1 (the file that backs this entity)", len(detail.Objects))
	}
	if detail.Objects[0].MimeType != "video/x-matroska" {
		t.Errorf("mime type = %q, want video/x-matroska", detail.Objects[0].MimeType)
	}
}

func TestEnrichEndpoint(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)
	root := t.TempDir()
	writeMediaFile(t, filepath.Join(root, "Dune (2021).mkv"), "dune")
	lib := createLibrary(t, env, "Movies", root, "movies")

	env.do(t, http.MethodPost, "/api/libraries/"+lib.ID+"/scan", "")

	recorder := env.do(t, http.MethodPost, "/api/metadata/enrich", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("enriching: status %d body %s", recorder.Code, recorder.Body.String())
	}
	result := decodeBody[metadata.EnrichResult](t, recorder)
	if result.Enriched != 1 {
		t.Errorf("enriched = %d, want 1", result.Enriched)
	}

	recorder = env.do(t, http.MethodGet, "/api/libraries/"+lib.ID+"/entities?status=Complete", "")
	complete := decodeBody[[]library.MediaEntity](t, recorder)
	if len(complete) != 1 {
		t.Fatalf("complete entities = %d, want 1", len(complete))
	}
	if complete[0].Metadata == nil || complete[0].Metadata.Title != "Dune" {
		t.Errorf("metadata = %+v, want a title of Dune", complete[0].Metadata)
	}
}

func TestUnknownAPIRouteReturnsJSONEnvelope(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "unknown api path", method: http.MethodGet, path: "/api/nope"},
		{name: "wrong method on a known path", method: http.MethodPost, path: "/api/health"},
		{name: "unknown streaming path", method: http.MethodGet, path: "/hls/nope"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			recorder := env.do(t, tt.method, tt.path, "")
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("status = %d, want 404 (body %s)", recorder.Code, recorder.Body.String())
			}
			if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
				t.Errorf("content type = %q, want JSON", got)
			}
			body := decodeBody[errorBody](t, recorder)
			if body.Code != "not_found" {
				t.Errorf("error code = %q, want not_found", body.Code)
			}
		})
	}
}

func TestEntity_TopLevelParentIsOmitted(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)
	root := t.TempDir()
	writeMediaFile(t, filepath.Join(root, "Dune (2021).mkv"), "dune")
	lib := createLibrary(t, env, "Movies", root, "movies")
	env.do(t, http.MethodPost, "/api/libraries/"+lib.ID+"/scan", "")

	recorder := env.do(t, http.MethodGet, "/api/libraries/"+lib.ID+"/entities", "")
	entities := decodeBody[[]library.MediaEntity](t, recorder)
	if len(entities) != 1 {
		t.Fatalf("got %d entities, want 1", len(entities))
	}
	if entities[0].ParentID != nil {
		t.Errorf("parent id = %v, want nil for a top-level entity", *entities[0].ParentID)
	}
}

func TestScanAllEndpoint(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)

	moviesRoot := t.TempDir()
	writeMediaFile(t, filepath.Join(moviesRoot, "Dune (2021).mkv"), "dune")
	showsRoot := t.TempDir()
	writeMediaFile(t, filepath.Join(showsRoot, "Show", "Season 01", "Show.S01E01.mkv"), "ep")
	createLibrary(t, env, "Movies", moviesRoot, "movies")
	createLibrary(t, env, "Shows", showsRoot, "shows")

	recorder := env.do(t, http.MethodPost, "/api/scan", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}

	outcomes := decodeBody[[]library.ScanOutcome](t, recorder)
	if len(outcomes) != 2 {
		t.Fatalf("got %d outcomes, want 2", len(outcomes))
	}
	files := 0
	for _, outcome := range outcomes {
		if outcome.Error != "" {
			t.Errorf("library %q failed: %s", outcome.LibraryName, outcome.Error)
		}
		files += outcome.Result.FilesSeen
	}
	if files != 2 {
		t.Errorf("files seen = %d, want 2", files)
	}

	// Running it again must not create anything.
	recorder = env.do(t, http.MethodPost, "/api/scan", "")
	outcomes = decodeBody[[]library.ScanOutcome](t, recorder)
	for _, outcome := range outcomes {
		if outcome.Result.EntitiesCreated != 0 {
			t.Errorf("library %q created %d entities on a repeat scan",
				outcome.LibraryName, outcome.Result.EntitiesCreated)
		}
	}
}

func TestScanAllEndpoint_UnavailableWithoutScheduler(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t, func(deps *Deps) { deps.Scheduler = nil })

	recorder := env.do(t, http.MethodPost, "/api/scan", "")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", recorder.Code, recorder.Body.String())
	}
	if code := decodeBody[errorBody](t, recorder).Code; code != "scanning_unavailable" {
		t.Errorf("error code = %q, want scanning_unavailable", code)
	}
}

func TestEntityAndLibraryNotFound(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)

	tests := []struct {
		name   string
		method string
		path   string
	}{
		{name: "missing entity", method: http.MethodGet, path: "/api/entities/nope"},
		{name: "missing library", method: http.MethodGet, path: "/api/libraries/nope"},
		{name: "scan missing library", method: http.MethodPost, path: "/api/libraries/nope/scan"},
		{name: "delete missing library", method: http.MethodDelete, path: "/api/libraries/nope"},
		{name: "entities of missing library", method: http.MethodGet, path: "/api/libraries/nope/entities"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			recorder := env.do(t, tt.method, tt.path, "")
			if recorder.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404 (body %s)", recorder.Code, recorder.Body.String())
			}
			body := decodeBody[errorBody](t, recorder)
			if body.Code != "not_found" {
				t.Errorf("error code = %q, want not_found", body.Code)
			}
		})
	}
}

func TestEntitiesOfShowsLibraryExposeHierarchy(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)
	root := t.TempDir()
	writeMediaFile(t, filepath.Join(root, "Breaking Bad", "Season 01", "Breaking Bad S01E01 - Pilot.mkv"), "1")

	lib := createLibrary(t, env, "Shows", root, "shows")
	env.do(t, http.MethodPost, "/api/libraries/"+lib.ID+"/scan", "")

	recorder := env.do(t, http.MethodGet, "/api/libraries/"+lib.ID+"/entities", "")
	entities := decodeBody[[]library.MediaEntity](t, recorder)

	var seriesID string
	for _, entity := range entities {
		if entity.Type == library.SeriesEntity {
			seriesID = entity.ID
		}
	}
	if seriesID == "" {
		t.Fatalf("no series entity in %+v", entities)
	}

	recorder = env.do(t, http.MethodGet, "/api/entities/"+seriesID, "")
	detail := decodeBody[entityDetail](t, recorder)
	if len(detail.Children) != 1 {
		t.Fatalf("series children = %d, want 1 season", len(detail.Children))
	}
	season := detail.Children[0]
	if season.Type != library.SeasonEntity {
		t.Errorf("child type = %q, want Season", season.Type)
	}

	recorder = env.do(t, http.MethodGet, "/api/entities/"+season.ID, "")
	seasonDetail := decodeBody[entityDetail](t, recorder)
	if len(seasonDetail.Children) != 1 {
		t.Fatalf("season children = %d, want 1 episode", len(seasonDetail.Children))
	}
	if seasonDetail.Parent == nil || seasonDetail.Parent.ID != seriesID {
		t.Errorf("season parent = %+v, want the series", seasonDetail.Parent)
	}
	episode := seasonDetail.Children[0]
	if episode.Name != "S01E01 - Pilot" {
		t.Errorf("episode name = %q, want S01E01 - Pilot", episode.Name)
	}
}
