package metadata

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ykzird/astraeus/internal/library"
)

func TestTMDB_FetchMetadata_Movie(t *testing.T) {
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

	provider := NewTMDB("test-key")
	provider.baseURL = server.URL

	entity := &library.MediaEntity{
		ID:       "entity-1",
		Type:     library.MovieEntity,
		Name:     "Dune",
		Metadata: &library.MetadataSet{Extra: map[string]string{"year": "2021"}},
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

func TestTMDB_FetchMetadata_Series(t *testing.T) {
	t.Parallel()

	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"id":1396,"name":"Breaking Bad","overview":"A chemistry teacher.","first_air_date":"2008-01-20"}]}`))
	}))
	defer server.Close()

	provider := NewTMDB("test-key")
	provider.baseURL = server.URL

	meta, err := provider.FetchMetadata(context.Background(), &library.MediaEntity{
		ID:   "entity-2",
		Type: library.SeriesEntity,
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

func TestTMDB_FetchMetadata_EpisodesDoNotHitTheNetwork(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to %s for an episode entity", r.URL.Path)
	}))
	defer server.Close()

	provider := NewTMDB("test-key")
	provider.baseURL = server.URL

	entity := &library.MediaEntity{
		ID:       "entity-3",
		Type:     library.EpisodeEntity,
		Name:     "S02E05 - Breakage",
		Metadata: &library.MetadataSet{Extra: map[string]string{"season": "2", "episode": "5"}},
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

func TestTMDB_FetchMetadata_Errors(t *testing.T) {
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
			wantErrIs:  library.ErrNotFound,
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

			provider := NewTMDB(tt.apiKey)
			provider.baseURL = server.URL

			_, err := provider.FetchMetadata(context.Background(), &library.MediaEntity{
				ID:   "entity-4",
				Type: library.MovieEntity,
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

func TestTMDB_FetchMetadata_UnsupportedType(t *testing.T) {
	t.Parallel()

	provider := NewTMDB("test-key")

	_, err := provider.FetchMetadata(context.Background(), &library.MediaEntity{
		ID:   "entity-5",
		Type: library.EntityType("Playlist"),
		Name: "Whatever",
	})
	if err == nil {
		t.Fatal("expected an error for an unsupported entity type")
	}
}

// TestTMDB_UnreachableDoesNotLeakTheKey is the regression test for L-10.
//
// The v3 API key travels as a query parameter, and net/http reports a failed
// request as a *url.Error whose message embeds the whole URL. A worker that
// logs the wrapped error - which it does - therefore logged the key in full
// every time TMDB was unreachable.
func TestTMDB_UnreachableDoesNotLeakTheKey(t *testing.T) {
	t.Parallel()

	const key = "SECRET-KEY-123"

	provider := NewTMDB(key)
	// A port nothing is listening on: the request fails at the transport, which
	// is the path that produces the *url.Error.
	provider.baseURL = "http://127.0.0.1:1"

	_, err := provider.FetchMetadata(context.Background(), &library.MediaEntity{
		ID:   "entity-6",
		Type: library.MovieEntity,
		Name: "Dune",
	})
	if err == nil {
		t.Fatal("expected the unreachable provider to fail")
	}
	if strings.Contains(err.Error(), key) {
		t.Errorf("the API key leaked into the error: %v", err)
	}
	if !strings.Contains(err.Error(), "api_key=REDACTED") {
		t.Errorf("error = %v, want the query redacted so the cause stays readable", err)
	}
	// The error must still say what failed.
	if !errors.As(err, new(*url.Error)) {
		t.Errorf("error = %v, want it to keep the *url.Error cause", err)
	}
}

func TestRedactQuery(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "the key is replaced, the query is kept",
			raw:  "https://api.themoviedb.org/3/search/movie?api_key=secret&query=Dune",
			want: "https://api.themoviedb.org/3/search/movie?api_key=REDACTED&query=REDACTED",
		},
		{
			name: "no query is unchanged",
			raw:  "https://api.themoviedb.org/3/search/movie",
			want: "https://api.themoviedb.org/3/search/movie",
		},
		{
			name: "an unparseable URL is dropped rather than echoed",
			raw:  "http://[::1]:namedport/search?api_key=secret",
			want: "REDACTED",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := redactQuery(tt.raw); got != tt.want {
				t.Errorf("redactQuery(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// TestTMDB_RefusesAnOversizedResponse is L-18's third item.
//
// The response body went straight into a JSON decoder with no bound, and the body
// is somebody else's: a misconfiguration, a proxy's error page or a hostile
// response would be read into memory without limit.
func TestTMDB_RefusesAnOversizedResponse(t *testing.T) {
	t.Parallel()

	// A response that is valid JSON of the expected shape, but far larger than the
	// limit. Being valid is the point: the size is what has to refuse it.
	var builder strings.Builder
	builder.WriteString(`{"results":[{"id":1,"title":"`)
	builder.WriteString(strings.Repeat("x", maxResponseBytes))
	builder.WriteString(`"}]}`)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(builder.String()))
	}))
	t.Cleanup(server.Close)

	provider := NewTMDB("test-key")
	provider.baseURL = server.URL

	entity := library.MediaEntity{ID: "e1", Type: library.MovieEntity, Name: "Dune"}
	if _, err := provider.FetchMetadata(context.Background(), &entity); err == nil {
		t.Errorf("a %d-byte response was accepted, want a refusal: the body is somebody "+
			"else's and has no size guarantee", builder.Len())
	} else if !strings.Contains(err.Error(), "exceeded") {
		t.Errorf("the refusal does not say the response was too large: %v", err)
	}

	// And a normal response still works, so the bound is not refusing everything.
	small := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"id":1,"title":"Dune","release_date":"2021-10-22"}]}`))
	}))
	t.Cleanup(small.Close)
	provider.baseURL = small.URL

	meta, err := provider.FetchMetadata(context.Background(), &entity)
	if err != nil {
		t.Fatalf("a normal response was refused: %v", err)
	}
	if meta == nil || meta.Title != "Dune" {
		t.Errorf("meta = %+v, want the parsed title", meta)
	}
}
