// Command astraeus-server is the Astraeus Media server and administration CLI.
//
// Usage:
//
//	astraeus-server serve [flags]                  start the HTTP API
//	astraeus-server scan --path DIR --kind movies  scan a directory
//	astraeus-server scan --library ID              re-scan a registered library
//	astraeus-server library add --name N --path D --kind movies
//	astraeus-server library list
//	astraeus-server library rm ID
//	astraeus-server enrich                         run one metadata pass
//	astraeus-server version
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/ykzird/astraeus/internal/access"
	"github.com/ykzird/astraeus/internal/api"
	"github.com/ykzird/astraeus/internal/images"
	"github.com/ykzird/astraeus/internal/library"
	"github.com/ykzird/astraeus/internal/library/sqlite"
	"github.com/ykzird/astraeus/internal/metadata"
	"github.com/ykzird/astraeus/internal/observability"
	"github.com/ykzird/astraeus/internal/ratelimit"
	"github.com/ykzird/astraeus/internal/streaming"
	"github.com/ykzird/astraeus/internal/subtitles"
	"github.com/ykzird/astraeus/internal/tracing"
)

// version is the build identifier reported by `astraeus-server version` and
// recorded as service.version on exported traces. It is a variable rather than
// a constant so a release build can set it from the tag with
// `-ldflags "-X main.version=..."`. A plain `go build` leaves it as "dev",
// which is what an untagged working-tree binary honestly is.
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "astraeus:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("a command is required")
	}

	switch args[0] {
	case "serve":
		return runServe(args[1:])
	case "scan":
		return runScan(args[1:])
	case "library":
		return runLibrary(args[1:])
	case "enrich":
		return runEnrich(args[1:])
	case "version", "--version", "-version":
		fmt.Printf("astraeus-server %s\n", version)
		return nil
	case "help", "--help", "-h":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `astraeus-server — media library server

Commands:
  serve                        start the HTTP API
  scan --path DIR --kind KIND  scan a directory (registers the library if new)
  scan --library ID            re-scan a registered library
  library add --name N --path D --kind KIND
  library list
  library rm ID
  enrich                       run one metadata enrichment pass
  version

Flags are per-command; run a command with -h for its options.
`)
}

// config holds the flags every subcommand shares.
type config struct {
	dbPath    string
	tmdbKey   string
	logLevel  string
	logFormat string
}

func (c *config) register(fs *flag.FlagSet) {
	fs.StringVar(&c.dbPath, "db", "astraeus.db", "path to the SQLite database file")
	fs.StringVar(&c.tmdbKey, "tmdb-key", os.Getenv("TMDB_API_KEY"),
		"TMDB API key; without one, synthetic metadata is used instead")
	fs.StringVar(&c.logLevel, "log-level", "info", "log level: debug, info, warn or error")
	fs.StringVar(&c.logFormat, "log-format", "text", "log format: text or json")
}

// newLogger builds the structured logger described by the config.
func (c *config) newLogger() (*slog.Logger, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(c.logLevel)); err != nil {
		return nil, fmt.Errorf("invalid log level %q: %w", c.logLevel, err)
	}

	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	switch c.logFormat {
	case "json":
		handler = slog.NewJSONHandler(os.Stderr, opts)
	case "text":
		handler = slog.NewTextHandler(os.Stderr, opts)
	default:
		return nil, fmt.Errorf("invalid log format %q: want text or json", c.logFormat)
	}
	return slog.New(handler), nil
}

// env bundles the wired-up application dependencies.
type env struct {
	repo     *sqlite.Repository
	scanner  *library.Scanner
	worker   *metadata.Worker
	provider metadata.Provider
	logger   *slog.Logger
}

