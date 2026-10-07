// Package images proxies remote artwork (TMDB posters and backdrops) through
// the server, so the browser never talks to a third-party origin and the
// server can cache what it fetches.
package images

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// DefaultBaseURL is TMDB's image endpoint. It is a configuration value so tests
// can point it at a stub origin.
const DefaultBaseURL = "https://image.tmdb.org/t/p"

// AllowedSizes are the TMDB image size segments this proxy will forward. The
// set is an allowlist because the size becomes part of an outbound URL.
var AllowedSizes = map[string]bool{
	"w45": true, "w92": true, "w154": true, "w185": true, "w300": true,
	"w342": true, "w500": true, "w780": true, "w1280": true,
	"h632": true, "original": true,
}

// fileNameRe is the allowlist for the image file name: a bare TMDB-style
// basename. Anything with a separator, a traversal segment or an unlisted
// extension is rejected before it can reach either the cache or the upstream
// URL.
var fileNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*\.(jpg|jpeg|png|webp)$`)

// ErrInvalidRequest is returned for a size or file name that is not allowed.
var ErrInvalidRequest = errors.New("invalid image request")

// ErrUpstream is returned when the remote origin could not supply the image.
var ErrUpstream = errors.New("upstream image request failed")

// Config configures a Proxy.
type Config struct {
	// BaseURL is the upstream image root, without a trailing slash.
	BaseURL string
	// CacheDir is where fetched images are stored.
	CacheDir string
	// Client performs the upstream requests.
	Client *http.Client
	// MaxBytes caps a single image; larger responses are rejected.
	MaxBytes int64
	// Logger records what the proxy does.
	Logger *slog.Logger
	// Now is injectable for tests.
	Now func() time.Time
}

// Proxy fetches and caches remote artwork.
type Proxy struct {
	baseURL  string
	cacheDir string
	client   *http.Client
	maxBytes int64
	logger   *slog.Logger
}

// New creates a Proxy and prepares its cache directory.
func New(cfg Config) (*Proxy, error) {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultBaseURL
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 20 * time.Second}
	}
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = 8 << 20 // 8 MiB
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.CacheDir == "" {
		return nil, errors.New("images: CacheDir is required")
	}
	if err := os.MkdirAll(cfg.CacheDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating image cache %s: %w", cfg.CacheDir, err)
	}

	return &Proxy{
		baseURL:  strings.TrimSuffix(cfg.BaseURL, "/"),
		cacheDir: cfg.CacheDir,
		client:   cfg.Client,
		maxBytes: cfg.MaxBytes,
		logger:   cfg.Logger,
	}, nil
}

// PathFor renders the server-local URL for an artwork reference. It returns ""
// when either part is not allowed, so callers can leave the field out of a
// response rather than emitting a link that would 400.
func PathFor(size, file string) string {
	if !validSize(size) || !validFileName(file) {
		return ""
	}
	return "/api/images/" + size + "/" + file
}

// PathForReference resolves a MetadataSet artwork reference (a TMDB path such
// as "/abc.jpg") into a server-local URL at the given size.
func PathForReference(size, reference string) string {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return ""
	}
	// Only a bare basename is accepted; a full URL or a nested path is not
	// something this proxy will forward.
	name := strings.TrimPrefix(reference, "/")
	if strings.ContainsAny(name, "/\\") {
		return ""
	}
	return PathFor(size, name)
}

// Serve answers a /api/images/{size}/{file} request from the cache, fetching it
// from the upstream origin on a miss.
func (p *Proxy) Serve(w http.ResponseWriter, r *http.Request, size, file string) {
	if !validSize(size) || !validFileName(file) {
		http.Error(w, "invalid image request", http.StatusBadRequest)
		return
	}

	cachePath := p.cachePath(size, file)
	if info, err := os.Stat(cachePath); err == nil && !info.IsDir() {
		p.serveCached(w, r, cachePath)
		return
	}

	if err := p.fetch(r.Context(), size, file, cachePath); err != nil {
		if errors.Is(err, ErrInvalidRequest) {
			http.Error(w, "invalid image request", http.StatusBadRequest)
			return
		}
		p.logger.WarnContext(r.Context(), "image fetch failed",
			"size", size, "file", file, "error", err)
		http.Error(w, "image unavailable", http.StatusBadGateway)
		return
	}

	p.serveCached(w, r, cachePath)
}

func (p *Proxy) serveCached(w http.ResponseWriter, r *http.Request, cachePath string) {
	// Artwork is immutable for a given upstream path, so it can be cached
	// aggressively by the browser.
	w.Header().Set("Cache-Control", "public, max-age=86400, immutable")
	if contentType := contentTypeFor(cachePath); contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	http.ServeFile(w, r, cachePath)
}

// fetch downloads the image and stores it in the cache atomically.
func (p *Proxy) fetch(ctx context.Context, size, file, cachePath string) error {
	upstream := p.baseURL + "/" + size + "/" + file

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstream, nil)
	if err != nil {
		return fmt.Errorf("%w: building request: %v", ErrUpstream, err)
	}

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUpstream, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: upstream returned HTTP %d", ErrUpstream, resp.StatusCode)
	}

	// Read one byte past the cap so an oversized body is detected rather than
	// silently truncated.
	limited := io.LimitReader(resp.Body, p.maxBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return fmt.Errorf("%w: reading body: %v", ErrUpstream, err)
	}
	if int64(len(body)) > p.maxBytes {
		return fmt.Errorf("%w: image exceeds the %d byte limit", ErrUpstream, p.maxBytes)
	}
	if len(body) == 0 {
		return fmt.Errorf("%w: upstream returned an empty body", ErrUpstream)
	}

	// Write to a temporary file and rename, so a concurrent reader never sees a
	// half-written image.
	tmp, err := os.CreateTemp(p.cacheDir, "download-*")
	if err != nil {
		return fmt.Errorf("creating cache temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing cache file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing cache file: %w", err)
	}
	if err := os.Rename(tmpName, cachePath); err != nil {
		return fmt.Errorf("committing cache file: %w", err)
	}

	p.logger.DebugContext(ctx, "cached image", "size", size, "file", file, "bytes", len(body))
	return nil
}

// cachePath maps a validated (size, file) pair onto a cache file name. The
// hash keeps the on-disk name bounded and opaque.
func (p *Proxy) cachePath(size, file string) string {
	sum := sha256.Sum256([]byte(size + "/" + file))
	return filepath.Join(p.cacheDir, hex.EncodeToString(sum[:16])+filepath.Ext(file))
}

// CacheDir reports where images are cached.
func (p *Proxy) CacheDir() string { return p.cacheDir }

// CachedCount reports how many images are currently cached.
func (p *Proxy) CachedCount() int {
	entries, err := os.ReadDir(p.cacheDir)
	if err != nil {
		return 0
	}
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			count++
		}
	}
	return count
}

func validSize(size string) bool { return AllowedSizes[size] }

func validFileName(file string) bool {
	// Reject anything with a path separator or traversal before matching, so
	// the regexp is a second line of defence rather than the only one.
	if strings.ContainsAny(file, `/\`) || strings.Contains(file, "..") {
		return false
	}
	return fileNameRe.MatchString(file)
}

func contentTypeFor(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	default:
		return ""
	}
}
