//go:build integration

// These tests execute the real ffmpeg and ffprobe binaries. Run them with:
//
//	go test -tags=integration ./internal/streaming/
package streaming

import (
	"context"
	"fmt"
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

	"github.com/ykzird/astraeus/internal/observability"
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

// generateOddDimensionClip renders a source whose height is odd, in a container
// that carries it unchanged.
//
// This is the shape S-4 was reported against: an Xvid or MPEG-4 rip at 640x271.
// The obvious spelling does not produce one - libx264 refuses it for the very
// reason the test exists, and even a lossless encoder inside a container that
// wants even dimensions will quietly nudge it - so the frames go in as raw
// video, which preserves the geometry exactly.
func generateOddDimensionClip(t *testing.T, dir, name string) string {
	t.Helper()

	const width, height, seconds, rate = 640, 271, 2, 10
	raw := filepath.Join(dir, "odd.raw")
	if output, err := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=%dx%d:rate=%d", width, height, rate),
		"-t", fmt.Sprint(seconds),
		"-pix_fmt", "yuv420p", "-f", "rawvideo", raw,
	).CombinedOutput(); err != nil {
		t.Fatalf("rendering the raw frames: %v\n%s", err, output)
	}

	path := filepath.Join(dir, name)
	if output, err := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "rawvideo", "-pix_fmt", "yuv420p",
		"-s", fmt.Sprintf("%dx%d", width, height), "-r", fmt.Sprint(rate),
		"-i", raw,
		"-c:v", "ffv1", "-level", "3",
		path,
	).CombinedOutput(); err != nil {
		t.Fatalf("wrapping the odd frames in %s: %v\n%s", name, err, output)
	}

	// The whole point is the geometry, so refuse to test anything else.
	info, err := NewFFProbe("ffprobe").Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("probing the odd-dimension fixture: %v", err)
	}
	if info.Width != width || info.Height != height {
		t.Fatalf("the fixture is %dx%d, want %dx%d", info.Width, info.Height, width, height)
	}
	return path
}

