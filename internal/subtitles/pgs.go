// PGS (Blu-ray Presentation Graphic Stream, ".sup") decoding. The format
// carries subtitles as pictures, so turning one into text means decoding the
// bitmap and then recognising the characters in it; this file is the decoding
// half and produces the image an OCR engine reads.
//
// Only what a subtitle needs is decoded: the composition (PCS), the palette
// (PDS) and the object (ODS). Window definitions (WDS) clip an object on
// screen, which does not change the object itself, so they are skipped: the
// bitmap and its own position are enough to crop it. Fragments of one object
// that arrive in several ODS segments are reassembled, and a display set that
// only updates the palette does not end the cue that is on screen.

package subtitles

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"time"
)

// PGS segment types.
const (
	pgsSegmentPDS = 0x14 // palette definition
	pgsSegmentODS = 0x15 // object definition
	pgsSegmentPCS = 0x16 // presentation composition
	pgsSegmentWDS = 0x17 // window definition
	pgsSegmentEND = 0x80 // end of a display set
)

// pgsClock is the 90 kHz clock the format timestamps with.
const pgsClock = 90000

// ErrInvalidPGS reports a stream that is not a well-formed presentation graphic
// stream. A malformed .sup is a data problem rather than a server fault, so the
// error names the format instead of surfacing a decoding bug.
var ErrInvalidPGS = errors.New("invalid presentation graphic stream")

// ImageCue is one bitmap subtitle decoded from a PGS stream: when it is on
// screen and the picture that was on screen. Image is cropped to the objects
// the display set composed, so it contains the subtitle and not the black
// around it.
type ImageCue struct {
	Start time.Duration
	End   time.Duration
	Image *image.RGBA
}

// ParsePGS decodes a .sup stream into its cues, in presentation order.
func ParsePGS(r io.Reader) ([]ImageCue, error) {
	decoder := newPGSDecoder()

	var (
		cues    []ImageCue
		open    *ImageCue
		lastPTS uint32
	)

	for {
		segment, err := readPGSSegment(r)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		lastPTS = segment.pts

		switch segment.kind {
		case pgsSegmentPCS:
			err = decoder.addPCS(segment.pts, segment.body)
		case pgsSegmentPDS:
			err = decoder.addPDS(segment.body)
		case pgsSegmentODS:
			err = decoder.addODS(segment.body)
		case pgsSegmentWDS:
			// Deliberately unused; see the package comment.
		case pgsSegmentEND:
			var display *pgsDisplay
			display, err = decoder.complete()
			if err == nil && display != nil {
				if open != nil {
					open.End = pgsDuration(display.pts)
					cues = append(cues, *open)
					open = nil
				}
				if display.image != nil {
					open = &ImageCue{Start: pgsDuration(display.pts), Image: display.image}
				}
			}
		}
		if err != nil {
			return nil, err
		}
	}

	// A stream whose last cue is never cleared still has a cue: it ends where
	// the stream does. A zero-length one is dropped rather than handed on.
	if open != nil {
		if end := pgsDuration(lastPTS); end > open.Start {
			open.End = end
			cues = append(cues, *open)
		}
	}
	return cues, nil
}

// pgsDuration converts a 90 kHz presentation timestamp to a duration.
func pgsDuration(pts uint32) time.Duration {
	return time.Duration(pts) * time.Second / pgsClock
}

// pgsSegment is one segment of the stream, as framed by its header.
type pgsSegment struct {
	pts  uint32
	kind byte
	body []byte
}

// readPGSSegment reads one framed segment. It returns io.EOF only at a clean
// segment boundary, so a truncated stream is an error rather than a short read.
func readPGSSegment(r io.Reader) (pgsSegment, error) {
	var header [13]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return pgsSegment{}, io.EOF
		}
		return pgsSegment{}, fmt.Errorf("%w: reading a segment header: %v", ErrInvalidPGS, err)
	}
	if header[0] != 'P' || header[1] != 'G' {
		return pgsSegment{}, fmt.Errorf("%w: expected a PG segment header, found %q", ErrInvalidPGS, header[:2])
	}

	size := int(binary.BigEndian.Uint16(header[11:13]))
	body := make([]byte, size)
	if _, err := io.ReadFull(r, body); err != nil {
		return pgsSegment{}, fmt.Errorf("%w: a %d-byte segment body ended early: %v", ErrInvalidPGS, size, err)
	}

	return pgsSegment{
		pts:  binary.BigEndian.Uint32(header[2:6]),
		kind: header[10],
		body: body,
	}, nil
}

// pgsPalette maps an object's pixel indexes to colours.
type pgsPalette map[byte]color.RGBA

// pgsObject is one decoded bitmap: a plane of palette indexes.
type pgsObject struct {
	width   int
	height  int
	indexes []byte
}

// pgsElement places one object in the frame.
type pgsElement struct {
	objectID uint16
	x        int
	y        int
}

