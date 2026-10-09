package subtitles

import (
	"bytes"
	"runtime"
	"testing"
)

// The parsers read library files, which are usually downloads, so their input is
// not trusted. These targets exist because the review's fuzzing found the
// Matroska reader panicking in seconds on a twelve-byte input, and because the
// PGS reader's 17 GB allocation (L-3) was reachable from a 125-byte stream. Both
// were limits questions rather than logic questions, so every target also
// asserts how much the parse allocated: a parser that survives by asking for
// gigabytes is not surviving.
//
// Run one for a while with:
//
//	go test -run '^$' -fuzz FuzzParsePGS -fuzztime 60s ./internal/subtitles/

// fuzzAllocationLimit is the most a single parse may allocate. An 8K PGS plane
// is 33 megapixels and a decoded cue holds four bytes per pixel, so a legitimate
// worst case is a few hundred megabytes - but that is a *whole* 8K subtitle, and
// no real one is one solid plane. The limit is set above every legitimate case
// the corpus contains and far below the failures: the PGS bomb was 17 GB and the
// Matroska makeslice was unbounded.
const fuzzAllocationLimit = 512 << 20 // 512 MiB

// checkFuzzAllocation reports how much one parse allocated, and fails the target
// when that is over the limit.
//
// TotalAlloc is used rather than HeapAlloc because it counts every byte ever
// allocated, including memory already freed - a parser that allocates 17 GB and
// then frees it is exactly the case this has to catch, and a heap reading after
// the fact would miss it.
//
// The failure is t.Errorf rather than t.Fatalf on purpose. The fuzzing engine
// treats a *failing* input as one worth keeping, and an input that fails only on
// this assertion is a large allocation by definition, so t.Fatalf fills the
// corpus with the very bytes the assertion is about and eventually overruns the
// engine's own shared-memory capacity for the corpus ("value length 818663451
// larger than shared memory capacity"). Recording the number instead keeps the
// signal visible without the corpus learning the wrong lesson.
func checkFuzzAllocation(t *testing.T, allocationBytes int64) {
	t.Helper()

	if allocationBytes > fuzzAllocationLimit {
		t.Errorf("one parse allocated %d bytes, over the %d-byte limit - the input "+
			"is a resource-exhaustion attempt, not a stream",
			allocationBytes, fuzzAllocationLimit)
	}
}

// fuzzInputLimit bounds the input a target will try.
//
// The PGS bomb the review found is 125 bytes and the Matroska panic is twelve, so
// there is nothing to learn from megabyte inputs - but the engine will generate
// them, and a generated input whose value grows past its own shared-memory
// capacity aborts the whole run with "value length ... larger than shared memory
// capacity", which says nothing about the parser. Refusing them keeps a long run
// running.
const fuzzInputLimit = 1 << 20 // 1 MiB

// measureAllocation runs fn and reports how many bytes it allocated.
func measureAllocation(fn func()) int64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return int64(after.TotalAlloc - before.TotalAlloc)
}

// pgsSeeds are the streams the corpus starts from: a well-formed one from the
// project's own fixture, and the shapes the review found.
func pgsSeeds() [][]byte {
	seeds := [][]byte{
		// A single small cue, which must parse without allocating much.
		pgsFixtureStream(640, 360),
		// Two 1x1 objects as far apart as the format allows: the 17 GB bounding
		// box of L-3.
		func() []byte {
			var out []byte
			out = append(out, limitSegment(0x16, 0, pgsComposition(1920, 1080, [][2]int{{0, 0}, {65535, 65535}}))...)
			return append(out, limitSegment(0x15, 0, pgsObjectSegment(1, 1))...)
		}(),
		// A declared-object geometry over the side limit, which exercises the
		// refusal path without making the harness carry a megabyte-sized seed:
		// the plane is never decoded, because the geometry is refused first.
		func() []byte {
			var out []byte
			out = append(out, limitSegment(0x16, 0, pgsComposition(1920, 1080, [][2]int{{0, 0}}))...)
			return append(out, limitSegment(0x15, 0, pgsObjectSegment(8193, 4))...)
		}(),
		// A truncated segment header, which is what a fuzzer will reach first.
		[]byte("PG\x00\x00"),
	}
	return seeds
}

