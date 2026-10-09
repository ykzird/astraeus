// Package pgs writes the Blu-ray Presentation Graphic Stream (.sup) format.
//
// It exists because nothing else can produce an image-subtitle sample here.
// ffmpeg cannot encode a bitmap subtitle from text ("only possible from text to
// text or bitmap to bitmap"), and while a bitmap-to-bitmap transcode does
// produce VobSub (scripts/make-vobsub-fixture.sh), no PGS file ships with this
// project. Without a fixture, image-subtitle handling could only be asserted
// against its own command line, which is exactly the evidence this project does
// not accept. The package is used by integration tests and by scripts/pgsgen,
// which builds a fixture for the browser harnesses.
//
// A cue draws one solid rectangle in opaque white on a transparent background.
// That is enough to prove a bitmap is decoded, positioned and composited; it is
// deliberately not a text renderer.
package pgs

import "encoding/binary"

// Segment types. Only the five a display set needs are written.
const (
	segPCS = 0x16 // presentation composition
	segWDS = 0x17 // window definition
	segPDS = 0x14 // palette definition
	segODS = 0x15 // object definition
	segEND = 0x80 // end of display set
)

// Cue is one rectangle shown between two times.
type Cue struct {
	// StartMS and EndMS are in milliseconds of presentation time.
	StartMS int
	EndMS   int
	// X and Y are the rectangle's position in the video's frame.
	X int
	Y int
	// Width and Height are the rectangle's size.
	Width  int
	Height int
}

// BitmapCue is one cue whose object carries an explicit indexed bitmap rather
// than a solid rectangle. It is what a text fixture needs: the OCR work can
// only be verified against a picture that actually contains letters.
type BitmapCue struct {
	// StartMS and EndMS are in milliseconds of presentation time.
	StartMS int
	EndMS   int
	// X and Y are the object's position in the video's frame.
	X int
	Y int
	// Width and Height are the bitmap's size.
	Width  int
	Height int
	// Indexes selects a palette entry per pixel, row major, Width*Height long.
	// Entry 0 is transparent and entries 1-3 are opaque white, so a fixture
	// that wants visible ink sets 1 and everything else 0.
	Indexes []byte
}

// Subtitle renders a .sup for a video of the given size, with one display set
// showing each cue and an empty display set clearing it at the cue's end.
func Subtitle(videoWidth, videoHeight int, cues []Cue) []byte {
	bitmaps := make([]BitmapCue, 0, len(cues))
	for _, cue := range cues {
		indexes := make([]byte, cue.Width*cue.Height)
		for i := range indexes {
			indexes[i] = 1
		}
		bitmaps = append(bitmaps, BitmapCue{
			StartMS: cue.StartMS, EndMS: cue.EndMS,
			X: cue.X, Y: cue.Y, Width: cue.Width, Height: cue.Height,
			Indexes: indexes,
		})
	}
	return Bitmaps(videoWidth, videoHeight, bitmaps)
}

// Bitmaps renders a .sup for a video of the given size from explicit bitmaps,
// with one display set showing each cue and an empty display set clearing it at
// the cue's end. Subtitle is this function with a solid rectangle per cue.
func Bitmaps(videoWidth, videoHeight int, cues []BitmapCue) []byte {
	var out []byte
	composition := uint16(0)
	for _, cue := range cues {
		out = append(out, displaySet(msToPTS(cue.StartMS), videoWidth, videoHeight, composition, cue, true)...)
		composition++
		out = append(out, displaySet(msToPTS(cue.EndMS), videoWidth, videoHeight, composition, cue, false)...)
		composition++
	}
	return out
}

// msToPTS converts milliseconds to the 90 kHz clock the format uses.
func msToPTS(ms int) uint32 { return uint32(int64(ms) * 90) }

func segment(segmentType byte, pts uint32, payload []byte) []byte {
	out := []byte("PG")
	out = binary.BigEndian.AppendUint32(out, pts)
	// The .sup convention is a zero DTS; ffmpeg reads presentation times.
	out = binary.BigEndian.AppendUint32(out, 0)
	out = append(out, segmentType)
	out = binary.BigEndian.AppendUint16(out, uint16(len(payload)))
	return append(out, payload...)
}

