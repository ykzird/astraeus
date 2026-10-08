package subtitles

import (
	"context"
	"errors"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ykzird/astraeus/internal/testfixtures/pgs"
)

// TestConvertImage_WithoutTesseractKeepsTheRefusal pins the runtime-dependency
// decision: the OCR engine is optional, and an install without it must answer
// an image track exactly as the server did before OCR existed rather than fail
// partway through. The refusal is what the API turns into a 415.
func TestConvertImage_WithoutTesseractKeepsTheRefusal(t *testing.T) {
	t.Parallel()

	service, err := New(Config{
		FFmpegBin:    "ffmpeg",
		TesseractBin: "definitely-not-tesseract",
		CacheDir:     t.TempDir(),
		Logger:       testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if service.OCRReady() {
		t.Fatal("OCRReady = true without an OCR engine")
	}

	media := filepath.Join(t.TempDir(), "movie.mkv")
	if err := os.WriteFile(media, []byte("not really a video"), 0o644); err != nil {
		t.Fatalf("writing media file: %v", err)
	}

	_, err = service.ConvertImage(context.Background(), media, 3)
	if !errors.Is(err, ErrUnsupportedFormat) {
		t.Fatalf("error = %v, want ErrUnsupportedFormat", err)
	}
}

func TestOCRReady_FollowsTheConfiguredBinary(t *testing.T) {
	t.Parallel()

	// A binary that exists on every host this runs on.
	present, err := New(Config{CacheDir: t.TempDir(), TesseractBin: "/bin/sh", Logger: testLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !present.OCRReady() {
		t.Error("OCRReady = false for a binary that exists")
	}

	absent, err := New(Config{CacheDir: t.TempDir(), TesseractBin: "definitely-not-tesseract", Logger: testLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if absent.OCRReady() {
		t.Error("OCRReady = true for a binary that does not exist")
	}
}

// TestRenderForOCR_PutsDarkTextOnAWhitePage is the unit-level check of the
// image handed to the recogniser: light subtitle pixels become dark ink, the
// dark parts of a bitmap fall away into the page, and the glyphs are not flush
// against the edge.
func TestRenderForOCR_PutsDarkTextOnAWhitePage(t *testing.T) {
	t.Parallel()

	const (
		clear = iota
		light
		dark
	)
	src := image.NewRGBA(image.Rect(0, 0, 3, 1))
	src.SetRGBA(0, 0, color.RGBA{A: 0})
	src.SetRGBA(1, 0, color.RGBA{R: 235, G: 235, B: 235, A: 255})
	src.SetRGBA(2, 0, color.RGBA{A: 255})

	page := renderForOCR(src)

	if page.Bounds().Dx() <= src.Bounds().Dx() || page.Bounds().Dy() <= src.Bounds().Dy() {
		t.Fatalf("the OCR page has no margin: %v for a %v bitmap", page.Bounds(), src.Bounds())
	}
	if got := page.GrayAt(0, 0).Y; got != 0xff {
		t.Errorf("the page corner is %d, want white", got)
	}

	marginX := (page.Bounds().Dx() - src.Bounds().Dx()) / 2
	marginY := (page.Bounds().Dy() - src.Bounds().Dy()) / 2
	if got := page.GrayAt(marginX+clear, marginY).Y; got != 0xff {
		t.Errorf("a transparent pixel is %d, want white", got)
	}
	if got := page.GrayAt(marginX+light, marginY).Y; got > 0x20 {
		t.Errorf("a light subtitle pixel is %d, want near-black ink", got)
	}
	if got := page.GrayAt(marginX+dark, marginY).Y; got != 0xff {
		t.Errorf("a dark pixel is %d, want it to fall into the page", got)
	}
}

func TestBuildWebVTT_FormatsAndEscapes(t *testing.T) {
	t.Parallel()

	body := buildWebVTT([]ocrCue{
		{start: 1500 * time.Millisecond, end: 2 * time.Second, text: "Hello & <world>"},
		{start: time.Hour + 2*time.Minute + 3*time.Second, end: time.Hour + 2*time.Minute + 4*time.Second, text: "Late"},
	})

	want := "WEBVTT\n\n" +
		"00:00:01.500 --> 00:00:02.000\n" +
		"Hello &amp; &lt;world&gt;\n\n" +
		"01:02:03.000 --> 01:02:04.000\n" +
		"Late\n\n"
	if body != want {
		t.Errorf("WebVTT body =\n%q\nwant\n%q", body, want)
	}
}

func TestCleanOCRText_DropsBlankLinesAndCollapsesSpaces(t *testing.T) {
	t.Parallel()

	got := cleanOCRText("  Hel   lo  \n\n  World \n\n")
	if got != "Hel lo\nWorld" {
		t.Errorf("cleanOCRText = %q, want %q", got, "Hel lo\nWorld")
	}
}

func TestMergeOCRCue_FoldsOnlyAdjacentIdenticalText(t *testing.T) {
	t.Parallel()

	cues := []ocrCue{{start: 0, end: time.Second, text: "Same"}}
	cues = mergeOCRCue(cues, ocrCue{start: time.Second, end: 2 * time.Second, text: "Same"})
	if len(cues) != 1 {
		t.Fatalf("identical adjacent cues were not merged: %+v", cues)
	}
	if cues[0].end != 2*time.Second {
		t.Errorf("merged cue end = %v, want 2s", cues[0].end)
	}

	cues = mergeOCRCue(cues, ocrCue{start: 10 * time.Second, end: 11 * time.Second, text: "Same"})
	if len(cues) != 2 {
		t.Errorf("a cue after a long gap was merged: %+v", cues)
	}

	cues = mergeOCRCue(cues, ocrCue{start: 11 * time.Second, end: 12 * time.Second, text: "Different"})
	if len(cues) != 3 {
		t.Errorf("cues with different text were merged: %+v", cues)
	}
}

func TestVTTTimestamp_NeverPrintsNegativeTime(t *testing.T) {
	t.Parallel()

	if got := vttTimestamp(-time.Second); got != "00:00:00.000" {
		t.Errorf("vttTimestamp(-1s) = %q, want 00:00:00.000", got)
	}
	if got := vttTimestamp(3661*time.Second + 234*time.Millisecond); got != "01:01:01.234" {
		t.Errorf("vttTimestamp = %q, want 01:01:01.234", got)
	}
}

// TestConvertImage_ReportsNoCuesWhenNothingIsRecognised pins the answer for an
// image track whose OCR finds no words: the API maps ErrNoCues to a 404 rather
// than serving an empty overlay. Both tools are stubs, so this runs everywhere:
// the "ffmpeg" hands back a real PGS fixture and the "recogniser" prints
// nothing, which is exactly an image with no text in it.
func TestConvertImage_ReportsNoCuesWhenNothingIsRecognised(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	stream := pgs.Text(640, 360, []pgs.TextCue{{StartMS: 0, EndMS: 1000, Text: "HI"}})
	sup := filepath.Join(dir, "fixture.sup")
	if err := os.WriteFile(sup, stream, 0o644); err != nil {
		t.Fatalf("writing the PGS fixture: %v", err)
	}

	fakeFFmpeg := filepath.Join(dir, "ffmpeg")
	script := "#!/bin/sh\nout=\"\"\nfor a in \"$@\"; do out=\"$a\"; done\ncp " + sup + " \"$out\"\n"
	if err := os.WriteFile(fakeFFmpeg, []byte(script), 0o755); err != nil {
		t.Fatalf("writing the stub ffmpeg: %v", err)
	}
	fakeTesseract := filepath.Join(dir, "tesseract")
	if err := os.WriteFile(fakeTesseract, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writing the stub recogniser: %v", err)
	}

	service, err := New(Config{
		FFmpegBin:    fakeFFmpeg,
		TesseractBin: fakeTesseract,
		CacheDir:     filepath.Join(dir, "cache"),
		Timeout:      30 * time.Second,
		Logger:       testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !service.OCRReady() {
		t.Fatal("OCRReady = false for an existing recogniser")
	}

	media := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(media, []byte("pretend media"), 0o644); err != nil {
		t.Fatalf("writing media file: %v", err)
	}

	_, err = service.ConvertImage(context.Background(), media, 1)
	if !errors.Is(err, ErrNoCues) {
		t.Fatalf("error = %v, want ErrNoCues", err)
	}
}

// TestOCRSupportsCodec pins the honesty of the format claim: the reader is a
// PGS decoder, so a VobSub track must not be advertised as readable and then
// fail inside the extractor.
func TestOCRSupportsCodec(t *testing.T) {
	t.Parallel()

	if !OCRSupportsCodec("hdmv_pgs_subtitle") {
		t.Error("PGS must be OCR-readable")
	}
	if !OCRSupportsCodec("HDMV_PGS_SUBTITLE") {
		t.Error("codec matching must not be case-sensitive")
	}
	for _, codec := range []string{"dvd_subtitle", "dvb_subtitle", "subrip", "ass", ""} {
		if OCRSupportsCodec(codec) {
			t.Errorf("%q must not be reported as OCR-readable", codec)
		}
	}
}
