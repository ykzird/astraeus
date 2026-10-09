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

	"github.com/ykzird/astraeus/internal/observability"
	"github.com/ykzird/astraeus/internal/tracing"
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

// recordStreamFailure counts one failed session, in the one place that does it.
func recordStreamFailure(metrics *observability.Metrics, metric string, mode PlaybackMode) {
	metrics.IncCounter(metric,
		"Segmented streaming failures: sessions that never produced a playlist, plus ffmpeg exiting unexpectedly.",
		map[string]string{"mode": string(mode)})
}

// ErrTooManySessions is returned when the concurrent stream limit is reached.
var ErrTooManySessions = errors.New("streaming: too many concurrent sessions")

// errFFmpegFailed marks a failure that came from ffmpeg itself, as opposed to
// one the request hit before ffmpeg was any part of the problem.
//
// The distinction decides whether the hardware-to-software retry is worth
// attempting (S-11 of the 2026-10-09 review). Falling back on a capacity refusal
// logged "hardware transcode failed" and started a second ffmpeg for a request
// the software encoder would refuse just as fast, and falling back on a client
// disconnect spawned a process nobody was waiting for.
var errFFmpegFailed = errors.New("streaming: ffmpeg failed")

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
// the path from a matched name is what makes path traversal impossible, so this
// is the security boundary of segmented delivery and stays anchored and narrow.
//
// Three shapes are served: a session with no ladder (playlist.m3u8, seg00000.ts),
// a ladder (master.m3u8, playlist_0.m3u8, seg0_00000.ts), and the files of a
// single-rendition session created before ladders existed, which the first two
// alternatives still match.
var NamedSegmentRe = regexp.MustCompile(
	`^(master\.m3u8|playlist(_[0-9]{1,2})?\.m3u8|seg([0-9]{1,2}_)?[0-9]{5}\.ts)$`)

// Session is one in-flight segmented delivery of a MediaObject.
type Session struct {
	ID         string
	EntityID   string
	ObjectPath string
	Decision   Decision
	Dir        string
	// PlaylistName is the playlist a client should open, which is the master
	// when the decision carries a ladder and the media playlist otherwise.
	PlaylistName string

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

// PlaylistFile is the name of the playlist a client should open: the master when
// the session has a ladder, the media playlist otherwise. It falls back rather
// than returning an empty name, so a Session built without one - by a test double,
// or by a future caller that forgets - still yields a URL a player can open.
func (s *Session) PlaylistFile() string {
	if s.PlaylistName == "" {
		return MediaPlaylistName
	}
	return s.PlaylistName
}

// PlaylistPath is the absolute path of the playlist a client should open.
func (s *Session) PlaylistPath() string {
	return filepath.Join(s.Dir, s.PlaylistFile())
}

// Done is closed once the underlying ffmpeg process has exited.
func (s *Session) Done() <-chan struct{} { return s.done }

// Failed reports whether the session's encoder exited with an error.
//
// It is false while the session runs and false when it finishes successfully,
// which is what lets a caller tell "this stream is broken" from "this stream is
// over" - the first is worth ending early, the second must keep serving its last
// segments.
func (s *Session) Failed() bool {
	select {
	case <-s.done:
		return s.Err() != nil
	default:
		return false
	}
}

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
	// Tracer records a span around starting each session, which is the slowest
	// thing this server does: it forks ffmpeg and waits for the first segment. A
	// nil tracer is a no-op.
	Tracer *tracing.Tracer
	Logger *slog.Logger
}

// Manager owns the active streaming sessions.
type Manager struct {
	cfg     ManagerConfig
	baseCtx context.Context

	mu       sync.Mutex
	sessions map[string]*Session
	// pending counts sessions that have reserved a slot but are not in
	// sessions yet: mkdir, building the command line and cmd.Start all happen
	// before a session can be published, and checking the limit without
	// reserving let concurrent callers all pass the same check (S-2 of the
	// 2026-10-09 review: ten parallel starts against a limit of two left nine
	// ffmpeg processes running).
	pending int
}

