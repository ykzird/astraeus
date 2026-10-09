// A small Matroska reader, enough to reach a VobSub track's two halves: its
// codec private (the .idx text that carries the palette) and its packets.
//
// It exists because the decoder can then be proved against a committed fixture
// without shelling out to ffmpeg. This is deliberately not a general demuxer:
// it walks the EBML tree, finds the subtitle track entry and reads that track's
// blocks. Elements it does not need are skipped by their declared size, so an
// unfamiliar one is stepped over rather than mistaken for data.

package subtitles

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"
)

// Matroska element ids this reader recognises.
const (
	matroskaSegment    = 0x18538067
	matroskaTimestamp  = 0xE7 // Cluster Timestamp, in TimestampScale ticks
	matroskaTracks     = 0x1654AE6B
	matroskaTrackEntry = 0xAE
	matroskaTrackNum   = 0xD7
	matroskaCodecID    = 0x86
	matroskaCodecPriv  = 0x63A2
	matroskaCluster    = 0x1F43B675
	matroskaSimpleBlk  = 0xA3
	matroskaBlockGroup = 0xA0
	matroskaBlock      = 0xA1
)

// matroskaElement is one EBML element. A master element keeps its children in
// order; a leaf keeps its payload.
type matroskaElement struct {
	id       uint64
	body     []byte
	children []*matroskaElement
}

func (e *matroskaElement) child(id uint64) *matroskaElement {
	if e == nil {
		return nil
	}
	for _, c := range e.children {
		if c.id == id {
			return c
		}
	}
	return nil
}

func (e *matroskaElement) childrenWith(id uint64) []*matroskaElement {
	if e == nil {
		return nil
	}
	var found []*matroskaElement
	for _, c := range e.children {
		if c.id == id {
			found = append(found, c)
		}
	}
	return found
}

// matroskaContainers are the elements this reader opens up. Everything else is
// a leaf even where the format says otherwise, which keeps the walk bounded and
// means an unexpected master element is skipped rather than descended into.
var matroskaContainers = map[uint64]bool{
	matroskaSegment:    true,
	matroskaTracks:     true,
	matroskaTrackEntry: true,
	matroskaCluster:    true,
	matroskaBlockGroup: true,
}

// matroskaSubtitleTrack is what a VobSub track needs from its TrackEntry.
type matroskaSubtitleTrack struct {
	number    uint64
	codecID   string
	codecPriv []byte
}

// demuxMatroskaVobSub reads a Matroska file's dvd_subtitle track and its
// packets. A file with no such track is ErrInvalidVobSub rather than an empty
// result, because a caller that asked for one has been told it exists.
func demuxMatroskaVobSub(r io.Reader) (matroskaSubtitleTrack, []vobsubCue, error) {
	elements, err := readMatroskaTree(r, -1)
	if err != nil {
		return matroskaSubtitleTrack{}, nil, err
	}
	root := &matroskaElement{children: elements}
	segment := root.child(matroskaSegment)
	if segment == nil {
		return matroskaSubtitleTrack{}, nil, fmt.Errorf("%w: not a Matroska file", ErrInvalidVobSub)
	}

	var (
		track matroskaSubtitleTrack
		found bool
	)
	for _, entry := range segment.child(matroskaTracks).childrenWith(matroskaTrackEntry) {
		if codec := entry.child(matroskaCodecID); codec == nil || string(codec.body) != "S_VOBSUB" {
			continue
		}
		found = true
		if number := entry.child(matroskaTrackNum); number != nil {
			track.number = decodeUint(number.body)
		}
		if priv := entry.child(matroskaCodecPriv); priv != nil {
			track.codecPriv = priv.body
		}
	}
	if !found {
		return matroskaSubtitleTrack{}, nil, fmt.Errorf("%w: the file has no Matroska VobSub track", ErrInvalidVobSub)
	}
	track.codecID = "S_VOBSUB"

	var cues []vobsubCue
	for _, cluster := range segment.childrenWith(matroskaCluster) {
		var base int64
		if stamp := cluster.child(matroskaTimestamp); stamp != nil {
			base = decodeInt(stamp.body)
		}
		for _, element := range cluster.children {
			block := element
			if element.id == matroskaBlockGroup {
				block = element.child(matroskaBlock)
			}
			if block == nil || (block.id != matroskaSimpleBlk && block.id != matroskaBlock) {
				continue
			}
			number, relative, payload, err := readMatroskaBlock(block.body, block.id)
			if err != nil {
				return matroskaSubtitleTrack{}, nil, err
			}
			if number != track.number {
				continue
			}
			cues = append(cues, vobsubCue{
				start: time.Duration(base+relative) * time.Millisecond,
				spu:   payload,
			})
		}
	}
	return track, cues, nil
}

