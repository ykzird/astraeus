//go:build integration

// The artefact-level test for OCR: a real Matroska file carrying a real PGS
// track is put through the real pipeline, and the WebVTT that comes out must
// contain the words the fixture drew. Asserting the tesseract command line
// would pass for a pipeline that recognises nothing.
//
// tesseract must be installed, otherwise the test skips: the server's behaviour
// without it is a separate, always-run unit test.
//
//	go test -tags=integration -run OCR ./internal/subtitles/
package subtitles

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ykzird/astraeus/internal/testfixtures/pgs"
)

// muxPGSFixture writes a .sup and muxes it into a Matroska file, the way a
// Blu-ray remux carries one, returning the file and the subtitle stream's
// global index.
func muxPGSFixture(t *testing.T, dir string, subtitle []byte) (string, int) {
	t.Helper()

	sup := filepath.Join(dir, "caption.sup")
	if err := os.WriteFile(sup, subtitle, 0o644); err != nil {
		t.Fatalf("writing the PGS fixture: %v", err)
	}

	base := filepath.Join(dir, "base.mp4")
	runOCRFFmpeg(t, "-f", "lavfi", "-i", "color=c=0x202020:s=640x360:r=15",
		"-t", "6", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", base)

	source := filepath.Join(dir, "source.mkv")
	runOCRFFmpeg(t, "-i", base, "-i", sup,
		"-map", "0:v", "-map", "1:s", "-c:v", "copy", "-c:s", "copy", source)

	probe := exec.Command("ffprobe", "-v", "error", "-select_streams", "s",
		"-show_entries", "stream=index,codec_name", "-of", "csv=p=0", source)
	output, err := probe.Output()
	if err != nil {
		t.Fatalf("probing the fixture: %v", err)
	}
	fields := strings.Split(strings.TrimSpace(string(output)), ",")
	if len(fields) < 2 || fields[1] != "hdmv_pgs_subtitle" {
		t.Fatalf("the fixture has no PGS track: %q", string(output))
	}
	index, err := strconv.Atoi(fields[0])
	if err != nil {
		t.Fatalf("parsing the subtitle stream index from %q: %v", fields[0], err)
	}
	return source, index
}

func runOCRFFmpeg(t *testing.T, args ...string) {
	t.Helper()

	cmd := exec.Command("ffmpeg", append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, output)
	}
}

func newOCRService(t *testing.T, dir string) *Service {
	t.Helper()

	service, err := New(Config{
		FFmpegBin:    "ffmpeg",
		TesseractBin: "tesseract",
		CacheDir:     filepath.Join(dir, "cache"),
		Timeout:      2 * time.Minute,
		Logger:       testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return service
}

// TestService_OCRsAnImageSubtitleIntoWebVTT is the end-to-end check: bitmaps
// in, words out.
func TestService_OCRsAnImageSubtitleIntoWebVTT(t *testing.T) {
	requireOCRTools(t)

	dir := t.TempDir()
	const caption = "ASTRAEUS MEDIA"
	source, index := muxPGSFixture(t, dir, pgs.Text(640, 360, []pgs.TextCue{{
		StartMS: 500, EndMS: 5500, X: 70, Y: 150, Text: caption,
	}}))

	service := newOCRService(t, dir)
	if !service.OCRReady() {
		t.Fatal("OCRReady = false although tesseract is installed")
	}

	path, err := service.ConvertImage(context.Background(), source, index)
	if err != nil {
		t.Fatalf("ConvertImage: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the recognised track: %v", err)
	}
	text := string(body)

	if !strings.HasPrefix(text, "WEBVTT") {
		t.Errorf("the recognised track is not WebVTT:\n%s", text)
	}
	if !strings.Contains(text, caption) {
		t.Errorf("the recognised subtitles do not contain %q; the OCR output was:\n%s", caption, text)
	}
	if !strings.Contains(text, "-->") {
		t.Errorf("the recognised track has no cue timing:\n%s", text)
	}

	// The cache must be reused rather than recognised twice.
	again, err := service.ConvertImage(context.Background(), source, index)
	if err != nil {
		t.Fatalf("second ConvertImage: %v", err)
	}
	if again != path {
		t.Errorf("second conversion produced a different path: %q then %q", path, again)
	}
}

// requireOCRTools skips when the runtime dependencies for the OCR path are not
// installed, rather than failing a workstation that does not have them.
func requireOCRTools(t *testing.T) {
	t.Helper()

	for _, binary := range []string{"ffmpeg", "ffprobe", "tesseract"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skipf("%s is not installed", binary)
		}
	}
}
