// Package api exposes the media library over HTTP. It contains no domain
// logic of its own: it validates requests, calls into the library package and
// renders the result as JSON.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jok/astraeus-media/internal/access"
	"github.com/jok/astraeus-media/internal/images"
	"github.com/jok/astraeus-media/internal/library"
	"github.com/jok/astraeus-media/internal/metadata"
	"github.com/jok/astraeus-media/internal/observability"
	"github.com/jok/astraeus-media/internal/streaming"
	"github.com/jok/astraeus-media/internal/subtitles"
)

// maxRequestBody bounds request bodies so a malformed client cannot exhaust
// memory.
const maxRequestBody = 1 << 20 // 1 MiB

// StreamManager is the part of the media engine the API drives. It is an
// interface so handler tests do not need ffmpeg installed.
type StreamManager interface {
	Start(ctx context.Context, entityID, objectPath string, decision streaming.Decision) (*streaming.Session, error)
	// StartAt begins delivery at an offset into the source, which is what a
	// seek beyond produced content, or a quality change, needs.
	StartAt(ctx context.Context, entityID, objectPath string, decision streaming.Decision, startSeconds float64) (*streaming.Session, error)
	Session(id string) (*streaming.Session, bool)
	// Stop ends a session and removes its output. Without this a client that
	// changes quality leaves the old transcode running until the idle reaper
	// notices, which means two encoders for the same viewer.
	Stop(id string)
	ServeFile(w http.ResponseWriter, r *http.Request, sessionID, name string)
}

// SubtitleConverter renders a subtitle track as WebVTT. It is an interface so
// handler tests do not need ffmpeg.
type SubtitleConverter interface {
	Convert(ctx context.Context, mediaPath string, trackIndex int) (string, error)
}

// Deps are the collaborators the API server needs.
type Deps struct {
	Repository library.Repository
	Scanner    *library.Scanner
	// Scheduler re-scans every library; when nil, the scan-all endpoint is
	// unavailable.
	Scheduler *library.ScanScheduler
	Worker    *metadata.Worker
	// Prober inspects media files. When nil, playback negotiation is disabled.
	Prober streaming.Prober
	// Streams produces segmented streams. When nil, only direct play works.
	Streams StreamManager
	// Server describes this host's transcoding capability.
	Server streaming.ServerCapability
	// Images proxies remote artwork locally. When nil, no artwork URLs are
	// emitted and the image route reports itself as unavailable.
	Images *images.Proxy
	// Metrics collects request and playback KPIs. A nil value disables
	// instrumentation.
	Metrics *observability.Metrics
	// Subtitles converts text subtitle tracks to WebVTT. When nil, no subtitle
	// tracks are advertised.
	Subtitles SubtitleConverter
	// WebDir is a directory of static UI assets served at /. When it is empty
	// or missing, only the API is served.
	WebDir string
	Logger *slog.Logger
}

// Server renders library state as JSON over HTTP.
type Server struct {
	repo      library.Repository
	scanner   *library.Scanner
	scheduler *library.ScanScheduler
	worker    *metadata.Worker
	prober    streaming.Prober
	streams   StreamManager
	server    streaming.ServerCapability
	images    *images.Proxy
	metrics   *observability.Metrics
	subtitles SubtitleConverter
	webFS     http.Handler
	logger    *slog.Logger
}

// NewServer creates a Server.
func NewServer(deps Deps) *Server {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}

	server := &Server{
		repo:      deps.Repository,
		scanner:   deps.Scanner,
		scheduler: deps.Scheduler,
		worker:    deps.Worker,
		prober:    deps.Prober,
		streams:   deps.Streams,
		server:    deps.Server,
		images:    deps.Images,
		metrics:   deps.Metrics,
		subtitles: deps.Subtitles,
		logger:    logger,
	}

	// The UI is served from the same origin as the API, so the browser needs
	// no CORS configuration and no separate web server.
	if deps.WebDir != "" {
		if info, err := os.Stat(deps.WebDir); err == nil && info.IsDir() {
			server.webFS = http.FileServer(http.Dir(deps.WebDir))
			logger.Info("serving the web UI", "dir", deps.WebDir)
		} else {
			logger.Warn("web UI directory is not present; serving the API only", "dir", deps.WebDir)
		}
	}

	return server
}