// pgsPresentation accumulates one display set until its END segment.
type pgsPresentation struct {
	pts             uint32
	havePCS         bool
	paletteID       byte
	paletteUpdate   bool
	elements        []pgsElement
	pendingPalettes map[byte]pgsPalette
}

// pgsDisplay is a finished display set: an image to show, or nothing to clear
// the screen.
type pgsDisplay struct {
	pts   uint32
	image *image.RGBA
}

// pgsDecoder keeps the state that outlives one display set: the palettes and
// objects earlier sets defined, and the fragments of an object still arriving.
type pgsDecoder struct {
	palettes  map[byte]pgsPalette
	objects   map[uint16]*pgsObject
	fragments map[uint16][]byte
	pending   pgsPresentation
}

func newPGSDecoder() *pgsDecoder {
	return &pgsDecoder{
		palettes:  map[byte]pgsPalette{},
		objects:   map[uint16]*pgsObject{},
		fragments: map[uint16][]byte{},
	}
}

// addPCS parses a presentation composition. The layout is video size, frame
// rate, composition number, state, palette update flag, palette id, then one
// entry per composition object; a cropped object carries four more rectangle
// fields that are skipped.
func (d *pgsDecoder) addPCS(pts uint32, body []byte) error {
	if len(body) < 11 {
		return fmt.Errorf("%w: a %d-byte composition segment is too short", ErrInvalidPGS, len(body))
	}
	d.pending.pts = pts
	d.pending.paletteUpdate = body[8]&0x80 != 0
	d.pending.paletteID = body[9]

	objects := int(body[10])
	offset := 11
	for i := 0; i < objects; i++ {
		if offset+8 > len(body) {
			return fmt.Errorf("%w: composition object %d is cut short", ErrInvalidPGS, i)
		}
		element := pgsElement{
			objectID: binary.BigEndian.Uint16(body[offset : offset+2]),
			x:        int(binary.BigEndian.Uint16(body[offset+4 : offset+6])),
			y:        int(binary.BigEndian.Uint16(body[offset+6 : offset+8])),
		}
		cropped := body[offset+3]&0x40 != 0
		offset += 8
		if cropped {
			if offset+8 > len(body) {
				return fmt.Errorf("%w: composition object %d has a cut-short crop", ErrInvalidPGS, i)
			}
			offset += 8
		}
		d.pending.elements = append(d.pending.elements, element)
	}

	d.pending.havePCS = true
	return nil
}

// addPDS parses a palette definition: an id, a version, then five bytes per
// entry (index, Y, Cr, Cb, alpha).
func (d *pgsDecoder) addPDS(body []byte) error {
	if len(body) < 2 {
		return fmt.Errorf("%w: a %d-byte palette segment is too short", ErrInvalidPGS, len(body))
	}
	palette := pgsPalette{}
	for offset := 2; offset+5 <= len(body); offset += 5 {
		palette[body[offset]] = pgsColor(body[offset+1], body[offset+2], body[offset+3], body[offset+4])
	}
	if d.pending.pendingPalettes == nil {
		d.pending.pendingPalettes = map[byte]pgsPalette{}
	}
	d.pending.pendingPalettes[body[0]] = palette
	return nil
}

// addODS parses an object definition, reassembling fragments. The header is
// object id, version, sequence flags and a three-byte data length; the data is
// a size followed by run-length-encoded palette indexes.
func (d *pgsDecoder) addODS(body []byte) error {
	if len(body) < 7 {
		return fmt.Errorf("%w: a %d-byte object segment is too short", ErrInvalidPGS, len(body))
	}
	id := binary.BigEndian.Uint16(body[0:2])
	flags := body[3]
	if flags&0x80 != 0 {
		d.fragments[id] = d.fragments[id][:0] // first fragment starts the object
	}
	d.fragments[id] = append(d.fragments[id], body[7:]...)
	if flags&0x40 == 0 {
		return nil // more fragments follow
	}

	object, err := decodePGSObject(d.fragments[id])
	delete(d.fragments, id)
	if err != nil {
		return err
	}
	if object != nil {
		d.objects[id] = object
	}
	return nil
}

// complete turns the accumulated display set into an image, or into a clear.
// A set with no composition objects clears the screen, except when it is only
// updating the palette, which leaves what is on screen in place.
func (d *pgsDecoder) complete() (*pgsDisplay, error) {
	defer func() { d.pending = pgsPresentation{} }()

	if !d.pending.havePCS {
		return nil, nil
	}
	for id, palette := range d.pending.pendingPalettes {
		d.palettes[id] = palette
	}
	if d.pending.paletteUpdate && len(d.pending.elements) == 0 {
		return nil, nil
	}
	if len(d.pending.elements) == 0 {
		return &pgsDisplay{pts: d.pending.pts}, nil
	}
	return &pgsDisplay{pts: d.pending.pts, image: d.compose()}, nil
}

