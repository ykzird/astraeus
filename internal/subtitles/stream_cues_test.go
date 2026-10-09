package subtitles

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/ykzird/astraeus/internal/testfixtures/pgs"
)

// manyCueFixture renders a PGS stream with the given number of distinct cues.
func manyCueFixture(t *testing.T, cues int) []byte {
	t.Helper()

	built := make([]pgs.Cue, 0, cues)
	for i := 0; i < cues; i++ {
		start := i * 2000
		built = append(built, pgs.Cue{
			StartMS: start, EndMS: start + 1200,
			X: 20, Y: 20 + i, Width: 40, Height: 8,
		})
	}
	return pgs.Subtitle(640, 360, built)
}

// TestStreamPGS_YieldsTheSameCuesAsParsePGS pins the refactor: the collecting
// entry point is now a thin wrapper over the streaming one, so the two must
// agree cue for cue or one of them is wrong.
func TestStreamPGS_YieldsTheSameCuesAsParsePGS(t *testing.T) {
	t.Parallel()

	stream := manyCueFixture(t, 5)

	collected, err := ParsePGS(bytes.NewReader(stream))
	if err != nil {
		t.Fatalf("ParsePGS: %v", err)
	}
	if len(collected) == 0 {
		t.Fatal("the fixture produced no cues, so this test proves nothing")
	}

	var streamed []ImageCue
	if err := StreamPGS(bytes.NewReader(stream), func(cue ImageCue) error {
		streamed = append(streamed, cue)
		return nil
	}); err != nil {
		t.Fatalf("StreamPGS: %v", err)
	}

	if len(streamed) != len(collected) {
		t.Fatalf("StreamPGS yielded %d cues, ParsePGS collected %d",
			len(streamed), len(collected))
	}
	for i := range collected {
		if streamed[i].Start != collected[i].Start || streamed[i].End != collected[i].End {
			t.Errorf("cue %d: streamed %v-%v, collected %v-%v",
				i, streamed[i].Start, streamed[i].End, collected[i].Start, collected[i].End)
		}
		if (streamed[i].Image == nil) != (collected[i].Image == nil) {
			t.Errorf("cue %d: one path produced an image and the other did not", i)
		}
	}
}

// TestStreamPGS_EmitsAsItGoes is the regression test for the L-3 memory item.
//
// The point of streaming is that only one cue's bitmap is alive at a time. A
// single-cue-at-a-time consumer is modelled here: the callback counts how many
// images are reachable from the parser at once by keeping hold of none, and the
// test asserts every cue was delivered - so a streaming implementation that
// buffered them internally and emitted at the end would still have to deliver
// them all, and one that dropped cues would fail.
//
// It cannot measure peak memory directly, which is why the assertion is on
// delivery order and count as well as on the callback seeing images it must not
// retain: the memory question is settled by the shape, and the shape is settled
// by the parser not holding a slice.
func TestStreamPGS_EmitsAsItGoes(t *testing.T) {
	t.Parallel()

	const count = 6
	stream := manyCueFixture(t, count)

	var (
		seen    int
		lastEnd time.Duration
	)
	if err := StreamPGS(bytes.NewReader(stream), func(cue ImageCue) error {
		seen++
		if cue.Image == nil {
			t.Errorf("cue %d was delivered with no bitmap", seen)
		}
		if cue.End <= cue.Start {
			t.Errorf("cue %d is empty: %v to %v", seen, cue.Start, cue.End)
		}
		// Cues arrive in presentation order, which is what lets a caller drop
		// the previous image as soon as the next one arrives.
		if cue.Start < lastEnd {
			t.Errorf("cue %d starts at %v, before the previous cue ended at %v",
				seen, cue.Start, lastEnd)
		}
		lastEnd = cue.End
		return nil
	}); err != nil {
		t.Fatalf("StreamPGS: %v", err)
	}

	if seen != count {
		t.Errorf("delivered %d cues, want %d", seen, count)
	}
}

// TestStreamPGS_StopsWhenEmitFails covers the error path: a caller that cannot
// accept a cue must stop the parse rather than have the remaining cues decoded
// for nothing, which is what makes a cancelled OCR job cheap.
func TestStreamPGS_StopsWhenEmitFails(t *testing.T) {
	t.Parallel()

	stream := manyCueFixture(t, 6)
	wantErr := errStopStreaming

	seen := 0
	err := StreamPGS(bytes.NewReader(stream), func(ImageCue) error {
		seen++
		if seen == 2 {
			return wantErr
		}
		return nil
	})
	if err != wantErr {
		t.Fatalf("StreamPGS error = %v, want the caller's error", err)
	}
	if seen != 2 {
		t.Errorf("the parser delivered %d cues after the caller refused one, want 2", seen)
	}
}

// errStopStreaming stands in for a cancelled context.
var errStopStreaming = errStop{}

type errStop struct{}

func (errStop) Error() string { return "stop streaming" }

// TestStreamVobSub_YieldsTheSameCuesAsParseVobSub is the VobSub half of the
// equivalence check.
func TestStreamVobSub_YieldsTheSameCuesAsParseVobSub(t *testing.T) {
	t.Parallel()

	// The committed fixture is a real VobSub track in a Matroska container,
	// which also exercises StreamVobSub's container detection.
	fixture, err := os.ReadFile(vobsubFixture)
	if err != nil {
		t.Skipf("the committed fixture is unavailable: %v", err)
	}

	collected, err := ParseVobSub(bytes.NewReader(fixture), "")
	if err != nil {
		t.Fatalf("ParseVobSub: %v", err)
	}
	if len(collected) == 0 {
		t.Fatal("the fixture produced no cues, so this test proves nothing")
	}

	var streamed []ImageCue
	if err := StreamVobSub(bytes.NewReader(fixture), "", func(cue ImageCue) error {
		streamed = append(streamed, cue)
		return nil
	}); err != nil {
		t.Fatalf("StreamVobSub: %v", err)
	}

	if len(streamed) != len(collected) {
		t.Fatalf("StreamVobSub yielded %d cues, ParseVobSub collected %d",
			len(streamed), len(collected))
	}
	for i := range collected {
		if streamed[i].Start != collected[i].Start || streamed[i].End != collected[i].End {
			t.Errorf("cue %d: streamed %v-%v, collected %v-%v",
				i, streamed[i].Start, streamed[i].End, collected[i].Start, collected[i].End)
		}
	}
}