// TestManager_OddDimensionsEndToEnd covers S-4 through the session manager.
//
// The source already fits the client's box, so negotiation asks for no
// downscale; before the fix that also meant no scale filter, and libx264 failed
// the attempt with "height not divisible by 2". The session must start and
// produce an even-dimension segment.
//
// Note what this test can and cannot prove. It passes whether or not the
// even-dimension clamp is present, because a failed attempt is retried through
// softwareOnlyDecision, and that retry changes the encoder - so on a host whose
// negotiator picked a hardware encoder, the retry hides the first failure. The
// load-bearing regression test for the clamp itself is
// TestVideoFilters_OddSourceGetsAnEvenOutput, which asserts the filter chain
// directly and fails without it. This one is kept because it is what proves the
// viewer gets a playable stream, and because on a host with no hardware encoder
// the first attempt is the only attempt.
func TestManager_OddDimensionsEndToEnd(t *testing.T) {
	requireFFmpeg(t)

	root := t.TempDir()
	source := generateOddDimensionClip(t, root, "odd.mkv")

	info, err := NewFFProbe("ffprobe").Probe(context.Background(), source)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}

	// The browser profile accepts h264 but not ffv1, so this is a transcode. Its
	// height ceiling is above 271, so nothing needs downscaling - which is the
	// case that used to emit no scale at all.
	decision := Negotiate(info, BrowserCapability())
	if decision.VideoAction != ActionTranscode {
		t.Fatalf("video action = %q, want a transcode of ffv1", decision.VideoAction)
	}
	if decision.TargetHeight != 0 {
		t.Fatalf("target height = %d, want 0: this test is about the no-downscale path",
			decision.TargetHeight)
	}

	manager, session, _ := startSession(t, root, source, decision)
	defer manager.Stop(session.ID)

	segment := waitForSegment(t, session.Dir, 90*time.Second)
	if segment == "" {
		t.Fatal("no segment was produced: an odd-dimension re-encode must not fail the session")
	}

	produced, err := NewFFProbe("ffprobe").Probe(context.Background(), filepath.Join(session.Dir, segment))
	if err != nil {
		t.Fatalf("probing the produced segment: %v", err)
	}
	if produced.Width%2 != 0 || produced.Height%2 != 0 {
		t.Errorf("the segment is %dx%d, want both dimensions even",
			produced.Width, produced.Height)
	}
	t.Logf("odd source %dx%d produced a %dx%d segment", info.Width, info.Height, produced.Width, produced.Height)
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
	// QuickSync needs a device and VAAPI needs a render node; with no device
	// directory neither can have been verified, whatever the host's GPU is.
	for _, family := range capability.HardwareAcceleration {
		if family == "qsv" || family == "vaapi" {
			t.Errorf("family %q was claimed without a device directory", family)
		}
	}
	if capability.RenderNode != "" {
		t.Errorf("render node = %q, want none without a device directory", capability.RenderNode)
	}
	// Anything ffmpeg lists but the host cannot use has to say why, or a broken
	// driver is indistinguishable from no GPU.
	for _, rejected := range capability.RejectedEncoders {
		if rejected.Encoder == "" || rejected.Reason == "" {
			t.Errorf("rejection recorded without an encoder and a reason: %+v", rejected)
		}
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

// TestManager_TranscodeProducesPlayableBitDepth is the end-to-end regression
// test for a 10-bit HDR source. Such a source is what exposed the bug: the
// codec name said "h264", so nothing objected, segmentation went to 10-bit
// H.264 "High 10", and the browser fetched every segment and never played one.
func TestManager_TranscodeProducesPlayableBitDepth(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	source := filepath.Join(dir, "ten-bit.mkv")
	cmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=15:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p10le",
		"-c:a", "aac", "-shortest",
		source,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("this ffmpeg cannot encode 10-bit H.264, so the fixture cannot be built: %v\n%s", err, output)
	}

	info, err := NewFFProbe("ffprobe").Probe(context.Background(), source)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.BitDepth != 10 {
		t.Fatalf("probe reports bit depth %d (pixel format %q), want 10",
			info.BitDepth, info.PixelFormat)
	}

	// The probe must be enough on its own to refuse the source.
	decision := Negotiate(info, BrowserCapability())
	if decision.Mode != ModeTranscode {
		t.Fatalf("mode = %q, want transcode for a 10-bit source", decision.Mode)
	}

	manager, session, _ := startSession(t, dir, source, decision)
	defer manager.Stop(session.ID)

	segment := waitForSegment(t, session.Dir, 60*time.Second)
	if segment == "" {
		t.Fatal("no segment was produced")
	}

	produced, err := NewFFProbe("ffprobe").Probe(context.Background(), filepath.Join(session.Dir, segment))
	if err != nil {
		t.Fatalf("probing the produced segment: %v", err)
	}
	if produced.BitDepth != 8 {
		t.Errorf("transcoded segment is %d-bit (%s), want 8-bit: browsers cannot decode 10-bit H.264",
			produced.BitDepth, produced.PixelFormat)
	}
}

// TestManager_TranscodeDownmixesMultiChannelAudio is the regression test for a
// real 5.1 film. Chromium refuses a 5.1 AAC SourceBuffer, and because the audio
// append fails first it tears the whole MediaSource down: the player attaches,
// fetches every segment, and never shows a frame.
func TestManager_TranscodeDownmixesMultiChannelAudio(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	source := filepath.Join(dir, "surround.mkv")
	cmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=15:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-af", "pan=5.1|FL=c0|FR=c0|FC=c0|LFE=c0|BL=c0|BR=c0",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "ac3", "-shortest",
		source,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("this ffmpeg cannot build a 5.1 fixture: %v\n%s", err, output)
	}

	info, err := NewFFProbe("ffprobe").Probe(context.Background(), source)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.AudioChannels != 6 {
		t.Fatalf("fixture has %d audio channels, want 6", info.AudioChannels)
	}

	decision := Negotiate(info, BrowserCapability())
	if decision.AudioAction != ActionTranscode {
		t.Fatalf("audio action = %q, want transcode", decision.AudioAction)
	}
	if decision.TargetAudioChannels != 2 {
		t.Fatalf("target audio channels = %d, want 2", decision.TargetAudioChannels)
	}

	manager, session, _ := startSession(t, dir, source, decision)
	defer manager.Stop(session.ID)

	segment := waitForSegment(t, session.Dir, 60*time.Second)
	if segment == "" {
		t.Fatal("no segment was produced")
	}

	produced, err := NewFFProbe("ffprobe").Probe(context.Background(), filepath.Join(session.Dir, segment))
	if err != nil {
		t.Fatalf("probing the produced segment: %v", err)
	}
	if produced.AudioChannels != 2 {
		t.Errorf("produced audio has %d channels (%s), want 2: browsers refuse a 5.1 AAC SourceBuffer",
			produced.AudioChannels, produced.AudioCodec)
	}
}

