package httplabel

import (
	"net/http"
	"strings"
	"testing"
)

// TestMethod_BoundsTheMethodSet is the regression test for A-4's first half.
//
// net/http accepts any token as a method. Recording r.Method verbatim meant a
// client could create one metric series per invented verb, and the series are
// never freed - the review measured 300 invented methods taking /metrics from a
// few lines to 5,441.
func TestMethod_BoundsTheMethodSet(t *testing.T) {
	t.Parallel()

	// The standard methods keep their own series, so dashboards are unaffected.
	for _, method := range []string{
		http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodConnect,
		http.MethodOptions, http.MethodTrace,
	} {
		if got := Method(method); got != method {
			t.Errorf("Method(%q) = %q, want it unchanged", method, got)
		}
	}

	// Case is normalised, so `get` and `GET` are one series rather than two.
	if got := Method("get"); got != http.MethodGet {
		t.Errorf("Method(\"get\") = %q, want %q", got, http.MethodGet)
	}
	if got := Method("  GET  "); got != http.MethodGet {
		t.Errorf("Method with whitespace = %q, want %q", got, http.MethodGet)
	}

	// Everything else collapses to one label, however many there are.
	invented := map[string]bool{}
	for i := 0; i < 300; i++ {
		invented[Method("M"+strings.Repeat("X", i%5)+string(rune('a'+i%26)))] = true
	}
	if len(invented) != 1 || !invented["other"] {
		t.Errorf("300 invented methods produced %d labels (%v), want exactly one: a client "+
			"must not be able to create metric series at will", len(invented), invented)
	}

	// An empty method is not a method.
	if got := Method(""); got != "other" {
		t.Errorf("Method(\"\") = %q, want other", got)
	}
}

// TestMethod_LongMethodIsNotALabel is the second half: five raw-socket requests
// with 100 KB methods took /metrics to 9.3 MB, because the method became the
// label.
func TestMethod_LongMethodIsNotALabel(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("A", 100_000)
	got := Method(huge)
	if got != "other" {
		t.Errorf("a 100 KB method produced a %d-byte label, want other", len(got))
	}
	if len(got) > maxLabelBytes {
		t.Errorf("a label is %d bytes, want at most %d", len(got), maxLabelBytes)
	}
}

// TestPath_IsBoundedAndClean covers the same problem for paths, which reach span
// names and attributes.
func TestPath_IsBoundedAndClean(t *testing.T) {
	t.Parallel()

	long := "/api/entities/" + strings.Repeat("x", 10_000)
	if got := Path(long); len(got) > maxLabelBytes {
		t.Errorf("Path returned %d bytes for a long path, want at most %d", len(got), maxLabelBytes)
	}

	// Control characters have no place in a label and are replaced rather than
	// passed through into an exposition format.
	if got := Path("/api/\x00\x01\x02/x"); strings.ContainsAny(got, "\x00\x01\x02") {
		t.Errorf("Path(%q) = %q, want control characters replaced", "/api/\x00\x01\x02/x", got)
	}
	if got := Path("/api/\x00/x"); got != "/api/_/x" {
		t.Errorf("Path with a control character = %q, want %q", got, "/api/_/x")
	}

	// A normal path is untouched.
	if got := Path("/api/libraries/abc/entities"); got != "/api/libraries/abc/entities" {
		t.Errorf("Path mangled an ordinary path: %q", got)
	}

	// A path of nothing but control characters still yields something.
	if got := Path("\x00\x01"); got == "" {
		t.Error("Path returned an empty label, which is indistinguishable from a missing one")
	}
}

// TestPath_TruncatesOnARuneBoundary guards the encoding: slicing mid-character
// produces invalid UTF-8, which the exposition format requires.
func TestPath_TruncatesOnARuneBoundary(t *testing.T) {
	t.Parallel()

	// Each of these is three bytes, so a byte-wise cut at 128 lands mid-character.
	path := "/" + strings.Repeat("→", 200)
	got := Path(path)
	if !utf8Valid(got) {
		t.Errorf("Path returned invalid UTF-8 (%d bytes)", len(got))
	}
	if len(got) > maxLabelBytes {
		t.Errorf("Path returned %d bytes, want at most %d", len(got), maxLabelBytes)
	}
}

// TestSpanName_IsBounded covers the span half: a name is indexed by backends, so
// one name per invented URL is the same unbounded series.
func TestSpanName_IsBounded(t *testing.T) {
	t.Parallel()

	name := SpanName("GET", "", "/api/entities/"+strings.Repeat("y", 5_000))
	if len(name) > maxLabelBytes {
		t.Errorf("SpanName returned %d bytes, want at most %d", len(name), maxLabelBytes)
	}
	if !strings.HasPrefix(name, "GET ") {
		t.Errorf("SpanName(%q) = %q, want it to start with the method", "GET", name)
	}

	// A pattern is preferred when there is one, because it is low-cardinality.
	patterned := SpanName("GET", "/api/entities/{id}", "/api/entities/12345")
	if patterned != "GET /api/entities/{id}" {
		t.Errorf("SpanName with a pattern = %q, want the pattern", patterned)
	}

	// An invented method is labelled as other here too, so span names are bounded
	// the same way the metric labels are.
	if got := SpanName("MADEUP", "", "/x"); !strings.HasPrefix(got, "other ") {
		t.Errorf("SpanName with an invented method = %q, want it to start with other", got)
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == 0xFFFD {
			// The replacement character is what ranging over invalid UTF-8 yields.
			return false
		}
	}
	return true
}
