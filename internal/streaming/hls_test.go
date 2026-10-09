package streaming

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ykzird/astraeus/internal/observability"
)

// newTestLogger keeps manager logging out of the test output.
func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNewManager_SweepsStaleSessionDirectories(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	stale := filepath.Join(root, "stale-session")
	fresh := filepath.Join(root, "fresh-session")
	for _, dir := range []string{stale, fresh} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("creating %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "playlist.m3u8"), []byte("#EXTM3U"), 0o644); err != nil {
			t.Fatalf("writing playlist: %v", err)
		}
		// Rendered by the server for every session it starts; the reaper only
		// touches directories carrying it.
		if err := os.WriteFile(filepath.Join(dir, sessionMarkerFile), []byte("session\n"), 0o644); err != nil {
			t.Fatalf("writing the session marker: %v", err)
		}
	}

	// Age one directory well past the session TTL.
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("ageing %s: %v", stale, err)
	}

	manager, err := NewManager(context.Background(), ManagerConfig{
		RootDir:    root,
		SessionTTL: time.Minute,
		Logger:     newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(manager.Close)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale session directory survived startup: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a session inside the TTL was removed: %v", err)
	}
}

func TestNewManager_RequiresRootDir(t *testing.T) {
	t.Parallel()

	if _, err := NewManager(context.Background(), ManagerConfig{Logger: newTestLogger()}); err == nil {
		t.Fatal("expected an error when RootDir is empty")
	}
}