// displaySet writes the five segments of one presentation. A set with no object
// clears the screen, which is how a cue ends.
func displaySet(pts uint32, videoWidth, videoHeight int, composition uint16, cue BitmapCue, withObject bool) []byte {
	out := segment(segPCS, pts, pcs(videoWidth, videoHeight, composition, cue, withObject))
	if withObject {
		out = append(out, segment(segWDS, pts, wds(cue))...)
		out = append(out, segment(segPDS, pts, pds())...)
		out = append(out, segment(segODS, pts, ods(cue))...)
	}
	return append(out, segment(segEND, pts, nil)...)
}

func pcs(videoWidth, videoHeight int, composition uint16, cue BitmapCue, withObject bool) []byte {
	out := binary.BigEndian.AppendUint16(nil, uint16(videoWidth))
	out = binary.BigEndian.AppendUint16(out, uint16(videoHeight))
	out = append(out, 0x10) // 25 fps
	out = binary.BigEndian.AppendUint16(out, composition)
	out = append(out, 0x00, 0x00, 0x00) // normal state, no palette update, palette 0
	if !withObject {
		return append(out, 0x00)
	}
	out = append(out, 0x01)                     // one composition object
	out = binary.BigEndian.AppendUint16(out, 0) // object id 0
	out = append(out, 0x00, 0x00)               // window 0, not cropped
	out = binary.BigEndian.AppendUint16(out, uint16(cue.X))
	out = binary.BigEndian.AppendUint16(out, uint16(cue.Y))
	return out
}

func wds(cue BitmapCue) []byte {
	out := []byte{0x01, 0x00} // one window, id 0
	out = binary.BigEndian.AppendUint16(out, uint16(cue.X))
	out = binary.BigEndian.AppendUint16(out, uint16(cue.Y))
	out = binary.BigEndian.AppendUint16(out, uint16(cue.Width))
	out = binary.BigEndian.AppendUint16(out, uint16(cue.Height))
	return out
}

// pds defines the palette: entry 0 is transparent, the rest are opaque white,
// which is what the object's pixels select.
func pds() []byte {
	out := []byte{0x00, 0x00} // palette id 0, version 0
	out = append(out, 0x00, 16, 128, 128, 0)
	for i := 1; i < 4; i++ {
		out = append(out, byte(i), 235, 128, 128, 255)
	}
	return out
}

func ods(cue BitmapCue) []byte {
	data := binary.BigEndian.AppendUint16(nil, uint16(cue.Width))
	data = binary.BigEndian.AppendUint16(data, uint16(cue.Height))
	for line := 0; line < cue.Height; line++ {
		row := cue.Indexes[line*cue.Width : (line+1)*cue.Width]
		for start := 0; start < len(row); {
			end := start + 1
			for end < len(row) && row[end] == row[start] {
				end++
			}
			data = append(data, run(row[start], end-start)...)
			start = end
		}
		data = append(data, 0x00, 0x00) // end of line
	}

	out := binary.BigEndian.AppendUint16(nil, 0) // object id 0
	out = append(out, 0x00, 0xC0)                // version 0, first and last fragment
	out = append(out, byte(len(data)>>16), byte(len(data)>>8), byte(len(data)))
	return append(out, data...)
}

// run encodes one run of n pixels of a palette index. A zero index uses the
// two transparent forms; any other index uses a coloured form. A run of one or
// two pixels uses the format's shortest coloured form - a count byte and a
// colour byte - which is what its own encoders write and which is otherwise
// never exercised.
func run(color byte, n int) []byte {
	var out []byte
	for n > 0 {
		chunk := n
		if chunk > 16383 {
			chunk = 16383
		}
		switch {
		case color == 0 && chunk <= 63:
			out = append(out, 0x00, byte(chunk))
		case color == 0:
			out = append(out, 0x00, 0x40|byte(chunk>>8), byte(chunk))
		case chunk <= 2:
			out = append(out, byte(chunk), color)
		case chunk <= 63:
			out = append(out, 0x00, 0x80|byte(chunk), color)
		default:
			out = append(out, 0x00, 0xC0|byte(chunk>>8), byte(chunk), color)
		}
		n -= chunk
	}
	return out
}
