package subtitles

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// pgsSegment wraps a payload in the four-field segment header ParsePGS reads.
func limitSegment(segmentType byte, pts uint32, payload []byte) []byte {
	out := []byte("PG")
	out = binary.BigEndian.AppendUint32(out, pts)
	out = binary.BigEndian.AppendUint32(out, 0)
	out = append(out, segmentType)
	out = binary.BigEndian.AppendUint16(out, uint16(len(payload)))
	return append(out, payload...)
}

// pgsComposition builds a PCS with one element per position given.
func pgsComposition(videoWidth, videoHeight int, elements [][2]int) []byte {
	out := binary.BigEndian.AppendUint16(nil, uint16(videoWidth))
	out = binary.BigEndian.AppendUint16(out, uint16(videoHeight))
	out = append(out, 0x10)                     // 25 fps
	out = binary.BigEndian.AppendUint16(out, 0) // composition 0
	out = append(out, 0x00, 0x00, 0x00)         // state, no palette update, palette 0
	out = append(out, byte(len(elements)))      // the object count
	for _, element := range elements {
		out = binary.BigEndian.AppendUint16(out, 0) // object id 0
		out = append(out, 0x00, 0x00)               // window 0, not cropped
		out = binary.BigEndian.AppendUint16(out, uint16(element[0]))
		out = binary.BigEndian.AppendUint16(out, uint16(element[1]))
	}
	return out
}

// pgsObjectSegment builds an ODS payload: the object id, version, sequence flags
// and three-byte data length that wrap it, then the object's own size fields and
// its run-length plane.
//
// The plane is filled with one legal escape run per row rather than with real
// pixels. It has to decode - the parser reads it - but nothing here depends on
// what it draws, and filling an 8K plane faithfully would make the test's own
// fixture the memory problem it is trying to catch.
func pgsObjectSegment(width, height int) []byte {
	data := binary.BigEndian.AppendUint16(nil, uint16(width))
	data = binary.BigEndian.AppendUint16(data, uint16(height))
	for row := 0; row < height; row++ {
		remaining := width
		for remaining > 0 {
			chunk := remaining
			if chunk > 63 {
				chunk = 63
			}
			data = append(data, 0x00, 0x80|byte(chunk), 0x01)
			remaining -= chunk
		}
	}

	out := binary.BigEndian.AppendUint16(nil, 0) // object id 0
	out = append(out, 0x00, 0xC0)                // version 0, first and last fragment
	out = append(out, byte(len(data)>>16), byte(len(data)>>8), byte(len(data)))
	return append(out, data...)
}

// TestParsePGS_RefusesAnImpossibleObjectGeometry covers the object half of L-3.
//
// A PGS object's width and height are 16-bit fields, so a 125-byte stream can
// declare 65535x65535. Accepting that means asking for a 4 GB plane, and the
// limit is on the product as well as on each side, because a single huge side
// and an enormous area are different failures.
//
// Only the refused geometries are driven through the whole parser. A legal 8K
// frame cannot be built by a test fixture at all: an ODS data length is three
// bytes but a real 8K plane is far larger and arrives as many fragments, so a
// synthetic one is either truncated - which fails as "object data ended", the
// wrong reason - or is the memory problem this test exists to prevent. The
// accepted geometries are pinned by TestCheckPGSPlane instead.
func TestParsePGS_RefusesAnImpossibleObjectGeometry(t *testing.T) {
	t.Parallel()

	impossible := []struct {
		name          string
		width, height int
	}{
		{name: "a 65535-pixel side", width: 65535, height: 1},
		{name: "both sides at the maximum", width: 65535, height: 65535},
		{name: "an over-limit area", width: 9000, height: 9000},
		// Just over the side limit, with a plausible-looking area.
		{name: "a side just over the limit", width: maxPGSCanvasDimension + 1, height: 16},
	}

	for _, tt := range impossible {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var stream []byte
			stream = append(stream, limitSegment(0x16, 0, pgsComposition(1920, 1080, [][2]int{{0, 0}}))...)
			stream = append(stream, limitSegment(0x15, 0, pgsObjectSegment(tt.width, tt.height))...)

			if _, err := ParsePGS(bytes.NewReader(stream)); err == nil {
				t.Fatalf("a %dx%d object was accepted; %d pixels is not a subpicture",
					tt.width, tt.height, tt.width*tt.height)
			}
		})
	}
}

