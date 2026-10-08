package streaming

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jok/astraeus-media/internal/observability"
)

const (
	// defaultSegmentSeconds is the HLS target segment duration. Six seconds is
	// the usual compromise between startup latency and request overhead.
	defaultSegmentSeconds = 6
	// defaultSessionTTL is how long an unwatched session is kept before its
	// ffmpeg process is stopped.
	defaultSessionTTL = 2 * time.Minute
	// playlistWait bounds how long Start waits for ffmpeg to publish a
	// playlist before giving up.
	playlistWait = 30 * time.Second
	// stderrLimit bounds the captured ffmpeg diagnostics.
	stderrLimit = 8 << 10
)

// ErrTooManySessions is returned when the concurrent stream limit is reached.
var ErrTooManySessions = errors.New("streaming: too many concurrent sessions")

// sessionMarkerFile marks a directory as ours. The reaper removes only
// directories carrying it.
const sessionMarkerFile = ".astraeus-session"

// defaultMaxSessions is generous for a household and small enough that a loop
// of requests cannot fork unbounded ffmpeg processes.
const defaultMaxSessions = 8

// ErrDirectPlayHasNoSession is returned when a session is requested for a
// decision that does not need one.
var ErrDirectPlayHasNoSession = errors.New("direct play does not use a streaming session")

// NamedSegmentRe is the allowlist of files a session directory exposes. Building
// the path from a matched name is what makes path traversal impossible.
var NamedSegmentRe = regexp.MustCompile(`^(playlist\.m3u8|seg[0-9]{5}\.ts)$`)

// Session is one in-flight segmented delivery of a MediaObject.
type Session struct {
	ID         string
	EntityID   string
	ObjectPath string
	Decision   Decision
	Dir        string

	// StartSeconds is where this session begins in the source. A client maps
	// media time back to source time by adding it, which is what lets a seek or
	// a quality change present a continuous timeline.
	StartSeconds float64

	mu         sync.Mutex
	cancel     context.CancelFunc
	done       chan struct{}
	runErr     error
	stderr     strings.Builder
	startedAt  time.Time
	lastAccess time.Time

	// firstSegmentOnce makes the first-time-to-first-segment measurement record
	// exactly one observation per session.
	firstSegmentOnce sync.Once
}

// StartedAt reports when the session began.
func (s *Session) StartedAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startedAt
}

// LastAccess reports when the session was last served.
func (s *Session) LastAccess() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastAccess
}

// PlaylistPath is the absolute path of the session playlist.
func (s *Session) PlaylistPath() string { return filepath.Join(s.Dir, "playlist.m3u8") }

// Done is closed once the underlying ffmpeg process has exited.
func (s *Session) Done() <-chan struct{} { return s.done }

// Err reports why the session ended, if it failed.
func (s *Session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runErr
}

// Diagnostics returns the tail of the ffmpeg error output.
func (s *Session) Diagnostics() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(s.stderr.String())
}

func (s *Session) touch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastAccess = time.Now()
}

func (s *Session) recordStderr(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stderr.Len() < stderrLimit {
		s.stderr.WriteString(text)
	}
}

// stderrCollector funnels ffmpeg diagnostics into a session's bounded buffer.
type stderrCollector struct{ session *Session }

func (c stderrCollector) Write(p []byte) (int, error) {
	c.session.recordStderr(string(p))
	return len(p), nil
}

// ManagerConfig configures a Manager.
type ManagerConfig struct {
	// FFmpegBin is the ffmpeg executable to run.
	FFmpegBin string
	// RootDir is where session directories are created.
	RootDir string
	// SegmentSeconds is the HLS target segment duration.
	SegmentSeconds int
	// SessionTTL is how long an idle session survives.
	SessionTTL time.Duration
	// Server describes the encoders available on this host.
	Server ServerCapability
	// MaxSessions caps how many segmented streams may run at once. Each one is
	// an ffmpeg process and a directory of segments, so an unauthenticated
	// caller that loops on the playback endpoint can exhaust the host. Zero
	// means the default.
	MaxSessions int
	// Metrics collects the streaming KPIs. A nil value disables instrumentation.
	Metrics *observability.Metrics
	Logger  *slog.Logger
}

