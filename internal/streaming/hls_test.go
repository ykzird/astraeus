package streaming

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newTestLogger keeps manager logging out of the test output.
func newTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestNewManager_SweepsStaleSessionDirectories(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	stale := filepath.Join(root, "stale-session")
	fresh := filepath.Join(root, "fresh-session")
	for _, dir := range []string{stale, fresh} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("creating %s: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "playlist.m3u8"), []byte("#EXTM3U"), 0o644); err != nil {
			t.Fatalf("writing playlist: %v", err)
		}
	}

	// Age one directory well past the session TTL.
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("ageing %s: %v", stale, err)
	}

	manager, err := NewManager(context.Background(), ManagerConfig{
		RootDir:    root,
		SessionTTL: time.Minute,
		Logger:     newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(manager.Close)

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale session directory survived startup: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("a session inside the TTL was removed: %v", err)
	}
}

func TestNewManager_RequiresRootDir(t *testing.T) {
	t.Parallel()

	if _, err := NewManager(context.Background(), ManagerConfig{Logger: newTestLogger()}); err == nil {
		t.Fatal("expected an error when RootDir is empty")
	}
}

func TestNewManager_AppliesDefaults(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(context.Background(), ManagerConfig{
		RootDir: t.TempDir(),
		Logger:  newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(manager.Close)

	cfg := manager.Config()
	if cfg.FFmpegBin != "ffmpeg" {
		t.Errorf("ffmpeg binary = %q, want ffmpeg", cfg.FFmpegBin)
	}
	if cfg.SegmentSeconds != defaultSegmentSeconds {
		t.Errorf("segment seconds = %d, want %d", cfg.SegmentSeconds, defaultSegmentSeconds)
	}
	if cfg.SessionTTL != defaultSessionTTL {
		t.Errorf("session TTL = %v, want %v", cfg.SessionTTL, defaultSessionTTL)
	}
}

func TestManager_StartRejectsDirectPlay(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(context.Background(), ManagerConfig{
		RootDir: t.TempDir(),
		Logger:  newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(manager.Close)

	_, err = manager.Start(context.Background(), "entity", "/media/movie.mp4", Decision{
		Mode:        ModeDirectPlay,
		Deliverable: true,
	})
	if err == nil {
		t.Fatal("expected an error: direct play does not need a session")
	}
}

func TestManager_StartRejectsUndeliverableDecision(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(context.Background(), ManagerConfig{
		RootDir: t.TempDir(),
		Logger:  newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(manager.Close)

	_, err = manager.Start(context.Background(), "entity", "/media/movie.mkv", Decision{
		Mode:        ModeRemux,
		Deliverable: false,
	})
	if err == nil {
		t.Fatal("expected an error for an undeliverable decision")
	}
}

func TestManager_ReapStopsIdleSessions(t *testing.T) {
	t.Parallel()

	manager, err := NewManager(context.Background(), ManagerConfig{
		RootDir:    t.TempDir(),
		SessionTTL: time.Minute,
		Logger:     newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(manager.Close)

	// A session with no live process: Stop must still be safe and empty the map.
	session := &Session{
		ID:         "idle",
		Dir:        t.TempDir(),
		done:       make(chan struct{}),
		cancel:     func() {},
		startedAt:  time.Now().Add(-time.Hour),
		lastAccess: time.Now().Add(-time.Hour),
	}
	close(session.done)

	manager.mu.Lock()
	manager.sessions["idle"] = session
	manager.mu.Unlock()

	if reaped := manager.Reap(time.Now()); reaped != 1 {
		t.Fatalf("reaped %d sessions, want 1", reaped)
	}
	if _, ok := manager.Session("idle"); ok {
		t.Error("the idle session is still registered")
	}
}
