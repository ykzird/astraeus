// pgsgen writes a PGS (.sup) image-subtitle fixture for the browser harnesses.
//
// The Go integration tests build the same fixture in-process; this exists so a
// developer can make one for a real server without writing code. See
// scripts/ui-verify/README.md for the muxing recipe.
//
// With no -text it writes one solid rectangle, which is what the burn-in work
// needs. With -text it writes real letters, which is what the OCR work needs:
// tesseract can only be shown to read a caption if the caption contains words.
//
//	go run ./scripts/pgsgen -out fixture.sup
//	go run ./scripts/pgsgen -text "ASTRAEUS MEDIA" -out caption.sup
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/jok/astraeus-media/internal/testfixtures/pgs"
)

func main() {
	out := flag.String("out", "fixture.sup", "path to write the .sup to")
	videoWidth := flag.Int("video-width", 640, "video frame width the cues are positioned in")
	videoHeight := flag.Int("video-height", 360, "video frame height")
	x := flag.Int("x", 100, "rectangle or caption left edge")
	y := flag.Int("y", 250, "rectangle or caption top edge")
	width := flag.Int("w", 400, "rectangle width (ignored with -text)")
	height := flag.Int("h", 60, "rectangle height (ignored with -text)")
	start := flag.Int("start-ms", 500, "when the caption appears, in milliseconds")
	end := flag.Int("end-ms", 5500, "when it disappears, in milliseconds")
	text := flag.String("text", "", "render this caption instead of a rectangle; a newline starts a new line")
	scale := flag.Int("text-scale", pgs.DefaultTextScale, "glyph size multiplier with -text")
	flag.Parse()

	var subtitle []byte
	if *text != "" {
		subtitle = pgs.Text(*videoWidth, *videoHeight, []pgs.TextCue{{
			StartMS: *start, EndMS: *end,
			X: *x, Y: *y, Text: *text, Scale: *scale,
		}})
	} else {
		subtitle = pgs.Subtitle(*videoWidth, *videoHeight, []pgs.Cue{{
			StartMS: *start, EndMS: *end,
			X: *x, Y: *y, Width: *width, Height: *height,
		}})
	}

	if err := os.WriteFile(*out, subtitle, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "pgsgen:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d bytes)\n", *out, len(subtitle))
}