// Manager owns the active streaming sessions.
type Manager struct {
	cfg     ManagerConfig
	baseCtx context.Context

	mu       sync.Mutex
	sessions map[string]*Session
}

// NewManager creates a Manager and prepares its working directory.
func NewManager(ctx context.Context, cfg ManagerConfig) (*Manager, error) {
	if cfg.FFmpegBin == "" {
		cfg.FFmpegBin = "ffmpeg"
	}
	if cfg.SegmentSeconds <= 0 {
		cfg.SegmentSeconds = defaultSegmentSeconds
	}
	if cfg.SessionTTL <= 0 {
		cfg.SessionTTL = defaultSessionTTL
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = defaultMaxSessions
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.RootDir == "" {
		return nil, errors.New("streaming: RootDir is required")
	}
	if err := os.MkdirAll(cfg.RootDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating streaming root %s: %w", cfg.RootDir, err)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	manager := &Manager{cfg: cfg, baseCtx: ctx, sessions: make(map[string]*Session)}
	manager.sweepStaleDirectories(cfg.RootDir, cfg.SessionTTL)
	return manager, nil
}

// sweepStaleDirectories removes session output left behind by a previous run.
// A process that is killed outright never gets to clean up after itself, so
// without this the stream root would grow without bound.
//
// Only directories older than the TTL are removed, so a second instance sharing
// the same root does not lose its live sessions. Running more than one instance
// against one root is therefore discouraged.
func (m *Manager) sweepStaleDirectories(root string, olderThan time.Duration) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}

	cutoff := time.Now().Add(-olderThan)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		path := filepath.Join(root, entry.Name())
		// Only ever remove directories this server created. Without the marker
		// a mistyped or shared --stream-root turns an age check into deleting
		// somebody else's data.
		if _, err := os.Stat(filepath.Join(path, sessionMarkerFile)); err != nil {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			m.cfg.Logger.Warn("removing stale session directory", "path", path, "error", err)
			continue
		}
		m.cfg.Logger.Info("removed stale session directory", "path", path)
	}
}

// Config exposes the manager configuration for callers that need to build URLs
// or report capabilities.
func (m *Manager) Config() ManagerConfig { return m.cfg }

// Start begins a segmented delivery from the beginning of the source.
func (m *Manager) Start(ctx context.Context, entityID, objectPath string, decision Decision) (*Session, error) {
	return m.StartAt(ctx, entityID, objectPath, decision, 0)
}

// StartAt begins a segmented delivery whose timeline starts at startSeconds
// into the source.
//
// This is what makes a quality change, or a seek into a part of the film that
// has not been produced yet, resume where the viewer is rather than sending
// them back to the opening titles. Seeking is applied to the input, so ffmpeg
// starts near the requested point instead of decoding everything before it; the
// first segment may therefore begin slightly earlier, at the preceding
// keyframe.
//
// If the stream fails to come up and a hardware encoder was in play, the same
// decision is retried in software. A hardware encoder can be listed by ffmpeg
// and still fail to open a session - a missing driver, or simply the wrong GPU
// vendor - and failing the request when a working software path exists would be
// the wrong answer.
func (m *Manager) StartAt(ctx context.Context, entityID, objectPath string, decision Decision, startSeconds float64) (*Session, error) {
	if startSeconds < 0 {
		startSeconds = 0
	}
	cfg := m.cfg
	session, err := m.startOnce(ctx, entityID, objectPath, decision, cfg, startSeconds)
	if err == nil || !wouldUseHardware(decision, cfg) {
		return session, err
	}

	m.cfg.Logger.WarnContext(ctx, "hardware transcode failed; retrying with software encoding",
		"entity_id", entityID, "error", err)
	m.cfg.Metrics.IncCounter(observability.MetricTranscodeFallbacks,
		"Transcodes that failed on a hardware encoder and were retried in software.", nil)

	cfg = withoutHardware(cfg)
	return m.startOnce(ctx, entityID, objectPath, decision, cfg, startSeconds)
}

// wouldUseHardware reports whether this decision would be served by a hardware
// encoder under the given configuration.
func wouldUseHardware(decision Decision, cfg ManagerConfig) bool {
	if decision.Mode == ModeDirectPlay || decision.VideoAction != ActionTranscode {
		return false
	}
	return isHardwareEncoder(EncoderFor(decision.TargetVideoCodec, cfg.Server))
}

