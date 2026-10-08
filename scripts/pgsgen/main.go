// pgsgen writes a PGS (.sup) image-subtitle fixture for the browser harnesses.
//
// The Go integration tests build the same fixture in-process; this exists so a
// developer can make one for a real server without writing code. See
// scripts/ui-verify/README.md for the muxing recipe.
//
//	go run ./scripts/pgsgen -out fixture.sup
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
	x := flag.Int("x", 100, "rectangle left edge")
	y := flag.Int("y", 250, "rectangle top edge")
	width := flag.Int("w", 400, "rectangle width")
	height := flag.Int("h", 60, "rectangle height")
	start := flag.Int("start-ms", 500, "when the rectangle appears, in milliseconds")
	end := flag.Int("end-ms", 5500, "when it disappears, in milliseconds")
	flag.Parse()

	subtitle := pgs.Subtitle(*videoWidth, *videoHeight, []pgs.Cue{{
		StartMS: *start, EndMS: *end,
		X: *x, Y: *y, Width: *width, Height: *height,
	}})
	if err := os.WriteFile(*out, subtitle, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "pgsgen:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s (%d bytes)\n", *out, len(subtitle))
}
