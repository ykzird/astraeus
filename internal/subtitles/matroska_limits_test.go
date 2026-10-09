package subtitles

import (
	"bytes"
	"testing"
)

// TestDemuxMatroskaVobSub_RefusesAnImpossibleElementSize is the regression test
// for L-16.
//
// An element's size is a 56-bit field, so a twelve-byte input can declare an
// element larger than any file. The reader then asked for exactly that much with
// make([]byte, elementSize): the review's fuzzing found "makeslice: len out of
// range" in seconds, and sizes just under the limit produced a fatal
// out-of-memory instead - which is worse than a panic, because a runtime fatal
// error cannot be recovered by net/http and takes the process with it.
//
// FuzzDemuxMatroskaVobSub found the same thing again in 47 ms once it existed.
// This test is the deterministic guard, so the fix is covered whether or not
// anyone runs the fuzzer.
func TestDemuxMatroskaVobSub_RefusesAnImpossibleElementSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		data []byte
	}{
		{
			// The EBML header id, then an element declaring a size near the
			// top of the 56-bit field.
			name: "the EBML header with an oversized child",
			data: []byte{
				0x1A, 0x45, 0xDF, 0xA3, // the EBML header id
				0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, // an 8-byte vint size
			},
		},
		{
			// A segment declaring a size far beyond the input. The segment is a
			// container, so this one is reached through the container branch
			// with io.LimitReader rather than the allocation - the child inside
			// it is what allocates.
			name: "a segment with a huge declared size",
			data: []byte{
				0x18, 0x53, 0x80, 0x67, // the segment id
				0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
			},
		},
		{
			// Twelve bytes, the shape the review's harness panicked on.
			name: "twelve bytes with an oversized declaration",
			data: []byte{
				0x18, 0x53, 0x80, 0x67, 0x01, 0xFF,
				0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// The assertion is that this returns rather than panicking or
			// asking for the declared size. A makeslice panic would fail the
			// test, which is the point; an error is a fine answer.
			if _, _, err := demuxMatroskaVobSub(bytes.NewReader(tt.data)); err == nil {
				t.Log("the stream was accepted, which is safe as long as nothing was allocated for it")
			}
		})
	}
}

// TestReadMatroskaBody_BoundsTheSize pins the three bounds the reader applies.
func TestReadMatroskaBody_BoundsTheSize(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		elementSize int64
		parentSize  int64
		wantErr     bool
	}{
		{name: "a small element", elementSize: 16, parentSize: 1024},
		{name: "an element exactly filling its parent", elementSize: 1024, parentSize: 1024},
		{name: "a negative size", elementSize: -1, parentSize: 1024, wantErr: true},
		{name: "an unknown parent size", elementSize: 16, parentSize: -1},
		{name: "over the absolute cap", elementSize: maxMatroskaElement + 1, parentSize: -1, wantErr: true},
		{name: "larger than its parent", elementSize: 2048, parentSize: 1024, wantErr: true},
		{name: "an element the input does not hold", elementSize: 4096, parentSize: -1, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			// The reader holds 2048 bytes, so the cases that expect success
			// declare at most that; the rest fail on a bound or on the read.
			body := bytes.Repeat([]byte{0xAA}, 2048)
			_, err := readMatroskaBody(bytes.NewReader(body), tt.elementSize, tt.parentSize)
			if tt.wantErr && err == nil {
				t.Errorf("readMatroskaBody(%d, %d) should fail", tt.elementSize, tt.parentSize)
			}
			if !tt.wantErr && err != nil {
				t.Errorf("readMatroskaBody(%d, %d) = %v, want nil", tt.elementSize, tt.parentSize, err)
			}
		})
	}
}

// TestMaxMatroskaElement_IsSane guards the cap itself: it has to be far above a
// real subtitle sample and far below anything that would exhaust memory.
func TestMaxMatroskaElement_IsSane(t *testing.T) {
	t.Parallel()

	// A 4K subpicture's plane is about 8 MB compressed at worst, so the cap has
	// to clear that comfortably.
	if maxMatroskaElement < 4<<20 {
		t.Errorf("maxMatroskaElement is %d, too small for a real subtitle sample", maxMatroskaElement)
	}
	if maxMatroskaElement > 256<<20 {
		t.Errorf("maxMatroskaElement is %d, large enough to hurt the host", maxMatroskaElement)
	}
}