// Handler returns the HTTP routes for the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("GET /api/libraries", s.handleListLibraries)
	mux.HandleFunc("POST /api/libraries", s.handleCreateLibrary)
	mux.HandleFunc("GET /api/libraries/{id}", s.handleGetLibrary)
	mux.HandleFunc("DELETE /api/libraries/{id}", s.handleDeleteLibrary)
	mux.HandleFunc("POST /api/libraries/{id}/scan", s.handleScanLibrary)
	mux.HandleFunc("POST /api/scan", s.handleScanAll)
	mux.HandleFunc("GET /api/libraries/{id}/entities", s.handleListLibraryEntities)
	mux.HandleFunc("GET /api/entities", s.handleListEntities)
	mux.HandleFunc("GET /api/entities/{id}", s.handleGetEntity)
	mux.HandleFunc("POST /api/metadata/enrich", s.handleEnrich)
	mux.HandleFunc("POST /api/entities/{id}/playback", s.handlePlayback)
	mux.HandleFunc("GET /api/objects/{id}/file", s.handleObjectFile)
	mux.HandleFunc("GET /api/objects/{id}/subtitles/{file}", s.handleSubtitle)
	mux.HandleFunc("GET /api/system/capabilities", s.handleSystemCapabilities)
	mux.HandleFunc("GET /api/images/{size}/{file}", s.handleImage)

	// Metrics live outside /api because that is the convention scrapers expect.
	mux.Handle("GET /metrics", s.metrics.Handler())
	mux.HandleFunc("GET /hls/{session}/{file}", s.handleStreamFile)
	mux.HandleFunc("DELETE /api/streams/{id}", s.handleStopStream)

	// A single catch-all. Registering "GET /" alongside "/api/" would be an
	// ambiguous pattern set, so the root handler dispatches instead.
	mux.HandleFunc("/", s.handleRoot)

	return s.withRequestLogging(mux)
}

// handleRoot serves the UI for browser requests and the JSON error envelope for
// everything the API does not recognise.
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if isAPIPath(r.URL.Path) {
		s.handleNotFound(w, r)
		return
	}
	if s.webFS == nil || r.Method != http.MethodGet {
		s.handleNotFound(w, r)
		return
	}
	s.webFS.ServeHTTP(w, r)
}

// isAPIPath reports whether a path belongs to the API or the stream endpoints
// rather than to the UI's static assets.
func isAPIPath(path string) bool {
	return strings.HasPrefix(path, "/api/") || strings.HasPrefix(path, "/hls/")
}

// withRequestLogging records one structured line per request and feeds the
// request KPIs.
func (s *Server) withRequestLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)
		elapsed := time.Since(start)

		s.metrics.IncCounter("astraeus_http_requests_total",
			"HTTP requests served, by method and status code.",
			map[string]string{"method": r.Method, "status": strconv.Itoa(recorder.status)})
		s.metrics.ObserveHistogram("astraeus_http_request_seconds",
			"HTTP request duration.",
			elapsed.Seconds(),
			map[string]string{"method": r.Method})

		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", recorder.status,
			"duration_ms", elapsed.Milliseconds(),
		}
		// When an access gate is in front, record who came through it: an
		// identity-aware gate is only useful if the identity is auditable.
		if identity := access.IdentityFromContext(r.Context()); identity != "" {
			attrs = append(attrs, "user", identity)
		}
		s.logger.InfoContext(r.Context(), "http request", attrs...)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Unwrap lets http.ResponseController reach the real writer, so flushing a
// streamed response still works through the logging wrapper.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// ---- handlers --------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "service": "astraeus"})
}