// withoutHardware returns a configuration that can only choose software
// encoders.
func withoutHardware(cfg ManagerConfig) ManagerConfig {
	software := make([]string, 0, len(cfg.Server.VideoEncoders))
	for _, encoder := range cfg.Server.VideoEncoders {
		if !isHardwareEncoder(encoder) {
			software = append(software, encoder)
		}
	}
	cfg.Server.VideoEncoders = software
	cfg.Server.HardwareAcceleration = nil
	cfg.Server.RenderNode = ""
	return cfg
}

// startOnce prepares and launches one session with the given configuration.
func (m *Manager) startOnce(ctx context.Context, entityID, objectPath string, decision Decision, cfg ManagerConfig, startSeconds float64) (*Session, error) {
	m.mu.Lock()
	active := len(m.sessions)
	limit := m.cfg.MaxSessions
	m.mu.Unlock()
	if limit > 0 && active >= limit {
		return nil, fmt.Errorf("%w (%d running, limit %d)", ErrTooManySessions, active, limit)
	}

	if decision.Mode == ModeDirectPlay {
		return nil, ErrDirectPlayHasNoSession
	}
	if !decision.Deliverable {
		return nil, errors.New("streaming: the negotiation result is not deliverable")
	}

	sessionID := uuid.NewString()
	dir := filepath.Join(cfg.RootDir, sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating session directory: %w", err)
	}
	// The marker is what entitles the reaper to delete this directory later.
	if err := os.WriteFile(filepath.Join(dir, sessionMarkerFile), []byte(sessionID+"\n"), 0o644); err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("writing session marker: %w", err)
	}

	args, err := BuildFFmpegArgsAt(dir, objectPath, decision, cfg, startSeconds)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}

	runCtx, cancel := context.WithCancel(m.baseCtx)
	session := &Session{
		ID:           sessionID,
		EntityID:     entityID,
		ObjectPath:   objectPath,
		Decision:     decision,
		Dir:          dir,
		cancel:       cancel,
		done:         make(chan struct{}),
		startedAt:    time.Now(),
		lastAccess:   time.Now(),
		StartSeconds: startSeconds,
	}

	cmd := exec.CommandContext(runCtx, cfg.FFmpegBin, args...)
	// Assigning an io.Writer rather than reading a pipe ourselves makes
	// cmd.Wait wait until every byte of ffmpeg's diagnostics has been
	// collected, so the output is complete by the time the session reports
	// that it is done.
	cmd.Stderr = stderrCollector{session: session}
	// WaitDelay stops Wait from blocking forever if the process ignores the
	// cancellation and keeps its output open.
	cmd.WaitDelay = 5 * time.Second

	if err := cmd.Start(); err != nil {
		cancel()
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("starting ffmpeg: %w", err)
	}
	cfg.Logger.InfoContext(ctx, "streaming session started",
		"session_id", sessionID, "entity_id", entityID, "mode", decision.Mode)

	go func() {
		defer close(session.done)
		waitErr := cmd.Wait()
		session.mu.Lock()
		if waitErr != nil && runCtx.Err() == nil {
			session.runErr = waitErr
		}
		session.mu.Unlock()
		if waitErr != nil && runCtx.Err() == nil {
			cfg.Metrics.IncCounter("astraeus_stream_errors_total",
				"Segmented streaming failures: sessions that never produced a playlist, plus ffmpeg exiting unexpectedly.",
				map[string]string{"mode": string(decision.Mode)})
			cfg.Logger.Error("ffmpeg exited unexpectedly",
				"session_id", sessionID, "error", waitErr, "stderr", session.Diagnostics())
		}
	}()

	m.mu.Lock()
	m.sessions[sessionID] = session
	m.mu.Unlock()
	m.updateActiveGauge()

	startupStart := time.Now()
	if err := m.waitForPlaylist(ctx, session); err != nil {
		// transcode_startup_time is only recorded for a stream that actually
		// came up; a failure is an error, not a slow success.
		cfg.Metrics.IncCounter("astraeus_stream_sessions_total",
			"Segmented streaming sessions, by mode and outcome.",
			map[string]string{"mode": string(decision.Mode), "outcome": "failed"})
		cfg.Metrics.IncCounter("astraeus_stream_errors_total",
			"Segmented streaming failures: sessions that never produced a playlist, plus ffmpeg exiting unexpectedly.",
			map[string]string{"mode": string(decision.Mode)})
		m.Stop(sessionID)
		return nil, err
	}

	cfg.Metrics.ObserveHistogram("astraeus_transcode_startup_seconds",
		"Time from starting ffmpeg to the playlist being available, for the segmented modes.",
		time.Since(startupStart).Seconds(),
		map[string]string{"mode": string(decision.Mode)})
	cfg.Metrics.IncCounter("astraeus_stream_sessions_total",
		"Segmented streaming sessions, by mode and outcome.",
		map[string]string{"mode": string(decision.Mode), "outcome": "started"})

	return session, nil
}

