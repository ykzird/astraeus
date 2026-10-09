package subtitles

import (
	"testing"
	"time"
)

// spuControlPacket builds the smallest packet that carries a control sequence,
// so the block walk can be tested on its own.
//
// Layout: the four-byte header (size, control offset), the control blocks, then
// a byte of bitmap for the offsets to point at.
func spuControlPacket(firstBlock, secondBlock []byte) []byte {
	body := append(append([]byte{}, firstBlock...), secondBlock...)
	body = append(body, 0xAA) // one byte of "bitmap" at offset 8

	packet := []byte{
		0x00, byte(4 + len(body)), // the declared size
		0x00, 0x04, // the control offset
	}
	return append(packet, body...)
}

// TestParseSPUControl_ReadsTheStopDisplayTime is the regression test for L-11.
//
// A DVD subpicture carries two control blocks: the first draws the picture, the
// second carries the stop-display command and the date it takes effect. The
// reader used to stop at the first block's 0xff terminator, so the stop time was
// never read and every cue ended at the next packet's start - which is how a cue
// authored as 300 ms came back spanning seconds.
func TestParseSPUControl_ReadsTheStopDisplayTime(t *testing.T) {
	t.Parallel()

	// Block 1, at offset 4, draws: colour, alpha, rectangle, offsets, end. It is
	// 23 bytes long, and the next-block pointer is that length - the pointer is
	// relative to this block, so a pointer into the middle of a command parses
	// to no stop time rather than to an error.
	first := []byte{
		0x00, 0x00, // date 0
		0x00, 0x17, // the next block starts 23 bytes on, at offset 27
		0x03, 0x00, 0x00,
		0x04, 0x00, 0x00,
		0x05, 0x00, 0x00, 0x0f, 0x00, 0x00, 0x0f,
		0x06, 0x00, 0x08, 0x00, 0x09,
		0xff,
	}
	// Block 2, at offset 12, only stops the display. A date of 0x0100 is
	// 256 ticks, and a tick is 1024/90000 s, so about 2.9 seconds.
	stopTicks := 0x0100
	second := []byte{
		byte(stopTicks >> 8), byte(stopTicks & 0xFF), // the date
		0x00, 0x00, // no next block
		0x02, // stop display
		0xff,
	}

	colors, box, offsets, stopAfter, err := parseSPUControl(spuControlPacket(first, second))
	if err != nil {
		t.Fatalf("parseSPUControl: %v", err)
	}
	_ = colors
	if box == nil {
		t.Fatal("the drawing block gave no rectangle, so the packet decodes to nothing")
	}
	if offsets == nil {
		t.Fatal("the drawing block gave no offsets, so the packet decodes to nothing")
	}

	// The conversion the format documents is (date << 10) / 90, in ms.
	want := time.Duration(stopTicks) * 1024 * time.Millisecond / 90
	if stopAfter != want {
		t.Errorf("stop-display time = %v, want %v: the second block's date is when the "+
			"picture ends", stopAfter, want)
	}
}

// TestParseSPUControl_WithoutAStopBlock covers the fallback: a packet whose
// blocks never stop the display reports no stop time, so the caller can use the
// next packet's start instead of inventing one.
func TestParseSPUControl_WithoutAStopBlock(t *testing.T) {
	t.Parallel()

	only := []byte{
		0x00, 0x00, // date 0
		0x00, 0x00, // no next block
		0x03, 0x00, 0x00,
		0x04, 0x00, 0x00,
		0x05, 0x00, 0x00, 0x0f, 0x00, 0x00, 0x0f,
		0x06, 0x00, 0x08, 0x00, 0x09,
		0xff,
	}

	_, box, offsets, stopAfter, err := parseSPUControl(spuControlPacket(only, nil))
	if err != nil {
		t.Fatalf("parseSPUControl: %v", err)
	}
	if box == nil || offsets == nil {
		t.Fatal("the packet should still decode to a picture")
	}
	if stopAfter != 0 {
		t.Errorf("stop-display time = %v, want 0 when no block stops the display", stopAfter)
	}
}

// TestPickCueEnd pins the precedence: the packet's own stop command wins over the
// next packet's start, with the next start as the fallback.
func TestPickCueEnd(t *testing.T) {
	t.Parallel()

	const start = 5 * time.Second
	tests := []struct {
		name      string
		stopAfter time.Duration
		nextStart time.Duration
		want      time.Duration
	}{
		{
			// The L-11 case: a 300 ms cue followed much later by the next one.
			name:      "the stop command wins",
			stopAfter: start + 300*time.Millisecond,
			nextStart: 20 * time.Second,
			want:      start + 300*time.Millisecond,
		},
		{
			name:      "no stop command falls back to the next start",
			nextStart: 9 * time.Second,
			want:      9 * time.Second,
		},
		{
			name: "nothing known gets the default duration",
			want: start + vobsubDefaultCue,
		},
		{
			// A stop time at or before the start is not a duration the packet
			// can have meant; using it would produce a cue that never displays.
			name:      "an impossible stop time is ignored",
			stopAfter: start - time.Second,
			nextStart: 9 * time.Second,
			want:      9 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := pickCueEnd(start, tt.stopAfter, tt.nextStart); got != tt.want {
				t.Errorf("pickCueEnd(%v, %v, %v) = %v, want %v",
					start, tt.stopAfter, tt.nextStart, got, tt.want)
			}
		})
	}
}
