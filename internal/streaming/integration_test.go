//go:build integration

// These tests execute the real ffmpeg and ffprobe binaries. Run them with:
//
//	go test -tags=integration ./internal/streaming/
package streaming

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jok/astraeus-media/internal/observability"
)

// discardLogger keeps ffmpeg session logs out of the test output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func requireFFmpeg(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	if _, err := exec.LookPath("ffprobe"); err != nil {
		t.Skip("ffprobe is not installed")
	}
}

// generateClip renders a short H.264/AAC test clip into dir.
func generateClip(t *testing.T, dir, name string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	cmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=15:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-shortest",
		path,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating test clip %s: %v\n%s", name, err, output)
	}
	return path
}

func TestFFProbe_RealFile(t *testing.T) {
	requireFFmpeg(t)

	path := generateClip(t, t.TempDir(), "probe-source.mkv")

	info, err := NewFFProbe("ffprobe").Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}

	if info.VideoCodec != "h264" {
		t.Errorf("video codec = %q, want h264", info.VideoCodec)
	}
	if info.AudioCodec != "aac" {
		t.Errorf("audio codec = %q, want aac", info.AudioCodec)
	}
	if info.Width != 320 || info.Height != 240 {
		t.Errorf("dimensions = %dx%d, want 320x240", info.Width, info.Height)
	}
	if info.Container != "matroska" {
		t.Errorf("container = %q, want matroska", info.Container)
	}
	if info.DurationSeconds <= 0 {
		t.Errorf("duration = %v, want a positive value", info.DurationSeconds)
	}
}

func TestFFProbe_MissingFile(t *testing.T) {
	requireFFmpeg(t)

	_, err := NewFFProbe("ffprobe").Probe(context.Background(), filepath.Join(t.TempDir(), "nope.mkv"))
	if err == nil {
		t.Fatal("expected an error probing a file that does not exist")
	}
}

