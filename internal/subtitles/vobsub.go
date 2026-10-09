// VobSub ("dvd_subtitle") decoding. Like PGS, VobSub carries subtitles as
// pictures, so reading one as text means decoding the bitmap and recognising
// the characters in it. This file is the decoding half; ocr.go does the
// recognising.
//
// A VobSub track is split across two places, and neither half is enough on its
// own. The picture stream (an MPEG-PS private stream, or the packets a Matroska
// remux of one carries) holds a run-length-encoded bitmap per cue plus the
// control sequence that says which palette entries and alpha values it uses.
// The palette itself lives in the container: a .idx sidecar for a disc rip, or
// Matroska's codec private, which carries the same "size:"/"palette:" text. A
// bare MPEG-PS sample carries no palette at all, which is why the extraction
// keeps a container that has one.
//
// The picture stream's own framing is unusual and worth stating once. A packet
// is a two-byte size and a two-byte control-sequence offset, so the offset that
// points at the control sequence is also the total packet length; the stated
// size is two bytes shorter and is not used to frame anything. The bitmap sits
// between the header and the control sequence. Everything below follows
// ffmpeg's dvdsub decoder, which is the reference this project can compare
// against, and the field interleaving was checked against ffmpeg's own output
// rather than reasoned about.

package subtitles

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"strconv"
	"strings"
	"time"
)

// ErrInvalidVobSub reports a stream that is not a well-formed VobSub track. A
// malformed packet is a data problem rather than a server fault, so the error
// names the format instead of surfacing a decoding bug.
var ErrInvalidVobSub = errors.New("invalid vobsub stream")

// vobsubStream is the picture half of a VobSub track: the packets, in
// presentation order, and the palette text the container carried beside them.
type vobsubStream struct {
	paletteText string
	cues        []vobsubCue
}

// matroskaHeader is the EBML magic every Matroska file starts with, which is
// how the extractor's framed packet stream is told apart from a container.
var matroskaHeader = []byte{0x1A, 0x45, 0xDF, 0xA3}

// vobsubCanvas is the "size:" line of the container's palette text. It records
// the frame the subtitles were authored against, which the decoder does not
// need to place them: a packet carries its own rectangle.
type vobsubCanvas struct {
	width  int
	height int
}

// vobsubCue is one SPU packet: when it is on screen and the picture it draws.
// A cue with no bitmap is the packet that clears the screen; ParseVobSub hands
// it on so the previous cue can be closed.
type vobsubCue struct {
	start time.Duration
	spu   []byte
}

// vobsubPalette is the container's palette: sixteen RGB colours, as the .idx
// text lists them. The .idx format writes each colour as a six-digit hex number
// whose bytes are red, green and blue, and Matroska's codec private carries the
// identical text.
type vobsubPalette [16]color.RGBA

// parseVobSubPalette reads the "size:" and "palette:" lines of an .idx-style
// header. Unknown lines are skipped so a fuller .idx sidecar (an "id:",
// "langidx:" or timestamp line) still parses; a palette that is missing or
// short is an error, because a decoder without one cannot render anything.
func parseVobSubPalette(text string) (vobsubCanvas, vobsubPalette, error) {
	var (
		canvas  vobsubCanvas
		palette vobsubPalette
		found   bool
	)
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		switch {
		case strings.HasPrefix(line, "size:"):
			size := strings.TrimSpace(strings.TrimPrefix(line, "size:"))
			width, height, ok := strings.Cut(size, "x")
			if !ok {
				return canvas, palette, fmt.Errorf("%w: size line %q is not WxH", ErrInvalidVobSub, line)
			}
			canvas.width, _ = strconv.Atoi(strings.TrimSpace(width))
			canvas.height, _ = strconv.Atoi(strings.TrimSpace(height))
		case strings.HasPrefix(line, "palette:"):
			entries := strings.Split(strings.TrimSpace(strings.TrimPrefix(line, "palette:")), ",")
			for i, entry := range entries {
				if i >= len(palette) {
					break
				}
				value, err := strconv.ParseUint(strings.TrimSpace(entry), 16, 32)
				if err != nil {
					return canvas, palette, fmt.Errorf("%w: palette entry %q: %v", ErrInvalidVobSub, entry, err)
				}
				palette[i] = color.RGBA{
					R: uint8(value >> 16),
					G: uint8(value >> 8),
					B: uint8(value),
					A: 0xff,
				}
			}
			found = true
		}
	}
	if !found {
		return canvas, palette, fmt.Errorf("%w: the container carries no palette", ErrInvalidVobSub)
	}
	return canvas, palette, nil
}

