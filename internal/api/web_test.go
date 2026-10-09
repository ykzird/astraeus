package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHandler_ServesWebUI(t *testing.T) {
	t.Parallel()

	webDir := t.TempDir()
	index := []byte("<!doctype html><title>Astraeus</title>")
	if err := os.WriteFile(filepath.Join(webDir, "index.html"), index, 0o644); err != nil {
		t.Fatalf("writing index.html: %v", err)
	}
	if err := os.WriteFile(filepath.Join(webDir, "app.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatalf("writing app.js: %v", err)
	}

	env := newTestEnv(t, withWebDir(webDir))

	recorder := env.do(t, http.MethodGet, "/", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", recorder.Code)
	}
	if recorder.Body.String() != string(index) {
		t.Errorf("GET / body = %q, want index.html", recorder.Body.String())
	}

	recorder = env.do(t, http.MethodGet, "/app.js", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /app.js status = %d, want 200", recorder.Code)
	}

	// API routes must still win over the static catch-all.
	recorder = env.do(t, http.MethodGet, "/api/health", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/health status = %d, want 200", recorder.Code)
	}
	if got := decodeBody[map[string]string](t, recorder)["service"]; got != "astraeus" {
		t.Errorf("health service = %q, want astraeus", got)
	}
}

func TestHandler_WithoutWebDirServesOnlyTheAPI(t *testing.T) {
	t.Parallel()

	recorder := newTestEnv(t).do(t, http.MethodGet, "/", "")
	if recorder.Code != http.StatusNotFound {
		t.Errorf("GET / status = %d, want 404 when no UI directory is configured", recorder.Code)
	}
}

func TestHandler_MissingWebDirIsNotFatal(t *testing.T) {
	t.Parallel()

	env := newTestEnv(t, withWebDir(filepath.Join(t.TempDir(), "does-not-exist")))

	recorder := env.do(t, http.MethodGet, "/api/health", "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /api/health status = %d, want 200", recorder.Code)
	}
}

// TestHandler_DoesNotExposeTheWebDirectory is the regression test for A-10's
// static-file item.
//
// http.FileServer is a directory browser. `GET /vendor/` answered with a listing of
// everything in the directory, and a dotfile in the tree was served to anyone who
// named it: a map of the install for a stranger. The UI is a fixed set of files the
// build produces, so the server does not need to answer "what else is there".
func TestHandler_DoesNotExposeTheWebDirectory(t *testing.T) {
	t.Parallel()

	webDir := t.TempDir()
	// A tree shaped like the real one: an asset, a directory with no index, and
	// things nobody browsing the UI should be handed.
	files := map[string]string{
		"index.html":           "<!doctype html><title>Astraeus</title>",
		"app.js":               "console.log(1)",
		"core.test.js":         "// the test file, which ships beside the code",
		"README.md":            "# the front end",
		"vendor/version.js":    "window.version = '1';",
		"vendor/.version":      "0.18.0",
		"vendor/.git/config":   "[core]",
		"icons/.hidden-icon":   "svg",
		"vendor/no-index-dir/": "", // a directory, created below
	}
	for name, body := range files {
		full := filepath.Join(webDir, name)
		if strings.HasSuffix(name, "/") {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatalf("creating %s: %v", name, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("creating the directory for %s: %v", name, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	env := newTestEnv(t, withWebDir(webDir))

	// The UI itself still works, including the root, which is how a browser asks
	// for it - and which a directory check must not mistake for a listing.
	for _, path := range []string{"/", "/app.js", "/vendor/version.js"} {
		if recorder := env.do(t, http.MethodGet, path, ""); recorder.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200: the UI is a fixed set of files and must be "+
				"servable", path, recorder.Code)
		}
	}

	// A named directory is refused rather than listed.
	for _, path := range []string{"/vendor/", "/icons/", "/vendor/no-index-dir/"} {
		recorder := env.do(t, http.MethodGet, path, "")
		if recorder.Code == http.StatusOK {
			t.Errorf("GET %s = 200: a directory request answered, which is a listing unless "+
				"there is an index file - and these have none:\n%s", path, recorder.Body.String())
		}
	}

	// Dotfiles are refused wherever they are in the tree.
	for _, path := range []string{
		"/vendor/.version",
		"/vendor/.git/config",
		"/icons/.hidden-icon",
		"/.git/config",
	} {
		recorder := env.do(t, http.MethodGet, path, "")
		if recorder.Code == http.StatusOK {
			t.Errorf("GET %s = 200, so a dotfile in the served tree was handed out:\n%s",
				path, recorder.Body.String())
		}
		if strings.Contains(recorder.Body.String(), "0.18.0") {
			t.Errorf("GET %s returned the contents of a dotfile", path)
		}
	}
}

// TestUIPathGuards pins the two rules, including the case that must stay served.
func TestUIPathGuards(t *testing.T) {
	t.Parallel()

	hidden := map[string]bool{
		"/.env":               true,
		"/vendor/.version":    true,
		"/vendor/.git/config": true,
		"/a/.b/c.js":          true,
		"/app.js":             false,
		"/vendor/version.js":  false,
		"/":                   false,
		"/a/b/../c.js":        false,
	}
	for path, want := range hidden {
		if got := uiPathIsHidden(path); got != want {
			t.Errorf("uiPathIsHidden(%q) = %v, want %v", path, got, want)
		}
	}

	directories := map[string]bool{
		"/":             false, // the UI root, served as index.html
		"/vendor/":      true,
		"/icons/small/": true,
		"/app.js":       false,
		"/vendor/a.js":  false,
	}
	for path, want := range directories {
		if got := uiPathIsDirectory(path); got != want {
			t.Errorf("uiPathIsDirectory(%q) = %v, want %v", path, got, want)
		}
	}
}