// TestDetectServerCapability_RejectsEncodersThatCannotRun is the regression test
// for the reported failure. h264_qsv is compiled into this ffmpeg and the host
// exposed a device directory, so the old detection offered QuickSync - on a
// machine with no Intel GPU, where every transcode then died with
// "Error creating a MFX session: -9".
func TestDetectServerCapability_RejectsEncodersThatCannotRun(t *testing.T) {
	requireFFmpeg(t)

	// A device directory that exists and looks populated, which is the only
	// thing the old check looked at.
	deviceDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(deviceDir, "renderD128"), nil, 0o644); err != nil {
		t.Fatalf("creating a fake render node: %v", err)
	}

	capability := DetectServerCapability(context.Background(), "ffmpeg", "ffprobe", deviceDir)

	// The invariant: never advertise an encoder that cannot encode, using the
	// same device the detection found.
	device := encoderDevice{RenderNode: capability.RenderNode}
	for _, encoder := range capability.VideoEncoders {
		if !encoderWorks(context.Background(), "ffmpeg", encoder, device) {
			t.Errorf("capability advertises %q, which cannot actually encode", encoder)
		}
	}

	// This host cannot drive QuickSync, so it must not be offered even though
	// it is listed by ffmpeg.
	if containsFold(capability.VideoEncoders, "h264_qsv") {
		if !encoderWorks(context.Background(), "ffmpeg", "h264_qsv", device) {
			t.Error("h264_qsv is advertised although it cannot create a session")
		}
	}

	// Whatever else is true, a working software encoder must remain.
	if !containsFold(capability.VideoEncoders, "libx264") {
		t.Errorf("libx264 is missing from the capability report: %v", capability.VideoEncoders)
	}

	// Every claimed family must be backed by an encoder of that family which is
	// actually offered. Writing it per family would need updating whenever one
	// is added, which is exactly how a claim drifts away from reality.
	for _, family := range capability.HardwareAcceleration {
		backed := false
		for _, encoder := range capability.VideoEncoders {
			if got, ok := hardwareFamilyOf(encoder); ok && got.name == family {
				backed = true
				break
			}
		}
		if !backed {
			t.Errorf("hardware acceleration claims %q with no working encoder of that family: %v",
				family, capability.VideoEncoders)
		}
	}
}

// generateHDRClip renders a short PQ / BT.2020 HEVC clip, which is the shape of
// the two real 4K films the dynamic-range path was written against.
//
// The colour is set through -x265-params rather than -color_primaries because
// that is what was measured to work; the generic options did not reach the
// output at all.
func generateHDRClip(t *testing.T, dir, name string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	cmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=15:duration=2",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=2",
		"-c:v", "libx265", "-preset", "ultrafast", "-pix_fmt", "yuv420p10le",
		"-x265-params", "colorprim=bt2020:transfer=smpte2084:colormatrix=bt2020nc:range=limited",
		"-c:a", "aac", "-shortest",
		path,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("this ffmpeg cannot produce a 10-bit PQ fixture: %v\n%s", err, output)
	}
	return path
}

