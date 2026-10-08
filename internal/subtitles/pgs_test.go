package subtitles

import (
	"bytes"
	"image"
	"testing"
	"time"

	"github.com/ykzird/astraeus/internal/testfixtures/pgs"
)

func TestParsePGS_ReadsTheTextFixture(t *testing.T) {
	t.Parallel()

	const caption = "ASTRAEUS MEDIA"
	stream := pgs.Text(640, 360, []pgs.TextCue{
		{StartMS: 500, EndMS: 5500, X: 100, Y: 250, Text: caption},
	})

	cues, err := ParsePGS(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("ParsePGS: %v", err)
	}
	if len(cues) != 1 {
		t.Fatalf("got %d cues, want 1: %+v", len(cues), cues)
	}

	cue := cues[0]
	if cue.Start != 500*time.Millisecond {
		t.Errorf("cue start = %v, want 500ms", cue.Start)
	}
	if cue.End != 5500*time.Millisecond {
		t.Errorf("cue end = %v, want 5500ms", cue.End)
	}
	if cue.Image == nil {
		t.Fatal("the cue has no bitmap")
	}

	// The rendered bitmap is the artefact: its size and its ink must match what
	// the fixture drew, pixel for pixel.
	wantWidth, wantHeight, wantIndexes := pgs.RenderText(caption, pgs.DefaultTextScale)
	if got := cue.Image.Bounds().Dx(); got != wantWidth {
		t.Errorf("bitmap width = %d, want %d", got, wantWidth)
	}
	if got := cue.Image.Bounds().Dy(); got != wantHeight {
		t.Errorf("bitmap height = %d, want %d", got, wantHeight)
	}

	wantInk := 0
	for _, index := range wantIndexes {
		if index != 0 {
			wantInk++
		}
	}
	if got := countOpaque(cue.Image); got != wantInk {
		t.Errorf("bitmap has %d opaque pixels, want %d", got, wantInk)
	}

	// The opaque pixels must be the palette's white, not a default black.
	if _, _, _, a := cue.Image.At(cue.Image.Bounds().Min.X, cue.Image.Bounds().Min.Y).RGBA(); a != 0 {
		t.Errorf("the transparent corner is opaque (alpha %d)", a)
	}
	white := false
	for y := cue.Image.Bounds().Min.Y; y < cue.Image.Bounds().Max.Y && !white; y++ {
		for x := cue.Image.Bounds().Min.X; x < cue.Image.Bounds().Max.X; x++ {
			r, g, b, a := cue.Image.At(x, y).RGBA()
			if a == 0xffff && r > 0xc000 && g > 0xc000 && b > 0xc000 {
				white = true
				break
			}
		}
	}
	if !white {
		t.Error("no fully opaque white pixel was decoded from the palette")
	}
}

func TestParsePGS_SeparatesConsecutiveCues(t *testing.T) {
	t.Parallel()

	stream := pgs.Text(640, 360, []pgs.TextCue{
		{StartMS: 0, EndMS: 1000, X: 10, Y: 10, Text: "ONE"},
		{StartMS: 2000, EndMS: 3000, X: 10, Y: 10, Text: "TWO"},
	})

	cues, err := ParsePGS(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("ParsePGS: %v", err)
	}
	if len(cues) != 2 {
		t.Fatalf("got %d cues, want 2", len(cues))
	}
	if cues[0].Start != 0 || cues[0].End != time.Second {
		t.Errorf("first cue = %v..%v, want 0s..1s", cues[0].Start, cues[0].End)
	}
	if cues[1].Start != 2*time.Second || cues[1].End != 3*time.Second {
		t.Errorf("second cue = %v..%v, want 2s..3s", cues[1].Start, cues[1].End)
	}
}

// TestParsePGS_DecodesLongTransparentRuns exercises the run-length forms that
// the rectangle fixture never reaches: a long transparent run carries a count
// that does not fit in the short form.
func TestParsePGS_DecodesLongTransparentRuns(t *testing.T) {
	t.Parallel()

	const width, height = 200, 1
	indexes := make([]byte, width*height)
	indexes[width-1] = 1 // one ink pixel at the far right, after 199 clear ones

	stream := pgs.Bitmaps(640, 360, []pgs.BitmapCue{{
		StartMS: 0, EndMS: 1000, X: 0, Y: 0,
		Width: width, Height: height, Indexes: indexes,
	}})

	cues, err := ParsePGS(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("ParsePGS: %v", err)
	}
	if len(cues) != 1 {
		t.Fatalf("got %d cues, want 1", len(cues))
	}
	if got := cues[0].Image.Bounds().Dx(); got != width {
		t.Fatalf("bitmap width = %d, want %d", got, width)
	}
	if got := countOpaque(cues[0].Image); got != 1 {
		t.Errorf("bitmap has %d opaque pixels, want 1", got)
	}
	if _, _, _, a := cues[0].Image.At(width-1, 0).RGBA(); a != 0xffff {
		t.Errorf("the ink pixel is not opaque (alpha %d)", a)
	}
	if _, _, _, a := cues[0].Image.At(0, 0).RGBA(); a != 0 {
		t.Errorf("a transparent pixel became opaque (alpha %d)", a)
	}
}

func TestParsePGS_RejectsGarbage(t *testing.T) {
	t.Parallel()

	if _, err := ParsePGS(bytes.NewReader([]byte("this is not a PGS stream"))); err == nil {
		t.Fatal("expected an error for input that is not a PGS stream")
	}
}

func TestParsePGS_RejectsATruncatedSegment(t *testing.T) {
	t.Parallel()

	stream := pgs.Text(640, 360, []pgs.TextCue{{StartMS: 0, EndMS: 1000, Text: "HI"}})
	if _, err := ParsePGS(bytes.NewReader(stream[:len(stream)-40])); err == nil {
		t.Fatal("expected an error for a truncated segment")
	}
}

// countOpaque counts the pixels whose palette alpha is fully opaque.
func countOpaque(img *image.RGBA) int {
	count := 0
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a == 0xffff {
				count++
			}
		}
	}
	return count
}