// ParseVobSub decodes the packets of a VobSub track into its cues, in
// presentation order, given the palette text the container carried beside them.
// Each cue's image is cropped to the rectangle the packet drew, like the PGS
// decoder's, so what OCR reads contains the subtitle and not the picture
// around it.
//
// The input is either a Matroska file holding the track (which carries its own
// codec private, so paletteText is ignored) or the extractor's framed packet
// stream, for a source whose container is not Matroska.
func ParseVobSub(r io.Reader, paletteText string) ([]ImageCue, error) {
	buffered := bufio.NewReader(r)
	if header, err := buffered.Peek(len(matroskaHeader)); err == nil && bytes.Equal(header, matroskaHeader) {
		track, cues, err := demuxMatroskaVobSub(buffered)
		if err != nil {
			return nil, err
		}
		return decodeVobSub(vobsubStream{paletteText: string(track.codecPriv), cues: cues})
	}
	stream, err := readVobSubPackets(buffered)
	if err != nil {
		return nil, err
	}
	stream.paletteText = paletteText
	return decodeVobSub(stream)
}

// readVobSubPackets reads the "size: <n>\n" framed packets the extractor
// writes. The framing is the extractor's, not the format's: it is the only way
// to know where one SPU ends and the next begins once ffmpeg has demuxed them,
// because a packet's own length field is not the packet's length.
func readVobSubPackets(r io.Reader) (vobsubStream, error) {
	var stream vobsubStream
	for {
		var sizeBytes [2]byte
		if _, err := io.ReadFull(r, sizeBytes[:]); err != nil {
			if errors.Is(err, io.EOF) {
				return stream, nil
			}
			return vobsubStream{}, fmt.Errorf("%w: reading a packet length: %v", ErrInvalidVobSub, err)
		}
		size := int(binary.BigEndian.Uint16(sizeBytes[:]))
		if size == 0 {
			return vobsubStream{}, fmt.Errorf("%w: a packet claims to be empty", ErrInvalidVobSub)
		}
		spu := make([]byte, size)
		if _, err := io.ReadFull(r, spu); err != nil {
			return vobsubStream{}, fmt.Errorf("%w: a %d-byte packet ended early: %v", ErrInvalidVobSub, size, err)
		}
		stream.cues = append(stream.cues, vobsubCue{spu: spu})
	}
}

// vobsubColors is one control sequence's palette selection: which of the
// container's sixteen colours each bitmap index uses, and the transparency of
// each. The arrays are 256 long because the HD form of the rectangle command
// selects an eight-bit palette, whose indexes range far past a DVD's four.
type vobsubColors struct {
	colormap [256]byte
	alpha    [256]byte
	eightBit bool
}

// vobsubBox is the rectangle a packet draws into the frame.
type vobsubBox struct {
	x1, y1 int
	x2, y2 int
}

func (b vobsubBox) width() int  { return b.x2 - b.x1 + 1 }
func (b vobsubBox) height() int { return b.y2 - b.y1 + 1 }

// vobsubDefaultCue is how long a cue is taken to last when nothing in the
// stream says when it ends.
//
// A Matroska remux of a VobSub track is the case that needs it: the container
// gives one packet a duration and the next packet a timestamp, and a track
// whose subtitle is never cleared -- the ordinary shape, because a DVD clears
// the screen with a separate erase packet that a single-cue sample may not
// carry -- leaves the last cue open. Ending it at its own start would make the
// cue empty, and an empty cue is indistinguishable from one that was never
// drawn, so OCR would drop it. Three seconds is a placeholder that keeps the
// cue servable; a real rip carries the erase packet and never reaches it.
const vobsubDefaultCue = 3 * time.Second

