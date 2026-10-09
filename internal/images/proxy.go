// Package images proxies remote artwork (TMDB posters and backdrops) through
// the server, so the browser never talks to a third-party origin and the
// server can cache what it fetches.
package images

import (
	"bytes"
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

// maxRedirects bounds a redirect chain, so an upstream that loops cannot hold a
// request open until the client's timeout.
const maxRedirects = 3

// checkRedirect refuses a redirect that leaves the host we asked.
//
// The proxy exists to fetch one image from one configured origin. Following a
// redirect is normal there - a CDN moving a path - and staying on the same host is
// what that looks like. Leaving the host is the case that matters: an upstream that
// answers 302 to http://127.0.0.1:8642/ makes the server fetch its own API, or any
// other service it can reach and the caller cannot, and then hands the result back
// and caches it. That is a request-forgery primitive, and refusing it costs
// nothing a legitimate image origin needs.
//
// The comparison is on the request's own host, so the decision does not depend on
// DNS resolving to something sensible.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	if len(via) == 0 {
		return nil
	}
	origin := via[0].URL
	if !strings.EqualFold(req.URL.Host, origin.Host) {
		return fmt.Errorf("refusing a redirect from %s to %s: the image proxy fetches from one host",
			origin.Host, req.URL.Host)
	}
	if req.URL.Scheme != origin.Scheme && req.URL.Scheme != "https" {
		return fmt.Errorf("refusing a redirect from %s to %s", origin.Scheme, req.URL.Scheme)
	}
	return nil
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
		cfg.Client = &http.Client{
			Timeout:       20 * time.Second,
			CheckRedirect: checkRedirect,
		}
	} else if cfg.Client.CheckRedirect == nil {
		// The caller supplied a client, so its timeout and transport are theirs -
		// but a client without a redirect policy follows a redirect anywhere, and
		// that is a request-forgery shape rather than a preference. This is the
		// one setting the proxy will not inherit as nil (A-8 of the 2026-10-09
		// review). A caller that genuinely wants open redirects can set
		// CheckRedirect to a no-op function, which is a decision made on purpose.
		cfg.Client.CheckRedirect = checkRedirect
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
		switch {
		case errors.Is(err, ErrInvalidRequest):
			http.Error(w, "invalid image request", http.StatusBadRequest)
		case errors.Is(err, ErrNotFound):
			// The poster does not exist. A 404 says so; a 502 claimed this server
			// was broken, and a Warn line per request meant a library full of
			// deleted artwork filled the log while nothing was actually wrong.
			http.Error(w, "no such image", http.StatusNotFound)
		default:
			p.logger.WarnContext(r.Context(), "image fetch failed",
				"size", size, "file", file, "error", err)
			http.Error(w, "image unavailable", http.StatusBadGateway)
		}
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

	if resp.StatusCode == http.StatusNotFound {
		// The image does not exist upstream. That is not a fault in this server,
		// and answering 502 made every missing poster look like an outage - with a
		// Warn line per request, so a library of deleted artwork filled the log
		// (A-8 of the 2026-10-09 review).
		return fmt.Errorf("%w: upstream has no such image", ErrNotFound)
	}
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
	if err := checkImage(resp.Header.Get("Content-Type"), body, file); err != nil {
		return err
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

// ErrNotFound is returned when the upstream origin has no such image.
//
// It is separate from ErrUpstream because the two mean different things to a
// client: one is "there is no poster", the other is "the proxy is not working".
var ErrNotFound = errors.New("no such image upstream")

// imageMagic lists the byte prefixes that identify the formats an artwork origin
// serves. The declared Content-Type is a claim; this is the evidence.
var imageMagic = [][]byte{
	{0xFF, 0xD8, 0xFF}, // JPEG
	{0x89, 'P', 'N', 'G'},
	{'G', 'I', 'F', '8'},     // GIF87a and GIF89a
	{'R', 'I', 'F', 'F'},     // WebP is RIFF....WEBP
	{0x00, 0x00, 0x01, 0x00}, // ICO
	{'B', 'M'},               // BMP
}

// textImageWindow is how far into a body the "<svg" element is looked for. Every
// SVG begins with a root element or a declaration plus one, so this is generous;
// it is bounded so a large body is not scanned on every miss.
const textImageWindow = 1024

// looksLikeSVG reports whether the body is a text image rather than a document.
//
// It requires an actual "<svg" element rather than a "<" of any kind, because the
// bodies this check exists to refuse - an HTML error page, a captive portal's
// login form, an XML error from an origin - begin with one too. A prefix test on
// "<!DOCTYPE" accepted "<!DOCTYPE html>", which is precisely the page that ended
// up cached as image/jpeg.
func looksLikeSVG(body []byte) bool {
	window := body
	if len(window) > textImageWindow {
		window = window[:textImageWindow]
	}
	lower := bytes.ToLower(window)
	return bytes.Contains(lower, []byte("<svg")) ||
		bytes.Contains(lower, []byte(":svg"))
}

// checkImage refuses a response that is not an image.
//
// Two checks, because either alone is easy to satisfy by accident. The
// Content-Type is what an origin claims, and an upstream that answers 200 with an
// HTML error page - a captive portal, a misconfigured CDN, an origin whose
// credentials lapsed - claims text/html and would otherwise be cached as
// image/jpeg and served to a browser as a broken image. The magic bytes are what
// the body is, so a mislabelled response is refused too.
//
// SVG is allowed by prefix because a poster origin may serve it; it is served
// back with the type the origin declared, and this server never renders it.
func checkImage(contentType string, body []byte, file string) error {
	mediaType := contentType
	if index := strings.IndexByte(mediaType, ';'); index >= 0 {
		mediaType = mediaType[:index]
	}
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))

	// An absent Content-Type is tolerated only if the bytes are recognisable,
	// which the second check decides. A present one has to be an image.
	if mediaType != "" && !strings.HasPrefix(mediaType, "image/") {
		return fmt.Errorf("%w: upstream served %s for %s, not an image",
			ErrUpstream, mediaType, file)
	}

	for _, prefix := range imageMagic {
		if bytes.HasPrefix(body, prefix) {
			return nil
		}
	}
	return fmt.Errorf("%w: upstream served %s for %s, which is not a recognised image format",
		ErrUpstream, mediaType, file)
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