// ActiveSessions reports how many sessions are running, including the ones that
// have claimed a slot but are not yet published.
//
// The pending count is the point: a caller that wants to know whether the limit
// is actually respected has to see the reservations too, or it sees a number
// that dips during exactly the window the limit exists to guard.
func (m *Manager) ActiveSessions() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions) + m.pending
}

// reserveSlot claims one of the MaxSessions slots, or reports the limit as
// reached.
//
// The claim and the check are the same critical section on purpose. A caller
// that passes the check must already own the slot, because otherwise the work
// between the check and the publication of the session - which includes forking
// ffmpeg - is a window every concurrent caller can pass through at once.
func (m *Manager) reserveSlot() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	limit := m.cfg.MaxSessions
	active := len(m.sessions) + m.pending
	if limit > 0 && active >= limit {
		return fmt.Errorf("%w (%d running, limit %d)", ErrTooManySessions, active, limit)
	}
	m.pending++
	return nil
}

// releaseSlot returns a reservation that did not become a session.
func (m *Manager) releaseSlot() {
	m.mu.Lock()
	if m.pending > 0 {
		m.pending--
	}
	m.mu.Unlock()
}

// publishSlot turns a reservation into a published session.
func (m *Manager) publishSlot(session *Session) {
	m.mu.Lock()
	if m.pending > 0 {
		m.pending--
	}
	m.sessions[session.ID] = session
	m.mu.Unlock()
	m.updateActiveGauge()
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

	// The span covers the whole start, including the hardware-then-software
	// retry, because the retry is part of how long the viewer waited.
	ctx, span := m.cfg.Tracer.Start(ctx, "stream.session.start", tracing.SpanKindInternal,
		tracing.String("stream.entity_id", entityID),
		tracing.String("stream.mode", string(decision.Mode)),
		tracing.String("stream.video_action", string(decision.VideoAction)),
		tracing.Int("stream.start_seconds", int64(startSeconds)))
	defer span.End()

	cfg := m.cfg
	session, err := m.startOnce(ctx, entityID, objectPath, decision, cfg, startSeconds)
	// A software retry only makes sense when ffmpeg is what failed. A capacity
	// refusal would be refused again just as fast, and a client that has gone
	// away is not waiting for either attempt (S-11).
	if err == nil || !wouldUseHardware(decision, cfg) || !errors.Is(err, errFFmpegFailed) {
		if err != nil {
			span.RecordError(err)
		} else {
			span.SetAttributes(tracing.String("stream.session_id", session.ID))
		}
		return session, err
	}

	m.cfg.Logger.WarnContext(ctx, "hardware transcode failed; retrying with software encoding",
		"entity_id", entityID, "error", err)
	m.cfg.Metrics.IncCounter(observability.MetricTranscodeFallbacks,
		"Transcodes that failed on a hardware encoder and were retried in software.", nil)
	span.SetAttributes(tracing.Bool("stream.hardware_fallback", true))

	cfg = withoutHardware(cfg)
	decision = softwareOnlyDecision(decision, cfg.Server)
	session, err = m.startOnce(ctx, entityID, objectPath, decision, cfg, startSeconds)
	if err != nil {
		span.RecordError(err)
	} else {
		span.SetAttributes(
			tracing.String("stream.session_id", session.ID),
			tracing.String("stream.fallback_video_action", string(decision.VideoAction)),
		)
	}
	return session, err
}

// wouldUseHardware reports whether this decision would be served by a hardware
// encoder under the given configuration.
func wouldUseHardware(decision Decision, cfg ManagerConfig) bool {
	if decision.Mode == ModeDirectPlay || decision.VideoAction != ActionTranscode {
		return false
	}
	encoder, _, err := videoEncoderFor(decision, cfg.Server)
	return err == nil && isHardwareEncoder(encoder)
}

// withoutHardware returns a configuration that can only choose software
// encoders, including for HDR: leaving a failed hardware encoder in the
// verified 10-bit list would let the retry pick it again.
func withoutHardware(cfg ManagerConfig) ManagerConfig {
	software := make([]string, 0, len(cfg.Server.VideoEncoders))
	for _, encoder := range cfg.Server.VideoEncoders {
		if !isHardwareEncoder(encoder) {
			software = append(software, encoder)
		}
	}
	cfg.Server.VideoEncoders = software

	softwareHDR := make([]HDREncoder, 0, len(cfg.Server.HDRVideoEncoders))
	for _, support := range cfg.Server.HDRVideoEncoders {
		if !isHardwareEncoder(support.Encoder) {
			softwareHDR = append(softwareHDR, support)
		}
	}
	cfg.Server.HDRVideoEncoders = softwareHDR

	cfg.Server.HardwareAcceleration = nil
	cfg.Server.RenderNode = ""
	return cfg
}

