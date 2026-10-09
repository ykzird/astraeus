package subtitles

import (
	"bytes"
	"os"
	"testing"
)

// vobsubFixture is the committed sample: a real VobSub track made by ffmpeg's
// dvdsub encoder from this project's own PGS fixture, so it draws the same
// caption the PGS tests use.
const vobsubFixture = "testdata/vobsub-caption.mkv"

// TestParseVobSub_ReadsTheCommittedFixture is the decoder's end-to-end unit
// check, and it needs no ffmpeg: the file on disk is a Matroska container with
// a dvd_subtitle track, so the reader supplies both halves of the format from
// one artefact. The geometry asserted is the fixture's own, and is also what
// ffmpeg's decoder produces from the same packets.
func TestParseVobSub_ReadsTheCommittedFixture(t *testing.T) {
	t.Parallel()

	file, err := os.Open(vobsubFixture)
	if err != nil {
		t.Fatalf("opening the fixture: %v", err)
	}
	defer file.Close()

	cues, err := ParseVobSub(file, "")
	if err != nil {
		t.Fatalf("ParseVobSub: %v", err)
	}
	if len(cues) != 1 {
		t.Fatalf("got %d cues, want 1: %+v", len(cues), cues)
	}

	// The picture plane the PGS fixture draws is 332x28, and re-encoding it as
	// VobSub preserves the geometry. Anything else means the rectangle was read
	// from the wrong place or the two RLE fields were interleaved wrongly.
	img := cues[0].Image
	if img == nil {
		t.Fatal("the cue has no bitmap")
	}
	if got := img.Bounds().Dx(); got != 332 {
		t.Errorf("bitmap width = %d, want 332", got)
	}
	if got := img.Bounds().Dy(); got != 28 {
		t.Errorf("bitmap height = %d, want 28", got)
	}

	// Colour index 0 is the transparent background and index 1 is the palette's
	// white, so the opaque pixels are exactly the drawn glyphs.
	ink := 0
	for y := 0; y < img.Bounds().Dy(); y++ {
		for x := 0; x < img.Bounds().Dx(); x++ {
			r, g, b, a := img.At(x, y).RGBA()
			if a == 0 {
				continue
			}
			ink++
			if r != 0xffff || g != 0xffff || b != 0xffff {
				t.Fatalf("pixel (%d,%d) is drawn but not white: r=%d g=%d b=%d a=%d", x, y, r, g, b, a)
			}
		}
	}
	if ink != 3440 {
		t.Errorf("bitmap has %d drawn pixels, want 3440", ink)
	}
}

// vobsubTestField encodes one run-length field the way the format does: each
// line is a sequence of four-bit values, a run's count in units of one pixel
// then its colour, and every line ends on a byte boundary. The count is written
// as a whole nibble because the decoder reads a nibble at a time; the fixture
// this decoder was checked against writes counts of four or more.
// vobsubRunBytes encodes one run of the run-length field. The reader consumes
// four-bit values and keeps going until the number it has built passes a
// threshold that doubles each time, so a run is the nibbles of (run<<2 | index)
// laid down one after another, padded to a whole byte. Writing it this way
// keeps the encoding the reader's inverse rather than a hand-written guess.
func vobsubRunBytes(run, index int) []byte {
	value := run<<2 | index
	var nibbles []int
	if value >= 0x100 {
		nibbles = append(nibbles, value>>8)
	}
	nibbles = append(nibbles, value>>4&0xf, value&0xf)
	if len(nibbles)%2 != 0 {
		nibbles = append([]int{0}, nibbles...)
	}
	out := make([]byte, 0, len(nibbles)/2)
	for i := 0; i < len(nibbles); i += 2 {
		out = append(out, byte(nibbles[i]<<4|nibbles[i+1]))
	}
	return out
}

