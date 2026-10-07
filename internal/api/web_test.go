package api

import (
	"net/http"
	"os"
	"path/filepath"
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