// decodeVobSub turns the packet stream into cues. A packet whose bitmap has
// nothing opaque in it closes the cue that is on screen rather than replacing
// it; a stream whose last cue is never cleared still has a cue, which ends
// where the stream does.
func decodeVobSub(stream vobsubStream) ([]ImageCue, error) {
	_, palette, err := parseVobSubPalette(stream.paletteText)
	if err != nil {
		return nil, err
	}

	var (
		cues []ImageCue
		open *ImageCue
	)
	last := time.Duration(0)

	for _, packet := range stream.cues {
		last = packet.start
		img, stopAfter, err := decodeSPU(packet.spu, palette)
		if err != nil {
			return nil, err
		}

		// The packet's own stop-display command is the authority on when this
		// subpicture ends. The next packet's start is only the fallback, and it
		// is a poor one: it stretched a 300 ms cue to the gap before the next
		// one, and on a sparse track that is seconds (L-11).
		end := time.Duration(0)
		if stopAfter > 0 {
			end = packet.start + stopAfter
		}

		if img == nil {
			if open != nil {
				open.End = pickCueEnd(open.Start, end, packet.start)
				cues = append(cues, *open)
				open = nil
			}
			continue
		}
		if open != nil {
			open.End = pickCueEnd(open.Start, end, packet.start)
			cues = append(cues, *open)
		}
		open = &ImageCue{Start: packet.start, Image: img}
	}

	if open != nil {
		open.End = last
		if open.End <= open.Start {
			open.End = open.Start + vobsubDefaultCue
		}
		cues = append(cues, *open)
	}
	return cues, nil
}

// pickCueEnd chooses a cue's end time from the packet's own stop-display time
// and the start of whatever came next.
//
// The stop command wins when it is present and plausible: an end at or before
// the cue's start is not a duration the packet can have meant, and assuming it
// did would produce a cue that never displays. The next packet's start is the
// fallback for a stream with no stop blocks at all.
func pickCueEnd(start, stopAfter, nextStart time.Duration) time.Duration {
	if stopAfter > start {
		return stopAfter
	}
	if nextStart > start {
		return nextStart
	}
	return start + vobsubDefaultCue
}

// decodeSPU decodes one SPU packet into the rectangle it draws, or nil when the
// packet is empty (the control sequence that takes a subtitle off screen).
//
// It also reports when the packet's own stop-display command says the picture
// ends, as a duration from the packet's start. Zero means no block carried one,
// and the caller falls back to the next packet's start - which is what every
// cue used to do, and why a cue authored as 300 ms came back as seconds long
// (L-11 of the 2026-10-09 review).
func decodeSPU(spu []byte, palette vobsubPalette) (*image.RGBA, time.Duration, error) {
	colors, box, offsets, stopAfter, err := parseSPUControl(spu)
	if err != nil {
		return nil, 0, err
	}
	if box == nil || offsets == nil {
		return nil, 0, nil
	}
	width, height := box.width(), box.height()
	if width <= 0 || height <= 1 {
		return nil, 0, nil
	}

	plane, err := decodeVobSubRLE(spu, *offsets, width, height, colors.eightBit)
	if err != nil {
		return nil, 0, err
	}
	if !hasInk(plane, colors.alpha) {
		return nil, 0, nil
	}

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			index := plane[y*width+x]
			tint := palette[colors.colormap[index]]
			// The subpicture's alpha nibble is opacity before it is scaled:
			// zero is fully transparent and fifteen fully opaque, which is the
			// opposite sense from an RGBA image. An alpha of zero means the
			// pixel is not drawn at all, which is what the plane's zero index
			// carries in this encoding.
			tint.A = uint8(int(colors.alpha[index]) * 17)
			img.SetRGBA(x, y, tint)
		}
	}
	return img, stopAfter, nil
}

// hasInk reports whether any pixel of the plane is drawn at all. ffmpeg crops a
// packet with nothing to show away instead of handing on an empty rectangle,
// which is how a clear packet that still carries a rectangle behaves. A colour
// the control sequence left fully transparent is not ink, however much of the
// plane it covers.
func hasInk(plane []byte, alpha [256]byte) bool {
	for _, index := range plane {
		if alpha[index] != 0 {
			return true
		}
	}
	return false
}