func (c *config) open() (*env, error) {
	logger, err := c.newLogger()
	if err != nil {
		return nil, err
	}

	repo, err := sqlite.Open(c.dbPath)
	if err != nil {
		return nil, err
	}

	if err := repo.Migrate(context.Background()); err != nil {
		_ = repo.Close()
		return nil, err
	}

	// With a key the real provider is used, and a miss stays visible as an
	// incomplete entity. Without one, synthetic metadata keeps the pipeline
	// exercisable end to end.
	var provider metadata.Provider
	if c.tmdbKey != "" {
		provider = metadata.NewTMDB(c.tmdbKey)
		logger.Info("using TMDB for metadata")
	} else {
		provider = metadata.NewMock()
		logger.Warn("no TMDB API key configured; writing synthetic metadata")
	}

	return &env{
		repo:     repo,
		scanner:  library.NewScanner(repo, logger),
		worker:   metadata.NewWorker(repo, provider, 6*time.Hour, logger),
		provider: provider,
		logger:   logger,
	}, nil
}

func (e *env) close() error { return e.repo.Close() }

// ---- serve -----------------------------------------------------------------

func runServe(args []string) error {
	var cfg config
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfg.register(fs)
	addr := fs.String("addr", ":8642", "address to listen on")
	enrichInterval := fs.Duration("enrich-interval", 6*time.Hour,
		"how often the background metadata worker runs; 0 disables it")
	scanInterval := fs.Duration("scan-interval", 6*time.Hour,
		"how often every library is re-scanned for new files; 0 disables periodic scanning")
	ffmpegBin := fs.String("ffmpeg", "ffmpeg", "ffmpeg executable used for segmented streaming")
	ffprobeBin := fs.String("ffprobe", "ffprobe", "ffprobe executable used for media inspection")
	maxSessions := fs.Int("max-sessions", 8,
		"maximum concurrent segmented streams; each is an ffmpeg process")
	streamRoot := fs.String("stream-root", filepath.Join(os.TempDir(), "astraeus-streams"),
		"directory holding HLS session output")
	deviceDir := fs.String("device-dir", "/dev/dri",
		"directory containing hardware transcoding devices (Intel QuickSync / VAAPI)")
	segmentSeconds := fs.Int("segment-seconds", 6, "target HLS segment duration in seconds")
	webDir := fs.String("web-dir", "web", "directory of static UI assets served at /; empty serves the API only")
	imageCache := fs.String("image-cache", filepath.Join(os.TempDir(), "astraeus-images"),
		"directory caching artwork proxied from the metadata provider")
	tmdbImageBase := fs.String("tmdb-image-base", images.DefaultBaseURL,
		"upstream root for artwork images")
	subtitleCache := fs.String("subtitle-cache", filepath.Join(os.TempDir(), "astraeus-subtitles"),
		"directory caching subtitle tracks converted to WebVTT")
	tesseractBin := fs.String("tesseract-bin", "tesseract",
		"OCR executable used to read image subtitles (PGS) into text; a missing one leaves them burn-only")
	ocrLanguage := fs.String("ocr-language", "",
		"language passed to the OCR executable, for example eng; empty uses its own default")

	authMode := fs.String("auth-mode", string(access.ModeNone),
		"access gate: none, proxy (trust an identity header from the access proxy) or token")
	authHeaders := fs.String("auth-header", strings.Join(access.DefaultIdentityHeaders, ","),
		"identity headers believed from the access proxy (comma separated, proxy mode)")
	trustedProxies := fs.String("trusted-proxy", "",
		"addresses or CIDRs whose identity headers are believed (comma separated, required in proxy mode)")
	authToken := fs.String("auth-token", os.Getenv("ASTRAEUS_AUTH_TOKEN"),
		"bearer token for token mode; prefer setting ASTRAEUS_AUTH_TOKEN over passing it as an argument")
	authExempt := fs.String("auth-exempt", "/api/health",
		"paths that bypass the access gate (comma separated, exact matches)")
	accessPolicy := fs.String("access-policy", "",
		"access policy file: which libraries each viewer may see, and who may change the library "+
			"(empty means every admitted viewer sees and may change everything)")
	rateLimit := fs.Float64("rate-limit", 0,
		"API requests per second allowed per client (0 disables; only /api paths are limited)")
	rateLimitBurst := fs.Int("rate-limit-burst", 0,
		"requests a client may make at once above the rate (0 means the rate rounded up)")
	otelEndpoint := fs.String("otel-endpoint", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		"OTLP/HTTP endpoint to export traces to, for example http://127.0.0.1:4318 (empty disables tracing)")
	otelServiceName := fs.String("otel-service-name", "astraeus-media",
		"service.name recorded on exported traces")
	if err := fs.Parse(args); err != nil {
		return err
	}

	app, err := cfg.open()
	if err != nil {
		return err
	}
	defer func() { _ = app.close() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Metrics are collected unconditionally and exposed at /metrics. The KPI
	// registry is declared up front so an idle server still reports every
	// metric, rather than only the ones that have fired.
	metrics := observability.New()
	observability.DeclareKPIs(metrics)
	app.worker.SetMetrics(metrics)

	// Validate the access configuration before anything starts, so a mistake
	// fails immediately instead of after the server has begun working.
	mode, err := access.ParseMode(*authMode)
	if err != nil {
		return err
	}
	prefixes, err := access.ParseTrustedProxies([]string{*trustedProxies})
	if err != nil {
		return err
	}
	gate, err := access.New(access.Config{
		Mode:            mode,
		IdentityHeaders: splitList(*authHeaders),
		TrustedProxies:  prefixes,
		Token:           *authToken,
		ExemptPaths:     splitList(*authExempt),
		Metrics:         metrics,
		Logger:          app.logger,
	})
	if err != nil {
		return err
	}

	// The access policy is what turns a gate that admits a request into one that
	// decides what the request may see. It is optional: without a file every
	// admitted viewer sees every library, which is what an install has always
	// done. A file that cannot be read is an error rather than a fallback,
	// because starting open when the operator asked for restricted is the one
	// failure mode this must not have.
	var policy *access.Policy
	if *accessPolicy != "" {
		policy, err = access.LoadPolicy(*accessPolicy)
		if err != nil {
			return err
		}
		app.logger.Info("access policy loaded",
			"path", policy.Source(),
			"viewers", policy.Viewers(),
			"admins", policy.Admins(),
			"default", map[bool]string{true: "all", false: "none"}[policy.DefaultAll()])
	}

	if *enrichInterval > 0 {
		worker := metadata.NewWorker(app.repo, app.provider, *enrichInterval, app.logger)
		worker.SetMetrics(metrics)
		go worker.Start(ctx)
	}

	// Periodic scanning is what makes files appear without anyone asking; the
	// CLI and the API remain the manual override.
	scheduler := library.NewScanScheduler(app.repo, app.scanner, *scanInterval, app.logger)
	scheduler.SetMetrics(metrics)
	go scheduler.Start(ctx)

	deps := api.Deps{
		Repository: app.repo,
		Scanner:    app.scanner,
		Scheduler:  scheduler,
		Worker:     app.worker,
		Metrics:    metrics,
		Policy:     policy,
		WebDir:     *webDir,
		Logger:     app.logger,
	}

	// Tracing is opt-in and off without an endpoint: a media server should not
	// need a collector to start. The flush on the way out is what makes the last
	// spans arrive rather than being cut off with the process.
	tracer, err := tracing.New(tracing.Config{
		ServiceName: *otelServiceName,
		Version:     version,
		Endpoint:    *otelEndpoint,
		Metrics:     metrics,
		Logger:      app.logger,
	})
	if err != nil {
		return err
	}
	deps.Tracer = tracer
	if tracer.Enabled() {
		app.logger.Info("trace export enabled", "endpoint", *otelEndpoint, "service", *otelServiceName)
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tracer.Shutdown(flushCtx)
	}()

	if *rateLimit > 0 {
		limiter, err := ratelimit.New(ratelimit.Config{
			Rate:  *rateLimit,
			Burst: *rateLimitBurst,
			Key:   apiClientKey,
			// Only the API is bounded. The UI, its assets and the HLS segments
			// are delivery rather than work: a page load pulls several files at
			// once and one playback fetches a segment every few seconds, and
			// neither is what a limit is for. Sessions are capped separately by
			// --max-sessions. /api/health stays open for liveness probes.
			Exempt: func(r *http.Request) bool {
				return !strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/api/health"
			},
			Metrics: metrics,
			Logger:  app.logger,
		})
		if err != nil {
			return err
		}
		deps.RateLimit = limiter.Middleware
		app.logger.Info("API rate limit enabled",
			"rate_per_second", *rateLimit, "burst", limiter.Burst(),
			"keyed_by", "access identity when present, otherwise the peer address")
	}

	// The media engine is optional: without ffmpeg/ffprobe the library still
	// works and the API reports what is unavailable instead of failing.
	deps.Server = streaming.DetectServerCapability(ctx, *ffmpegBin, *ffprobeBin, *deviceDir)
	app.logger.Info("server capability",
		"ffmpeg", deps.Server.FFmpegAvailable,
		"ffprobe", deps.Server.FFprobeAvailable,
		"hardware_acceleration", deps.Server.HardwareAcceleration,
		"video_encoders", len(deps.Server.VideoEncoders),
		"render_node", deps.Server.RenderNode)

	// Say which hardware encoders were tried and why any of them was turned
	// down. This is the first thing to read when hardware transcoding is
	// expected and the server is quietly using its CPU instead.
	for _, rejected := range deps.Server.RejectedEncoders {
		app.logger.Warn("hardware encoder rejected",
			"encoder", rejected.Encoder, "reason", rejected.Reason)
	}
	if len(deps.Server.RejectedEncoders) > 0 && len(deps.Server.HardwareAcceleration) == 0 {
		app.logger.Warn("no hardware encoder is usable on this host; transcoding will use the CPU",
			"rejected", len(deps.Server.RejectedEncoders))
	}

	if deps.Server.FFprobeAvailable {
		deps.Prober = streaming.NewCachingProber(streaming.NewFFProbe(*ffprobeBin))
	} else {
		app.logger.Warn("ffprobe not found; playback negotiation is disabled")
	}

	// Artwork is a nicety: if the cache directory is unusable the server still
	// starts, just without posters.
	imageProxy, err := images.New(images.Config{
		BaseURL:  *tmdbImageBase,
		CacheDir: *imageCache,
		Logger:   app.logger,
	})
	if err != nil {
		app.logger.Warn("artwork proxying is disabled", "error", err)
	} else {
		deps.Images = imageProxy
		app.logger.Info("artwork proxy enabled", "cache", *imageCache, "upstream", *tmdbImageBase)
	}

	if deps.Server.FFmpegAvailable {
		manager, err := streaming.NewManager(ctx, streaming.ManagerConfig{
			FFmpegBin:      *ffmpegBin,
			RootDir:        *streamRoot,
			SegmentSeconds: *segmentSeconds,
			MaxSessions:    *maxSessions,
			Server:         deps.Server,
			Metrics:        metrics,
			Tracer:         tracer,
			Logger:         app.logger,
		})
		if err != nil {
			return err
		}
		defer manager.Close()
		deps.Streams = manager
		go manager.ReapLoop(ctx)

		// Subtitles need ffmpeg, so they are only offered when it is present.
		// OCR is a second, optional dependency: without it image tracks keep
		// their burn-in path, and this is said at startup rather than
		// discovered when a client asks for one.
		subtitleService, err := subtitles.New(subtitles.Config{
			FFmpegBin:    *ffmpegBin,
			TesseractBin: *tesseractBin,
			OCRLanguage:  *ocrLanguage,
			CacheDir:     *subtitleCache,
			Logger:       app.logger,
		})
		if err != nil {
			app.logger.Warn("subtitle conversion is disabled", "error", err)
		} else {
			deps.Subtitles = subtitleService
			if subtitleService.OCRReady() {
				app.logger.Info("subtitle conversion enabled",
					"cache", *subtitleCache, "ocr", *tesseractBin, "image_subtitles", "read as text")
			} else {
				app.logger.Info("subtitle conversion enabled",
					"cache", *subtitleCache, "image_subtitles", "burned in",
					"reason", *tesseractBin+" is not installed")
			}
		}
	} else {
		app.logger.Warn("ffmpeg not found; only direct play will be available")
	}

	handler := api.NewServer(deps).Handler()

	// The access gate wraps everything, including the UI and /metrics.
	handler = gate.Middleware(handler)
	if mode != access.ModeNone {
		app.logger.Info("access gate enabled",
			"mode", string(mode), "trusted_proxies", len(prefixes), "exempt", splitList(*authExempt))
	}

	server := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		app.logger.Info("listening", "addr", *addr, "version", version)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		app.logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutting down http server: %w", err)
		}
	}
	return nil
}