// TestManager_ToneMapsHDRForASDRClient is the end-to-end check of the increment:
// a PQ source delivered to a client that cannot show HDR must come out as
// tagged BT.709 8-bit, because an untagged or incorrectly tagged PQ stream is
// what looks washed out.
func TestManager_ToneMapsHDRForASDRClient(t *testing.T) {
	requireFFmpeg(t)

	ctx := context.Background()
	dir := t.TempDir()
	source := generateHDRClip(t, dir, "hdr10.mkv")

	prober := NewFFProbe("ffprobe")
	info, err := prober.Probe(ctx, source)
	if err != nil {
		t.Fatalf("probing the fixture: %v", err)
	}
	if !info.IsHDR() {
		t.Fatalf("the fixture is not HDR: range=%q transfer=%q", info.DynamicRange, info.ColorTransfer)
	}
	if info.BitDepth != 10 {
		t.Fatalf("the fixture is %d-bit, want 10", info.BitDepth)
	}

	decision := NegotiateForServer(info, BrowserCapability(),
		DetectServerCapability(ctx, "ffmpeg", "ffprobe", ""))
	if decision.Mode != ModeTranscode {
		t.Fatalf("mode = %q, want transcode", decision.Mode)
	}
	if !decision.ToneMap || decision.TargetDynamicRange != RangeSDR {
		t.Fatalf("a browser must be sent tone-mapped SDR, got tonemap=%v range=%q\nreasons: %s",
			decision.ToneMap, decision.TargetDynamicRange, strings.Join(decision.Reasons, "; "))
	}

	manager, session, _ := startSession(t, dir, source, decision)
	defer manager.Stop(session.ID)

	segment := waitForSegment(t, session.Dir, 60*time.Second)
	if segment == "" {
		t.Fatal("no segment was produced")
	}

	produced, err := prober.Probe(ctx, filepath.Join(session.Dir, segment))
	if err != nil {
		t.Fatalf("probing the produced segment: %v", err)
	}
	if produced.BitDepth != 8 {
		t.Errorf("tone-mapped segment is %d-bit (%s), want 8-bit",
			produced.BitDepth, produced.PixelFormat)
	}
	if produced.ColorTransfer != "bt709" || produced.ColorPrimaries != "bt709" {
		t.Errorf("tone-mapped segment is tagged %q/%q, want bt709/bt709: a player that "+
			"reads neither shows the picture washed out",
			produced.ColorTransfer, produced.ColorPrimaries)
	}
	if produced.DynamicRange != RangeSDR || produced.IsHDR() {
		t.Errorf("tone-mapped segment reports range %q, want sdr", produced.DynamicRange)
	}
}

// TestManager_KeepsHDRForAClientThatCanShowIt is the other half end to end: the
// same film, a client that declares HDR, and a host with a verified 10-bit
// encoder. The delivered segment must still be 10-bit and still be tagged as PQ
// BT.2020, which is what makes it an HDR stream rather than a washed-out one.
func TestManager_KeepsHDRForAClientThatCanShowIt(t *testing.T) {
	requireFFmpeg(t)

	ctx := context.Background()
	dir := t.TempDir()
	source := generateHDRClip(t, dir, "hdr10.mkv")

	prober := NewFFProbe("ffprobe")
	info, err := prober.Probe(ctx, source)
	if err != nil {
		t.Fatalf("probing the fixture: %v", err)
	}
	if !info.IsHDR() {
		t.Fatalf("the fixture is not HDR: range=%q transfer=%q", info.DynamicRange, info.ColorTransfer)
	}

	server := DetectServerCapability(ctx, "ffmpeg", "ffprobe", "")
	if _, ok := EncoderForHDR("hevc", server); !ok {
		t.Skipf("no 10-bit HEVC encoder on this host, so HDR cannot be delivered: %+v",
			server.HDRVideoEncoders)
	}

	capability := hdrCapability()
	// Match the fixture's audio and force a re-encode by capping the height
	// below the source, so this exercises the HDR *encoder* path rather than a
	// copy.
	capability.Containers = []string{"hls"}
	capability.MaxHeight = 120

	decision := NegotiateForServer(info, capability, server)
	if decision.Mode != ModeTranscode {
		t.Fatalf("mode = %q, want transcode", decision.Mode)
	}
	if decision.ToneMap || decision.TargetDynamicRange != RangeHDR10 {
		t.Fatalf("an HDR-capable client should keep HDR, got tonemap=%v range=%q\nreasons: %s",
			decision.ToneMap, decision.TargetDynamicRange, strings.Join(decision.Reasons, "; "))
	}

	manager, session, _ := startSession(t, dir, source, decision)
	defer manager.Stop(session.ID)

	segment := waitForSegment(t, session.Dir, 60*time.Second)
	if segment == "" {
		t.Fatal("no segment was produced")
	}

	produced, err := prober.Probe(ctx, filepath.Join(session.Dir, segment))
	if err != nil {
		t.Fatalf("probing the produced segment: %v", err)
	}
	if produced.BitDepth != 10 {
		t.Errorf("HDR segment is %d-bit (%s), want 10-bit",
			produced.BitDepth, produced.PixelFormat)
	}
	if produced.ColorTransfer != "smpte2084" || produced.ColorPrimaries != "bt2020" {
		t.Errorf("HDR segment is tagged %q/%q, want smpte2084/bt2020: an untagged PQ stream "+
			"is shown as if it were SDR",
			produced.ColorTransfer, produced.ColorPrimaries)
	}
	if !produced.IsHDR() {
		t.Errorf("HDR segment reports range %q, want hdr10", produced.DynamicRange)
	}
}