// compose draws the display set's objects onto one bitmap, cropped to the
// rectangle that contains them all. An object is placed at its own position and
// sized by its own pixels; nothing is scaled, because OCR reads the native
// resolution and the video's size is irrelevant to it.
func (d *pgsDecoder) compose() *image.RGBA {
	palette := d.palettes[d.pending.paletteID]

	type placed struct {
		object *pgsObject
		x, y   int
	}
	var (
		items      []placed
		minX, minY int
		maxX, maxY int
	)
	for _, element := range d.pending.elements {
		object := d.objects[element.objectID]
		if object == nil || object.width <= 0 || object.height <= 0 {
			continue
		}
		if len(items) == 0 {
			minX, minY = element.x, element.y
			maxX, maxY = element.x+object.width, element.y+object.height
		} else {
			minX = min(minX, element.x)
			minY = min(minY, element.y)
			maxX = max(maxX, element.x+object.width)
			maxY = max(maxY, element.y+object.height)
		}
		items = append(items, placed{object: object, x: element.x, y: element.y})
	}
	if len(items) == 0 {
		return nil
	}

	img := image.NewRGBA(image.Rect(0, 0, maxX-minX, maxY-minY))
	for _, item := range items {
		for row := 0; row < item.object.height; row++ {
			for column := 0; column < item.object.width; column++ {
				tint, ok := palette[item.object.indexes[row*item.object.width+column]]
				if !ok || tint.A == 0 {
					continue
				}
				img.SetRGBA(item.x-minX+column, item.y-minY+row, tint)
			}
		}
	}
	return img
}

// decodePGSObject decodes one object's data: its size, then a run-length
// encoded plane of palette indexes.
func decodePGSObject(data []byte) (*pgsObject, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("%w: a %d-byte object is too short", ErrInvalidPGS, len(data))
	}
	width := int(binary.BigEndian.Uint16(data[0:2]))
	height := int(binary.BigEndian.Uint16(data[2:4]))
	if width <= 0 || height <= 0 {
		return nil, nil
	}
	indexes, err := decodePGSRLE(data[4:], width, height)
	if err != nil {
		return nil, err
	}
	return &pgsObject{width: width, height: height, indexes: indexes}, nil
}

// decodePGSRLE expands the object's run-length encoding into width*height
// palette indexes. The encoding has four forms, chosen by the top two bits of
// the byte after a zero: a short clear run, a long clear run, a short coloured
// run, and a long coloured run. A non-zero first byte is itself a run length.
func decodePGSRLE(data []byte, width, height int) ([]byte, error) {
	total := width * height
	indexes := make([]byte, 0, total)

	for offset := 0; len(indexes) < total; {
		if offset >= len(data) {
			return nil, fmt.Errorf("%w: object data ended after %d of %d pixels", ErrInvalidPGS, len(indexes), total)
		}

		var (
			value byte
			count int
		)
		if first := data[offset]; first != 0 {
			offset++
			if offset >= len(data) {
				return nil, fmt.Errorf("%w: object data ended inside a run", ErrInvalidPGS)
			}
			value, count = data[offset], int(first)
			offset++
		} else {
			offset++
			if offset >= len(data) {
				return nil, fmt.Errorf("%w: object data ended inside a run", ErrInvalidPGS)
			}
			second := data[offset]
			offset++
			switch second & 0xC0 {
			case 0x00: // short run of transparent pixels
				value, count = 0, int(second)
			case 0x40: // long run of transparent pixels
				if offset >= len(data) {
					return nil, fmt.Errorf("%w: object data ended inside a long run", ErrInvalidPGS)
				}
				value, count = 0, int(second&0x3F)<<8|int(data[offset])
				offset++
			case 0x80: // short run of one palette index
				if offset >= len(data) {
					return nil, fmt.Errorf("%w: object data ended inside a run", ErrInvalidPGS)
				}
				value, count = data[offset], int(second&0x3F)
				offset++
			default: // 0xC0: long run of one palette index
				if offset+1 >= len(data) {
					return nil, fmt.Errorf("%w: object data ended inside a long run", ErrInvalidPGS)
				}
				count = int(second&0x3F)<<8 | int(data[offset])
				value = data[offset+1]
				offset += 2
			}
		}

		// A run that would overrun the plane is clamped: the encoders that
		// produce these streams sometimes pad the last line.
		if remaining := total - len(indexes); count > remaining {
			count = remaining
		}
		for i := 0; i < count; i++ {
			indexes = append(indexes, value)
		}
	}
	return indexes, nil
}

// pgsColor converts one palette entry from the BT.709 limited-range YCbCr the
// format stores into RGB. The transparent entry (alpha 0) is kept as it is.
func pgsColor(luma, cr, cb, alpha byte) color.RGBA {
	y := (float64(luma) - 16) * 255 / 219
	pb := float64(cb) - 128
	pr := float64(cr) - 128
	return color.RGBA{
		R: clamp8(y + 1.5748*pr),
		G: clamp8(y - 0.1873*pb - 0.4681*pr),
		B: clamp8(y + 1.8556*pb),
		A: alpha,
	}
}

func clamp8(value float64) uint8 {
	switch {
	case value <= 0:
		return 0
	case value >= 255:
		return 255
	default:
		return uint8(value + 0.5)
	}
}