// ---- scan ------------------------------------------------------------------

func runScan(args []string) error {
	var cfg config
	fs := flag.NewFlagSet("scan", flag.ExitOnError)
	cfg.register(fs)
	path := fs.String("path", "", "directory to scan")
	kind := fs.String("kind", "", "library kind: movies or shows (required with --path)")
	name := fs.String("name", "", "library name (defaults to the directory name)")
	libraryID := fs.String("library", "", "id of a registered library to re-scan")
	if err := fs.Parse(args); err != nil {
		return err
	}

	app, err := cfg.open()
	if err != nil {
		return err
	}
	defer func() { _ = app.close() }()

	ctx := context.Background()
	lib, err := resolveLibrary(ctx, app, *libraryID, *path, *kind, *name)
	if err != nil {
		return err
	}

	result, err := app.scanner.ScanLibrary(ctx, lib)
	if err != nil {
		return err
	}

	fmt.Printf("Library %q (%s)\n", lib.Name, lib.Kind)
	fmt.Printf("  files seen:       %d\n", result.FilesSeen)
	fmt.Printf("  entities created: %d (reused %d)\n", result.EntitiesCreated, result.EntitiesReused)
	fmt.Printf("  objects created:  %d (updated %d)\n", result.ObjectsCreated, result.ObjectsUpdated)
	for _, warning := range result.Warnings {
		fmt.Printf("  warning: %s\n", warning)
	}
	fmt.Println("\nRun 'astraeus-server enrich' to attach metadata.")
	return nil
}