// generateExpensiveClip renders a deliberately high-bitrate clip, so that a
// bitrate ceiling has something real to reduce. testsrc2 is noisy, which is what
// keeps a CRF encode from being tiny on its own.
func generateExpensiveClip(t *testing.T, dir, name string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	cmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=30:duration=6",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=6",
		"-c:v", "libx264", "-preset", "ultrafast", "-b:v", "8M",
		"-c:a", "aac", "-shortest",
		path,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("this ffmpeg cannot produce the fixture: %v\n%s", err, output)
	}
	return path
}

// TestManager_BitrateCeilingHoldsEndToEnd is the check that max_bitrate_kbps is
// not merely echoed back. It encodes the same source twice - once unlimited and
// once under a 500 kbps ceiling - and compares what actually came out, because a
// flag in a command line is not evidence that the rate was bounded.
func TestManager_BitrateCeilingHoldsEndToEnd(t *testing.T) {
	requireFFmpeg(t)

	ctx := context.Background()
	dir := t.TempDir()
	source := generateExpensiveClip(t, dir, "expensive.mkv")

	prober := NewFFProbe("ffprobe")
	info, err := prober.Probe(ctx, source)
	if err != nil {
		t.Fatalf("probing the fixture: %v", err)
	}
	server := DetectServerCapability(ctx, "ffmpeg", "ffprobe", "")

	const ceiling = 500
	capability := BrowserCapability()
	capability.MaxBitrateKbps = ceiling

	capped := NegotiateForServer(info, capability, server)
	if capped.Mode != ModeTranscode {
		t.Fatalf("mode = %q, want transcode for a source over the limit", capped.Mode)
	}
	if capped.TargetBitrateKbps <= 0 || capped.TargetBitrateKbps > ceiling {
		t.Fatalf("target bitrate = %d, want a positive ceiling no higher than %d",
			capped.TargetBitrateKbps, ceiling)
	}

	// The unlimited decision differs only in the cap, so anything the two have
	// in common - codec, scaling, audio - cannot explain the difference.
	unlimited := capped
	unlimited.TargetBitrateKbps = 0
	unlimited.Reasons = append([]string{}, capped.Reasons...)

	measure := func(decision Decision) int {
		t.Helper()
		manager, session, _ := startSession(t, dir, source, decision)
		defer manager.Stop(session.ID)

		segment := waitForSegment(t, session.Dir, 90*time.Second)
		if segment == "" {
			t.Fatal("no segment was produced")
		}
		produced, err := prober.Probe(ctx, filepath.Join(session.Dir, segment))
		if err != nil {
			t.Fatalf("probing the produced segment: %v", err)
		}
		return produced.BitrateKbps
	}

	cappedKbps := measure(capped)
	unlimitedKbps := measure(unlimited)

	// If the fixture is not expensive enough there is nothing to prove, and a
	// threshold picked to pass anyway would be a test that cannot fail.
	if unlimitedKbps < 3*ceiling {
		t.Skipf("the fixture only reached %d kbps unlimited, too close to the %d kbps ceiling to be evidence",
			unlimitedKbps, ceiling)
	}

	if cappedKbps > 2*ceiling {
		t.Errorf("capped segment is %d kbps, want it near the %d kbps ceiling", cappedKbps, ceiling)
	}
	if cappedKbps*2 > unlimitedKbps {
		t.Errorf("capped segment is %d kbps against %d kbps unlimited: the ceiling did not bite",
			cappedKbps, unlimitedKbps)
	}
	t.Logf("bitrate: unlimited %d kbps, capped %d kbps (ceiling %d, video target %d)",
		unlimitedKbps, cappedKbps, ceiling, capped.TargetBitrateKbps)
}

