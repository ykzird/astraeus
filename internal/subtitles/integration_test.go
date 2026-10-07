//go:build integration

// These tests execute the real ffmpeg and ffprobe binaries. Run them with:
//
//	go test -tags=integration ./internal/subtitles/
package subtitles

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jok/astraeus-media/internal/streaming"
)

const sampleSRT = `1
00:00:00,500 --> 00:00:02,000
Hello from Astraeus

2
00:00:02,100 --> 00:00:02,900
Second cue
`

// generateSubtitledClip renders a short clip carrying a real subrip track and
// returns its path. The subtitle stream lands at ffmpeg index 2.
func generateSubtitledClip(t *testing.T, dir string) string {
	t.Helper()

	for _, binary := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("%s is not installed", binary)
		}
	}

	srtPath := filepath.Join(dir, "captions.srt")
	if err := os.WriteFile(srtPath, []byte(sampleSRT), 0o644); err != nil {
		t.Fatalf("writing the subtitle source: %v", err)
	}

	clipPath := filepath.Join(dir, "subtitled.mkv")
	cmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=15:duration=3",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3",
		"-i", srtPath,
		"-map", "0:v", "-map", "1:a", "-map", "2:s",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-c:s", "srt",
		"-shortest",
		clipPath,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generating a subtitled clip: %v\n%s", err, output)
	}
	return clipPath
}

func TestProbe_ReportsSubtitleTracks(t *testing.T) {
	dir := t.TempDir()
	clip := generateSubtitledClip(t, dir)

	info, err := streaming.NewFFProbe("ffprobe").Probe(context.Background(), clip)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}

	if len(info.Subtitles) != 1 {
		t.Fatalf("got %d subtitle tracks, want 1: %+v", len(info.Subtitles), info.Subtitles)
	}

	track := info.Subtitles[0]
	if track.Index != 2 {
		t.Errorf("track index = %d, want 2", track.Index)
	}
	if track.Codec != "subrip" {
		t.Errorf("track codec = %q, want subrip", track.Codec)
	}
	if !track.Text {
		t.Error("a subrip track must be reported as text-based")
	}
}

func TestService_ExtractsRealSubtitlesAsWebVTT(t *testing.T) {
	dir := t.TempDir()
	clip := generateSubtitledClip(t, dir)

	service, err := New(Config{
		FFmpegBin: "ffmpeg",
		CacheDir:  filepath.Join(dir, "cache"),
		Timeout:   60 * time.Second,
		Logger:    testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	path, err := service.Convert(context.Background(), clip, 2)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the converted track: %v", err)
	}
	text := string(body)

	if !strings.HasPrefix(text, "WEBVTT") {
		t.Errorf("converted track does not start with the WebVTT header:\n%s", text)
	}
	if !strings.Contains(text, "Hello from Astraeus") {
		t.Errorf("converted track lost the first cue:\n%s", text)
	}
	if !strings.Contains(text, "Second cue") {
		t.Errorf("converted track lost the second cue:\n%s", text)
	}

	// A second conversion must be served from the cache rather than re-run.
	again, err := service.Convert(context.Background(), clip, 2)
	if err != nil {
		t.Fatalf("second Convert: %v", err)
	}
	if again != path {
		t.Errorf("second conversion produced a different path: %q then %q", path, again)
	}

	entries, err := os.ReadDir(service.CacheDir())
	if err != nil {
		t.Fatalf("reading the cache directory: %v", err)
	}
	vttCount := 0
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".vtt" {
			vttCount++
		}
	}
	if vttCount != 1 {
		t.Errorf("cached %d WebVTT files, want 1", vttCount)
	}
}

func TestService_ReportsTrackWithoutCues(t *testing.T) {
	dir := t.TempDir()
	clip := generateSubtitledClip(t, dir)

	service, err := New(Config{
		FFmpegBin: "ffmpeg",
		CacheDir:  filepath.Join(dir, "cache"),
		Timeout:   60 * time.Second,
		Logger:    testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// There is no subtitle stream at index 0 of this clip.
	if _, err := service.Convert(context.Background(), clip, 99); err == nil {
		t.Fatal("expected an error for a subtitle track that does not exist")
	}
}
