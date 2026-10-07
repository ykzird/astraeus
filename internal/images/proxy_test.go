package images

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// testLogger keeps proxy logging out of the test output.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stubOrigin serves a small fake image and counts requests.
type stubOrigin struct {
	requests atomic.Int64
	status   int
	body     []byte
	delay    time.Duration
}

func (s *stubOrigin) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		if s.delay > 0 {
			time.Sleep(s.delay)
		}
		if s.status != 0 && s.status != http.StatusOK {
			w.WriteHeader(s.status)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(s.body)
	})
}

func newTestProxy(t *testing.T, origin *stubOrigin) (*Proxy, *httptest.Server) {
	t.Helper()

	server := httptest.NewServer(origin.handler())
	t.Cleanup(server.Close)

	proxy, err := New(Config{
		BaseURL:  server.URL,
		CacheDir: t.TempDir(),
		Client:   server.Client(),
		Logger:   testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return proxy, server
}

func TestProxy_FetchesThenServesFromCache(t *testing.T) {
	t.Parallel()

	origin := &stubOrigin{body: []byte("jpeg-bytes")}
	proxy, _ := newTestProxy(t, origin)

	serve := func() *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		proxy.Serve(recorder, httptest.NewRequest(http.MethodGet, "/api/images/w500/abc.jpg", nil), "w500", "abc.jpg")
		return recorder
	}

	first := serve()
	if first.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", first.Code)
	}
	if first.Body.String() != "jpeg-bytes" {
		t.Errorf("body = %q, want the upstream bytes", first.Body.String())
	}
	if got := first.Header().Get("Content-Type"); got != "image/jpeg" {
		t.Errorf("content type = %q, want image/jpeg", got)
	}
	if got := first.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Errorf("cache control = %q, want an immutable directive", got)
	}

	second := serve()
	if second.Code != http.StatusOK {
		t.Fatalf("second request status = %d, want 200", second.Code)
	}
	if got := origin.requests.Load(); got != 1 {
		t.Errorf("upstream was contacted %d times, want 1 (the second request must hit the cache)", got)
	}
	if proxy.CachedCount() != 1 {
		t.Errorf("cached count = %d, want 1", proxy.CachedCount())
	}
}

func TestProxy_DistinctSizesAreCachedSeparately(t *testing.T) {
	t.Parallel()

	origin := &stubOrigin{body: []byte("jpeg-bytes")}
	proxy, _ := newTestProxy(t, origin)

	for _, size := range []string{"w185", "w500"} {
		recorder := httptest.NewRecorder()
		proxy.Serve(recorder, httptest.NewRequest(http.MethodGet, "/", nil), size, "abc.jpg")
		if recorder.Code != http.StatusOK {
			t.Fatalf("size %s: status = %d", size, recorder.Code)
		}
	}

	if got := origin.requests.Load(); got != 2 {
		t.Errorf("upstream was contacted %d times, want 2", got)
	}
}

func TestProxy_RejectsInvalidRequests(t *testing.T) {
	t.Parallel()

	origin := &stubOrigin{body: []byte("jpeg-bytes")}
	proxy, _ := newTestProxy(t, origin)

	tests := []struct {
		name string
		size string
		file string
	}{
		{name: "unknown size", size: "w9999", file: "abc.jpg"},
		{name: "size with a path separator", size: "../w500", file: "abc.jpg"},
		{name: "empty size", size: "", file: "abc.jpg"},
		{name: "traversal in file", size: "w500", file: "../../etc/passwd"},
		{name: "nested path in file", size: "w500", file: "sub/abc.jpg"},
		{name: "backslash path in file", size: "w500", file: `sub\abc.jpg`},
		{name: "unlisted extension", size: "w500", file: "abc.sh"},
		{name: "no extension", size: "w500", file: "abc"},
		{name: "hidden file", size: "w500", file: ".secret.jpg"},
		{name: "empty file", size: "w500", file: ""},
		{name: "absolute path", size: "w500", file: "/etc/passwd.jpg"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			recorder := httptest.NewRecorder()
			proxy.Serve(recorder, httptest.NewRequest(http.MethodGet, "/", nil), tt.size, tt.file)
			if recorder.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", recorder.Code)
			}
		})
	}

	if got := origin.requests.Load(); got != 0 {
		t.Errorf("upstream was contacted %d times for invalid requests, want 0", got)
	}
}