// generateTallClip renders a 720p HEVC/AAC clip: tall enough to have rungs below
// it, and in a codec a browser profile does not accept, so the session has to
// re-encode and a ladder is what it builds.
func generateTallClip(t *testing.T, dir, name string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	cmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=1280x720:rate=15:duration=4",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=4",
		"-c:v", "libx265", "-preset", "ultrafast", "-crf", "30",
		"-c:a", "aac", "-shortest",
		path,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("this ffmpeg cannot produce the fixture: %v\n%s", err, output)
	}
	return path
}

// TestManager_LadderProducesAMasterPlaylistAndItsRungs is the end-to-end check
// for adaptive delivery: one ffmpeg process, several rungs, a master playlist
// that names them, and segments that really are the sizes the decision claimed.
func TestManager_LadderProducesAMasterPlaylistAndItsRungs(t *testing.T) {
	requireFFmpeg(t)

	ctx := context.Background()
	dir := t.TempDir()
	source := generateTallClip(t, dir, "tall.mkv")

	prober := NewFFProbe("ffprobe")
	info, err := prober.Probe(ctx, source)
	if err != nil {
		t.Fatalf("probing the fixture: %v", err)
	}
	if info.Height < 720 {
		t.Fatalf("the fixture is %dx%d, too short to have rungs", info.Width, info.Height)
	}

	// No height limit, so the client is asking to adapt, and a codec the source
	// does not use, so a re-encode is unavoidable.
	capability := ClientCapability{
		Containers:       []string{"hls"},
		VideoCodecs:      []string{"h264"},
		AudioCodecs:      []string{"aac"},
		MaxBitDepth:      8,
		MaxAudioChannels: 6,
		SupportsHLS:      true,
	}

	server := DetectServerCapability(ctx, "ffmpeg", "ffprobe", "")
	decision := NegotiateForServer(info, capability, server)
	if len(decision.Renditions) < 2 {
		t.Fatalf("expected a ladder, got %+v (mode %q, reasons: %s)",
			decision.Renditions, decision.Mode, strings.Join(decision.Reasons, "; "))
	}

	manager, session, _ := startSession(t, dir, source, decision)
	defer manager.Stop(session.ID)

	if got := session.PlaylistFile(); got != MasterPlaylistName {
		t.Fatalf("the client was pointed at %q, want %q", got, MasterPlaylistName)
	}

	segment := waitForSegment(t, session.Dir, 90*time.Second)
	if segment == "" {
		t.Fatal("no segment was produced")
	}

	// The master playlist has to name every rung, and the decision's rungs are
	// what it must agree with.
	master, err := os.ReadFile(session.PlaylistPath())
	if err != nil {
		t.Fatalf("reading the master playlist: %v", err)
	}
	text := string(master)
	if got := strings.Count(text, "#EXT-X-STREAM-INF"); got != len(decision.Renditions) {
		t.Errorf("master playlist lists %d rungs, want %d:\n%s", got, len(decision.Renditions), text)
	}
	for index := range decision.Renditions {
		if !strings.Contains(text, fmt.Sprintf("playlist_%d.m3u8", index)) {
			t.Errorf("master playlist does not reference rung %d:\n%s", index, text)
		}
	}

	// And each rung has to be the size it was advertised as, which is the part a
	// missing stream specifier would quietly get wrong.
	for index, rung := range decision.Renditions {
		segmentPath := filepath.Join(session.Dir, fmt.Sprintf("seg%d_00000.ts", index))
		if _, err := os.Stat(segmentPath); err != nil {
			t.Errorf("rung %d produced no segment: %v", index, err)
			continue
		}
		produced, err := prober.Probe(ctx, segmentPath)
		if err != nil {
			t.Fatalf("probing rung %d: %v", index, err)
		}
		if produced.Height != rung.Height {
			t.Errorf("rung %d is %dx%d, want height %d", index, produced.Width, produced.Height, rung.Height)
		}
	}
}

