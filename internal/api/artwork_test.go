package api

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ykzird/astraeus/internal/images"
	"github.com/ykzird/astraeus/internal/library"
)

// artworkOrigin stands in for the remote image CDN.
// artworkBytes is what the stub origin serves: a body with JPEG magic, so the
// proxy's own checks accept it. Named rather than repeated, so the fixture and the
// assertion cannot drift apart.
var artworkBytes = append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte("artwork-bytes")...)

func artworkOrigin(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()

	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "image/jpeg")
		// A body the proxy recognises as an image. The bytes have to be a real
		// format now: the proxy checks the magic bytes as well as the declared
		// type, so a placeholder string is refused rather than cached as a JPEG
		// (A-8 of the 2026-10-09 review).
		_, _ = w.Write(artworkBytes)
	}))
	t.Cleanup(server.Close)

	return server, &requests
}

func mustImageProxy(t *testing.T, upstream string) *images.Proxy {
	t.Helper()

	proxy, err := images.New(images.Config{
		BaseURL:  upstream,
		CacheDir: t.TempDir(),
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("creating image proxy: %v", err)
	}
	return proxy
}

// entityWithArtwork seeds an entity and attaches artwork references to it.
func entityWithArtwork(t *testing.T, env *testEnv, poster, backdrop string) library.MediaEntity {
	t.Helper()

	entity, _ := seedPlayableEntity(t, env, "Dune (2021).mkv", "bytes")
	entity.Metadata = &library.MetadataSet{
		Title:        "Dune",
		PosterPath:   poster,
		BackdropPath: backdrop,
		Provider:     "tmdb",
	}
	if err := env.repo.UpdateEntity(context.Background(), &entity); err != nil {
		t.Fatalf("updating entity: %v", err)
	}
	return entity
}

func TestEntityResponsesExposeArtworkURLs(t *testing.T) {
	t.Parallel()

	origin, requests := artworkOrigin(t)
	env := newTestEnv(t, withImages(mustImageProxy(t, origin.URL)))
	entity := entityWithArtwork(t, env, "/poster123.jpg", "/backdrop456.jpg")

	recorder := env.do(t, http.MethodGet, "/api/entities/"+entity.ID, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", recorder.Code, recorder.Body.String())
	}
	detail := decodeBody[entityDetail](t, recorder)

	if detail.Entity.PosterURL != "/api/images/w500/poster123.jpg" {
		t.Errorf("poster_url = %q, want the proxied w500 path", detail.Entity.PosterURL)
	}
	if detail.Entity.BackdropURL != "/api/images/w1280/backdrop456.jpg" {
		t.Errorf("backdrop_url = %q, want the proxied w1280 path", detail.Entity.BackdropURL)
	}

	// The advertised URLs must actually resolve through the proxy.
	for _, path := range []string{detail.Entity.PosterURL, detail.Entity.BackdropURL} {
		recorder = env.do(t, http.MethodGet, path, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", path, recorder.Code)
		}
		if !bytes.Equal(recorder.Body.Bytes(), artworkBytes) {
			t.Errorf("GET %s body = %q, want the upstream bytes", path, recorder.Body.String())
		}
	}

	// Two distinct renditions of two distinct files, then served from cache.
	if got := requests.Load(); got != 2 {
		t.Errorf("upstream requests = %d, want 2 (one per rendition)", got)
	}
	recorder = env.do(t, http.MethodGet, detail.Entity.PosterURL, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("cached fetch status = %d, want 200", recorder.Code)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("upstream requests = %d after a repeat, want 2 (the cache should serve it)", got)
	}
}

func TestEntityListExposesArtworkURLs(t *testing.T) {
	t.Parallel()

	origin, _ := artworkOrigin(t)
	env := newTestEnv(t, withImages(mustImageProxy(t, origin.URL)))
	entity := entityWithArtwork(t, env, "/poster123.jpg", "")

	recorder := env.do(t, http.MethodGet, "/api/entities", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	entities := decodeBody[[]entityResource](t, recorder)
	if len(entities) != 1 {
		t.Fatalf("got %d entities, want 1", len(entities))
	}
	if entities[0].ID != entity.ID {
		t.Errorf("entity id = %q, want %q", entities[0].ID, entity.ID)
	}
	if entities[0].PosterURL == "" {
		t.Error("poster_url is missing from the list payload")
	}
	if entities[0].BackdropURL != "" {
		t.Errorf("backdrop_url = %q, want it omitted when there is no reference", entities[0].BackdropURL)
	}
}

func TestArtworkURLsAreOmittedWithoutAProxy(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)
	entity := entityWithArtwork(t, env, "/poster123.jpg", "/backdrop456.jpg")

	recorder := env.do(t, http.MethodGet, "/api/entities/"+entity.ID, "")
	detail := decodeBody[entityDetail](t, recorder)

	if detail.Entity.PosterURL != "" || detail.Entity.BackdropURL != "" {
		t.Errorf("artwork URLs should be omitted without a proxy: %+v", detail.Entity)
	}
	// The raw metadata reference is still there for clients that want it.
	if detail.Entity.Metadata == nil || detail.Entity.Metadata.PosterPath != "/poster123.jpg" {
		t.Errorf("metadata poster path = %+v, want it preserved", detail.Entity.Metadata)
	}
}

func TestArtworkURLsRejectUnusableReferences(t *testing.T) {
	t.Parallel()

	origin, requests := artworkOrigin(t)
	env := newTestEnv(t, withImages(mustImageProxy(t, origin.URL)))

	tests := []struct {
		name     string
		poster   string
		backdrop string
	}{
		{name: "absolute url", poster: "https://evil.example.com/a.jpg", backdrop: "http://evil.example.com/b.jpg"},
		{name: "nested path", poster: "/a/b.jpg", backdrop: "/deep/path.jpg"},
		{name: "traversal", poster: "/../../etc/passwd", backdrop: "/../secret.jpg"},
		{name: "unlisted extension", poster: "/a.svg", backdrop: "/b.gif"},
		{name: "empty", poster: "", backdrop: ""},
	}

	// These subtests share one database, and SQLite serialises writers, so they
	// stay sequential.
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entity := entityWithArtwork(t, env, tt.poster, tt.backdrop)
			recorder := env.do(t, http.MethodGet, "/api/entities/"+entity.ID, "")
			detail := decodeBody[entityDetail](t, recorder)

			if detail.Entity.PosterURL != "" {
				t.Errorf("poster_url = %q, want it omitted for reference %q", detail.Entity.PosterURL, tt.poster)
			}
			if detail.Entity.BackdropURL != "" {
				t.Errorf("backdrop_url = %q, want it omitted for reference %q", detail.Entity.BackdropURL, tt.backdrop)
			}
		})
	}

	if got := requests.Load(); got != 0 {
		t.Errorf("the upstream origin was contacted %d times for unusable references, want 0", got)
	}
}