func TestProxy_UpstreamFailureIsReportedAndNotCached(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		origin *stubOrigin
	}{
		{name: "upstream 404", origin: &stubOrigin{status: http.StatusNotFound}},
		{name: "upstream 500", origin: &stubOrigin{status: http.StatusInternalServerError}},
		{name: "empty body", origin: &stubOrigin{body: []byte{}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			proxy, _ := newTestProxy(t, tt.origin)

			recorder := httptest.NewRecorder()
			proxy.Serve(recorder, httptest.NewRequest(http.MethodGet, "/", nil), "w500", "abc.jpg")
			if recorder.Code != http.StatusBadGateway {
				t.Errorf("status = %d, want 502", recorder.Code)
			}
			if proxy.CachedCount() != 0 {
				t.Errorf("a failed fetch must not be cached, cached count = %d", proxy.CachedCount())
			}
		})
	}
}

func TestProxy_RejectsOversizedImages(t *testing.T) {
	t.Parallel()

	origin := &stubOrigin{body: []byte(strings.Repeat("x", 2048))}
	server := httptest.NewServer(origin.handler())
	t.Cleanup(server.Close)

	proxy, err := New(Config{
		BaseURL:  server.URL,
		CacheDir: t.TempDir(),
		Client:   server.Client(),
		MaxBytes: 1024,
		Logger:   testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	recorder := httptest.NewRecorder()
	proxy.Serve(recorder, httptest.NewRequest(http.MethodGet, "/", nil), "w500", "big.jpg")
	if recorder.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 for an oversized image", recorder.Code)
	}
	if proxy.CachedCount() != 0 {
		t.Error("an oversized image must not be cached")
	}
}

func TestProxy_UnreachableUpstream(t *testing.T) {
	t.Parallel()

	proxy, err := New(Config{
		BaseURL:  "http://127.0.0.1:1",
		CacheDir: t.TempDir(),
		Client:   &http.Client{Timeout: 2 * time.Second},
		Logger:   testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	recorder := httptest.NewRecorder()
	proxy.Serve(recorder, httptest.NewRequest(http.MethodGet, "/", nil), "w500", "abc.jpg")
	if recorder.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", recorder.Code)
	}
}

func TestProxy_RequiresCacheDir(t *testing.T) {
	t.Parallel()

	if _, err := New(Config{Logger: testLogger()}); err == nil {
		t.Fatal("expected an error when CacheDir is empty")
	}
}

func TestProxy_ContextCancellation(t *testing.T) {
	t.Parallel()

	origin := &stubOrigin{body: []byte("jpeg-bytes"), delay: 2 * time.Second}
	proxy, _ := newTestProxy(t, origin)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	recorder := httptest.NewRecorder()
	proxy.Serve(recorder, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx), "w500", "abc.jpg")
	if recorder.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 for a cancelled request", recorder.Code)
	}
}

func TestPathForReference(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		size      string
		reference string
		want      string
	}{
		{name: "tmdb path", size: "w500", reference: "/abc123.jpg", want: "/api/images/w500/abc123.jpg"},
		{name: "path without a leading slash", size: "w185", reference: "abc123.jpg", want: "/api/images/w185/abc123.jpg"},
		{name: "empty reference", size: "w500", reference: "", want: ""},
		{name: "whitespace reference", size: "w500", reference: "   ", want: ""},
		{name: "absolute URL is not forwarded", size: "w500", reference: "https://example.com/a.jpg", want: ""},
		{name: "nested path is not forwarded", size: "w500", reference: "/a/b.jpg", want: ""},
		{name: "unknown size", size: "huge", reference: "/a.jpg", want: ""},
		{name: "unlisted extension", size: "w500", reference: "/a.svg", want: ""},
		{name: "traversal", size: "w500", reference: "/../../etc/passwd", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := PathForReference(tt.size, tt.reference); got != tt.want {
				t.Errorf("PathForReference(%q, %q) = %q, want %q", tt.size, tt.reference, got, tt.want)
			}
		})
	}
}

