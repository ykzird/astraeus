//go:build integration

// This test builds a real image-subtitle fixture and runs the real ffmpeg, then
// looks at the produced segment's pixels. Asserting the filter graph in the
// argument list would pass for a command that composes nothing.
//
//	go test -tags=integration -run Burn ./internal/streaming/
package streaming

import (
	"context"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/ykzird/astraeus/internal/testfixtures/pgs"
)

// writePGSFixture writes a .sup holding one white rectangle shown from 0.5s to
// 5.5s, and returns its path.
func writePGSFixture(t *testing.T, dir string) string {
	t.Helper()

	subtitle := pgs.Subtitle(640, 360, []pgs.Cue{
		{StartMS: 500, EndMS: 5500, X: 100, Y: 250, Width: 400, Height: 60},
	})

	path := filepath.Join(dir, "fixture.sup")
	if err := os.WriteFile(path, subtitle, 0o644); err != nil {
		t.Fatalf("writing the PGS fixture: %v", err)
	}
	return path
}

func runFFmpeg(t *testing.T, args ...string) {
	t.Helper()

	cmd := exec.Command("ffmpeg", append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, output)
	}
}

// TestBurnIn_CompositesTheSubtitleIntoTheSegment is the artefact-level check for
// image subtitles: the same source is delivered twice, once with the subtitle
// burned in and once without, and the produced segment's pixels are compared. A
// command-line assertion could not tell a working composite from a filter that
// was accepted and ignored.
func TestBurnIn_CompositesTheSubtitleIntoTheSegment(t *testing.T) {
	requireFFmpeg(t)

	dir := t.TempDir()
	sup := writePGSFixture(t, dir)

	// A flat, dark picture makes the white subtitle unambiguous: any bright
	// pixel in the produced frame is the subtitle and nothing else.
	base := filepath.Join(dir, "base.mp4")
	runFFmpeg(t, "-f", "lavfi", "-i", "color=c=0x202020:s=640x360:r=15",
		"-t", "6", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", base)

	source := filepath.Join(dir, "source.mkv")
	runFFmpeg(t, "-i", base, "-i", sup,
		"-map", "0:v", "-map", "1:s", "-c:v", "copy", "-c:s", "copy", source)

	// The real prober must classify the hand-written track as an image subtitle,
	// which also proves the fixture itself is well formed.
	prober := &FFProbe{binary: "ffprobe", timeout: 30 * time.Second}
	info, err := prober.Probe(context.Background(), source)
	if err != nil {
		t.Fatalf("probing the fixture: %v", err)
	}
	var track SubtitleTrack
	for _, candidate := range info.Subtitles {
		if candidate.Codec == "hdmv_pgs_subtitle" {
			track = candidate
		}
	}
	if track.Codec == "" {
		t.Fatalf("the fixture has no PGS subtitle track: %+v", info.Subtitles)
	}
	if track.Text {
		t.Fatalf("the PGS track was classified as text: %+v", track)
	}

	server := ServerCapability{
		FFmpegAvailable: true, FFprobeAvailable: true, HLS: true,
		VideoEncoders: []string{"libx264"}, AudioEncoders: []string{"aac"},
	}
	capability := ClientCapability{
		Containers: []string{"matroska", "hls"}, VideoCodecs: []string{"h264"},
		AudioCodecs: []string{"aac"}, SupportsHLS: true, Subtitles: true,
		MaxWidth: 1920, MaxHeight: 1080, MaxBitDepth: 8, MaxAudioChannels: 2,
		BurnSubtitleIndex: track.Index,
	}

	decision := NegotiateForServer(info, capability, server)
	if decision.BurnedSubtitleIndex != track.Index {
		t.Fatalf("negotiation did not burn track %d: %+v", track.Index, decision)
	}

	burnedDir := filepath.Join(dir, "burned")
	plainDir := filepath.Join(dir, "plain")
	for _, d := range []string{burnedDir, plainDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("creating %s: %v", d, err)
		}
	}
	cfg := ManagerConfig{SegmentSeconds: 2, Server: server}

	burnedArgs, err := BuildFFmpegArgs(burnedDir, source, decision, cfg)
	if err != nil {
		t.Fatalf("building the burn command: %v", err)
	}
	runFFmpeg(t, burnedArgs...)

	// The control is the same decision with the burn removed, so the only
	// difference between the two runs is the composite.
	plainDecision := decision
	plainDecision.BurnedSubtitleIndex = 0
	plainArgs, err := BuildFFmpegArgs(plainDir, source, plainDecision, cfg)
	if err != nil {
		t.Fatalf("building the control command: %v", err)
	}
	runFFmpeg(t, plainArgs...)

	// Segment 1 covers 2-4s, which is inside the fixture's cue window.
	burnedBright := brightPixels(t, burnedDir, 1)
	plainBright := brightPixels(t, plainDir, 1)

	if burnedBright < 10_000 {
		t.Errorf("the burned segment has only %d bright pixels; the 400x60 subtitle is missing", burnedBright)
	}
	if plainBright != 0 {
		t.Errorf("the control segment has %d bright pixels; the dark source should have none", plainBright)
	}
}

// brightPixels decodes the first frame of one media segment and counts the
// near-white pixels. Go's standard library decodes the PNG, so the assertion
// needs no image dependency.
func brightPixels(t *testing.T, dir string, segment int) int {
	t.Helper()

	frame := filepath.Join(t.TempDir(), "frame.png")
	runFFmpeg(t, "-i", filepath.Join(dir, "seg"+padSegment(segment)+".ts"),
		"-frames:v", "1", "-update", "1", frame)

	file, err := os.Open(frame)
	if err != nil {
		t.Fatalf("opening the decoded frame: %v", err)
	}
	defer file.Close()

	img, err := png.Decode(file)
	if err != nil {
		t.Fatalf("decoding the frame: %v", err)
	}

	// RGBA() returns 16-bit channels, so the 8-bit threshold is scaled up.
	const threshold = 150 * 257
	bounds := img.Bounds()
	bright := 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			luma := (299*r + 587*g + 114*b) / 1000
			if luma > threshold {
				bright++
			}
		}
	}
	return bright
}

func padSegment(n int) string {
	digits := []byte("00000")
	for i := len(digits) - 1; i >= 0 && n > 0; i-- {
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits)
}