// resolveLibrary finds the library to scan, registering a new one when a path
// was supplied that is not yet known.
func resolveLibrary(ctx context.Context, app *env, libraryID, path, kind, name string) (*library.Library, error) {
	switch {
	case libraryID != "":
		return app.repo.GetLibrary(ctx, libraryID)
	case path != "":
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolving path %q: %w", path, err)
		}
		if existing, err := app.repo.GetLibraryByPath(ctx, abs); err == nil {
			return existing, nil
		} else if !errors.Is(err, library.ErrNotFound) {
			return nil, err
		}

		parsedKind, err := library.ParseLibraryKind(kind)
		if err != nil {
			return nil, fmt.Errorf("%w (required with --path)", err)
		}
		if name == "" {
			name = filepath.Base(abs)
		}

		lib := &library.Library{
			ID:        uuid.NewString(),
			Name:      name,
			Path:      abs,
			Kind:      parsedKind,
			CreatedAt: time.Now(),
		}
		if err := app.repo.CreateLibrary(ctx, lib); err != nil {
			return nil, err
		}
		fmt.Printf("Registered new library %q at %s\n", lib.Name, lib.Path)
		return lib, nil
	default:
		return nil, errors.New("either --library or --path is required")
	}
}

// ---- library ---------------------------------------------------------------