func TestManager_RemuxEndToEnd(t *testing.T) {
	requireFFmpeg(t)

	root := t.TempDir()
	source := generateClip(t, root, "source.mkv")

	info, err := NewFFProbe("ffprobe").Probe(context.Background(), source)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}

	decision := Negotiate(info, BrowserCapability())
	if decision.Mode != ModeRemux {
		t.Fatalf("mode = %q, want %q for a matroska source", decision.Mode, ModeRemux)
	}

	manager, session, metrics := startSession(t, root, source, decision)

	playlist, err := os.ReadFile(session.PlaylistPath())
	if err != nil {
		t.Fatalf("reading playlist: %v", err)
	}
	if !strings.HasPrefix(string(playlist), "#EXTM3U") {
		t.Errorf("playlist does not start with #EXTM3U:\n%s", playlist)
	}

	segment := waitForSegment(t, session.Dir, 20*time.Second)
	if segment == "" {
		t.Fatal("no segment was produced")
	}

	// The playlist and a segment must be servable over HTTP.
	recorder := httptest.NewRecorder()
	manager.ServeFile(recorder, httptest.NewRequest(http.MethodGet, "/hls/x/playlist.m3u8", nil), session.ID, "playlist.m3u8")
	if recorder.Code != http.StatusOK {
		t.Errorf("playlist status = %d, want 200", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/vnd.apple.mpegurl" {
		t.Errorf("playlist content type = %q", got)
	}

	recorder = httptest.NewRecorder()
	manager.ServeFile(recorder, httptest.NewRequest(http.MethodGet, "/hls/x/"+segment, nil), session.ID, segment)
	if recorder.Code != http.StatusOK {
		t.Errorf("segment status = %d, want 200", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != "video/mp2t" {
		t.Errorf("segment content type = %q", got)
	}
	body, _ := io.ReadAll(recorder.Body)
	if len(body) == 0 {
		t.Error("segment body is empty")
	}

	// Path traversal must be refused.
	recorder = httptest.NewRecorder()
	manager.ServeFile(recorder, httptest.NewRequest(http.MethodGet, "/hls/x/../../etc/passwd", nil), session.ID, "../../etc/passwd")
	if recorder.Code != http.StatusNotFound {
		t.Errorf("traversal status = %d, want 404", recorder.Code)
	}

	assertMetric(t, metrics, `astraeus_stream_sessions_total{mode="remux",outcome="started"} 1`)
	assertMetric(t, metrics, `astraeus_transcode_startup_seconds_count{mode="remux"} 1`)
	assertMetric(t, metrics, `astraeus_first_segment_seconds_count{mode="remux"} 1`)

	manager.Stop(session.ID)
	if _, err := os.Stat(session.Dir); !os.IsNotExist(err) {
		t.Errorf("session directory still exists after Stop: %v", err)
	}
	assertMetric(t, metrics, "astraeus_stream_sessions_active 0")
}

// assertMetric fails when the rendered metrics do not contain want.
func assertMetric(t *testing.T, metrics *observability.Metrics, want string) {
	t.Helper()
	if got := metrics.Render(); !strings.Contains(got, want) {
		t.Errorf("metrics are missing %q\n---\n%s", want, got)
	}
}

func TestManager_TranscodeEndToEnd(t *testing.T) {
	requireFFmpeg(t)

	root := t.TempDir()
	source := generateClip(t, root, "transcode-source.mkv")

	info, err := NewFFProbe("ffprobe").Probe(context.Background(), source)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}

	// A client that cannot decode H.264 forces a real video transcode.
	capability := ClientCapability{
		Containers:  []string{"hls"},
		VideoCodecs: []string{"h264"},
		AudioCodecs: []string{"aac"},
		MaxHeight:   120,
		SupportsHLS: true,
	}
	decision := Negotiate(info, capability)
	if decision.Mode != ModeTranscode {
		t.Fatalf("mode = %q, want %q", decision.Mode, ModeTranscode)
	}
	if decision.TargetHeight != 120 {
		t.Fatalf("target height = %d, want 120", decision.TargetHeight)
	}

	manager, session, metrics := startSession(t, root, source, decision)

	if segment := waitForSegment(t, session.Dir, 60*time.Second); segment == "" {
		diagnostics := session.Diagnostics()
		t.Fatalf("no segment was produced by the transcode\n%s", diagnostics)
	}

	// The output must actually have been scaled down to the negotiated height.
	probeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var scaled bool
	entries, err := os.ReadDir(session.Dir)
	if err != nil {
		t.Fatalf("reading session directory: %v", err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".ts" {
			continue
		}
		out, err := NewFFProbe("ffprobe").Probe(probeCtx, filepath.Join(session.Dir, entry.Name()))
		if err != nil {
			t.Fatalf("probing segment: %v", err)
		}
		if out.Height == 120 {
			scaled = true
		}
	}
	if !scaled {
		t.Error("no segment was scaled to the negotiated height of 120p")
	}

	assertMetric(t, metrics, `astraeus_stream_sessions_total{mode="transcode",outcome="started"} 1`)
	assertMetric(t, metrics, `astraeus_transcode_startup_seconds_count{mode="transcode"} 1`)

	manager.Stop(session.ID)
}

func TestDetectServerCapability(t *testing.T) {
	requireFFmpeg(t)

	capability := DetectServerCapability(context.Background(), "ffmpeg", "ffprobe", t.TempDir())
	if !capability.FFmpegAvailable {
		t.Error("ffmpeg was reported as unavailable")
	}
	if !capability.FFprobeAvailable {
		t.Error("ffprobe was reported as unavailable")
	}
	if !capability.HLS {
		t.Error("HLS should always be reported as supported")
	}
	if len(capability.VideoEncoders) == 0 {
		t.Error("no usable video encoders were detected")
	}
	// With no device directory there must be no hardware acceleration claim.
	if capability.HardwareAcceleration != "" {
		t.Errorf("hardware acceleration = %q, want none without a device",
			capability.HardwareAcceleration)
	}
}

// startSession creates a manager, starts a session and registers cleanup. The
// metrics collector is returned so the KPIs can be asserted.
func startSession(t *testing.T, root, source string, decision Decision) (*Manager, *Session, *observability.Metrics) {
	t.Helper()

	metrics := observability.New()
	manager, err := NewManager(context.Background(), ManagerConfig{
		FFmpegBin:      "ffmpeg",
		RootDir:        filepath.Join(root, "sessions"),
		SegmentSeconds: 1,
		SessionTTL:     time.Minute,
		Server:         DetectServerCapability(context.Background(), "ffmpeg", "ffprobe", ""),
		Metrics:        metrics,
		Logger:         discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(manager.Close)

	session, err := manager.Start(context.Background(), "entity-1", source, decision)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return manager, session, metrics
}

// waitForSegment waits for ffmpeg to publish at least one segment.
func waitForSegment(t *testing.T, dir string, timeout time.Duration) string {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("reading session directory: %v", err)
		}
		for _, entry := range entries {
			if filepath.Ext(entry.Name()) == ".ts" {
				if info, err := entry.Info(); err == nil && info.Size() > 0 {
					return entry.Name()
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return ""
}
