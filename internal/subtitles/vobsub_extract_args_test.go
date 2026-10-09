package subtitles

import (
	"os"
	"strings"
	"testing"
)

// TestProbeSubtitleStreamAsksForTheData pins the flag L-12's first claim is about.
//
// `ffprobe -show_entries stream=extradata` reports the codec private data as
// present and leaves it empty unless `-show_data` is also passed. The palette a
// VobSub track needs lives in that field for any container that carries it, so
// without the flag the extractor refused those containers for carrying no palette
// when it had simply not asked for it.
//
// This is a source assertion rather than a behavioural one, deliberately: the
// committed fixtures cannot show the consequence (the .mpg has no extradata at all,
// with or without the flag), and a behavioural test that cannot fail is the sort of
// thing this session has had to remove twice already. Reading the argument list is
// honest about what it checks.
func TestProbeSubtitleStreamAsksForTheData(t *testing.T) {
	t.Parallel()

	source, err := os.ReadFile("vobsub_extract.go")
	if err != nil {
		t.Fatalf("reading vobsub_extract.go: %v", err)
	}
	text := string(source)

	// Scoped to the extradata query, and to its *arguments*: the region between
	// the exec call and the argument that opens the parenthesis list. An earlier
	// version of this test searched to the first ")" and found one inside a
	// comment, so the region ended before the flag - and the test passed with the
	// flag removed, which is the whole failure mode it exists to catch.
	start := strings.Index(text, `"stream=extradata"`)
	if start < 0 {
		t.Fatal("the extradata query was not found in vobsub_extract.go")
	}
	// Forward to the end of the argument list: the media path is the last argument
	// and is written as a bare identifier followed by the closing parenthesis.
	rest := text[start:]
	end := strings.Index(rest, "mediaPath)")
	if end < 0 {
		t.Fatal("the argument list of the extradata query could not be delimited")
	}
	arguments := rest[:end]

	// Comments are not arguments.
	var code []string
	for _, line := range strings.Split(arguments, "\n") {
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "//") {
			continue
		}
		code = append(code, line)
	}
	joined := strings.Join(code, "\n")

	if !strings.Contains(joined, `"-show_data"`) {
		t.Error("the extradata query does not pass -show_data, so ffprobe reports the " +
			"codec private data as present and leaves it empty; a container whose palette " +
			"lives there is then refused for carrying none")
	}
}