// TestDecodeVobSubRLE_InterleavesTheTwoFields pins the part of the format that
// is easiest to get wrong: the bitmap is encoded as two run-length fields, and
// the first carries the even lines while the second carries the odd ones. The
// two offsets are packet offsets, which is what the control sequence supplies.
//
// The two fields are decoded on their own -- each one treats its own lines as
// consecutive -- and the interleaved result must be exactly those lines placed
// every other row, the first field's first.
func TestDecodeVobSubRLE_InterleavesTheTwoFields(t *testing.T) {
	t.Parallel()

	const (
		width  = 8
		height = 7
	)
	// Each line is a run of four ink and then four clear, or the reverse. The
	// two runs fill the line exactly and take a byte each.
	inkFirst := append(vobsubRunBytes(4, 1), vobsubRunBytes(4, 0)...)
	inkLast := append(vobsubRunBytes(4, 0), vobsubRunBytes(4, 1)...)

	even := bytes.Join([][]byte{inkFirst, inkLast, inkFirst, inkLast}, nil)
	odd := bytes.Join([][]byte{inkLast, inkFirst, inkLast}, nil)

	spu := []byte{0, 0, 0, 0}
	first := len(spu)
	spu = append(spu, even...)
	second := len(spu)
	spu = append(spu, odd...)

	plane, err := decodeVobSubRLE(spu, spuOffsets{first: first, second: second}, width, height, false)
	if err != nil {
		t.Fatalf("decodeVobSubRLE: %v", err)
	}

	// The expected plane is built from the two fields decoded on their own, so
	// this test does not restate the encoding: it states only where each field's
	// lines go.
	firstLines, err := decodeVobSubField(spu, first, width, 4, false)
	if err != nil {
		t.Fatalf("decoding the first field: %v", err)
	}
	secondLines, err := decodeVobSubField(spu, second, width, 3, false)
	if err != nil {
		t.Fatalf("decoding the second field: %v", err)
	}
	want := make([]byte, width*height)
	for i, line := range firstLines {
		copy(want[(2*i)*width:], line)
	}
	for i, line := range secondLines {
		copy(want[(2*i+1)*width:], line)
	}

	// Compare row by row, so a swapped field shows up as the wrong line rather
	// than as a byte offset.
	for row := 0; row < height; row++ {
		for column := 0; column < width; column++ {
			i := row*width + column
			if plane[i] != want[i] {
				t.Fatalf("row %d, column %d = %d, want %d", row, column, plane[i], want[i])
			}
		}
	}

	// And the two fields really do differ, and each field's lines really do
	// differ from each other, or the comparison would pass for a decoder that
	// drew one line everywhere. Both facts are taken from the case above.
	if string(firstLines[0]) == string(secondLines[0]) {
		t.Fatal("the two fields start with the same line, so the interleave is untested")
	}
	if string(firstLines[0]) == string(firstLines[1]) || string(secondLines[0]) == string(secondLines[1]) {
		t.Fatal("a field's first two lines are alike, so the row placement is untested")
	}
}

// TestParseVobSubPalette_ReadsTheIdxText pins the container half of the format:
// the palette is not in the picture stream, and the text an .idx sidecar carries
// is the same text Matroska's codec private does.
func TestParseVobSubPalette_ReadsTheIdxText(t *testing.T) {
	t.Parallel()

	canvas, palette, err := parseVobSubPalette("size: 640x360\npalette: 000000, 0000ff, 00ff00, ff0000\n")
	if err != nil {
		t.Fatalf("parseVobSubPalette: %v", err)
	}
	if canvas.width != 640 || canvas.height != 360 {
		t.Errorf("canvas = %dx%d, want 640x360", canvas.width, canvas.height)
	}
	if got := palette[1]; got.R != 0x00 || got.G != 0x00 || got.B != 0xff {
		t.Errorf("palette[1] = %+v, want blue", got)
	}
	if got := palette[3]; got.R != 0xff || got.G != 0x00 || got.B != 0x00 {
		t.Errorf("palette[3] = %+v, want red", got)
	}

	if _, _, err := parseVobSubPalette("size: 640x360\n"); err == nil {
		t.Error("a header with no palette must be an error, not a default")
	}
}

func TestParseVobSub_RejectsGarbage(t *testing.T) {
	t.Parallel()

	if _, err := ParseVobSub(bytes.NewReader([]byte("this is not a vobsub stream")), ""); err == nil {
		t.Fatal("expected an error for input that is not a VobSub stream")
	}
}

// TestFrameVobSubPackets_ReframesADemuxedBlob pins the extractor's own framing
// step. The data muxer concatenates SPU records with no boundaries and a
// packet's first two bytes are not its length, so the length has to come from
// the control offset, which is the record's total length. The blob here is the
// shape ffmpeg's data muxer produces for the project's MPEG-PS fixture: one
// record followed by the padding a PES payload carries.
func TestFrameVobSubPackets_ReframesADemuxedBlob(t *testing.T) {
	t.Parallel()

	// A record of twelve bytes: size, control offset (its own length), then the
	// body. Four bytes of padding follow, as a PES payload would have.
	record := []byte{
		0x00, 0x08, // the declared size, two bytes shorter than the record
		0x00, 0x0c, // the control offset, which is the record's length
		0x00, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07,
	}
	blob := append(append([]byte{}, record...), 0xff, 0xff, 0xff, 0xff)

	framed, err := frameVobSubPackets(blob)
	if err != nil {
		t.Fatalf("frameVobSubPackets: %v", err)
	}
	// The framed result is the record with a two-byte length in front of it.
	want := append([]byte{0x00, 0x0c}, record...)
	if !bytes.Equal(framed, want) {
		t.Fatalf("framed = % x, want % x", framed, want)
	}

	// The framed stream must be what the packet reader expects.
	stream, err := readVobSubPackets(bytes.NewReader(framed))
	if err != nil {
		t.Fatalf("reading the framed stream: %v", err)
	}
	if len(stream.cues) != 1 {
		t.Fatalf("got %d packets, want 1", len(stream.cues))
	}
	if !bytes.Equal(stream.cues[0].spu, record) {
		t.Errorf("packet = % x, want % x", stream.cues[0].spu, record)
	}

	// A blob with no complete record is a refusal, not an empty success.
	if _, err := frameVobSubPackets([]byte{0x00, 0x01, 0x02}); err == nil {
		t.Error("a blob with no complete record must be refused")
	}
}