// pgsFixtureStream builds one legal display set with a single small object, from
// the same builders the limit tests use, so the corpus starts from bytes the
// parser is meant to accept.
func pgsFixtureStream(videoWidth, videoHeight int) []byte {
	var out []byte
	out = append(out, limitSegment(0x16, 0, pgsComposition(videoWidth, videoHeight, [][2]int{{40, 40}}))...)
	return append(out, limitSegment(0x15, 0, pgsObjectSegment(16, 8))...)
}

// FuzzParsePGS drives the presentation graphic stream parser.
func FuzzParsePGS(f *testing.F) {
	for _, seed := range pgsSeeds() {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > fuzzInputLimit {
			t.Skip()
		}
		allocationBytes := measureAllocation(func() {
			_, _ = ParsePGS(bytes.NewReader(data))
		})
		checkFuzzAllocation(t, allocationBytes)
		t.Logf("len=%d allocated=%d", len(data), allocationBytes)
	})
}

// FuzzParseVobSub drives the SPU parser, which is reached from both the .sub
// container and a Matroska track.
func FuzzParseVobSub(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("this is not a vobsub stream"))
	// A packet header with a control offset pointing past the end.
	f.Add([]byte{0x00, 0xff, 0x00, 0xff, 0x03, 0x00, 0x00, 0xff})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > fuzzInputLimit {
			t.Skip()
		}
		allocationBytes := measureAllocation(func() {
			_, _ = ParseVobSub(bytes.NewReader(data), "")
		})
		checkFuzzAllocation(t, allocationBytes)
		t.Logf("len=%d allocated=%d", len(data), allocationBytes)
	})
}

// FuzzDemuxMatroskaVobSub drives the EBML reader directly.
//
// This is the target the review's fuzzing needed: a twelve-byte input reached
// make([]byte, elementSize) with an unchecked 56-bit size and panicked with
// "makeslice: len out of range", and slightly smaller sizes produced a fatal
// out-of-memory instead (L-16, and the allocation half of L-3). ParseVobSub does
// not reach this code path for arbitrary bytes, so it needs its own target.
func FuzzDemuxMatroskaVobSub(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0x1A, 0x45, 0xDF, 0xA3}) // the EBML header id, with nothing after it
	// A segment with a declared size far larger than the input.
	f.Add([]byte{0x18, 0x53, 0x80, 0x67, 0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > fuzzInputLimit {
			t.Skip()
		}
		allocationBytes := measureAllocation(func() {
			_, _, _ = demuxMatroskaVobSub(bytes.NewReader(data))
		})
		checkFuzzAllocation(t, allocationBytes)
		t.Logf("len=%d allocated=%d", len(data), allocationBytes)
	})
}

// FuzzDecodeVobSubRLE drives the bitmap decoder, which indexes a plane by its own
// declared geometry.
func FuzzDecodeVobSubRLE(f *testing.F) {
	f.Add([]byte{0x00, 0x04, 0x01, 0x02, 0x03, 0x04})
	f.Add([]byte{0x00, 0x03, 0x00, 0x01, 0x02})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > fuzzInputLimit {
			t.Skip()
		}
		allocationBytes := measureAllocation(func() {
			// The geometry a real caller derives comes from the display
			// rectangle, so bound it the same way rather than fuzzing an
			// unbounded size.
			_, _ = decodeVobSubRLE(data, spuOffsets{first: 0, second: 0}, 16, 16, false)
		})
		checkFuzzAllocation(t, allocationBytes)
		t.Logf("len=%d allocated=%d", len(data), allocationBytes)
	})
}
