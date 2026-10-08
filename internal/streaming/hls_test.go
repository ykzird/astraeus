package streaming

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jok/astraeus-media/internal/observability"
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