// readMatroskaBlock splits a block into its track number, its timestamp
// relative to the cluster and its payload.
//
// A block header is an EBML variable-length integer for the track number, a
// signed 16-bit timestamp relative to the cluster, and a flags byte. Both
// element types carry the flags byte - a SimpleBlock and the Block inside a
// BlockGroup are the same structure - and both parts were wrong here (L-2 of the
// 2026-10-09 review):
//
//   - the track number was read with binary.Uvarint, which decodes the
//     protobuf/LEB128 varint and not the EBML vint. EBML marks a vint's length
//     with the position of its first set bit and includes that marker in the
//     value, so 0x81 is one byte meaning 1 (not 129), and 0x40 0x02 is two bytes
//     meaning 2 (not 64, with a stray 0x02 left over);
//   - the flags byte was skipped only for a SimpleBlock, so a Block was read two
//     bytes short and its payload started inside the timestamp.
//
// The two errors cancelled each other out only while the cluster-relative
// timestamp stayed under 256 ms, which is why a five-cue fixture came back with
// two cues.
func readMatroskaBlock(body []byte, id uint64) (uint64, int64, []byte, error) {
	number, n, err := readEBMLVint(body)
	if err != nil {
		return 0, 0, nil, err
	}
	if len(body) < n+3 {
		return 0, 0, nil, fmt.Errorf("%w: a %d-byte Matroska block is too short", ErrInvalidVobSub, len(body))
	}
	relative := int64(int16(binary.BigEndian.Uint16(body[n : n+2])))
	payloadAt := n + 3 // the timestamp, then the flags byte both types carry
	return number, relative, body[payloadAt:], nil
}

// readEBMLVint decodes an EBML variable-length integer and reports how many
// bytes it occupied.
//
// The first byte's leading zero bits say how long the integer is - one to eight
// bytes, marked by the first set bit. That marker is part of the stored value
// and is masked off, which is what makes 0x81 mean 1 and 0x40 0x02 mean 2.
func readEBMLVint(body []byte) (uint64, int, error) {
	if len(body) == 0 {
		return 0, 0, fmt.Errorf("%w: a Matroska block has no track number", ErrInvalidVobSub)
	}

	first := body[0]
	if first == 0 {
		// 0x00 would announce a nine-byte integer, which the format does not have.
		return 0, 0, fmt.Errorf("%w: a Matroska track number starts with a zero byte", ErrInvalidVobSub)
	}

	length := 1
	for mask := byte(0x80); mask != 0 && first&mask == 0; mask >>= 1 {
		length++
	}
	if len(body) < length {
		return 0, 0, fmt.Errorf("%w: a %d-byte Matroska block holds a %d-byte track number",
			ErrInvalidVobSub, len(body), length)
	}

	value := uint64(first &^ (0x80 >> (length - 1)))
	for i := 1; i < length; i++ {
		value = value<<8 | uint64(body[i])
	}
	return value, length, nil
}

func decodeUint(body []byte) uint64 {
	var value uint64
	for _, b := range body {
		value = value<<8 | uint64(b)
	}
	return value
}

func decodeInt(body []byte) int64 {
	var value int64
	for _, b := range body {
		value = value<<8 | int64(b)
	}
	return value
}

// readMatroskaTree reads one EBML level and returns its elements in order. A
// negative size means the extent is not known, in which case the level runs to
// the end of the stream.
func readMatroskaTree(r io.Reader, size int64) ([]*matroskaElement, error) {
	var (
		elements []*matroskaElement
		consumed int64
	)

	for size < 0 || consumed < size {
		// What the parent still admits to. An element larger than this cannot
		// fit in its container however well-formed its own declaration is.
		remaining := int64(-1)
		if size >= 0 {
			remaining = size - consumed
		}

		id, idLen, err := readMatroskaID(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				// A level whose extent is not known, or one that ends on a byte
				// boundary, has simply finished.
				return elements, nil
			}
			return nil, err
		}
		elementSize, sizeLen, unknown, err := readMatroskaSize(r)
		if err != nil {
			return nil, err
		}
		consumed += int64(idLen + sizeLen)

		element := &matroskaElement{id: id}
		switch {
		case unknown:
			// An unknown-size element's children run to the end of the stream.
			// The Segment is the only one this reader meets.
			children, err := readMatroskaTree(r, -1)
			if err != nil {
				return nil, err
			}
			element.children = children
			return append(elements, element), nil
		case elementSize < 0:
			return nil, fmt.Errorf("%w: a Matroska element declares a negative size", ErrInvalidVobSub)
		case matroskaContainers[id]:
			children, err := readMatroskaTree(io.LimitReader(r, elementSize), elementSize)
			if err != nil {
				return nil, err
			}
			element.children = children
		default:
			element.body, err = readMatroskaBody(r, elementSize, remaining)
			if err != nil {
				return nil, err
			}
		}
		elements = append(elements, element)
		consumed += elementSize
	}
	return elements, nil
}