// updateActiveGauge publishes the number of live sessions.
func (m *Manager) updateActiveGauge() {
	m.mu.Lock()
	active := len(m.sessions)
	m.mu.Unlock()
	m.cfg.Metrics.SetGauge("astraeus_stream_sessions_active",
		"Segmented streaming sessions currently running.", float64(active), nil)
}

// waitForPlaylist blocks until ffmpeg has published a playlist, the process
// dies, or the deadline passes.
func (m *Manager) waitForPlaylist(ctx context.Context, session *Session) error {
	deadline := time.NewTimer(playlistWait)
	defer deadline.Stop()

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		if _, err := os.Stat(session.PlaylistPath()); err == nil {
			return nil
		}

		select {
		case <-session.Done():
			// The process can write the playlist and exit between the stat
			// above and this select, so check once more before declaring
			// failure.
			if _, err := os.Stat(session.PlaylistPath()); err == nil {
				return nil
			}
			diagnostics := session.Diagnostics()
			if diagnostics == "" {
				diagnostics = "no diagnostics produced"
			}
			return fmt.Errorf("ffmpeg stopped before producing a playlist: %s", diagnostics)
		case <-ctx.Done():
			return fmt.Errorf("waiting for playlist: %w", ctx.Err())
		case <-deadline.C:
			return fmt.Errorf("timed out after %s waiting for the playlist to appear", playlistWait)
		case <-ticker.C:
		}
	}
}

// Session returns an active session by id.
func (m *Manager) Session(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[id]
	return session, ok
}

// Stop ends a session and removes its directory.
func (m *Manager) Stop(id string) {
	m.mu.Lock()
	session, ok := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if !ok {
		return
	}

	session.cancel()
	<-session.done
	if err := os.RemoveAll(session.Dir); err != nil {
		m.cfg.Logger.Warn("removing session directory", "session_id", id, "error", err)
	}
	m.updateActiveGauge()
	m.cfg.Logger.Info("streaming session stopped", "session_id", id)
}

// Close stops every session.
func (m *Manager) Close() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	m.mu.Unlock()

	for _, id := range ids {
		m.Stop(id)
	}
}

// Reap stops sessions that have not been accessed within the TTL.
func (m *Manager) Reap(now time.Time) int {
	m.mu.Lock()
	var stale []string
	for id, session := range m.sessions {
		if now.Sub(session.LastAccess()) > m.cfg.SessionTTL {
			stale = append(stale, id)
		}
	}
	m.mu.Unlock()

	for _, id := range stale {
		m.cfg.Logger.Info("reaping idle streaming session", "session_id", id)
		m.Stop(id)
	}
	return len(stale)
}

// ReapLoop reaps idle sessions until the context is cancelled.
func (m *Manager) ReapLoop(ctx context.Context) {
	interval := m.cfg.SessionTTL / 2
	if interval < 10*time.Second {
		interval = 10 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.Close()
			return
		case <-ticker.C:
			m.Reap(time.Now())
		}
	}
}