// spuOffsets is the pair of byte offsets the control sequence gives for the
// two halves of the bitmap.
type spuOffsets struct {
	first  int
	second int
}

// spuTick is the duration of one SPU timestamp unit.
//
// A control block's date is in units of 1024/90000 of a second - the familiar
// 90 kHz clock divided by 1024 - which the format documents as the conversion
// (date << 10) / 90, in milliseconds. That division is written out rather than
// folded into a constant, because folding it in truncates: 1024/90000 does not
// divide evenly and the rounding showed up as a millisecond of drift over a few
// seconds, which a test comparing against the documented formula caught.
const spuTicksPerMS = 90

// spuDateToDuration converts a control block's date into a duration.
func spuDateToDuration(date int) time.Duration {
	return time.Duration(date) * 1024 * time.Millisecond / spuTicksPerMS
}

// parseSPUControl reads a packet's control sequence: the palette selection, the
// rectangle, the bitmap's two offsets, and - when a block carries one - the time
// its stop-display command names. A packet whose sequence stops before giving an
// offset or a rectangle draws nothing.
func parseSPUControl(spu []byte) (vobsubColors, *vobsubBox, *spuOffsets, time.Duration, error) {
	var (
		colors  vobsubColors
		box     *vobsubBox
		offsets *spuOffsets
		// stopDisplay is when the subpicture's own stop-display command says it
		// ends, and hasStop says whether a block actually carried one.
		stopDisplay time.Duration
		hasStop     bool
	)
	if len(spu) < 10 {
		return colors, nil, nil, 0, fmt.Errorf("%w: a %d-byte packet is too short", ErrInvalidVobSub, len(spu))
	}

	// An HD subpicture uses four-byte offsets; a DVD one uses two. Neither
	// appears in the fixture, but the branch is what the format's own first
	// field specifies, so it is read rather than assumed away.
	offsetSize, controlAt := 2, 2
	if binary.BigEndian.Uint16(spu[0:2]) == 0 {
		offsetSize, controlAt = 4, 6
	}
	controlPos := int(readSPUOffset(spu[controlAt:], offsetSize))
	if controlPos < 0 || controlPos+2+offsetSize > len(spu) {
		return colors, nil, nil, 0, fmt.Errorf("%w: control sequence at %d does not fit in a %d-byte packet",
			ErrInvalidVobSub, controlPos, len(spu))
	}

	// The control position points at the first block's header: a date, then the
	// offset of the next block. The commands follow the header. A DVD subpicture
	// normally carries two blocks - the first drawing the picture, the second
	// carrying the stop-display command - and the second block's date is when
	// the picture goes away.
	//
	// The blocks used to be abandoned at the first 0xff, which is how the first
	// block's command list ends, so the stop time was never read and every cue
	// ended at the next packet's start instead (L-11 of the 2026-10-09 review).
	// That is why a cue authored as 300 ms came back as 1.6 s to 6.5 s: the end
	// came from the next packet rather than from the packet's own stop command.
	//
	// So the walk follows the next-block pointer and records the date of any
	// block that carries 0x02 (stop display).
	for pos := controlPos; pos >= 0 && pos+2+offsetSize <= len(spu); {
		date := int(readSPUOffset(spu[pos:], 2))
		next := int(readSPUOffset(spu[pos+2:], offsetSize))

		cmds := pos + 2 + offsetSize
		for cmds < len(spu) {
			cmd := spu[cmds]
			cmds++

			if cmd == 0xff {
				break
			}

			need := spuCommandArguments(cmd)
			if need < 0 {
				// An unknown command means the rest of this block cannot be
				// framed, so it is dropped rather than guessed at.
				break
			}
			if cmds+need > len(spu) {
				return colors, nil, nil, 0, fmt.Errorf("%w: command %#02x at %d is cut short", ErrInvalidVobSub, cmd, cmds-1)
			}

			switch cmd {
			case 0x03: // set colour
				colors.colormap[3] = spu[cmds] >> 4
				colors.colormap[2] = spu[cmds] & 0x0f
				colors.colormap[1] = spu[cmds+1] >> 4
				colors.colormap[0] = spu[cmds+1] & 0x0f
			case 0x04: // set transparency
				colors.alpha[3] = spu[cmds] >> 4
				colors.alpha[2] = spu[cmds] & 0x0f
				colors.alpha[1] = spu[cmds+1] >> 4
				colors.alpha[0] = spu[cmds+1] & 0x0f
			case 0x05, 0x85: // set the display rectangle
				box = &vobsubBox{
					x1: int(spu[cmds])<<4 | int(spu[cmds+1])>>4,
					x2: int(spu[cmds+1]&0x0f)<<8 | int(spu[cmds+2]),
					y1: int(spu[cmds+3])<<4 | int(spu[cmds+4])>>4,
					y2: int(spu[cmds+4]&0x0f)<<8 | int(spu[cmds+5]),
				}
				colors.eightBit = colors.eightBit || cmd&0x80 != 0
			case 0x06: // the bitmap's two offsets
				offsets = &spuOffsets{
					first:  int(binary.BigEndian.Uint16(spu[cmds:])),
					second: int(binary.BigEndian.Uint16(spu[cmds+2:])),
				}
			case 0x86: // the same, in the HD form
				offsets = &spuOffsets{
					first:  int(binary.BigEndian.Uint32(spu[cmds:])),
					second: int(binary.BigEndian.Uint32(spu[cmds+4:])),
				}
			case 0x02: // stop display
				// The date is this block's, and it is when the picture ends.
				stopDisplay = spuDateToDuration(date)
				hasStop = true
			}
			cmds += need
		}

		if next <= 0 || pos+next == pos {
			// No further block, or a pointer that does not advance, which would
			// otherwise spin.
			break
		}
		pos += next
	}

	if box == nil || offsets == nil {
		return colors, nil, nil, stopDisplay, nil
	}
	// The offsets point into the packet, past the four-byte header and in order.
	// A packet that fails this carries no bitmap to draw, which is what an
	// erase or an unsupported block looks like, so it decodes to nothing rather
	// than to an error.
	if width := box.width(); width <= 0 || box.height() <= 1 {
		return colors, nil, nil, stopDisplay, nil
	}
	if offsets.first < 4 || offsets.second < offsets.first || offsets.first >= len(spu) {
		return colors, nil, nil, stopDisplay, nil
	}
	if !hasStop {
		// No block named a stop time, so the caller has only the next packet's
		// start to go on.
		stopDisplay = 0
	}
	return colors, box, offsets, stopDisplay, nil
}