// handleNotFound answers an unmatched API or streaming route in the same shape
// as every other error the API produces.
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "not_found", "no route matches "+r.Method+" "+r.URL.Path)
}

func (s *Server) handleListLibraries(w http.ResponseWriter, r *http.Request) {
	libraries, err := s.repo.ListLibraries(r.Context())
	if err != nil {
		s.writeRepoError(w, r, err, "listing libraries")
		return
	}
	writeJSON(w, http.StatusOK, libraries)
}

type createLibraryRequest struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Kind string `json:"kind"`
}

func (s *Server) handleCreateLibrary(w http.ResponseWriter, r *http.Request) {
	var req createLibraryRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	req.Path = strings.TrimSpace(req.Path)
	if req.Name == "" || req.Path == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "name and path are required")
		return
	}

	kind, err := library.ParseLibraryKind(req.Kind)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_kind", err.Error())
		return
	}

	info, err := os.Stat(req.Path)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_path", "path is not readable: "+err.Error())
		return
	}
	if !info.IsDir() {
		writeError(w, http.StatusBadRequest, "invalid_path", "path is not a directory")
		return
	}

	if existing, err := s.repo.GetLibraryByPath(r.Context(), req.Path); err == nil {
		writeError(w, http.StatusConflict, "already_exists", "library already registered with id "+existing.ID)
		return
	} else if !errors.Is(err, library.ErrNotFound) {
		s.writeRepoError(w, r, err, "checking for an existing library")
		return
	}

	lib := &library.Library{
		ID:        uuid.NewString(),
		Name:      req.Name,
		Path:      req.Path,
		Kind:      kind,
		CreatedAt: time.Now(),
	}
	if err := s.repo.CreateLibrary(r.Context(), lib); err != nil {
		s.writeRepoError(w, r, err, "creating library")
		return
	}

	writeJSON(w, http.StatusCreated, lib)
}

func (s *Server) handleGetLibrary(w http.ResponseWriter, r *http.Request) {
	lib, err := s.repo.GetLibrary(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeRepoError(w, r, err, "getting library")
		return
	}
	writeJSON(w, http.StatusOK, lib)
}

