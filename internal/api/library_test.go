package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// TestCreateLibrary_RefusesOverlappingRoots is the API half of L-15.
//
// The library package refuses the overlap; this pins that the endpoint asks it
// to. A rule the handler does not call is a rule that does not hold, and the
// review's evidence was gathered through the API.
func TestCreateLibrary_RefusesOverlappingRoots(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t)

	root := t.TempDir()
	media := filepath.Join(root, "media")
	if err := os.MkdirAll(media, 0o755); err != nil {
		t.Fatalf("creating the tree: %v", err)
	}
	if recorder := env.do(t, http.MethodPost, "/api/libraries",
		`{"name":"Media","path":`+strconv.Quote(media)+`,"kind":"movies"}`); recorder.Code != http.StatusCreated {
		t.Fatalf("registering the first library = %d: %s", recorder.Code, recorder.Body.String())
	}

	// A root inside it, a root containing it, and the same root again - all three
	// are the same mistake and all three must be refused.
	for _, candidate := range []string{
		filepath.Join(media, "movies"),
		root,
		media,
	} {
		if err := os.MkdirAll(candidate, 0o755); err != nil {
			t.Fatalf("creating %s: %v", candidate, err)
		}
		recorder := env.do(t, http.MethodPost, "/api/libraries",
			`{"name":"Second","path":`+strconv.Quote(candidate)+`,"kind":"movies"}`)
		if recorder.Code == http.StatusCreated {
			t.Errorf("registering %s against a library at %s was accepted, so the same "+
				"files would be owned by two libraries and the entity they belong to "+
				"would follow the scan order", candidate, media)
			continue
		}
		if recorder.Code != http.StatusConflict {
			t.Errorf("registering %s = %d, want 409 (body %s)",
				candidate, recorder.Code, recorder.Body.String())
		}
	}

	// An unrelated root is still accepted: the check must not refuse everything.
	other := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatalf("creating the unrelated root: %v", err)
	}
	if recorder := env.do(t, http.MethodPost, "/api/libraries",
		`{"name":"Elsewhere","path":`+strconv.Quote(other)+`,"kind":"movies"}`); recorder.Code != http.StatusCreated {
		t.Errorf("an unrelated root = %d, want 201: %s", recorder.Code, recorder.Body.String())
	}
}