// TestManager_LadderTopsAtThePreferredHeight is the artefact-level check for a
// quality choice expressed as a preference: the client asked for a ladder it
// will not exceed, and every rung's produced segment really is the height that
// was advertised. Asserting the decision's rungs alone would pass for a session
// that never built them or built them all at one size.
func TestManager_LadderTopsAtThePreferredHeight(t *testing.T) {
	requireFFmpeg(t)

	ctx := context.Background()
	dir := t.TempDir()
	source := generateTallClip(t, dir, "tall.mkv")

	prober := NewFFProbe("ffprobe")
	info, err := prober.Probe(ctx, source)
	if err != nil {
		t.Fatalf("probing the fixture: %v", err)
	}
	if info.Height < 720 {
		t.Fatalf("the fixture is %dx%d, too short for a preferred top below its height", info.Width, info.Height)
	}

	// A preferred height and no max_height: that is a request for a ladder,
	// topped below the source, in a codec the source does not use.
	capability := ClientCapability{
		Containers:       []string{"hls"},
		VideoCodecs:      []string{"h264"},
		AudioCodecs:      []string{"aac"},
		MaxBitDepth:      8,
		MaxAudioChannels: 6,
		SupportsHLS:      true,
		PreferredHeight:  480,
	}

	server := DetectServerCapability(ctx, "ffmpeg", "ffprobe", "")
	decision := NegotiateForServer(info, capability, server)
	if len(decision.Renditions) < 2 {
		t.Fatalf("expected a ladder, got %+v (mode %q, reasons: %s)",
			decision.Renditions, decision.Mode, strings.Join(decision.Reasons, "; "))
	}
	if decision.Renditions[0].Height != 480 {
		t.Fatalf("the top rung is %d, want the preferred 480", decision.Renditions[0].Height)
	}
	if decision.TargetHeight != 480 {
		t.Errorf("target height = %d, want the top rung 480", decision.TargetHeight)
	}

	manager, session, _ := startSession(t, dir, source, decision)
	defer manager.Stop(session.ID)

	if got := session.PlaylistFile(); got != MasterPlaylistName {
		t.Fatalf("the client was pointed at %q, want %q", got, MasterPlaylistName)
	}
	if segment := waitForSegment(t, session.Dir, 90*time.Second); segment == "" {
		t.Fatal("no segment was produced")
	}

	// Each rung has to be the size it was advertised as, measured from the
	// segment ffmpeg actually produced rather than from the argument list.
	for index, rung := range decision.Renditions {
		segmentPath := filepath.Join(session.Dir, fmt.Sprintf("seg%d_00000.ts", index))
		if _, err := os.Stat(segmentPath); err != nil {
			t.Errorf("rung %d produced no segment: %v", index, err)
			continue
		}
		produced, err := prober.Probe(ctx, segmentPath)
		if err != nil {
			t.Fatalf("probing rung %d: %v", index, err)
		}
		if produced.Height != rung.Height {
			t.Errorf("rung %d is %dx%d, want height %d", index, produced.Width, produced.Height, rung.Height)
		}
	}
}

// generateTwoAudioClip renders an MP4 whose two audio tracks differ in a way that
// survives being copied: track 1 is stereo, track 2 is mono and is marked the
// file's default. Both are AAC, so a browser takes either one as it is and what
// arrives says which was chosen.
func generateTwoAudioClip(t *testing.T, dir, name string) string {
	t.Helper()

	path := filepath.Join(dir, name)
	cmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=size=640x360:rate=15:duration=3",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3",
		"-f", "lavfi", "-i", "sine=frequency=880:duration=3",
		"-map", "0:v", "-map", "1:a", "-map", "2:a",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-b:a", "96k",
		"-ac:a:0", "2", "-ac:a:1", "1",
		"-disposition:a:0", "0", "-disposition:a:1", "default",
		path,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("this ffmpeg cannot produce a two-track fixture: %v\n%s", err, output)
	}
	return path
}

