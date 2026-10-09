package api

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestEveryRouteIsDocumented is the regression test for D-11.
//
// api.md's endpoint table was maintained by hand beside the mux, and it drifted:
// it was missing the job-status route, and it still described scanning and
// enriching as synchronous after the API started answering 202. A reference that
// does not list a route is worse than a thin one, because a client reading it
// concludes the route does not exist.
//
// The authority is the mux itself. This parses the route registrations out of
// this package's own source and requires each to appear in the table.
func TestEveryRouteIsDocumented(t *testing.T) {
	t.Parallel()

	source, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("reading server.go: %v", err)
	}

	// Routes are registered as mux.HandleFunc("METHOD /path", ...).
	registration := regexp.MustCompile(`mux\.HandleFunc\("([A-Z]+) ([^"]+)"`)
	matches := registration.FindAllStringSubmatch(string(source), -1)
	if len(matches) < 15 {
		t.Fatalf("only %d routes were parsed from server.go, so the parsing is wrong "+
			"rather than the documentation", len(matches))
	}

	docs, err := os.ReadFile("../../docs/api.md")
	if err != nil {
		t.Fatalf("reading docs/api.md: %v", err)
	}
	documented := string(docs)

	var missing []string
	for _, match := range matches {
		method, path := match[1], match[2]
		// The table's shape is | METHOD | `path` | ..., so both parts have to be
		// present. Checking the path alone would pass for a route listed with
		// the wrong method.
		row := "| " + method + " | `" + path + "` |"
		if !strings.Contains(documented, row) {
			missing = append(missing, method+" "+path)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("these routes exist but have no row in docs/api.md, so a client reading "+
			"the reference concludes they do not exist:\n  %s", strings.Join(missing, "\n  "))
	}
}
