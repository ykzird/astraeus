package pgs

import "strings"

// This file adds *text* to the PGS fixture. The rectangle in pgs.go proves a
// bitmap is decoded, positioned and composited; OCR needs something with
// letters to read, and no real PGS sample ships with this project, so the
// letters have to be drawn here.
//
// The font is a 5x7 bitmap, embedded rather than loaded, because a font file
// and a text renderer would be two more things for a test fixture to depend on.
// Every glyph is scaled by an integer, and the default scale makes the capitals
// about 40 pixels tall, which is the size tesseract reads most reliably.

// glyphs maps a character to seven rows of five pixels, top to bottom, with
// '1' for ink. Only the characters a fixture caption needs are present.
var glyphs = map[rune][7]string{
	'A': {"01110", "10001", "10001", "11111", "10001", "10001", "10001"},
	'B': {"11110", "10001", "10001", "11110", "10001", "10001", "11110"},
	'C': {"01110", "10001", "10000", "10000", "10000", "10001", "01110"},
	'D': {"11110", "10001", "10001", "10001", "10001", "10001", "11110"},
	'E': {"11111", "10000", "10000", "11110", "10000", "10000", "11111"},
	'F': {"11111", "10000", "10000", "11110", "10000", "10000", "10000"},
	'G': {"01110", "10001", "10000", "10111", "10001", "10001", "01111"},
	'H': {"10001", "10001", "10001", "11111", "10001", "10001", "10001"},
	'I': {"11111", "00100", "00100", "00100", "00100", "00100", "11111"},
	'J': {"00111", "00010", "00010", "00010", "00010", "10010", "01100"},
	'K': {"10001", "10010", "10100", "11000", "10100", "10010", "10001"},
	'L': {"10000", "10000", "10000", "10000", "10000", "10000", "11111"},
	'M': {"10001", "11011", "10101", "10101", "10001", "10001", "10001"},
	'N': {"10001", "11001", "10101", "10011", "10001", "10001", "10001"},
	'O': {"01110", "10001", "10001", "10001", "10001", "10001", "01110"},
	'P': {"11110", "10001", "10001", "11110", "10000", "10000", "10000"},
	'Q': {"01110", "10001", "10001", "10001", "10101", "10010", "01101"},
	'R': {"11110", "10001", "10001", "11110", "10100", "10010", "10001"},
	'S': {"01111", "10000", "10000", "01110", "00001", "00001", "11110"},
	'T': {"11111", "00100", "00100", "00100", "00100", "00100", "00100"},
	'U': {"10001", "10001", "10001", "10001", "10001", "10001", "01110"},
	'V': {"10001", "10001", "10001", "10001", "10001", "01010", "00100"},
	'W': {"10001", "10001", "10001", "10101", "10101", "11011", "10001"},
	'X': {"10001", "10001", "01010", "00100", "01010", "10001", "10001"},
	'Y': {"10001", "10001", "01010", "00100", "00100", "00100", "00100"},
	'Z': {"11111", "00001", "00010", "00100", "01000", "10000", "11111"},
	'0': {"01110", "10011", "10101", "10101", "11001", "10001", "01110"},
	'1': {"00100", "01100", "00100", "00100", "00100", "00100", "01110"},
	'2': {"01110", "10001", "00001", "00010", "00100", "01000", "11111"},
	'3': {"11111", "00010", "00100", "00010", "00001", "10001", "01110"},
	'4': {"00010", "00110", "01010", "10010", "11111", "00010", "00010"},
	'5': {"11111", "10000", "11110", "00001", "00001", "10001", "01110"},
	'6': {"00110", "01000", "10000", "11110", "10001", "10001", "01110"},
	'7': {"11111", "00001", "00010", "00100", "01000", "01000", "01000"},
	'8': {"01110", "10001", "10001", "01110", "10001", "10001", "01110"},
	'9': {"01110", "10001", "10001", "01111", "00001", "00010", "01100"},
	' ': {"00000", "00000", "00000", "00000", "00000", "00000", "00000"},
	'.': {"00000", "00000", "00000", "00000", "00000", "01100", "01100"},
	',': {"00000", "00000", "00000", "00000", "01100", "00100", "01000"},
	'!': {"00100", "00100", "00100", "00100", "00100", "00000", "00100"},
	'?': {"01110", "10001", "00001", "00010", "00100", "00000", "00100"},
	'-': {"00000", "00000", "00000", "11111", "00000", "00000", "00000"},
	':': {"00000", "01100", "01100", "00000", "01100", "01100", "00000"},
}

