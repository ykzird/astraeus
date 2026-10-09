package subtitles

import (
	"encoding/binary"
	"os"
	"testing"
)

// dvbFixture is the committed DVB sample: a real dvb_subtitle track made by
// ffmpeg's dvbsub encoder from a PGS fixture this project draws at the encoder's
// own 720x576 authoring canvas. scripts/make-dvb-fixture.sh regenerates it and
// refuses to leave one behind unless ffmpeg can decode the caption back out, so
// the picture's contents are vouched for by that script rather than here.
const dvbFixture = "testdata/dvb-caption.mkv"

// DVB subtitle segment types, from ETSI EN 300 743. The decoder that will read
// these does not exist yet; they are named here so the fixture's framing test
// can check for the segments a display set needs.
const (
	dvbSegmentPage            = 0x10
	dvbSegmentRegion          = 0x11
	dvbSegmentCLUT            = 0x12
	dvbSegmentObject          = 0x13
	dvbSegmentDisplay         = 0x14
	dvbSegmentEndOfDisplaySet = 0x80
	dvbSegmentSync            = 0x0F
)

// readDVBSubtitlePackets pulls the subtitle packets out of the fixture without
// ffmpeg, using the EBML walker this package already has for the VobSub
// fixture. ffmpeg writes the simple block form here, wrapped in a BlockGroup.
func readDVBSubtitlePackets(t *testing.T) [][]byte {
	t.Helper()

	file, err := os.Open(dvbFixture)
	if err != nil {
		t.Fatalf("opening the fixture: %v", err)
	}
	defer file.Close()

	elements, err := readMatroskaTree(file, -1)
	if err != nil {
		t.Fatalf("reading the fixture's Matroska tree: %v", err)
	}
	segment := (&matroskaElement{children: elements}).child(matroskaSegment)
	if segment == nil {
		t.Fatal("the fixture is not Matroska")
	}

	var number uint64
	for _, entry := range segment.child(matroskaTracks).childrenWith(matroskaTrackEntry) {
		if codec := entry.child(matroskaCodecID); codec == nil || string(codec.body) != "S_DVBSUB" {
			continue
		}
		if track := entry.child(matroskaTrackNum); track != nil {
			number = decodeUint(track.body)
		}
	}

	var packets [][]byte
	for _, cluster := range segment.childrenWith(matroskaCluster) {
		for _, element := range cluster.children {
			block := element
			if element.id == matroskaBlockGroup {
				block = element.child(matroskaBlock)
			}
			if block == nil || block.id != matroskaBlock {
				continue
			}
			track, _, payload, err := readMatroskaBlock(block.body, block.id)
			if err != nil {
				t.Fatalf("reading a block: %v", err)
			}
			if track == number {
				packets = append(packets, payload)
			}
		}
	}
	if len(packets) == 0 {
		t.Fatal("the fixture carries no DVB subtitle packets")
	}
	return packets
}

// TestDVBFixture_IsAFramedDisplaySet is the fixture's always-run check, with no
// ffmpeg and no decoder: every packet must be a run of segments whose declared
// lengths account for it exactly, ending on an end-of-display-set marker.
//
// The header order is the trap this pins. A segment is the sync byte, its type,
// a two-byte *segment id* and then a two-byte length -- the id first, which is
// the reverse of the VobSub framing and the one field easy to read backwards.
// A wrong guess there does not fail loudly; it silently walks into the picture
// data and reports nonsense, which is worse.
func TestDVBFixture_IsAFramedDisplaySet(t *testing.T) {
	t.Parallel()

	seen := map[byte]int{}
	for packetNumber, packet := range readDVBSubtitlePackets(t) {
		offset := 0
		for offset < len(packet) {
			if offset+6 > len(packet) {
				t.Fatalf("packet %d has %d trailing bytes, too few for a segment header",
					packetNumber, len(packet)-offset)
			}
			if packet[offset] != dvbSegmentSync {
				t.Fatalf("packet %d has %#02x at %d where a segment must start with the sync byte",
					packetNumber, packet[offset], offset)
			}
			kind := packet[offset+1]
			length := int(binary.BigEndian.Uint16(packet[offset+4 : offset+6]))
			end := offset + 6 + length
			if end > len(packet) {
				t.Fatalf("packet %d: a %#02x segment claims %d bytes but only %d remain",
					packetNumber, kind, length, len(packet)-(offset+6))
			}
			seen[kind]++
			offset = end
		}
		if offset != len(packet) {
			t.Fatalf("packet %d: the segments account for %d of %d bytes", packetNumber, offset, len(packet))
		}
	}

	// A display set that draws a subtitle needs all four composition segments;
	// a decoder that finds one missing cannot be blamed on the decoder.
	for _, kind := range []byte{dvbSegmentDisplay, dvbSegmentPage, dvbSegmentRegion, dvbSegmentCLUT, dvbSegmentObject} {
		if seen[kind] == 0 {
			t.Errorf("the fixture has no %#02x segment", kind)
		}
	}
	if seen[dvbSegmentEndOfDisplaySet] == 0 {
		t.Error("the fixture has no end-of-display-set segment")
	}
}

// TestDVBFixture_CarriesNoContainerPalette pins the difference from VobSub that
// shapes the extraction: a DVB track's colour table is in the stream, so the
// container carries no codec private for a decoder to read a palette from.
func TestDVBFixture_CarriesNoContainerPalette(t *testing.T) {
	t.Parallel()

	file, err := os.Open(dvbFixture)
	if err != nil {
		t.Fatalf("opening the fixture: %v", err)
	}
	defer file.Close()

	elements, err := readMatroskaTree(file, -1)
	if err != nil {
		t.Fatalf("reading the fixture's Matroska tree: %v", err)
	}
	segment := (&matroskaElement{children: elements}).child(matroskaSegment)
	for _, entry := range segment.child(matroskaTracks).childrenWith(matroskaTrackEntry) {
		codec := entry.child(matroskaCodecID)
		if codec == nil || string(codec.body) != "S_DVBSUB" {
			continue
		}
		if priv := entry.child(matroskaCodecPriv); priv != nil && len(priv.body) > 0 {
			t.Errorf("the track carries %d bytes of codec private; a DVB colour table belongs in the stream", len(priv.body))
		}
		return
	}
	t.Fatal("no DVB subtitle track in the fixture")
}