func runLibrary(args []string) error {
	if len(args) == 0 {
		return errors.New("library requires a subcommand: add, list or rm")
	}

	switch args[0] {
	case "add":
		return runLibraryAdd(args[1:])
	case "list":
		return runLibraryList(args[1:])
	case "rm":
		return runLibraryRemove(args[1:])
	default:
		return fmt.Errorf("unknown library subcommand %q", args[0])
	}
}

func runLibraryAdd(args []string) error {
	var cfg config
	fs := flag.NewFlagSet("library add", flag.ExitOnError)
	cfg.register(fs)
	name := fs.String("name", "", "library name")
	path := fs.String("path", "", "library directory (required)")
	kind := fs.String("kind", "", "library kind: movies or shows (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *path == "" || *kind == "" {
		return errors.New("--path and --kind are required")
	}

	app, err := cfg.open()
	if err != nil {
		return err
	}
	defer func() { _ = app.close() }()

	abs, err := filepath.Abs(*path)
	if err != nil {
		return fmt.Errorf("resolving path: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return fmt.Errorf("library path: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("library path %s is not a directory", abs)
	}

	parsedKind, err := library.ParseLibraryKind(*kind)
	if err != nil {
		return err
	}
	if *name == "" {
		*name = filepath.Base(abs)
	}

	lib := &library.Library{ID: uuid.NewString(), Name: *name, Path: abs, Kind: parsedKind, CreatedAt: time.Now()}
	if err := app.repo.CreateLibrary(context.Background(), lib); err != nil {
		return err
	}

	fmt.Printf("Created library %s\n", lib.ID)
	return nil
}

func runLibraryList(args []string) error {
	var cfg config
	fs := flag.NewFlagSet("library list", flag.ExitOnError)
	cfg.register(fs)
	asJSON := fs.Bool("json", false, "print the libraries as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}

	app, err := cfg.open()
	if err != nil {
		return err
	}
	defer func() { _ = app.close() }()

	libraries, err := app.repo.ListLibraries(context.Background())
	if err != nil {
		return err
	}

	if *asJSON {
		return printJSON(libraries)
	}
	if len(libraries) == 0 {
		fmt.Println("No libraries registered.")
		return nil
	}
	fmt.Printf("%-38s %-20s %-8s %s\n", "ID", "NAME", "KIND", "PATH")
	for _, lib := range libraries {
		fmt.Printf("%-38s %-20s %-8s %s\n", lib.ID, lib.Name, lib.Kind, lib.Path)
	}
	return nil
}

func runLibraryRemove(args []string) error {
	var cfg config
	fs := flag.NewFlagSet("library rm", flag.ExitOnError)
	cfg.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("library rm requires exactly one library id")
	}

	app, err := cfg.open()
	if err != nil {
		return err
	}
	defer func() { _ = app.close() }()

	if err := app.repo.DeleteLibrary(context.Background(), fs.Arg(0)); err != nil {
		return err
	}
	fmt.Println("Library deleted.")
	return nil
}

// ---- enrich ----------------------------------------------------------------

func runEnrich(args []string) error {
	var cfg config
	fs := flag.NewFlagSet("enrich", flag.ExitOnError)
	cfg.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	app, err := cfg.open()
	if err != nil {
		return err
	}
	defer func() { _ = app.close() }()

	result, err := app.worker.EnrichOnce(context.Background())
	if err != nil {
		return err
	}

	fmt.Printf("Metadata pass complete: processed %d, enriched %d, failed %d\n",
		result.Processed, result.Enriched, result.Failed)
	if result.Failed > 0 {
		fmt.Println("Entities that could not be enriched stay Incomplete; list them with:")
		fmt.Println("  curl 'localhost:8642/api/entities?status=Incomplete'")
	}
	return nil
}

func printJSON(v any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(v)
}

// splitList turns a comma-separated flag value into a trimmed slice.
func splitList(value string) []string {
	var out []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// apiClientKey names the client a rate-limit bucket belongs to.
//
// The access gate's identity is preferred when there is one. Behind a proxy every
// request arrives from the proxy's own address, so an address-keyed limit would
// be one global bucket for everyone the proxy serves - which is the opposite of
// what a per-client limit means. With no gate there is no identity, and the peer
// address is the honest answer: the same address the gate itself trusts, with
// X-Forwarded-For ignored for the same reason the gate ignores it.
func apiClientKey(r *http.Request) string {
	if identity := access.IdentityFromContext(r.Context()); identity != "" {
		return "identity:" + identity
	}
	if addr, ok := access.ClientAddress(r); ok {
		return "address:" + addr.String()
	}
	return ""
}