// maxMatroskaElement bounds one element's body. A subpicture track's samples are
// small - the committed fixture's largest is a few kilobytes - so anything in the
// megabytes is a malformed or hostile stream rather than a subtitle.
const maxMatroskaElement = 64 << 20 // 64 MiB

// readMatroskaBody reads one element's payload after bounding its size.
//
// An element's size is a 56-bit field, so a twelve-byte input can declare an
// element larger than any file and make the reader ask for that much memory
// outright: make([]byte, elementSize) panics with "makeslice: len out of range",
// and sizes just under the limit produce a fatal out-of-memory instead of a
// panic - which net/http cannot recover, so the process dies (L-16 of the
// 2026-10-09 review; found again by FuzzDemuxMatroskaVobSub in 47 ms).
//
// Three bounds, and all three matter: the declared size, the size the parent
// admits to (a child cannot be larger than its parent's remaining bytes), and an
// absolute cap so a well-formed declaration cannot be used to allocate anyway.
func readMatroskaBody(r io.Reader, elementSize int64, parentSize int64) ([]byte, error) {
	if elementSize < 0 {
		return nil, fmt.Errorf("%w: a Matroska element declares a negative size", ErrInvalidVobSub)
	}
	if elementSize > maxMatroskaElement {
		return nil, fmt.Errorf("%w: a Matroska element declares %d bytes, over the %d-byte limit",
			ErrInvalidVobSub, elementSize, maxMatroskaElement)
	}
	if parentSize >= 0 && elementSize > parentSize {
		return nil, fmt.Errorf("%w: a %d-byte Matroska element does not fit in its %d-byte parent",
			ErrInvalidVobSub, elementSize, parentSize)
	}

	body := make([]byte, elementSize)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, fmt.Errorf("%w: a %d-byte Matroska element ended early: %v", ErrInvalidVobSub, elementSize, err)
	}
	return body, nil
}

// readMatroskaID reads a variable-length element id, whose leading bit marks
// its length.
func readMatroskaID(r io.Reader) (uint64, int, error) {
	var first [1]byte
	if _, err := io.ReadFull(r, first[:]); err != nil {
		return 0, 0, err
	}
	length := 1
	for mask := byte(0x80); mask != 0 && first[0]&mask == 0; mask >>= 1 {
		length++
	}
	if length > 4 {
		return 0, 0, fmt.Errorf("%w: a %d-byte Matroska element id", ErrInvalidVobSub, length)
	}
	value := uint64(first[0])
	rest := make([]byte, length-1)
	if _, err := io.ReadFull(r, rest); err != nil {
		return 0, 0, err
	}
	for _, b := range rest {
		value = value<<8 | uint64(b)
	}
	return value, length, nil
}

// readMatroskaSize reads a variable-length size, whose leading bit marks its
// length. A payload of all ones is the format's "unknown size".
func readMatroskaSize(r io.Reader) (int64, int, bool, error) {
	var first [1]byte
	if _, err := io.ReadFull(r, first[:]); err != nil {
		return 0, 0, false, err
	}
	length := 1
	for mask := byte(0x80); mask != 0 && first[0]&mask == 0; mask >>= 1 {
		length++
	}
	if length > 8 {
		return 0, 0, false, fmt.Errorf("%w: a %d-byte Matroska element size", ErrInvalidVobSub, length)
	}
	payload := first[0] & (0xff >> uint(length))
	value := int64(payload)
	rest := make([]byte, length-1)
	if _, err := io.ReadFull(r, rest); err != nil {
		return 0, 0, false, err
	}
	for _, b := range rest {
		value = value<<8 | int64(b)
	}
	// Every data bit set is the format's "unknown size", and the check has to
	// consider the whole value: a payload of 1 followed by zeroes is 2^(7n), a
	// perfectly ordinary length.
	unknown := value == (int64(1)<<(7*uint(length)))-1
	return value, length, unknown, nil
}