func TestValidFileName(t *testing.T) {
	t.Parallel()

	valid := []string{"abc.jpg", "A1_b-c.png", "x.webp", "abc123.jpeg"}
	for _, name := range valid {
		if !validFileName(name) {
			t.Errorf("validFileName(%q) = false, want true", name)
		}
	}

	invalid := []string{"", "..", "../a.jpg", "a/b.jpg", `a\b.jpg`, ".hidden.jpg", "a.svg", "a.jpg.exe", "-a.jpg"}
	for _, name := range invalid {
		if validFileName(name) {
			t.Errorf("validFileName(%q) = true, want false", name)
		}
	}
}

func TestProxy_CacheSurvivesRestart(t *testing.T) {
	t.Parallel()

	origin := &stubOrigin{body: []byte("jpeg-bytes")}
	server := httptest.NewServer(origin.handler())
	t.Cleanup(server.Close)

	cacheDir := t.TempDir()
	cfg := Config{BaseURL: server.URL, CacheDir: cacheDir, Client: server.Client(), Logger: testLogger()}

	first, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	recorder := httptest.NewRecorder()
	first.Serve(recorder, httptest.NewRequest(http.MethodGet, "/", nil), "w500", "abc.jpg")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	// A fresh proxy over the same cache directory must not re-fetch.
	second, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	recorder = httptest.NewRecorder()
	second.Serve(recorder, httptest.NewRequest(http.MethodGet, "/", nil), "w500", "abc.jpg")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status after restart = %d, want 200", recorder.Code)
	}
	if got := origin.requests.Load(); got != 1 {
		t.Errorf("upstream contacted %d times, want 1 (the cache must survive a restart)", got)
	}
}

func TestProxy_ServesCachedFileEvenWhenUpstreamIsGone(t *testing.T) {
	t.Parallel()

	origin := &stubOrigin{body: []byte("jpeg-bytes")}
	proxy, server := newTestProxy(t, origin)

	recorder := httptest.NewRecorder()
	proxy.Serve(recorder, httptest.NewRequest(http.MethodGet, "/", nil), "w500", "abc.jpg")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}

	server.Close()

	recorder = httptest.NewRecorder()
	proxy.Serve(recorder, httptest.NewRequest(http.MethodGet, "/", nil), "w500", "abc.jpg")
	if recorder.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 from the cache after the origin went away", recorder.Code)
	}
}

func TestProxy_WritesNoPartialFileOnFailure(t *testing.T) {
	t.Parallel()

	origin := &stubOrigin{status: http.StatusInternalServerError}
	proxy, _ := newTestProxy(t, origin)

	recorder := httptest.NewRecorder()
	proxy.Serve(recorder, httptest.NewRequest(http.MethodGet, "/", nil), "w500", "abc.jpg")

	entries, err := os.ReadDir(proxy.CacheDir())
	if err != nil {
		t.Fatalf("reading cache dir: %v", err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, filepath.Base(entry.Name()))
		}
		t.Errorf("cache directory is not clean after a failed fetch: %v", names)
	}
}

func TestErrorsAreDistinguishable(t *testing.T) {
	t.Parallel()

	// ErrInvalidRequest and ErrUpstream are the two conditions a caller may
	// want to branch on.
	if !errors.Is(ErrInvalidRequest, ErrInvalidRequest) || !errors.Is(ErrUpstream, ErrUpstream) {
		t.Fatal("sentinel errors must be comparable with errors.Is")
	}
}