const (
	glyphWidth  = 5
	glyphHeight = 7
	// glyphGap is the space between glyphs, in font pixels.
	glyphGap = 1
	// DefaultTextScale is the glyph size the fixtures use. It is chosen for the
	// recogniser, not for the eye: at this size the capitals come out about 28
	// pixels tall with four-pixel strokes, which tesseract reads exactly, while
	// larger multiples of this blocky 5x7 font read as heavier and are misread
	// ("ASTRAEUS" came back as "ASTRAELS" at six).
	DefaultTextScale = 4
)

// TextCue is one caption rendered as a bitmap of text.
type TextCue struct {
	// StartMS and EndMS are in milliseconds of presentation time.
	StartMS int
	EndMS   int
	// X and Y place the caption's top-left corner in the video's frame.
	X int
	Y int
	// Text is the caption. A newline starts a second line.
	Text string
	// Scale is the integer size multiplier for the glyphs. Zero means
	// DefaultTextScale.
	Scale int
}

// Text renders cues as indexed bitmaps and writes them as a .sup.
func Text(videoWidth, videoHeight int, cues []TextCue) []byte {
	bitmaps := make([]BitmapCue, 0, len(cues))
	for _, cue := range cues {
		scale := cue.Scale
		if scale <= 0 {
			scale = DefaultTextScale
		}
		width, height, indexes := RenderText(cue.Text, scale)
		bitmaps = append(bitmaps, BitmapCue{
			StartMS: cue.StartMS, EndMS: cue.EndMS,
			X: cue.X, Y: cue.Y, Width: width, Height: height,
			Indexes: indexes,
		})
	}
	return Bitmaps(videoWidth, videoHeight, bitmaps)
}

// RenderText draws text into a row-major palette-index plane: 1 for ink and 0
// for transparent. Lines are separated by '\n' and stacked with one font pixel
// of leading. It returns the plane's width and height, which are zero for text
// that draws nothing.
func RenderText(text string, scale int) (int, int, []byte) {
	if scale < 1 {
		scale = 1
	}
	lines := strings.Split(text, "\n")

	width := 0
	for _, line := range lines {
		if n := len([]rune(line))*glyphWidth*scale + (len([]rune(line))-1)*glyphGap*scale; n > width {
			width = n
		}
	}
	lineHeight := glyphHeight * scale
	height := len(lines)*lineHeight + (len(lines)-1)*scale
	if width <= 0 || height <= 0 {
		return 0, 0, nil
	}

	indexes := make([]byte, width*height)
	for lineNumber, line := range lines {
		originY := lineNumber * (lineHeight + scale)
		penX := 0
		for _, r := range line {
			glyph, ok := glyphs[r]
			if !ok {
				glyph = glyphs['?']
			}
			for row := 0; row < glyphHeight; row++ {
				for col := 0; col < glyphWidth; col++ {
					if glyph[row][col] != '1' {
						continue
					}
					for dy := 0; dy < scale; dy++ {
						for dx := 0; dx < scale; dx++ {
							indexes[(originY+row*scale+dy)*width+penX+col*scale+dx] = 1
						}
					}
				}
			}
			penX += (glyphWidth + glyphGap) * scale
		}
	}
	return width, height, indexes
}
