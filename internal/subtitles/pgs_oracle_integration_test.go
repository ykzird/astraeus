//go:build integration

// The oracle test for L-1: the project's own PGS fixture is decoded by ffmpeg,
// not by this package, and the pixels ffmpeg produces are compared with what the
// fixture drew.
//
// This is the test the review asked for and the one the suite did not have. The
// decoder and the fixture encoder shared one misreading of the run-length
// format, so every test built on the fixture agreed with the bug; the only
// oracle that can catch that is an independent implementation. ffmpeg's
// pgssubdec.c is that implementation.
//
//	go test -tags=integration -run PGSOracle ./internal/subtitles/
package subtitles

import (
	"context"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/ykzird/astraeus/internal/testfixtures/pgs"
)

// renderPGSFrame muxes a .sup into a container and renders one frame of it with
// ffmpeg's own PGS decoder, returning the decoded image.
func renderPGSFrame(t *testing.T, dir string, subtitle []byte, atSeconds string) string {
	t.Helper()

	sup := filepath.Join(dir, "oracle.sup")
	if err := os.WriteFile(sup, subtitle, 0o644); err != nil {
		t.Fatalf("writing the PGS fixture: %v", err)
	}

	// The subtitle is composited over a black picture, so any bright pixel in
	// the output came from the subtitle and nothing else.
	frame := filepath.Join(dir, "frame.png")
	cmd := exec.Command("ffmpeg",
		"-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "color=c=black:s=640x360:r=25",
		"-i", sup,
		"-filter_complex", "[0:v][1:s]overlay=shortest=0",
		"-ss", atSeconds, "-frames:v", "1", "-update", "1",
		frame,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg could not decode the fixture: %v\n%s", err, output)
	}
	return frame
}

// countBrightPixels reports how many near-white pixels a decoded frame holds.
func countBrightPixels(t *testing.T, path string) int {
	t.Helper()

	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening the decoded frame: %v", err)
	}
	defer file.Close()

	img, err := png.Decode(file)
	if err != nil {
		t.Fatalf("decoding the frame: %v", err)
	}

	const threshold = 150 * 257 // RGBA() is 16-bit, so scale the 8-bit threshold
	bounds := img.Bounds()
	bright := 0
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			if (299*r+587*g+114*b)/1000 > threshold {
				bright++
			}
		}
	}
	return bright
}

// TestPGSOracle_FfmpegDecodesTheFixture runs the fixture past an independent
// implementation, which the suite had no equivalent of before.
//
// Be precise about what this can and cannot prove, because the distinction cost
// real time to establish. It catches a fixture that is not a valid PGS stream, a
// cue outside its window, an opaque index that is not actually opaque, and a
// regression that stops the streams being carried at all. It does NOT catch the
// L-1 misreading on its own: the old encoder's "count byte then colour" form is
// a semantically valid way to write the same run - three pixels of index 1
// spelled 03 01 or 00 83 01 - so both encodings render the same rectangle. The
// decoder's misreading only became visible on streams where a non-zero byte sits
// where an escape is expected, which is what real discs contain and what a
// self-consistent fixture never did.
//
// The instrument that does discriminate is
// TestDecodePGSRLE_MatchesTheSpecification, which asserts the byte-level reading
// against the specification. This test is kept because an independent decoder is
// worth having on the fixture regardless, and because the review asked for one.
func TestPGSOracle_FfmpegDecodesTheFixture(t *testing.T) {
	requireOCRFFmpeg(t)

	dir := t.TempDir()
	// The width is three, and that is the whole point. A wide row is encoded as
	// an escape run, and the escape forms were never wrong; the single-pixel
	// form is what the decoder misread and what the old fixture emitted for a
	// one-pixel run. A three-pixel-wide bar therefore exercises the broken form
	// on every row, and it is what anti-aliased glyph edges are made of on a
	// real disc.
	const (
		width, height = 3, 50
		x, y          = 100, 150
	)
	subtitle := pgs.Subtitle(640, 360, []pgs.Cue{
		{StartMS: 500, EndMS: 5500, X: x, Y: y, Width: width, Height: height},
	})

	// A second inside the cue's window.
	frame := renderPGSFrame(t, dir, subtitle, "1.0")
	bright := countBrightPixels(t, frame)

	area := width * height
	if bright < area/2 {
		t.Errorf("ffmpeg decoded %d bright pixels from a %dx%d bar; the fixture's "+
			"single-pixel runs are not in the form the format defines (want about %d). "+
			"A misread index becomes transparent, which is why real discs came out garbled.",
			bright, width, height, area)
	}
	if bright > area*2 {
		t.Errorf("ffmpeg decoded %d bright pixels, far more than the %d drawn: the "+
			"fixture is encoding a larger area than the cue describes", bright, area)
	}
	t.Logf("ffmpeg rendered %d bright pixels for a %d-pixel bar", bright, area)
}

// requireOCRFFmpeg skips when ffmpeg is not installed, like the OCR tests do.
func requireOCRFFmpeg(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	_ = context.Background()
}