// spuCommandArguments reports how many argument bytes a control command takes,
// or -1 for a command this decoder does not know.
func spuCommandArguments(cmd byte) int {
	switch cmd {
	case 0x00, 0x01, 0x02: // menu, start, end
		return 0
	case 0x03, 0x04: // colour, transparency
		return 2
	case 0x05, 0x85: // display rectangle
		return 6
	case 0x06: // bitmap offsets
		return 4
	case 0x86: // bitmap offsets, HD
		return 8
	case 0x83: // HD palette
		return 768
	case 0x84: // HD contrast
		return 256
	default:
		return -1
	}
}

func readSPUOffset(data []byte, size int) uint32 {
	if size == 4 {
		return binary.BigEndian.Uint32(data)
	}
	return uint32(binary.BigEndian.Uint16(data))
}

// decodeVobSubRLE expands a packet's run-length encoding into a plane of
// palette indexes, width*height bytes, row-major.
//
// The encoding uses two bits per pixel for a DVD subpicture, split into two
// interleaved fields: the first field's runs carry the even lines and the
// second's the odd lines, each starting at the offset the control sequence
// gave. A run of one or two pixels that carries a colour is written as a single
// byte whose top two bits are zero and whose low two bits choose a colour; any
// larger run is a nibble count followed by a colour nibble. Every line is
// byte-aligned when it ends.
func decodeVobSubRLE(spu []byte, offsets spuOffsets, width, height int, eightBit bool) ([]byte, error) {
	plane := make([]byte, width*height)
	for field, offset := range [2]int{offsets.first, offsets.second} {
		rows := (height + 1) / 2
		if field == 1 {
			rows = height / 2
		}
		lines, err := decodeVobSubField(spu, offset, width, rows, eightBit)
		if err != nil {
			return nil, err
		}
		for line, indexes := range lines {
			copy(plane[(field+2*line)*width:], indexes)
		}
	}
	return plane, nil
}

