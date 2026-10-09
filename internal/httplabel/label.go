// Package httplabel turns request values into bounded label text.
//
// Metric labels and span names come from the request, and the request is the
// client's to choose. `r.Method` is whatever the client wrote - net/http accepts
// any token, not the nine that exist - so a client that sends three hundred
// different methods creates three hundred label sets, and one that sends a
// hundred-kilobyte method creates a label that size. Neither series is ever
// freed. The review measured exactly that: 300 invented methods took /metrics
// from a few lines to 5,441, and five raw-socket requests with 100 KB methods
// took it to 9.3 MB (A-4 of the 2026-10-09 review).
//
// The same applies to span names, which are the method and the path
// concatenated, so those two are bounded here as well.
package httplabel

import (
	"net/http"
	"strings"
)

// otherMethod is the label an unrecognised method is recorded under.
//
// It is a real value rather than an empty string so a dashboard can tell
// "something odd arrived" from "no requests arrived", and so the unknown traffic
// is visible without being unbounded.
const otherMethod = "other"

// knownMethods is the set of methods this server routes or answers to. A method
// outside it is recorded as `other`.
//
// The list is deliberately the standard set: a WebDAV extension or a custom verb
// gains nothing from its own series, and every addition is a series a client can
// create at will.
var knownMethods = map[string]bool{
	http.MethodGet:     true,
	http.MethodHead:    true,
	http.MethodPost:    true,
	http.MethodPut:     true,
	http.MethodPatch:   true,
	http.MethodDelete:  true,
	http.MethodConnect: true,
	http.MethodOptions: true,
	http.MethodTrace:   true,
}

// maxLabelBytes bounds any label this package returns. It is far longer than the
// longest method and than any path segment this server routes, and short enough
// that a label cannot be the reason a scrape is large.
const maxLabelBytes = 128

// Method is the label to record a request's method under.
//
// A recognised method is returned in its canonical upper case, so `get` and `GET`
// are one series. Anything else becomes `other`, which keeps the label set
// bounded at ten values however creative a client is.
func Method(method string) string {
	upper := strings.ToUpper(strings.TrimSpace(method))
	if knownMethods[upper] {
		return upper
	}
	return otherMethod
}

// Path is the label to record a request's path under.
//
// It is truncated and stripped of control characters, because a path is a
// client-supplied string of unbounded length. Truncation is on rune boundaries so
// a label cannot end mid-character and produce invalid UTF-8 in an exposition
// format that has to be UTF-8.
func Path(path string) string {
	return bound(path)
}

// SpanName is the name to give a server span for a request.
//
// The route pattern is used when net/http knows it, because it is bounded and
// low-cardinality: `/api/entities/{id}` rather than one name per entity. Before
// the pattern is known - which is when the middleware runs, since the mux assigns
// it during dispatch - the path is used, truncated, and the name is bounded
// either way.
func SpanName(method, pattern, path string) string {
	route := pattern
	if route == "" {
		route = bound(path)
	}
	name := Method(method) + " " + route
	if len(name) > maxLabelBytes {
		name = bound(name)
	}
	return name
}

// bound truncates a string to maxLabelBytes on a rune boundary and replaces
// control characters, which have no place in a label.
func bound(value string) string {
	var out strings.Builder
	out.Grow(len(value))
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			out.WriteRune('_')
			continue
		}
		if out.Len()+utf8Len(r) > maxLabelBytes {
			break
		}
		out.WriteRune(r)
	}
	if out.Len() == 0 && value != "" {
		// Every rune was a control character, so the label would be empty. An
		// empty label is indistinguishable from a missing one.
		return "_"
	}
	return out.String()
}

// utf8Len is the encoded length of a rune, used to stop on a boundary rather
// than to slice a multi-byte character in half.
func utf8Len(r rune) int {
	switch {
	case r < 0x80:
		return 1
	case r < 0x800:
		return 2
	case r < 0x10000:
		return 3
	default:
		return 4
	}
}