// TestManager_DeliversTheChosenAudioTrack is the end-to-end check that the audio
// choice reaches the delivered bytes. The fixture's tracks differ in channel
// count, so probing the produced segment says which one was mapped - a test that
// only asserted the decision would pass even if the mapping were wrong.
func TestManager_DeliversTheChosenAudioTrack(t *testing.T) {
	requireFFmpeg(t)

	ctx := context.Background()
	dir := t.TempDir()
	source := generateTwoAudioClip(t, dir, "two-audio.mp4")

	prober := NewFFProbe("ffprobe")
	info, err := prober.Probe(ctx, source)
	if err != nil {
		t.Fatalf("probing the fixture: %v", err)
	}
	if len(info.AudioTracks) != 2 {
		t.Fatalf("the fixture has %d audio tracks, want two: %+v", len(info.AudioTracks), info.AudioTracks)
	}
	stereo, mono := info.AudioTracks[0], info.AudioTracks[1]
	if stereo.Channels != 2 || mono.Channels != 1 {
		t.Fatalf("fixture track channels = %d, %d; want 2 then 1", stereo.Channels, mono.Channels)
	}
	if !mono.Default {
		t.Fatalf("the second track should be the fixture's default: %+v", info.AudioTracks)
	}

	server := DetectServerCapability(ctx, "ffmpeg", "ffprobe", "")
	base := ClientCapability{
		Containers:       []string{"mp4", "hls"},
		VideoCodecs:      []string{"h264"},
		AudioCodecs:      []string{"aac"},
		MaxBitDepth:      8,
		MaxAudioChannels: 2,
		SupportsHLS:      true,
	}

	// Saying nothing delivers the track the file marks default, and an otherwise
	// playable file is still played directly.
	automatic := NegotiateForServer(info, base, server)
	if automatic.Mode != ModeDirectPlay {
		t.Fatalf("mode = %q, want direct play: %s", automatic.Mode, strings.Join(automatic.Reasons, "; "))
	}
	if automatic.TargetAudioStreamIndex != mono.Index {
		t.Errorf("automatic audio stream = %d, want the default track's %d",
			automatic.TargetAudioStreamIndex, mono.Index)
	}

	// Choosing a track repackages the file - direct play cannot isolate one -
	// and the segment has to carry the chosen track's channel count.
	for _, want := range []AudioTrack{stereo, mono} {
		capability := base
		capability.AudioTrackIndex = want.Index

		decision := NegotiateForServer(info, capability, server)
		if decision.TargetAudioStreamIndex != want.Index {
			t.Fatalf("target audio stream = %d, want %d", decision.TargetAudioStreamIndex, want.Index)
		}
		if decision.Mode != ModeRemux {
			t.Fatalf("mode = %q, want remux for a chosen track: %s",
				decision.Mode, strings.Join(decision.Reasons, "; "))
		}
		if decision.VideoAction != ActionCopy {
			t.Errorf("video action = %q, want copy: choosing audio must not re-encode the picture",
				decision.VideoAction)
		}

		manager, session, _ := startSession(t, dir, source, decision)
		segment := waitForSegment(t, session.Dir, 60*time.Second)
		if segment == "" {
			manager.Stop(session.ID)
			t.Fatalf("track %d produced no segment", want.Index)
		}

		// Probe before stopping: Stop removes the session directory, and the
		// segment with it.
		produced, err := prober.Probe(ctx, filepath.Join(session.Dir, segment))
		manager.Stop(session.ID)
		if err != nil {
			t.Fatalf("probing the segment for track %d: %v", want.Index, err)
		}
		if produced.AudioChannels != want.Channels {
			t.Errorf("chose track %d (%dch) but the delivered segment has %dch: the wrong stream was mapped",
				want.Index, want.Channels, produced.AudioChannels)
		}
		if produced.VideoCodec != "h264" {
			t.Errorf("video codec = %q, want h264 copied from the source", produced.VideoCodec)
		}
	}
}
