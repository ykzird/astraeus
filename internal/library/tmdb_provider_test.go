package library

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestTMDBProvider_FetchMetadata_Movie(t *testing.T) {
	t.Parallel()

	var gotPath, gotQuery, gotKey, gotYear string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("query")
		gotKey = r.URL.Query().Get("api_key")
		gotYear = r.URL.Query().Get("year")

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{
			"id": 438631,
			"title": "Dune",
			"overview": "Paul Atreides travels to Arrakis.",
			"poster_path": "/poster.jpg",
			"backdrop_path": "/backdrop.jpg",
			"release_date": "2021-10-22"
		}]}`))
	}))
	defer server.Close()

	provider := NewTMDBProvider("test-key")
	provider.baseURL = server.URL

	entity := &MediaEntity{
		ID:       "entity-1",
		Type:     MovieEntity,
		Name:     "Dune",
		Metadata: &MetadataSet{Extra: map[string]string{"year": "2021"}},
	}

	meta, err := provider.FetchMetadata(context.Background(), entity)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}

	if gotPath != "/search/movie" {
		t.Errorf("request path = %q, want /search/movie", gotPath)
	}
	if gotQuery != "Dune" {
		t.Errorf("query = %q, want Dune", gotQuery)
	}
	if gotKey != "test-key" {
		t.Errorf("api_key = %q, want test-key", gotKey)
	}
	if gotYear != "2021" {
		t.Errorf("year = %q, want 2021", gotYear)
	}

	if meta.Title != "Dune" {
		t.Errorf("title = %q, want Dune", meta.Title)
	}
	if meta.Provider != "tmdb" {
		t.Errorf("provider = %q, want tmdb", meta.Provider)
	}
	if meta.Extra["tmdb_id"] != "438631" {
		t.Errorf("tmdb_id = %q, want 438631", meta.Extra["tmdb_id"])
	}
	if meta.Extra["year"] != "2021" {
		t.Errorf("year = %q, want 2021", meta.Extra["year"])
	}
}

func TestTMDBProvider_FetchMetadata_Series(t *testing.T) {
	t.Parallel()

	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"id":1396,"name":"Breaking Bad","overview":"A chemistry teacher.","first_air_date":"2008-01-20"}]}`))
	}))
	defer server.Close()

	provider := NewTMDBProvider("test-key")
	provider.baseURL = server.URL

	meta, err := provider.FetchMetadata(context.Background(), &MediaEntity{
		ID:   "entity-2",
		Type: SeriesEntity,
		Name: "Breaking Bad",
	})
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}
	if gotPath != "/search/tv" {
		t.Errorf("request path = %q, want /search/tv", gotPath)
	}
	if meta.Title != "Breaking Bad" {
		t.Errorf("title = %q, want Breaking Bad", meta.Title)
	}
	if meta.Extra["year"] != "2008" {
		t.Errorf("year = %q, want 2008", meta.Extra["year"])
	}
}

func TestTMDBProvider_FetchMetadata_EpisodesDoNotHitTheNetwork(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s for an episode entity", r.URL.Path)
	}))
	defer server.Close()

	provider := NewTMDBProvider("test-key")
	provider.baseURL = server.URL

	entity := &MediaEntity{
		ID:       "entity-3",
		Type:     EpisodeEntity,
		Name:     "S02E05 - Breakage",
		Metadata: &MetadataSet{Extra: map[string]string{"season": "2", "episode": "5"}},
	}
	meta, err := provider.FetchMetadata(context.Background(), entity)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}
	if meta.Title != "S02E05 - Breakage" {
		t.Errorf("title = %q, want the entity name", meta.Title)
	}
	if meta.Extra["episode"] != "5" {
		t.Errorf("episode = %q, want 5 to be carried over", meta.Extra["episode"])
	}
}

func TestTMDBProvider_FetchMetadata_Errors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		apiKey     string
		statusCode int
		body       string
		wantErrIs  error
	}{
		{
			name:   "missing API key",
			apiKey: "",
		},
		{
			name:       "no results",
			apiKey:     "test-key",
			statusCode: http.StatusOK,
			body:       `{"results":[]}`,
			wantErrIs:  ErrNotFound,
		},
		{
			name:       "server error",
			apiKey:     "test-key",
			statusCode: http.StatusInternalServerError,
			body:       `{"status_message":"boom"}`,
		},
		{
			name:       "malformed body",
			apiKey:     "test-key",
			statusCode: http.StatusOK,
			body:       `{not json`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				status := tt.statusCode
				if status == 0 {
					status = http.StatusOK
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			provider := NewTMDBProvider(tt.apiKey)
			provider.baseURL = server.URL

			_, err := provider.FetchMetadata(context.Background(), &MediaEntity{
				ID:   "entity-4",
				Type: MovieEntity,
				Name: "Dune",
			})
			if err == nil {
				t.Fatal("expected an error")
			}
			if tt.wantErrIs != nil && !errors.Is(err, tt.wantErrIs) {
				t.Errorf("error = %v, want it to wrap %v", err, tt.wantErrIs)
			}
		})
	}
}

func TestTMDBProvider_FetchMetadata_UnsupportedType(t *testing.T) {
	t.Parallel()

	provider := NewTMDBProvider("test-key")

	_, err := provider.FetchMetadata(context.Background(), &MediaEntity{
		ID:   "entity-5",
		Type: EntityType("Playlist"),
		Name: "Whatever",
	})
	if err == nil {
		t.Fatal("expected an error for an unsupported entity type")
	}
}