// ServeFile serves one file from a session directory. The name is matched
// against an allowlist, so a client can never escape the directory.
func (m *Manager) ServeFile(w http.ResponseWriter, r *http.Request, sessionID, name string) {
	if !NamedSegmentRe.MatchString(name) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	session, ok := m.Session(sessionID)
	if !ok {
		http.Error(w, "streaming session not found", http.StatusNotFound)
		return
	}
	session.touch()

	switch filepath.Ext(name) {
	case ".m3u8":
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	case ".ts":
		w.Header().Set("Content-Type", "video/mp2t")
		// fttt_latency: the first time a segment is actually handed to a client
		// is the moment the stream became watchable.
		session.firstSegmentOnce.Do(func() {
			m.cfg.Metrics.ObserveHistogram("astraeus_first_segment_seconds",
				"Time from stream preparation starting to the first segment being delivered to a client.",
				time.Since(session.StartedAt()).Seconds(),
				map[string]string{"mode": string(session.Decision.Mode)})
		})
	}
	w.Header().Set("Cache-Control", "no-store")

	http.ServeFile(w, r, filepath.Join(session.Dir, name))
}

// BuildFFmpegArgs renders the ffmpeg invocation for a decision from the start
// of the source. It is exported so the command line can be asserted in tests
// without running ffmpeg.
func BuildFFmpegArgs(dir, inputPath string, decision Decision, cfg ManagerConfig) ([]string, error) {
	return BuildFFmpegArgsAt(dir, inputPath, decision, cfg, 0)
}

// BuildFFmpegArgsAt renders the ffmpeg invocation for a decision, beginning at
// startSeconds into the source. Placing -ss before -i makes ffmpeg seek on the
// input, which is fast, at the cost of landing on the preceding keyframe.
func BuildFFmpegArgsAt(dir, inputPath string, decision Decision, cfg ManagerConfig, startSeconds float64) ([]string, error) {
	if decision.Mode == ModeDirectPlay {
		return nil, ErrDirectPlayHasNoSession
	}

	// The encoder is chosen before the argument list is assembled because some
	// of its options are global and have to precede the input: VAAPI's
	// -vaapi_device, without which its upload filter has no device.
	encoder := ""
	if decision.VideoAction == ActionTranscode {
		encoder = EncoderFor(decision.TargetVideoCodec, cfg.Server)
		if encoder == "" {
			return nil, fmt.Errorf("no ffmpeg encoder available for video codec %q", decision.TargetVideoCodec)
		}
	}
	device := encoderDevice{RenderNode: cfg.Server.RenderNode}

	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-y",
	}
	if startSeconds > 0 {
		args = append(args, "-ss", strconv.FormatFloat(startSeconds, 'f', 3, 64))
	}
	args = append(args, encoderInputArgs(encoder, device)...)
	args = append(args,
		"-i", inputPath,
		"-map", "0:v:0",
		"-map", "0:a:0?",
	)

	switch decision.VideoAction {
	case ActionCopy:
		args = append(args, "-c:v", "copy")
	case ActionTranscode:
		args = append(args, encoderOutputArgs(encoder, decision.TargetHeight, device)...)
		// Cut on the requested segment boundary. Without this ffmpeg only cuts
		// at encoder keyframes - a ~10s default GOP - so -hls_time is advisory
		// and the first segment, and therefore first playback, arrives far
		// later than asked for.
		args = append(args, "-force_key_frames",
			fmt.Sprintf("expr:gte(t,n_forced*%d)", cfg.SegmentSeconds))
	default:
		return nil, fmt.Errorf("unsupported video action %q", decision.VideoAction)
	}

	switch decision.AudioAction {
	case ActionNone:
		args = append(args, "-an")
	case ActionCopy:
		args = append(args, "-c:a", "copy")
	case ActionTranscode:
		codec := decision.TargetAudioCodec
		if codec == "" {
			codec = "aac"
		}
		args = append(args, "-c:a", codec, "-b:a", "192k")
		if decision.TargetAudioChannels > 0 {
			// Chromium refuses a 5.1 AAC SourceBuffer outright, and a browser
			// outputs stereo anyway, so a negotiated downmix is what makes a
			// film with a 5.1 track playable at all.
			args = append(args, "-ac", strconv.Itoa(decision.TargetAudioChannels))
		}
	default:
		return nil, fmt.Errorf("unsupported audio action %q", decision.AudioAction)
	}

	args = append(args,
		"-f", "hls",
		"-hls_time", fmt.Sprint(cfg.SegmentSeconds),
		"-hls_list_size", "0",
		"-hls_playlist_type", "event",
		"-hls_segment_filename", filepath.Join(dir, "seg%05d.ts"),
		filepath.Join(dir, "playlist.m3u8"),
	)
	return args, nil
}