func TestNewManager_AppliesDefaults(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(context.Background(), ManagerConfig{
		RootDir: t.TempDir(),
		Logger:  newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(manager.Close)

	cfg := manager.Config()
	if cfg.FFmpegBin != "ffmpeg" {
		t.Errorf("ffmpeg binary = %q, want ffmpeg", cfg.FFmpegBin)
	}
	if cfg.SegmentSeconds != defaultSegmentSeconds {
		t.Errorf("segment seconds = %d, want %d", cfg.SegmentSeconds, defaultSegmentSeconds)
	}
	if cfg.SessionTTL != defaultSessionTTL {
		t.Errorf("session TTL = %v, want %v", cfg.SessionTTL, defaultSessionTTL)
	}
}

func TestManager_StartRejectsDirectPlay(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(context.Background(), ManagerConfig{
		RootDir: t.TempDir(),
		Logger:  newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(manager.Close)

	_, err = manager.Start(context.Background(), "entity", "/media/movie.mp4", Decision{
		Mode:        ModeDirectPlay,
		Deliverable: true,
	})
	if err == nil {
		t.Fatal("expected an error: direct play does not need a session")
	}
}

func TestManager_StartRejectsUndeliverableDecision(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(context.Background(), ManagerConfig{
		RootDir: t.TempDir(),
		Logger:  newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(manager.Close)

	_, err = manager.Start(context.Background(), "entity", "/media/movie.mkv", Decision{
		Mode:        ModeRemux,
		Deliverable: false,
	})
	if err == nil {
		t.Fatal("expected an error for an undeliverable decision")
	}
}

func TestManager_ReapStopsIdleSessions(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(context.Background(), ManagerConfig{
		RootDir:    t.TempDir(),
		SessionTTL: time.Minute,
		Logger:     newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(manager.Close)

	// A session with no live process: Stop must still be safe and empty the map.
	session := &Session{
		ID:         "idle",
		Dir:        t.TempDir(),
		done:       make(chan struct{}),
		cancel:     func() {},
		startedAt:  time.Now().Add(-time.Hour),
		lastAccess: time.Now().Add(-time.Hour),
	}
	close(session.done)

	manager.mu.Lock()
	manager.sessions["idle"] = session
	manager.mu.Unlock()

	if reaped := manager.Reap(time.Now()); reaped != 1 {
		t.Fatalf("reaped %d sessions, want 1", reaped)
	}
	if _, ok := manager.Session("idle"); ok {
		t.Error("the idle session is still registered")
	}
}

// TestManager_StartFallsBackToSoftwareWhenHardwareFails reproduces the reported
// failure: an encoder that is *listed* but cannot open a session. The stub
// ffmpeg below fails exactly the way h264_qsv does on a machine with no Intel
// GPU ("Error creating a MFX session"), and the manager must notice and retry
// in software rather than failing the request.
func TestManager_StartFallsBackToSoftwareWhenHardwareFails(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("the stub encoder is a shell script")
	}

	dir := t.TempDir()
	encoder := filepath.Join(dir, "ffmpeg-stub")
	stub := `#!/bin/sh
for arg in "$@"; do
  if [ "$arg" = "h264_qsv" ]; then
    echo "[h264_qsv @ 0x0] Error creating a MFX session: -9." >&2
    exit 1
  fi
done
out=""
for arg in "$@"; do out="$arg"; done
mkdir -p "$(dirname "$out")"
printf '#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:4.000000,\nseg00000.ts\n' > "$out"
printf 'segment' > "$(dirname "$out")/seg00000.ts"
exit 0
`
	if err := os.WriteFile(encoder, []byte(stub), 0o755); err != nil {
		t.Fatalf("writing the stub encoder: %v", err)
	}

	metrics := observability.New()
	manager, err := NewManager(context.Background(), ManagerConfig{
		FFmpegBin:      encoder,
		RootDir:        filepath.Join(dir, "sessions"),
		SegmentSeconds: 4,
		Server: ServerCapability{
			VideoEncoders:        []string{"h264_qsv", "libx264"},
			HardwareAcceleration: []string{"qsv"},
		},
		Metrics: metrics,
		Logger:  newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(manager.Close)

	// A hardware encoder would be chosen, so the first attempt fails.
	decision := Decision{
		Mode: ModeTranscode, Deliverable: true,
		VideoAction: ActionTranscode, AudioAction: ActionTranscode,
		TargetVideoCodec: "h264", TargetAudioCodec: "aac",
	}
	if !wouldUseHardware(decision, manager.Config()) {
		t.Fatal("this test needs a decision that would use the hardware encoder")
	}

	session, err := manager.Start(context.Background(), "entity-1", "/media/movie.mkv", decision)
	if err != nil {
		t.Fatalf("Start should have retried in software, got: %v", err)
	}
	t.Cleanup(func() { manager.Stop(session.ID) })

	if rendered := metrics.Render(); !strings.Contains(rendered, "astraeus_transcode_fallbacks_total 1") {
		t.Errorf("the fallback was not counted:\n%s", rendered)
	}
}

func TestWouldUseHardware(t *testing.T) {
	t.Parallel()

	quickSync := ManagerConfig{
		Server: ServerCapability{VideoEncoders: []string{"h264_qsv", "libx264"}, HardwareAcceleration: []string{"qsv"}},
	}
	software := ManagerConfig{
		Server: ServerCapability{VideoEncoders: []string{"libx264"}},
	}

	transcode := Decision{Mode: ModeTranscode, VideoAction: ActionTranscode, TargetVideoCodec: "h264"}
	remux := Decision{Mode: ModeRemux, VideoAction: ActionCopy}
	direct := Decision{Mode: ModeDirectPlay, VideoAction: ActionCopy}

	if !wouldUseHardware(transcode, quickSync) {
		t.Error("a transcode on a QuickSync host should use hardware")
	}
	if wouldUseHardware(transcode, software) {
		t.Error("a software-only host must not report hardware use")
	}
	if wouldUseHardware(remux, quickSync) {
		t.Error("a stream copy uses no encoder at all")
	}
	if wouldUseHardware(direct, quickSync) {
		t.Error("direct play uses no encoder at all")
	}
}

func TestWithoutHardware(t *testing.T) {
	t.Parallel()

	cfg := withoutHardware(ManagerConfig{
		Server: ServerCapability{
			VideoEncoders:        []string{"h264_qsv", "hevc_vaapi", "libx264"},
			HardwareAcceleration: []string{"qsv"},
		},
	})

	if len(cfg.Server.HardwareAcceleration) != 0 {
		t.Errorf("hardware acceleration = %v, want it cleared", cfg.Server.HardwareAcceleration)
	}
	if cfg.Server.RenderNode != "" {
		t.Errorf("render node = %q, want it cleared alongside the encoders", cfg.Server.RenderNode)
	}
	for _, encoder := range cfg.Server.VideoEncoders {
		if isHardwareEncoder(encoder) {
			t.Errorf("hardware encoder %q survived the downgrade", encoder)
		}
	}
	if len(cfg.Server.VideoEncoders) != 1 || cfg.Server.VideoEncoders[0] != "libx264" {
		t.Errorf("software encoders = %v, want just libx264", cfg.Server.VideoEncoders)
	}
}

// TestManager_SweepOnlyRemovesItsOwnDirectories is the guard on the one
// operation in this package that can destroy data: --stream-root pointed at a
// shared or mistyped directory must not delete what it finds there.
func TestManager_SweepOnlyRemovesItsOwnDirectories(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	// Something that is not ours, old enough to be swept on age alone.
	foreign := filepath.Join(root, "someone-elses-data")
	if err := os.MkdirAll(foreign, 0o755); err != nil {
		t.Fatalf("creating the foreign directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(foreign, "important.txt"), []byte("do not delete"), 0o644); err != nil {
		t.Fatalf("writing the foreign file: %v", err)
	}

	// One of ours, equally old.
	ours := filepath.Join(root, "11111111-2222-3333-4444-555555555555")
	if err := os.MkdirAll(ours, 0o755); err != nil {
		t.Fatalf("creating the session directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ours, sessionMarkerFile), []byte("session\n"), 0o644); err != nil {
		t.Fatalf("writing the marker: %v", err)
	}

	old := time.Now().Add(-48 * time.Hour)
	for _, dir := range []string{foreign, ours} {
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatalf("ageing %s: %v", dir, err)
		}
	}

	if _, err := NewManager(context.Background(), ManagerConfig{
		RootDir: root,
		Logger:  newTestLogger(),
	}); err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if _, err := os.Stat(foreign); err != nil {
		t.Error("a directory without the session marker was removed")
	}
	if _, err := os.Stat(ours); !os.IsNotExist(err) {
		t.Error("a stale session directory carrying the marker was not removed")
	}
}

// TestManager_RefusesToExceedMaxSessions covers the resource-exhaustion guard:
// each session is an ffmpeg process, so an unbounded loop of playback requests
// would fork the host to death.
func TestManager_RefusesToExceedMaxSessions(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("the stub encoder is a shell script")
	}

	dir := t.TempDir()
	encoder := filepath.Join(dir, "ffmpeg-stub")
	stub := `#!/bin/sh
out=""
for arg in "$@"; do out="$arg"; done
mkdir -p "$(dirname "$out")"
printf '#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:4.000000,\nseg00000.ts\n' > "$out"
printf 'segment' > "$(dirname "$out")/seg00000.ts"
exit 0
`
	if err := os.WriteFile(encoder, []byte(stub), 0o755); err != nil {
		t.Fatalf("writing the stub encoder: %v", err)
	}

	manager, err := NewManager(context.Background(), ManagerConfig{
		FFmpegBin:   encoder,
		RootDir:     filepath.Join(dir, "sessions"),
		MaxSessions: 1,
		Server:      ServerCapability{VideoEncoders: []string{"libx264"}},
		Metrics:     observability.New(),
		Logger:      newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(manager.Close)

	decision := Decision{
		Mode: ModeTranscode, Deliverable: true,
		VideoAction: ActionTranscode, AudioAction: ActionTranscode,
		TargetVideoCodec: "h264", TargetAudioCodec: "aac",
	}

	first, err := manager.Start(context.Background(), "entity-1", "/media/a.mkv", decision)
	if err != nil {
		t.Fatalf("the first session should be allowed: %v", err)
	}
	t.Cleanup(func() { manager.Stop(first.ID) })

	if _, err := manager.Start(context.Background(), "entity-2", "/media/b.mkv", decision); !errors.Is(err, ErrTooManySessions) {
		t.Fatalf("second session error = %v, want ErrTooManySessions", err)
	}

	// Freeing one must let the next in, or the cap becomes a permanent wedge.
	manager.Stop(first.ID)
	if _, err := manager.Start(context.Background(), "entity-3", "/media/c.mkv", decision); err != nil {
		t.Fatalf("a slot freed by stopping a session was not reusable: %v", err)
	}
}

// TestManager_MaxSessionsHoldsUnderConcurrency is the regression test for S-2.
//
// The old check read len(m.sessions) under the lock, released it, and inserted
// the session much later - after mkdir, after building the command line, after
// cmd.Start. Every concurrent caller could therefore pass the same check, and
// the review measured ten parallel starts against a limit of two leaving nine
// ffmpeg processes running. The slot is now claimed in the same critical
// section as the check, so the number of sessions that get to fork ffmpeg
// cannot exceed the limit however the callers interleave.
func TestManager_MaxSessionsHoldsUnderConcurrency(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("the stub encoder is a shell script")
	}

	dir := t.TempDir()
	encoder := filepath.Join(dir, "ffmpeg-stub")
	// The sleep is what makes this a test: it holds the window between the
	// check and the publication of the session open long enough for parallel
	// callers to pile into it.
	stub := `#!/bin/sh
sleep 0.3
out=""
for arg in "$@"; do out="$arg"; done
mkdir -p "$(dirname "$out")"
printf '#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:4.000000,\nseg00000.ts\n' > "$out"
printf 'segment' > "$(dirname "$out")/seg00000.ts"
exit 0
`
	if err := os.WriteFile(encoder, []byte(stub), 0o755); err != nil {
		t.Fatalf("writing the stub encoder: %v", err)
	}

	const (
		limit   = 2
		callers = 10
	)

	manager, err := NewManager(context.Background(), ManagerConfig{
		FFmpegBin:   encoder,
		RootDir:     filepath.Join(dir, "sessions"),
		MaxSessions: limit,
		Server:      ServerCapability{VideoEncoders: []string{"libx264"}},
		Metrics:     observability.New(),
		Logger:      newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(manager.Close)

	decision := Decision{
		Mode: ModeTranscode, Deliverable: true,
		VideoAction: ActionTranscode, AudioAction: ActionTranscode,
		TargetVideoCodec: "h264", TargetAudioCodec: "aac",
	}

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		started   []*Session
		refused   int
		otherErrs []error
	)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			session, err := manager.Start(context.Background(),
				fmt.Sprintf("entity-%d", i), fmt.Sprintf("/media/%d.mkv", i), decision)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				started = append(started, session)
			case errors.Is(err, ErrTooManySessions):
				refused++
			default:
				otherErrs = append(otherErrs, err)
			}
		}(i)
	}
	wg.Wait()

	for _, err := range otherErrs {
		t.Errorf("unexpected start error: %v", err)
	}
	if len(started) != limit {
		t.Errorf("%d of %d concurrent starts were allowed, want exactly %d (the limit)",
			len(started), callers, limit)
	}
	if refused != callers-limit {
		t.Errorf("%d starts were refused, want %d", refused, callers-limit)
	}
	if got := manager.ActiveSessions(); got > limit {
		t.Errorf("the manager reports %d live sessions, want at most the limit of %d", got, limit)
	}

	// A refused attempt must not have leaked its reservation.
	for _, session := range started {
		manager.Stop(session.ID)
	}
	if got := manager.ActiveSessions(); got != 0 {
		t.Errorf("after stopping every session the manager still reports %d, want 0 "+
			"(a reservation leaked on a failure path)", got)
	}
}

// TestManager_FailedStartReleasesItsSlot guards the other half of the protocol:
// a start that fails after claiming a slot has to give it back, or a broken
// encoder would permanently consume the session budget.
func TestManager_FailedStartReleasesItsSlot(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("the stub encoder is a shell script")
	}

	dir := t.TempDir()
	encoder := filepath.Join(dir, "ffmpeg-stub")
	// Starts, writes nothing, exits non-zero: the playlist never appears.
	if err := os.WriteFile(encoder, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("writing the stub encoder: %v", err)
	}

	manager, err := NewManager(context.Background(), ManagerConfig{
		FFmpegBin:   encoder,
		RootDir:     filepath.Join(dir, "sessions"),
		MaxSessions: 1,
		Server:      ServerCapability{VideoEncoders: []string{"libx264"}},
		Metrics:     observability.New(),
		Logger:      newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(manager.Close)

	decision := Decision{
		Mode: ModeTranscode, Deliverable: true,
		VideoAction: ActionTranscode, AudioAction: ActionTranscode,
		TargetVideoCodec: "h264", TargetAudioCodec: "aac",
	}

	for attempt := 0; attempt < 3; attempt++ {
		if _, err := manager.Start(context.Background(), "entity-1", "/media/a.mkv", decision); err == nil {
			t.Fatal("a start with a failing encoder should not succeed")
		}
		if got := manager.ActiveSessions(); got != 0 {
			t.Fatalf("attempt %d leaked a slot: %d sessions are still accounted for", attempt, got)
		}
	}
}

// sessionLimitStub writes a playlist and a segment and then exits, which is the
// shape that gets a session published and the error counter incremented.
const sessionLimitStub = `#!/bin/sh
out=""
for arg in "$@"; do out="$arg"; done
mkdir -p "$(dirname "$out")"
printf '#EXTM3U\n#EXT-X-VERSION:3\n#EXTINF:4.000000,\nseg00000.ts\n' > "$out"
printf 'segment' > "$(dirname "$out")/seg00000.ts"
exit 0
`

func writeStubEncoder(t *testing.T, dir, script string) string {
	t.Helper()

	path := filepath.Join(dir, "ffmpeg-stub")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing the stub encoder: %v", err)
	}
	return path
}

// TestManager_CapacityRefusalIsNotAHardwareFailure is the regression test for
// S-11.
//
// The hardware-to-software retry fired on any error, so a request refused
// because the session limit was reached logged "hardware transcode failed" and
// incremented the fallback counter - and then started a second ffmpeg process
// to be refused again. A capacity refusal says nothing about the encoder.
func TestManager_CapacityRefusalIsNotAHardwareFailure(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("the stub encoder is a shell script")
	}

	dir := t.TempDir()
	metrics := observability.New()
	manager, err := NewManager(context.Background(), ManagerConfig{
		FFmpegBin:   writeStubEncoder(t, dir, sessionLimitStub),
		RootDir:     filepath.Join(dir, "sessions"),
		MaxSessions: 1,
		// A hardware encoder, so wouldUseHardware is true and the fallback path
		// is reachable at all.
		Server: ServerCapability{
			VideoEncoders:        []string{"h264_vaapi", "libx264"},
			HardwareAcceleration: []string{"vaapi"},
		},
		Metrics: metrics,
		Logger:  newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(manager.Close)

	decision := Decision{
		Mode: ModeTranscode, Deliverable: true,
		VideoAction: ActionTranscode, AudioAction: ActionTranscode,
		TargetVideoCodec: "h264", TargetAudioCodec: "aac",
	}

	first, err := manager.Start(context.Background(), "entity-1", "/media/a.mkv", decision)
	if err != nil {
		t.Fatalf("the first session should be allowed: %v", err)
	}
	t.Cleanup(func() { manager.Stop(first.ID) })

	if _, err := manager.Start(context.Background(), "entity-2", "/media/b.mkv", decision); !errors.Is(err, ErrTooManySessions) {
		t.Fatalf("second session error = %v, want ErrTooManySessions", err)
	}

	rendered := metrics.Render()
	if strings.Contains(rendered, "astraeus_transcode_fallbacks_total") {
		t.Errorf("a capacity refusal was counted as a hardware failure:\n%s", rendered)
	}
	if !strings.Contains(rendered, `astraeus_stream_errors_total{mode="transcode"} 0`) &&
		strings.Contains(rendered, "astraeus_stream_errors_total") {
		t.Errorf("a capacity refusal counted as a stream error:\n%s", rendered)
	}
}

// TestManager_FailedSessionIsCountedOnce is the regression test for S-12.
//
// A failed start was counted by the wait goroutine and again by the startup
// failure branch, so astraeus_stream_errors_total was inflated and the same
// failure appeared in two places in the log.
func TestManager_FailedSessionIsCountedOnce(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("the stub encoder is a shell script")
	}

	dir := t.TempDir()
	metrics := observability.New()
	// ffmpeg dies without ever writing a playlist. That is the shape that was
	// counted twice: the wait goroutine records the unexpected exit, and then
	// the branch waiting for the playlist records the same failure again.
	failing := `#!/bin/sh
exit 1
`
	manager, err := NewManager(context.Background(), ManagerConfig{
		FFmpegBin:      writeStubEncoder(t, dir, failing),
		RootDir:        filepath.Join(dir, "sessions"),
		SegmentSeconds: 1,
		Server:         ServerCapability{VideoEncoders: []string{"libx264"}},
		Metrics:        metrics,
		Logger:         newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(manager.Close)

	decision := Decision{
		Mode: ModeTranscode, Deliverable: true,
		VideoAction: ActionTranscode, AudioAction: ActionTranscode,
		TargetVideoCodec: "h264", TargetAudioCodec: "aac",
	}

	if _, err := manager.Start(context.Background(), "entity-1", "/media/a.mkv", decision); err == nil {
		t.Fatal("a start whose ffmpeg never produces a playlist should fail")
	}

	// The wait goroutine that records the count is not the one that closed
	// Done, so poll briefly rather than reading a metric that may be a moment
	// behind. Reading the exact value is the point: the bug was a count of two.
	deadline := time.Now().Add(5 * time.Second)
	for {
		rendered := metrics.Render()
		if strings.Contains(rendered, `astraeus_stream_errors_total{mode="transcode"} 1`) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("one failed session should count exactly once; metrics were:\n%s", rendered)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestManager_ReapsASessionWhoseEncoderDied is the regression test for S-9.
//
// Reap looked only at lastAccess. When ffmpeg dies mid-film it never writes
// #EXT-X-ENDLIST, so hls.js keeps polling the playlist - and every poll calls
// touch(), which made the session look busy forever. The slot stayed held and the
// viewer watched a player that would never advance.
//
// The session here is touched *now* and has a minute of TTL, so the idle rule
// cannot fire. Only the failure rule can.
func TestManager_ReapsASessionWhoseEncoderDied(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(context.Background(), ManagerConfig{
		RootDir:    t.TempDir(),
		SessionTTL: time.Minute,
		Logger:     newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(manager.Close)

	done := make(chan struct{})
	close(done)
	session := &Session{
		ID:         "died",
		Dir:        t.TempDir(),
		done:       done,
		cancel:     func() {},
		startedAt:  time.Now().Add(-time.Minute),
		lastAccess: time.Now(), // touched just now, by the poll that keeps it alive
		runErr:     errors.New("ffmpeg exited with status 1"),
	}

	manager.mu.Lock()
	manager.sessions["died"] = session
	manager.mu.Unlock()

	if !session.Failed() {
		t.Fatal("a session whose process exited with an error does not report Failed")
	}
	if reaped := manager.Reap(time.Now()); reaped != 1 {
		t.Fatalf("reaped %d sessions, want 1: a session whose encoder died is finished "+
			"with however recently it was touched", reaped)
	}
	if _, ok := manager.Session("died"); ok {
		t.Error("the failed session is still registered")
	}
}

// TestManager_KeepsASessionThatFinishedCleanly guards the other direction. A
// session that produced a complete playlist must keep serving its last segments
// to a client that is still fetching them, so it is not reaped for being done.
func TestManager_KeepsASessionThatFinishedCleanly(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(context.Background(), ManagerConfig{
		RootDir:    t.TempDir(),
		SessionTTL: time.Minute,
		Logger:     newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(manager.Close)

	done := make(chan struct{})
	close(done)
	session := &Session{
		ID:         "finished",
		Dir:        t.TempDir(),
		done:       done,
		cancel:     func() {},
		startedAt:  time.Now().Add(-time.Minute),
		lastAccess: time.Now(),
		runErr:     nil, // a clean exit
	}

	manager.mu.Lock()
	manager.sessions["finished"] = session
	manager.mu.Unlock()

	if session.Failed() {
		t.Error("a session that finished successfully reports Failed")
	}
	if reaped := manager.Reap(time.Now()); reaped != 0 {
		t.Errorf("reaped %d sessions, want 0: a completed playlist must keep serving "+
			"until the client stops asking for it", reaped)
	}
	if _, ok := manager.Session("finished"); !ok {
		t.Error("a session that finished successfully was removed while it was still in use")
	}
}

// TestManager_FailedSessionIsGoneNotNotFound pins what a client is told. A failed
// stream answers 410 so the client renegotiates, rather than 404 which reads as "no
// such session" or a 200 with a playlist that will never grow.
func TestManager_FailedSessionIsGoneNotNotFound(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(context.Background(), ManagerConfig{
		RootDir:    t.TempDir(),
		SessionTTL: time.Minute,
		Logger:     newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(manager.Close)

	done := make(chan struct{})
	close(done)
	session := &Session{
		ID:         "died",
		Dir:        t.TempDir(),
		done:       done,
		cancel:     func() {},
		startedAt:  time.Now(),
		lastAccess: time.Now(),
		runErr:     errors.New("ffmpeg exited with status 1"),
	}
	manager.mu.Lock()
	manager.sessions["died"] = session
	manager.mu.Unlock()

	recorder := httptest.NewRecorder()
	// A name the route accepts: the handler rejects anything outside its own
	// pattern with a 404 before it looks at the session, so a made-up file name
	// would test the pattern rather than the failure.
	request := httptest.NewRequest(http.MethodGet, "/hls/died/master.m3u8", nil)
	manager.ServeFile(recorder, request, "died", "master.m3u8")

	if recorder.Code != http.StatusGone {
		t.Errorf("status = %d, want 410: a session whose encoder died is gone, and the "+
			"client has to know that to renegotiate", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "ffmpeg exited") {
		t.Errorf("the 410 does not say why the stream failed: %q", recorder.Body.String())
	}
}

// TestSessionTTL_CoversAPause is the regression test for S-10.
//
// The idle rule measures whether a client has asked for anything, which is not
// whether anyone is watching. hls.js stops polling once a playlist carries
// #EXT-X-ENDLIST, and a fast remux writes that well before the viewer has
// finished - so at the old two-minute default, pausing the film for three minutes
// had the session reaped underneath and the UI offered "Recovery was not
// possible".
//
// The default is asserted rather than left implicit: it is the value that decides
// whether the scenario happens, and a two-minute default is the bug.
func TestSessionTTL_CoversAPause(t *testing.T) {
	t.Parallel()

	if defaultSessionTTL < 10*time.Minute {
		t.Errorf("the default session TTL is %v; a viewer who pauses for longer than that "+
			"loses the stream, because a completed playlist stops the client polling",
			defaultSessionTTL)
	}

	// And the TTL is the operator's to set, because how long a pause is
	// reasonable depends on the install.
	manager, err := NewManager(context.Background(), ManagerConfig{
		RootDir:    t.TempDir(),
		SessionTTL: 45 * time.Minute,
		Logger:     newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(manager.Close)

	if got := manager.SessionTTL(); got != 45*time.Minute {
		t.Errorf("session TTL = %v, want the configured 45m", got)
	}
}

// TestReap_KeepsASessionWithinItsTTL is the other half: raising the default only
// helps if the reaper honours it.
func TestReap_KeepsASessionWithinItsTTL(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(context.Background(), ManagerConfig{
		RootDir:    t.TempDir(),
		SessionTTL: 30 * time.Minute,
		Logger:     newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(manager.Close)

	// A completed session - done closed with no error - touched three minutes
	// ago. That is the pause the finding describes.
	done := make(chan struct{})
	close(done)
	session := &Session{
		ID:         "paused",
		Dir:        t.TempDir(),
		done:       done,
		cancel:     func() {},
		startedAt:  time.Now().Add(-time.Hour),
		lastAccess: time.Now().Add(-3 * time.Minute),
	}
	manager.mu.Lock()
	manager.sessions["paused"] = session
	manager.mu.Unlock()

	if reaped := manager.Reap(time.Now()); reaped != 0 {
		t.Errorf("a session idle for three minutes with a thirty-minute TTL was reaped; " +
			"a fast remux has already finished producing, so the client has stopped " +
			"asking and the viewer is still watching")
	}

	// And one idle past the TTL is still reaped, so the bound is real.
	session.mu.Lock()
	session.lastAccess = time.Now().Add(-31 * time.Minute)
	session.mu.Unlock()
	if reaped := manager.Reap(time.Now()); reaped != 1 {
		t.Errorf("reaped %d sessions, want 1: the TTL bound must still hold", reaped)
	}
}
