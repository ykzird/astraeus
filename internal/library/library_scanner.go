package library

import (
	"context"
	"log/slog"
	"time"

	"github.com/jok/astraeus-media/internal/observability"
)

// ScanOutcome is the result of re-scanning one library.
type ScanOutcome struct {
	LibraryID   string     `json:"library_id"`
	LibraryName string     `json:"library_name"`
	Result      ScanResult `json:"result"`
	Error       string     `json:"error,omitempty"`
}

// ScanScheduler re-scans every registered library on an interval, so files that
// appear on disk show up without anyone asking. A manual scan via the CLI or
// the API remains the override.
type ScanScheduler struct {
	repo     Repository
	scanner  *Scanner
	interval time.Duration
	logger   *slog.Logger
	metrics  *observability.Metrics
}

// NewScanScheduler creates a ScanScheduler. A non-positive interval disables
// the periodic pass; ScanAll can still be called directly.
func NewScanScheduler(repo Repository, scanner *Scanner, interval time.Duration, logger *slog.Logger) *ScanScheduler {
	if logger == nil {
		logger = slog.Default()
	}
	return &ScanScheduler{
		repo:     repo,
		scanner:  scanner,
		interval: interval,
		logger:   logger,
	}
}

// SetMetrics attaches a metrics collector. Passing nil disables instrumentation.
func (s *ScanScheduler) SetMetrics(metrics *observability.Metrics) {
	s.metrics = metrics
}

// Start runs one pass immediately, then once per interval until the context is
// cancelled. It blocks, so callers usually run it in a goroutine.
func (s *ScanScheduler) Start(ctx context.Context) {
	if s.interval <= 0 {
		s.logger.InfoContext(ctx, "periodic scanning is disabled")
		return
	}

	s.runPass(ctx)

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			s.logger.InfoContext(ctx, "scan scheduler stopped", "reason", ctx.Err())
			return
		case <-ticker.C:
			s.runPass(ctx)
		}
	}
}

func (s *ScanScheduler) runPass(ctx context.Context) {
	outcomes, err := s.ScanAll(ctx)
	if err != nil {
		s.logger.ErrorContext(ctx, "scheduled scan pass failed", "error", err)
		return
	}

	for _, outcome := range outcomes {
		if outcome.Error != "" {
			s.logger.WarnContext(ctx, "scheduled scan failed for a library",
				"library", outcome.LibraryName, "error", outcome.Error)
			continue
		}
		if outcome.Result.FilesSeen > 0 || outcome.Result.EntitiesCreated > 0 {
			s.logger.InfoContext(ctx, "scheduled scan complete",
				"library", outcome.LibraryName,
				"files", outcome.Result.FilesSeen,
				"entities_created", outcome.Result.EntitiesCreated,
				"objects_created", outcome.Result.ObjectsCreated)
		}
	}
}

// ScanAll re-scans every registered library, in order. A failure for one
// library is recorded against it and does not stop the others.
func (s *ScanScheduler) ScanAll(ctx context.Context) ([]ScanOutcome, error) {
	libraries, err := s.repo.ListLibraries(ctx)
	if err != nil {
		return nil, err
	}

	outcomes := make([]ScanOutcome, 0, len(libraries))
	for i := range libraries {
		lib := libraries[i]
		if err := ctx.Err(); err != nil {
			return outcomes, err
		}

		outcome := ScanOutcome{LibraryID: lib.ID, LibraryName: lib.Name}
		scanStart := time.Now()
		result, err := s.scanner.ScanLibrary(ctx, &lib)
		s.metrics.ObserveHistogram(
			"astraeus_scan_seconds",
			"Time taken to walk one library directory and reconcile it with the database.",
			time.Since(scanStart).Seconds(),
			map[string]string{"library": lib.Name},
		)
		if err != nil {
			outcome.Error = err.Error()
		} else {
			outcome.Result = result
			s.metrics.AddCounter("astraeus_scan_files_total", "Media files seen by the scanner.", float64(result.FilesSeen), nil)
			s.metrics.IncCounter("astraeus_scan_runs_total", "Completed scan passes per library.", map[string]string{"library": lib.Name})
		}
		outcomes = append(outcomes, outcome)
	}

	return outcomes, nil
}

// Interval reports the configured period, or zero when periodic scanning is
// disabled.
func (s *ScanScheduler) Interval() time.Duration { return s.interval }