func (s *Server) handleDeleteLibrary(w http.ResponseWriter, r *http.Request) {
	if err := s.repo.DeleteLibrary(r.Context(), r.PathValue("id")); err != nil {
		s.writeRepoError(w, r, err, "deleting library")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleScanLibrary(w http.ResponseWriter, r *http.Request) {
	lib, err := s.repo.GetLibrary(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeRepoError(w, r, err, "getting library")
		return
	}

	result, err := s.scanner.ScanLibrary(r.Context(), lib)
	if err != nil {
		s.writeRepoError(w, r, err, "scanning library")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// handleScanAll re-scans every registered library in one pass. It is the manual
// override for the periodic scheduler.
func (s *Server) handleScanAll(w http.ResponseWriter, r *http.Request) {
	if s.scheduler == nil {
		writeError(w, http.StatusServiceUnavailable, "scanning_unavailable",
			"scanning every library is not configured on this server")
		return
	}

	outcomes, err := s.scheduler.ScanAll(r.Context())
	if err != nil {
		s.writeRepoError(w, r, err, "scanning all libraries")
		return
	}
	writeJSON(w, http.StatusOK, outcomes)
}

func (s *Server) handleListLibraryEntities(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	// Check the library exists so an unknown id is a 404 rather than an
	// indistinguishable empty list.
	if _, err := s.repo.GetLibrary(r.Context(), id); err != nil {
		s.writeRepoError(w, r, err, "getting library")
		return
	}

	entities, err := s.repo.ListEntitiesByLibrary(r.Context(), id)
	if err != nil {
		s.writeRepoError(w, r, err, "listing entities")
		return
	}
	writeJSON(w, http.StatusOK, s.decorateEntities(filterEntities(entities, r.URL.Query().Get("status"))))
}

func (s *Server) handleListEntities(w http.ResponseWriter, r *http.Request) {
	entities, err := s.repo.ListEntities(r.Context())
	if err != nil {
		s.writeRepoError(w, r, err, "listing entities")
		return
	}
	writeJSON(w, http.StatusOK, s.decorateEntities(filterEntities(entities, r.URL.Query().Get("status"))))
}

// entityDetail is the payload for a single entity: the entity itself plus the
// objects and children that give it context.
type entityDetail struct {
	Entity   entityResource        `json:"entity"`
	Objects  []library.MediaObject `json:"objects"`
	Children []entityResource      `json:"children"`
	Parent   *entityResource       `json:"parent,omitempty"`
}

func (s *Server) handleGetEntity(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	entity, err := s.repo.GetEntity(ctx, r.PathValue("id"))
	if err != nil {
		s.writeRepoError(w, r, err, "getting entity")
		return
	}

	objects, err := s.repo.GetObjectsByEntity(ctx, entity.ID)
	if err != nil {
		s.writeRepoError(w, r, err, "getting entity objects")
		return
	}
	children, err := s.repo.ListChildren(ctx, entity.ID)
	if err != nil {
		s.writeRepoError(w, r, err, "getting entity children")
		return
	}

	detail := entityDetail{
		Entity:   s.decorateEntity(*entity),
		Objects:  objects,
		Children: s.decorateEntities(children),
	}
	if entity.ParentID != nil {
		parent, err := s.repo.GetEntity(ctx, *entity.ParentID)
		if err == nil {
			parentResource := s.decorateEntity(*parent)
			detail.Parent = &parentResource
		} else if !errors.Is(err, library.ErrNotFound) {
			s.writeRepoError(w, r, err, "getting parent entity")
			return
		}
	}

	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleEnrich(w http.ResponseWriter, r *http.Request) {
	result, err := s.worker.EnrichOnce(r.Context())
	if err != nil {
		s.writeRepoError(w, r, err, "enriching metadata")
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// ---- playback --------------------------------------------------------------

// playbackResponse tells a client exactly how to play an entity.
type playbackResponse struct {
	EntityID  string                 `json:"entity_id"`
	ObjectID  string                 `json:"object_id"`
	Mode      streaming.PlaybackMode `json:"mode"`
	URL       string                 `json:"url"`
	SessionID string                 `json:"session_id,omitempty"`
	Decision  streaming.Decision     `json:"decision"`
	MediaInfo *streaming.MediaInfo   `json:"media_info,omitempty"`
	// Subtitles lists every subtitle track, and whether it can be delivered.
	Subtitles []subtitleResource `json:"subtitles,omitempty"`
	// StartSeconds echoes where this stream begins in the source, so a client
	// can present a continuous timeline across a seek or a quality change.
	StartSeconds float64 `json:"start_seconds,omitempty"`
}

// playbackRequest is the optional body of a playback request: the client's
// capability manifest, plus where in the source it wants to begin.
type playbackRequest struct {
	streaming.ClientCapability
	StartSeconds float64 `json:"start_seconds"`
}

// audioTrackIndexList renders the indices a client could have meant, so a
// rejected request tells the caller what the file actually has.
func audioTrackIndexList(tracks []streaming.AudioTrack) string {
	indices := make([]string, 0, len(tracks))
	for _, track := range tracks {
		indices = append(indices, fmt.Sprintf("%d (%s)", track.Index, streaming.AudioTrackLabel(track)))
	}
	return strings.Join(indices, ", ")
}

// handlePlayback negotiates how to deliver an entity to the calling client and
// returns the URL to use. The client describes itself in the optional request
// body; without one, the browser profile is assumed.
func (s *Server) handlePlayback(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if s.prober == nil {
		writeError(w, http.StatusServiceUnavailable, "streaming_unavailable",
			"media probing is not configured on this server")
		return
	}

	entity, err := s.repo.GetEntity(ctx, r.PathValue("id"))
	if err != nil {
		s.writeRepoError(w, r, err, "getting entity")
		return
	}

	objects, err := s.repo.GetObjectsByEntity(ctx, entity.ID)
	if err != nil {
		s.writeRepoError(w, r, err, "getting entity objects")
		return
	}
	if len(objects) == 0 {
		writeError(w, http.StatusBadRequest, "no_media",
			"this entity has no media object to play; it is a container, not a leaf")
		return
	}
	object := objects[0]

	capability := streaming.BrowserCapability()
	startSeconds := 0.0
	// A non-zero ContentLength includes the -1 sent by chunked requests, so a
	// streamed capability body is still honoured.
	if r.ContentLength != 0 {
		var request playbackRequest
		if !decodeJSON(w, r, &request) {
			return
		}
		declared := request.ClientCapability
		if err := declared.Validate(); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_capability", err.Error())
			return
		}
		if math.IsNaN(request.StartSeconds) || math.IsInf(request.StartSeconds, 0) || request.StartSeconds < 0 {
			writeError(w, http.StatusBadRequest, "invalid_start",
				"start_seconds must be a finite, non-negative number of seconds")
			return
		}
		capability = declared
		startSeconds = request.StartSeconds
	}

	info, err := s.prober.Probe(ctx, object.FilePath)
	if err != nil {
		s.logger.ErrorContext(ctx, "probing media object", "object_id", object.ID, "error", err)
		s.metrics.IncCounter("astraeus_probe_errors_total", "Media probes that failed.", nil)
		writeError(w, http.StatusInternalServerError, "probe_failed",
			"the media file could not be inspected: "+err.Error())
		return
	}

	// Starting past the end produces no output at all, and ffmpeg's complaint
	// about it is not something a client could act on.
	if startSeconds > 0 && info.DurationSeconds > 0 && startSeconds >= info.DurationSeconds {
		writeError(w, http.StatusBadRequest, "start_beyond_end",
			fmt.Sprintf("start_seconds %.3f is at or past the end of the media (%.3f seconds)",
				startSeconds, info.DurationSeconds))
		return
	}

	// A named audio track that this file does not have is a malformed request,
	// not an unsupported one: the client was told which indices exist when it
	// read the entity, so answering 400 with the real list is more useful than
	// silently delivering a different track. The negotiation would fall back and
	// explain, but the client should not have to read a reason to find a typo.
	if capability.AudioTrackIndex > 0 && len(info.AudioTracks) > 0 {
		if _, ok := info.AudioTrackByIndex(capability.AudioTrackIndex); !ok {
			writeError(w, http.StatusBadRequest, "unknown_audio_track",
				fmt.Sprintf("this file has no audio track with stream index %d; available: %s",
					capability.AudioTrackIndex, audioTrackIndexList(info.AudioTracks)))
			return
		}
	}

	// The pure negotiation answers what the client and the media allow. This
	// server may still be unable to encode the chosen target - no libx264, or a
	// client that only accepts AV1 - and that has to be a 409 with a reason
	// rather than a 500 from ffmpeg later.
	decision := streaming.NegotiateForServer(info, capability, s.server)
	s.metrics.IncCounter("astraeus_playback_decisions_total",
		"Playback negotiations, by the mode they chose.",
		map[string]string{"mode": string(decision.Mode)})
	response := playbackResponse{
		EntityID:     entity.ID,
		ObjectID:     object.ID,
		Mode:         decision.Mode,
		Decision:     decision,
		MediaInfo:    info,
		Subtitles:    s.subtitleResources(object.ID, info.Subtitles),
		StartSeconds: startSeconds,
	}

	if decision.Mode == streaming.ModeDirectPlay {
		response.URL = "/api/objects/" + object.ID + "/file"
		writeJSON(w, http.StatusOK, response)
		return
	}

	if !decision.Deliverable {
		writeError(w, http.StatusConflict, "not_deliverable", strings.Join(decision.Reasons, "; "))
		return
	}
	if s.streams == nil {
		writeError(w, http.StatusServiceUnavailable, "streaming_unavailable",
			"segmented streaming is not configured on this server")
		return
	}

	session, err := s.streams.StartAt(ctx, entity.ID, object.FilePath, decision, startSeconds)
	if err != nil {
		// A full server is not a fault in the request, and retrying later is
		// exactly what the client should do.
		if errors.Is(err, streaming.ErrTooManySessions) {
			s.logger.WarnContext(ctx, "refusing a streaming session: at capacity", "entity_id", entity.ID)
			writeError(w, http.StatusTooManyRequests, "too_many_sessions", err.Error())
			return
		}
		s.logger.ErrorContext(ctx, "starting streaming session", "entity_id", entity.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "stream_start_failed", err.Error())
		return
	}

	response.SessionID = session.ID
	// The master playlist when the session carries a ladder, the media playlist
	// otherwise; either way the client is handed the one file it should open.
	response.URL = "/hls/" + session.ID + "/" + session.PlaylistFile()
	writeJSON(w, http.StatusOK, response)
}

// handleStopStream ends a streaming session immediately.
//
// Sessions are otherwise only reaped once idle, so without this a quality
// change, or the viewer closing the player, leaves an ffmpeg process encoding
// frames nobody will watch until the reaper catches up.
func (s *Server) handleStopStream(w http.ResponseWriter, r *http.Request) {
	if s.streams == nil {
		writeError(w, http.StatusServiceUnavailable, "streaming_unavailable",
			"segmented streaming is not configured on this server")
		return
	}

	id := r.PathValue("id")
	if _, ok := s.streams.Session(id); !ok {
		writeError(w, http.StatusNotFound, "session_not_found",
			"no streaming session with that id is running")
		return
	}

	s.streams.Stop(id)
	s.logger.InfoContext(r.Context(), "streaming session stopped by request", "session_id", id)
	w.WriteHeader(http.StatusNoContent)
}

// handleObjectFile serves the original media file, which is what direct play
// needs. http.ServeFile handles range requests, without which seeking in a
// large file would not work.
func (s *Server) handleObjectFile(w http.ResponseWriter, r *http.Request) {
	object, err := s.repo.GetObject(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeRepoError(w, r, err, "getting media object")
		return
	}

	// The path comes from the database, never from the request.
	info, err := os.Stat(object.FilePath)
	if err != nil {
		s.logger.ErrorContext(r.Context(), "media file is missing", "object_id", object.ID, "path", object.FilePath)
		writeError(w, http.StatusNotFound, "file_missing", "the media file is no longer on disk")
		return
	}
	if info.IsDir() {
		writeError(w, http.StatusBadRequest, "not_a_file", "the media object does not point at a file")
		return
	}

	if object.MimeType != "" {
		w.Header().Set("Content-Type", object.MimeType)
	}
	http.ServeFile(w, r, object.FilePath)
}

func (s *Server) handleStreamFile(w http.ResponseWriter, r *http.Request) {
	if s.streams == nil {
		writeError(w, http.StatusServiceUnavailable, "streaming_unavailable",
			"segmented streaming is not configured on this server")
		return
	}
	s.streams.ServeFile(w, r, r.PathValue("session"), r.PathValue("file"))
}

// handleImage serves artwork through the local proxy, so the browser never
// contacts a third-party origin and the server caches what it fetches.
func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	if s.images == nil {
		writeError(w, http.StatusServiceUnavailable, "images_unavailable",
			"artwork proxying is not configured on this server")
		return
	}
	s.images.Serve(w, r, r.PathValue("size"), r.PathValue("file"))
}

// entityResource decorates a MediaEntity with server-local artwork URLs. It is
// what the API returns for entities; the embedded struct's fields are promoted
// into the same JSON object.
type entityResource struct {
	library.MediaEntity
	// PosterURL and BackdropURL point at this server's image proxy and are
	// omitted when there is no usable artwork reference.
	PosterURL   string `json:"poster_url,omitempty"`
	BackdropURL string `json:"backdrop_url,omitempty"`
}

// artworkSizePoster and artworkSizeBackdrop are the TMDB renditions the API
// links to: large enough for a canvas, small enough for a card grid.
const (
	artworkSizePoster   = "w500"
	artworkSizeBackdrop = "w1280"
)

// decorateEntity resolves artwork references into local URLs.
func (s *Server) decorateEntity(entity library.MediaEntity) entityResource {
	resource := entityResource{MediaEntity: entity}
	if s.images == nil || entity.Metadata == nil {
		return resource
	}
	resource.PosterURL = images.PathForReference(artworkSizePoster, entity.Metadata.PosterPath)
	resource.BackdropURL = images.PathForReference(artworkSizeBackdrop, entity.Metadata.BackdropPath)
	return resource
}

func (s *Server) decorateEntities(entities []library.MediaEntity) []entityResource {
	resources := make([]entityResource, 0, len(entities))
	for _, entity := range entities {
		resources = append(resources, s.decorateEntity(entity))
	}
	return resources
}

// subtitleResource is one subtitle track as advertised to a client.
type subtitleResource struct {
	Index    int    `json:"index"`
	Codec    string `json:"codec"`
	Language string `json:"language,omitempty"`
	Label    string `json:"label"`
	Default  bool   `json:"default,omitempty"`
	Forced   bool   `json:"forced,omitempty"`
	// Text is false for image-based tracks, which cannot be converted to WebVTT.
	Text bool `json:"text"`
	// URL is present only when the track can actually be served as WebVTT.
	URL string `json:"url,omitempty"`
}

// subtitleLabel picks the most useful human label for a track.
func subtitleLabel(track streaming.SubtitleTrack) string {
	if track.Title != "" {
		return track.Title
	}
	if track.Language != "" {
		return strings.ToUpper(track.Language)
	}
	return "Track " + strconv.Itoa(track.Index)
}

// subtitleResources turns probed tracks into client-facing resources.
func (s *Server) subtitleResources(objectID string, tracks []streaming.SubtitleTrack) []subtitleResource {
	if len(tracks) == 0 {
		return nil
	}

	resources := make([]subtitleResource, 0, len(tracks))
	for _, track := range tracks {
		resource := subtitleResource{
			Index:    track.Index,
			Codec:    track.Codec,
			Language: track.Language,
			Label:    subtitleLabel(track),
			Default:  track.Default,
			Forced:   track.Forced,
			Text:     track.Text,
		}
		// Only advertise a URL when the track can actually be delivered.
		if track.Text && s.subtitles != nil {
			resource.URL = "/api/objects/" + objectID + "/subtitles/" + strconv.Itoa(track.Index) + ".vtt"
		}
		resources = append(resources, resource)
	}
	return resources
}

// handleSubtitle extracts and serves one subtitle track as WebVTT.
func (s *Server) handleSubtitle(w http.ResponseWriter, r *http.Request) {
	if s.subtitles == nil {
		writeError(w, http.StatusServiceUnavailable, "subtitles_unavailable",
			"subtitle conversion is not configured on this server")
		return
	}

	fileName := r.PathValue("file")
	indexPart, ok := strings.CutSuffix(fileName, ".vtt")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "subtitle tracks are served as <index>.vtt")
		return
	}
	trackIndex, err := strconv.Atoi(indexPart)
	if err != nil || trackIndex < 0 {
		writeError(w, http.StatusBadRequest, "invalid_track", "subtitle track must be a non-negative integer")
		return
	}

	object, err := s.repo.GetObject(r.Context(), r.PathValue("id"))
	if err != nil {
		s.writeRepoError(w, r, err, "getting media object")
		return
	}

	// Confirm the track exists and is text-based before invoking ffmpeg.
	if s.prober != nil {
		info, err := s.prober.Probe(r.Context(), object.FilePath)
		if err != nil {
			s.logger.ErrorContext(r.Context(), "probing for a subtitle track", "object_id", object.ID, "error", err)
			writeError(w, http.StatusInternalServerError, "probe_failed",
				"the media file could not be inspected: "+err.Error())
			return
		}
		track, found := findSubtitleTrack(info.Subtitles, trackIndex)
		if !found {
			writeError(w, http.StatusNotFound, "track_not_found",
				"this file has no subtitle track with index "+indexPart)
			return
		}
		if !track.Text {
			writeError(w, http.StatusUnsupportedMediaType, "subtitle_format_unsupported",
				"subtitle track "+indexPart+" is image-based ("+track.Codec+
					") and would need optical character recognition to become text")
			return
		}
	}

	path, err := s.subtitles.Convert(r.Context(), object.FilePath, trackIndex)
	if err != nil {
		switch {
		case errors.Is(err, subtitles.ErrNoCues):
			writeError(w, http.StatusNotFound, "no_subtitles", err.Error())
		case errors.Is(err, subtitles.ErrUnsupportedFormat):
			writeError(w, http.StatusUnsupportedMediaType, "subtitle_format_unsupported", err.Error())
		default:
			s.logger.ErrorContext(r.Context(), "extracting subtitles",
				"object_id", object.ID, "track", trackIndex, "error", err)
			writeError(w, http.StatusInternalServerError, "subtitle_extraction_failed", err.Error())
		}
		return
	}

	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	http.ServeFile(w, r, path)
}

func findSubtitleTrack(tracks []streaming.SubtitleTrack, index int) (streaming.SubtitleTrack, bool) {
	for _, track := range tracks {
		if track.Index == index {
			return track, true
		}
	}
	return streaming.SubtitleTrack{}, false
}

// systemCapabilitiesResponse reports what this server can do, including
// whether a hardware transcoding path is usable.
type systemCapabilitiesResponse struct {
	streaming.ServerCapability
	NegotiationEnabled bool `json:"negotiation_enabled"`
	SegmentingEnabled  bool `json:"segmenting_enabled"`
	ArtworkEnabled     bool `json:"artwork_enabled"`
	SubtitlesEnabled   bool `json:"subtitles_enabled"`
}

func (s *Server) handleSystemCapabilities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, systemCapabilitiesResponse{
		ServerCapability:   s.server,
		NegotiationEnabled: s.prober != nil,
		SegmentingEnabled:  s.streams != nil,
		ArtworkEnabled:     s.images != nil,
		SubtitlesEnabled:   s.subtitles != nil,
	})
}

// ---- helpers ---------------------------------------------------------------

func filterEntities(entities []library.MediaEntity, status string) []library.MediaEntity {
	if status == "" {
		return entities
	}
	filtered := make([]library.MediaEntity, 0, len(entities))
	for _, entity := range entities {
		if strings.EqualFold(string(entity.Status), status) {
			filtered = append(filtered, entity)
		}
	}
	return filtered
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dest any) bool {
	body := http.MaxBytesReader(w, r.Body, maxRequestBody)
	defer func() { _ = body.Close() }()

	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(dest); err != nil {
		if errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "empty_body", "a JSON body is required")
			return false
		}
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// The status line is already sent, so all that is left is to record it.
		slog.Default().Error("encoding response", "error", err)
	}
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Code: code, Message: message})
}

// writeRepoError maps domain errors onto HTTP status codes and logs the rest.
func (s *Server) writeRepoError(w http.ResponseWriter, r *http.Request, err error, doing string) {
	if errors.Is(err, library.ErrNotFound) {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	s.logger.ErrorContext(r.Context(), "request failed", "doing", doing, "error", err)
	writeError(w, http.StatusInternalServerError, "internal_error", doing+" failed")
}