// decodeVobSubField decodes one field: its rows, each a row of palette indexes.
// The bits are read from the field's own offset, and each row is byte-aligned
// when it ends. Splitting this out of the interleave is what lets a test hold
// one field against the other.
func decodeVobSubField(data []byte, offset, width, rows int, eightBit bool) ([][]byte, error) {
	if rows <= 0 {
		return nil, nil
	}
	out := make([][]byte, 0, rows)
	bits := newSPUBits(data, offset)
	for row := 0; row < rows; {
		line := make([]byte, 0, width)
		for x := 0; x < width; {
			length, index, err := bits.run(eightBit, width-x)
			if err != nil {
				return nil, err
			}
			for i := 0; i < length; i++ {
				line = append(line, index)
			}
			x += length
		}
		bits.align()
		out = append(out, line)
		row++
	}
	return out, nil
}

// spuBits reads the run-length encoding bit by bit. VobSub's runs are not
// byte-aligned, so the decoder cannot work a byte at a time.
type spuBits struct {
	data []byte
	bit  int
}

func newSPUBits(data []byte, start int) *spuBits {
	return &spuBits{data: data, bit: start * 8}
}

// bitsRemaining reports how many bits are left before the packet ends.
func (b *spuBits) bitsRemaining() int { return len(b.data)*8 - b.bit }

// get reads n bits, most significant first. It returns 0 once the data runs
// out; callers that must not read past the end check bitsRemaining first.
func (b *spuBits) get(n int) int {
	value := 0
	for i := 0; i < n; i++ {
		byteIndex := b.bit >> 3
		if byteIndex >= len(b.data) {
			b.bit++
			continue
		}
		value = value<<1 | int(b.data[byteIndex]>>(7-uint(b.bit&7)))&1
		b.bit++
	}
	return value
}

// align moves to the next byte boundary, which ends every encoded line.
func (b *spuBits) align() { b.bit = (b.bit + 7) &^ 7 }

// run reads one run: its length and the palette index it paints. A run that
// reaches the end of the packet, or that would run past the line, is clamped to
// what is left rather than refused, because the last line of a packet is
// sometimes padded.
func (b *spuBits) run(eightBit bool, remaining int) (int, byte, error) {
	if b.bitsRemaining() <= 0 {
		return 0, 0, fmt.Errorf("%w: the bitmap ended before every line was drawn", ErrInvalidVobSub)
	}
	length, index := b.rawRun(eightBit)
	if length > remaining {
		length = remaining
	}
	return length, index, nil
}

// rawRun reads one run without clamping it.
func (b *spuBits) rawRun(eightBit bool) (int, byte) {
	if eightBit {
		return b.run8()
	}
	return b.run2()
}

// run2 reads a two-bit-colour run. A value below four is not a run at all: it
// is the code for "the rest of this line is transparent", and the three-colour
// fixture never produces one.
func (b *spuBits) run2() (int, byte) {
	value, threshold := 0, 1
	for value < threshold && threshold <= 0x40 && b.bitsRemaining() > 0 {
		value = value<<4 | b.get(4)
		threshold <<= 2
	}
	index := byte(value & 3)
	if value < 4 {
		return 1 << 30, index
	}
	return value >> 2, index
}

// run8 reads the eight-bit form a subpicture selects with the high bit of its
// rectangle command, where a colour can be any of 256.
func (b *spuBits) run8() (int, byte) {
	hasRun := b.get(1)
	index := byte(b.get(2 + 6*b.get(1)))
	if hasRun == 0 {
		return 1, index
	}
	if b.get(1) != 0 {
		length := b.get(7)
		if length == 0 {
			return 1 << 30, index
		}
		return length + 9, index
	}
	return b.get(3) + 2, index
}