// TestClampPGSCoord covers the composition half of L-3.
//
// This is the exact shape the review used: two 1x1 objects, one at the origin
// and one at (65535,65535). Each object is tiny and legal on its own; it is the
// bounding box across them that asked for 65536^2 pixels - about 17 GB as one
// image.NewRGBA, and a runtime fatal error rather than a panic, so net/http
// cannot recover it and the process dies.
//
// The clamp is asserted directly rather than through a composed cue. Reaching
// compose needs a palette, a window definition and a decodable plane in the
// stream, and building that fixture buys nothing here: the defect is the
// arithmetic on the two positions, which is what this pins. A stream-driven test
// was written first and passed when no cue was produced at all, which is a test
// that asserts nothing.
func TestClampPGSCoord(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value int
		want  int
	}{
		{name: "the origin", value: 0, want: 0},
		{name: "inside a 4K frame", value: 3840, want: 3840},
		{name: "at the limit", value: maxPGSCanvasDimension, want: maxPGSCanvasDimension},
		{name: "the review's second object", value: 65535, want: maxPGSCanvasDimension},
		{name: "a negative position", value: -1, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := clampPGSCoord(tt.value); got != tt.want {
				t.Errorf("clampPGSCoord(%d) = %d, want %d", tt.value, got, tt.want)
			}
		})
	}

	// The point of the clamp: the bounding box across (0,0) and (65535,65535) is
	// bounded by the canvas limit rather than by the hostile coordinate, so the
	// composed image is a size the parser is willing to allocate.
	// The bounding box across the review's two positions, with and without the
	// clamp. Bounding box dimensions are (far edge - near edge), where a 1x1
	// object at position p has its far edge at p+1.
	const hostile = 65535
	unclamped := (hostile + 1) * (hostile + 1)
	clamped := (clampPGSCoord(hostile) + 1) * (clampPGSCoord(hostile) + 1)

	if clamped >= unclamped {
		t.Fatalf("clamping did not shrink the bounding box: %d vs %d", clamped, unclamped)
	}

	// Both sizes are refused by the plane check, which is the backstop: the
	// clamp alone bounds a coordinate, and 8192 is one past the dimension limit
	// once a one-pixel object sits on it.
	if err := checkPGSPlane(hostile+1, hostile+1); err == nil {
		t.Error("the unclamped 17 GB bounding box was accepted for allocation")
	}
	if err := checkPGSPlane(clampPGSCoord(hostile)+1, clampPGSCoord(hostile)+1); err == nil {
		t.Errorf("the clamped %d-pixel bounding box was accepted for allocation", clamped)
	}
}

// TestCheckPGSPlane pins the predicate the two fixes share.
func TestCheckPGSPlane(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		width, height int
		wantErr       bool
	}{
		{name: "a 1x1 object", width: 1, height: 1},
		{name: "a 4K subpicture", width: 1920, height: 1080},
		{name: "an empty plane", width: 0, height: 10, wantErr: true},
		{name: "a negative dimension", width: -1, height: 10, wantErr: true},
		{name: "a side over the limit", width: maxPGSCanvasDimension + 1, height: 1, wantErr: true},
		{name: "exactly at the area limit", width: 8192, height: 8192},
		{name: "an area over the limit", width: 9000, height: 9000, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := checkPGSPlane(tt.width, tt.height)
			if tt.wantErr && err == nil {
				t.Errorf("checkPGSPlane(%d, %d) should fail", tt.width, tt.height)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("checkPGSPlane(%d, %d) = %v, want nil", tt.width, tt.height, err)
			}
		})
	}
}