func TestImageRoute_UnavailableWithoutProxy(t *testing.T) {
	t.Parallel()

	recorder := newTestEnv(t).do(t, http.MethodGet, "/api/images/w500/abc.jpg", "")
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", recorder.Code)
	}
	if code := decodeBody[errorBody](t, recorder).Code; code != "images_unavailable" {
		t.Errorf("error code = %q, want images_unavailable", code)
	}
}

func TestImageRoute_RejectsInvalidRequests(t *testing.T) {
	t.Parallel()

	origin, requests := artworkOrigin(t)
	env := newTestEnv(t, withImages(mustImageProxy(t, origin.URL)))

	for _, path := range []string{
		"/api/images/huge/abc.jpg",
		"/api/images/w500/abc.svg",
		"/api/images/w500/abc.jpg.exe",
	} {
		recorder := env.do(t, http.MethodGet, path, "")
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("GET %s status = %d, want 400", path, recorder.Code)
		}
	}

	if got := requests.Load(); got != 0 {
		t.Errorf("the upstream origin was contacted %d times for invalid requests, want 0", got)
	}
}

func TestSystemCapabilitiesReportsArtwork(t *testing.T) {
	t.Parallel()

	origin, _ := artworkOrigin(t)

	withProxy := newTestEnv(t, withImages(mustImageProxy(t, origin.URL)))
	recorder := withProxy.do(t, http.MethodGet, "/api/system/capabilities", "")
	if !decodeBody[systemCapabilitiesResponse](t, recorder).ArtworkEnabled {
		t.Error("artwork_enabled = false, want true when a proxy is configured")
	}

	withoutProxy := newTestEnv(t)
	recorder = withoutProxy.do(t, http.MethodGet, "/api/system/capabilities", "")
	if decodeBody[systemCapabilitiesResponse](t, recorder).ArtworkEnabled {
		t.Error("artwork_enabled = true, want false without a proxy")
	}
}

func TestArtworkResponsesAreCacheable(t *testing.T) {
	t.Parallel()

	origin, _ := artworkOrigin(t)
	env := newTestEnv(t, withImages(mustImageProxy(t, origin.URL)))

	recorder := env.do(t, http.MethodGet, "/api/images/w500/abc.jpg", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := recorder.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("cache control = %q, want an immutable directive", got)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "image/") {
		t.Errorf("content type = %q, want an image type", got)
	}
}