// softwareOnlyDecision adjusts a decision that was going to be delivered by a
// hardware encoder so a software encoder can still serve it.
//
// The only adjustment needed is HDR. When the failed hardware encoder was the
// only verified 10-bit one, there is no way to keep HDR, and a tone-mapped SDR
// stream is a far better answer than an error - the client asked for a film,
// not for 10 bits.
func softwareOnlyDecision(decision Decision, server ServerCapability) Decision {
	if !decision.TargetDynamicRange.IsHDR() {
		return decision
	}
	if _, ok := EncoderForHDR(decision.TargetVideoCodec, server); ok {
		return decision
	}
	decision.ToneMap = true
	decision.TargetDynamicRange = RangeSDR
	decision.Reasons = append(decision.Reasons,
		"the hardware encoder failed and no software encoder here produces 10-bit, so this retry is tone mapped to SDR")
	return decision
}

// startOnce prepares and launches one session with the given configuration.
func (m *Manager) startOnce(ctx context.Context, entityID, objectPath string, decision Decision, cfg ManagerConfig, startSeconds float64) (*Session, error) {
	if err := m.reserveSlot(); err != nil {
		return nil, err
	}
	// Every path from here to publishSlot has to release the reservation, or a
	// failed start leaks a slot until the process restarts.
	reserved := true
	defer func() {
		if reserved {
			m.releaseSlot()
		}
	}()

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
		PlaylistName: sessionPlaylistName(decision),
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
		return nil, fmt.Errorf("starting ffmpeg: %w: %w", err, errFFmpegFailed)
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
		// One failure, one count. The wait goroutine is the only recorder, so a
		// failure observed here and again by the startup branch below is no
		// longer counted twice (S-12).
		if waitErr != nil && runCtx.Err() == nil {
			recordStreamFailure(cfg.Metrics, observability.MetricStreamErrors, decision.Mode)
			cfg.Logger.Error("ffmpeg exited unexpectedly",
				"session_id", sessionID, "error", waitErr, "stderr", session.Diagnostics())
		}
	}()

	// The session is published and the reservation becomes a real slot in one
	// critical section, so the limit counts it from here on.
	m.publishSlot(session)
	reserved = false

	startupStart := time.Now()
	if err := m.waitForPlaylist(ctx, session); err != nil {
		// transcode_startup_time is only recorded for a stream that actually
		// came up; a failure is an error, not a slow success.
		cfg.Metrics.IncCounter("astraeus_stream_sessions_total",
			"Segmented streaming sessions, by mode and outcome.",
			map[string]string{"mode": string(decision.Mode), "outcome": "failed"})
		// The error count belongs to the wait goroutine, which records it when
		// ffmpeg exits unexpectedly - that is the only place that knows whether
		// the failure was ffmpeg's or this context's. Counting again here
		// inflated the metric (S-12), and the count is asynchronous, so there is
		// no way to tell from here whether it has happened yet.
		m.Stop(sessionID)
		return nil, fmt.Errorf("%w: %w", err, errFFmpegFailed)
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
			m.cfg.Logger.Info("reaping idle streaming session", "session_id", id)
			stale = append(stale, id)
			continue
		}
		// A session whose ffmpeg died is also finished with, however recently a
		// client touched it. hls.js keeps polling a playlist that will never be
		// completed, and every poll is a touch, so the idle rule never fires and
		// the slot is held while the viewer watches a stalled player (S-9 of the
		// 2026-10-09 review).
		//
		// Only a *failed* session is reaped here. A session that finished
		// successfully has a complete playlist with #EXT-X-ENDLIST, and a client
		// still fetching its last segments must keep being served.
		if session.Failed() {
			m.cfg.Logger.Warn("reaping a streaming session whose encoder died",
				"session_id", id, "error", session.Err())
			stale = append(stale, id)
		}
	}
	m.mu.Unlock()

	for _, id := range stale {
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

	// A failed session is Gone rather than 404: the client had a session and it
	// broke, and it needs to know that so it can renegotiate instead of retrying a
	// playlist that will never grow. The check is before touch(), because a poll
	// for a dead session is not evidence that anybody is watching it.
	if session.Failed() {
		w.Header().Set("Cache-Control", "no-store")
		http.Error(w, "the stream failed: "+session.Err().Error(), http.StatusGone)
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

// hdrTransferName is the ffmpeg name of the transfer function for a dynamic
// range that keeps HDR.
func hdrTransferName(dynamicRange DynamicRange) string {
	if dynamicRange == RangeHLG {
		return "arib-std-b67"
	}
	return "smpte2084"
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

	// The encoder and its picture plans are chosen before the argument list is
	// assembled because some options are global and have to precede the input:
	// VAAPI's -vaapi_device, without which its upload filter has no device. An
	// HDR decision must also be encoded by the encoder that was verified to
	// produce 10-bit, which is not always the one the preference order picks.
	// There is one plan per rendition, and one rendition unless the decision
	// carries a ladder.
	encoder := ""
	var plans []videoPlan
	if decision.VideoAction == ActionTranscode {
		var err error
		plans, encoder, err = videoPlansFor(decision, cfg.Server)
		if err != nil {
			return nil, err
		}
	}
	device := encoderDevice{RenderNode: cfg.Server.RenderNode}

	// Burning an image subtitle is composited by one filter graph, so it is a
	// single-rendition affair. Negotiation never builds a ladder for a burn; a
	// hand-built decision that does is refused here rather than silently
	// burning only part of the picture.
	burning := decision.BurnedSubtitleIndex > 0 && decision.VideoAction == ActionTranscode
	if burning && len(plans) > 1 {
		return nil, fmt.Errorf("burning subtitle stream %d needs a single rendition, got %d",
			decision.BurnedSubtitleIndex, len(plans))
	}

	args := []string{
		"-hide_banner",
		"-loglevel", "error",
		"-y",
	}
	if startSeconds > 0 {
		args = append(args, "-ss", strconv.FormatFloat(startSeconds, 'f', 3, 64))
	}
	args = append(args, encoderInputArgs(encoder, device)...)
	args = append(args, "-i", inputPath)
	if burning {
		// The subtitle stream is decoded from a second copy of the input. Asking
		// one input for [0:v:0] and [0:s:N] together in the same graph did not
		// deliver subtitle frames in testing, so the file is opened again; the
		// second opening only ever supplies the subtitle, and it is seeked with
		// the same offset so the cue timeline lines up with the picture.
		if startSeconds > 0 {
			args = append(args, "-ss", strconv.FormatFloat(startSeconds, 'f', 3, 64))
		}
		args = append(args, "-i", inputPath)
	}

	audioMap := audioMapSpec(decision)
	switch decision.VideoAction {
	case ActionCopy:
		args = append(args, "-map", "0:v:0")
		if decision.AudioAction != ActionNone {
			args = append(args, "-map", audioMap)
		}
		args = append(args, "-c:v", "copy")
	case ActionTranscode:
		if burning {
			args = append(args, "-filter_complex", burnFilterGraph(encoder, plans[0], decision.BurnedSubtitleIndex))
		}
		// One -map per rendition: the same source stream feeds every rung, and
		// what differs is the options that follow it.
		for index, plan := range plans {
			if burning {
				// The picture is the filter graph's output, not the source
				// stream: the burned subtitle is already part of it.
				args = append(args, "-map", "[v]")
			} else {
				args = append(args, "-map", "0:v:0")
			}
			if decision.AudioAction != ActionNone {
				args = append(args, "-map", audioMap)
			}
			// The picture plan carries the colour: the tone-map chain tags its
			// frames BT.709, and an HDR plan tags them BT.2020/PQ, which is what
			// the encoder writes into the stream. No -color_primaries option is
			// passed because ffmpeg ignores it in favour of the frame's own
			// properties.
			if burning {
				args = append(args, encoderBurnedVideoArgs(encoder, plan)...)
			} else {
				args = append(args, encoderOutputArgs(encoder, plan, device, len(plans), index)...)
			}
			// Cut on the requested segment boundary. Without this ffmpeg only
			// cuts at encoder keyframes - a ~10s default GOP - so -hls_time is
			// advisory and the first segment, and therefore first playback,
			// arrives far later than asked for. It has to be per rendition for
			// the same reason the encoder options are: unsuffixed, only the
			// first rung would be cut on the boundary.
			args = append(args, "-force_key_frames"+videoStreamSuffix(len(plans), index),
				fmt.Sprintf("expr:gte(t,n_forced*%d)", cfg.SegmentSeconds))
			// Forcing a keyframe is not the same as forcing an IDR frame, and
			// only an IDR frame is a key the HLS muxer can cut at. NVENC, QSV
			// and AMF all default their forced-IDR flags to false, so without
			// this the forced keyframes are not marked as key and the cut falls
			// back to the encoder's native GOP - the slow-first-segment problem
			// -force_key_frames exists to remove (S-15). Software and VAAPI need
			// no flag: both produce a keyframe that is already what the muxer
			// cuts on, measured at exactly 2.000 s on this host.
			if flag := forcedIDRFlag(encoder); len(flag) > 0 {
				args = append(args, flag[0]+videoStreamSuffix(len(plans), index), flag[1])
			}
		}
	default:
		return nil, fmt.Errorf("unsupported video action %q", decision.VideoAction)
	}

	switch decision.AudioAction {
	case ActionNone:
		args = append(args, "-an")
	case ActionCopy:
		args = append(args, "-c:a", "copy")
	case ActionTranscode:
		// The encoder, not the codec: ffmpeg's "opus" is its native
		// experimental encoder and refuses to run, while the one that works is
		// "libopus". NegotiateForServer resolves this into a decision; a
		// decision built by hand with only a codec falls back to its most
		// portable encoder here rather than passing the codec through.
		encoder := decision.TargetAudioEncoder
		if encoder == "" {
			encoder = audioEncoderOrDefault(decision.TargetAudioCodec)
		}
		args = append(args, "-c:a", encoder, "-b:a", "192k")
		if decision.TargetAudioChannels > 0 {
			// Chromium refuses a 5.1 AAC SourceBuffer outright, and a browser
			// outputs stereo anyway, so a negotiated downmix is what makes a
			// film with a 5.1 track playable at all.
			args = append(args, "-ac", strconv.Itoa(decision.TargetAudioChannels))
		}
	default:
		return nil, fmt.Errorf("unsupported audio action %q", decision.AudioAction)
	}

	// No -hls_segment_type, so ffmpeg's default MPEG-TS muxer is what produces
	// the segments. That is load-bearing rather than incidental:
	// segmentContainerCanCarry in negotiate.go decides which codecs negotiation
	// may copy on the strength of it, because MPEG-TS carries H.264 and HEVC and
	// writes everything else out as bin_data without complaining (S-3 of the
	// 2026-10-09 review). Switching this to fmp4 would be the better long-term
	// answer - fMP4 carries AV1, VP9, Opus and FLAC, and Apple requires it for
	// HEVC - but it changes the segment extension, the playlist and what the UI
	// fetches, so the two must move together.
	args = append(args,
		"-f", "hls",
		"-hls_time", fmt.Sprint(cfg.SegmentSeconds),
		"-hls_list_size", "0",
		"-hls_playlist_type", "event",
	)
	if len(plans) > 1 {
		// A ladder gets a master playlist and one media playlist per rung. The
		// %v placeholders are what ffmpeg substitutes the variant index for;
		// without them it refuses to write more than one variant at all.
		args = append(args,
			"-master_pl_name", MasterPlaylistName,
			"-var_stream_map", variantStreamMap(decision.AudioAction != ActionNone, len(plans)),
			"-hls_segment_filename", filepath.Join(dir, "seg%v_%05d.ts"),
			filepath.Join(dir, "playlist_%v.m3u8"),
		)
		return args, nil
	}

	args = append(args,
		"-hls_segment_filename", filepath.Join(dir, "seg%05d.ts"),
		filepath.Join(dir, MediaPlaylistName),
	)
	return args, nil
}

// burnFilterGraph composites an image subtitle stream into the picture.
//
// The subtitle comes from the second input and is scaled to the picture with
// scale2ref, so a downscaled re-encode places and sizes it correctly without
// the graph needing to know the source's aspect ratio. The compositing happens
// *after* the plan's own filters, which is what makes it right for a tone map:
// a subtitle bitmap is SDR white, and it must be laid over the finished SDR
// picture rather than passed through the HDR-to-SDR conversion with it.
//
// The graph is built in three stages for a hardware encoder, and that split is
// the S-5 fix. scale2ref and overlay are software filters, so a chain that
// uploaded to a VAAPI surface first handed them hardware frames and ffmpeg
// refused: "Impossible to convert between the formats supported by the filter
// 'Parsed_scale2ref_3' and the filter 'auto_scale_2'". Every burn on a VAAPI
// host therefore failed once and was retried in software, which cost a second
// ffmpeg start, logged a misleading "hardware transcode failed", and bumped the
// fallback counter for a session that was going to fail every time.
//
// So: software filters, then the overlay, then upload and scale on the GPU. The
// hardware path is still used, and it is used after the point that cannot work
// on hardware.
func burnFilterGraph(encoder string, plan videoPlan, subtitleIndex int) string {
	software, needsUpload := videoFilters(encoder, plan)

	var graph strings.Builder
	graph.WriteString("[0:v:0]")
	if len(software) > 0 {
		graph.WriteString(strings.Join(software, ","))
	} else {
		graph.WriteString("null")
	}
	graph.WriteString("[base];")
	// The index is the *global* stream index the probe reports, not the
	// subtitle-relative form ffmpeg would read from ":s:N": a file whose audio
	// tracks come first would otherwise burn the wrong stream.
	fmt.Fprintf(&graph, "[1:%d]format=rgba[burn0];", subtitleIndex)
	graph.WriteString("[burn0][base]scale2ref=flags=neighbor[burn][base2];")
	graph.WriteString("[base2][burn]overlay=format=auto")

	if !needsUpload {
		graph.WriteString("[v]")
		return graph.String()
	}

	// The composited software frames are what the hardware encoder needs; upload
	// them and scale on the device, exactly as the non-burn chain does.
	graph.WriteString("[composited];[composited]")
	graph.WriteString(strings.Join(hardwareUploadFilters(plan), ","))
	graph.WriteString("[v]")
	return graph.String()
}

// audioMapSpec renders the -map specifier for the audio stream this session
// delivers.
//
// It maps by global stream index rather than by the audio-relative form
// ("0:a:0"), because the track a client chose is identified by its global index
// in the probe - the same number subtitle extraction maps by - and the two only
// coincide when the first audio stream is also the chosen one. The trailing "?"
// keeps a file with no audio stream from failing the session. A decision with no
// resolved index (a hand-built one, or a source that was never probed for
// tracks) falls back to the first audio stream, which is what it always meant.
func audioMapSpec(decision Decision) string {
	if decision.TargetAudioStreamIndex > 0 {
		return "0:" + strconv.Itoa(decision.TargetAudioStreamIndex) + "?"
	}
	return "0:a:0?"
}

// MasterPlaylistName and MediaPlaylistName are the two playlist names a session
// can expose. A player is given the master when there is a ladder and the media
// playlist when there is not.
const (
	MasterPlaylistName = "master.m3u8"
	MediaPlaylistName  = "playlist.m3u8"
)

// variantStreamMap tells ffmpeg which mapped streams belong to which rendition,
// pairing each video with its audio when there is audio to pair.
func variantStreamMap(withAudio bool, renditions int) string {
	parts := make([]string, 0, renditions)
	for index := 0; index < renditions; index++ {
		if withAudio {
			parts = append(parts, fmt.Sprintf("v:%d,a:%d", index, index))
			continue
		}
		parts = append(parts, fmt.Sprintf("v:%d", index))
	}
	return strings.Join(parts, " ")
}

// sessionPlaylistName is the file a client should open for this decision.
func sessionPlaylistName(decision Decision) string {
	if len(decision.Renditions) > 1 {
		return MasterPlaylistName
	}
	return MediaPlaylistName
}
