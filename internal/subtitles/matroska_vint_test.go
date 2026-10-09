package subtitles

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// TestReadEBMLVint covers the EBML variable-length integer, which is not the
// protobuf varint (L-2 of the 2026-10-09 review).
//
// EBML marks a vint's length with the position of the first set bit, and that
// marker is part of the stored value: 0x81 is one byte meaning 1, where
// binary.Uvarint reads 129. The old code used binary.Uvarint, so a track number
// written as two bytes (0x40 0x02, meaning 2) was read as 64 in one byte and
// left the 0x02 behind to be mistaken for part of the timestamp.
func TestReadEBMLVint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       []byte
		want       uint64
		wantLength int
		wantErr    bool
	}{
		{name: "one byte, value 1", body: []byte{0x81, 0xAA, 0xAA}, want: 1, wantLength: 1},
		{name: "one byte, maximum", body: []byte{0xFE, 0xAA}, want: 126, wantLength: 1},
		{name: "two bytes, value 2", body: []byte{0x40, 0x02, 0xAA}, want: 2, wantLength: 2},
		{name: "two bytes, large value", body: []byte{0x7F, 0xFF}, want: 0x3FFF, wantLength: 2},
		{name: "three bytes", body: []byte{0x20, 0x01, 0x00}, want: 0x100, wantLength: 3},
		{name: "eight bytes", body: []byte{0x01, 0, 0, 0, 0, 0, 0, 0x05}, want: 5, wantLength: 8},
		{name: "an empty block", body: nil, wantErr: true},
		{name: "a zero first byte", body: []byte{0x00, 0x01}, wantErr: true},
		{name: "a truncated two-byte vint", body: []byte{0x40}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, length, err := readEBMLVint(tt.body)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("readEBMLVint(% x) should fail", tt.body)
				}
				return
			}
			if err != nil {
				t.Fatalf("readEBMLVint(% x): %v", tt.body, err)
			}
			if got != tt.want {
				t.Errorf("readEBMLVint(% x) = %d, want %d", tt.body, got, tt.want)
			}
			if length != tt.wantLength {
				t.Errorf("readEBMLVint(% x) consumed %d bytes, want %d",
					tt.body, length, tt.wantLength)
			}
		})
	}
}

// block builds a Matroska block body: the track number's vint, a signed
// relative timestamp, a flags byte, then the payload.
func block(trackNumber []byte, relative int16, flags byte, payload []byte) []byte {
	body := append([]byte{}, trackNumber...)
	body = binary.BigEndian.AppendUint16(body, uint16(relative))
	body = append(body, flags)
	return append(body, payload...)
}

// TestReadMatroskaBlock_AlignsThePayload covers both halves of L-2.
//
// The payload has to start after the track number, the timestamp and the flags
// byte. The old reader mis-parsed the first and skipped the third only for a
// SimpleBlock, so a Block read two bytes short and its payload began inside the
// timestamp; the two errors cancelled only while the relative timestamp stayed
// under 256 ms.
func TestReadMatroskaBlock_AlignsThePayload(t *testing.T) {
	t.Parallel()

	payload := []byte{0xDE, 0xAD, 0xBE, 0xEF}

	tests := []struct {
		name        string
		trackNumber []byte
		relative    int16
		wantNumber  uint64
		elementID   uint64
	}{
		{
			// A one-byte vint is where the two readings happen to agree, so
			// this case is the control.
			name:        "a one-byte track number in a SimpleBlock",
			trackNumber: []byte{0x81},
			relative:    300,
			wantNumber:  1,
			elementID:   matroskaSimpleBlk,
		},
		{
			// Two bytes: binary.Uvarint reads 0x40 as 64 and consumes one byte,
			// leaving 0x02 to be read as the timestamp.
			name:        "a two-byte track number in a SimpleBlock",
			trackNumber: []byte{0x40, 0x02},
			relative:    300,
			wantNumber:  2,
			elementID:   matroskaSimpleBlk,
		},
		{
			// A Block inside a BlockGroup carries the flags byte too. The old
			// reader skipped it only for a SimpleBlock.
			name:        "a Block inside a BlockGroup",
			trackNumber: []byte{0x81},
			relative:    300,
			wantNumber:  1,
			elementID:   matroskaBlock,
		},
		{
			name:        "a two-byte track number in a Block",
			trackNumber: []byte{0x40, 0x02},
			relative:    -5,
			wantNumber:  2,
			elementID:   matroskaBlock,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			body := block(tt.trackNumber, tt.relative, 0x80, payload)
			number, relative, got, err := readMatroskaBlock(body, tt.elementID)
			if err != nil {
				t.Fatalf("readMatroskaBlock: %v", err)
			}
			if number != tt.wantNumber {
				t.Errorf("track number = %d, want %d", number, tt.wantNumber)
			}
			if relative != int64(tt.relative) {
				t.Errorf("relative timestamp = %d, want %d", relative, tt.relative)
			}
			if !bytes.Equal(got, payload) {
				t.Errorf("payload = % x, want % x", got, payload)
			}
		})
	}
}
