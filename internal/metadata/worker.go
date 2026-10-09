package metadata

import (
	"context"
	"log/slog"
	"time"

	"github.com/ykzird/astraeus/internal/library"
	"github.com/ykzird/astraeus/internal/observability"
)

// Store is the persistence this package needs, and nothing more: read the
// entities, write one back. Keeping it this narrow is what lets the worker be
// tested without a database and stops the metadata service from depending on
// the whole library repository.
//
// library.Repository satisfies this, as does the SQLite implementation.
type Store interface {
	ListEntities(ctx context.Context) ([]library.MediaEntity, error)
	UpdateEntity(ctx context.Context, entity *library.MediaEntity) error
}

// The library repository is what the server hands this package, so the port has
// to stay satisfied by it. This fails the build if the repository drifts away
// from what the worker needs, rather than failing at the call site with a
// confusing type error. The concrete SQLite adapter is asserted at the
// composition root, where the two are actually wired together, so this package
// never has to know which implementation exists.
var _ Store = (library.Repository)(nil)

// EnrichResult reports the outcome of one enrichment pass.
type EnrichResult struct {
	Processed int `json:"processed"`
	Enriched  int `json:"enriched"`
	Failed    int `json:"failed"`
}

// Worker periodically attaches MetadataSets to incomplete entities. An entity
// stays Incomplete until a provider succeeds, which is what the
// administrator-facing "needs attention" view reports.
type Worker struct {
	store    Store
	provider Provider
	interval time.Duration
	logger   *slog.Logger
	now      func() time.Time
	metrics  *observability.Metrics
	// dedupe, when set, decides whether a pass should run. The background loop
	// and an API-triggered pass share it, so the two cannot enrich the same
	// entities at once (A-B7 of the 2026-10-09 review).
	dedupe Dedupe
}

// Dedupe reports whether an enrichment pass may start now.
//
// It exists so the periodic loop and the API can share one answer without either
// knowing about the other. RunOnce is called when the pass may proceed; it may
// return (nil, nil) when the pass was refused, which is not an error - another
// pass is simply already doing the work.
type Dedupe interface {
	RunOnce(key string, work func() (any, error)) (any, error)
}

// NewWorker creates a Worker.
func NewWorker(store Store, provider Provider, interval time.Duration, logger *slog.Logger) *Worker {
	if logger == nil {
		logger = slog.Default()
	}
	if interval <= 0 {
		interval = time.Hour
	}
	return &Worker{
		store:    store,
		provider: provider,
		interval: interval,
		logger:   logger,
		now:      time.Now,
	}
}

// EnrichKey is the job key that makes one enrichment pass exclusive. The API and
// the background loop both use it, which is what stops them overlapping.
const EnrichKey = "enrich:all"

// SetMetrics attaches a metrics collector. Passing nil disables instrumentation.
func (w *Worker) SetMetrics(metrics *observability.Metrics) {
	w.metrics = metrics
}

// SetDedupe attaches the guard the background loop and the API share. Passing
// nil means every pass runs, which is what a worker used on its own does.
func (w *Worker) SetDedupe(dedupe Dedupe) {
	w.dedupe = dedupe
}

// Start runs one enrichment pass immediately, then once per interval until the
// context is cancelled. It blocks, so callers usually run it in a goroutine.
func (w *Worker) Start(ctx context.Context) {
	w.runPass(ctx)

	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.logger.InfoContext(ctx, "metadata worker stopped", "reason", ctx.Err())
			return
		case <-ticker.C:
			w.runPass(ctx)
		}
	}
}

func (w *Worker) runPass(ctx context.Context) {
	// A pass the API is already running is not run again. Without this, the
	// ticker can fire while a manual enrich is in flight and both walk the same
	// incomplete entities, calling the provider twice for each.
	if w.dedupe != nil {
		_, err := w.dedupe.RunOnce(EnrichKey, func() (any, error) {
			return w.EnrichOnce(ctx)
		})
		if err != nil {
			w.logger.ErrorContext(ctx, "metadata enrichment pass failed", "error", err)
		}
		return
	}

	result, err := w.EnrichOnce(ctx)
	if err != nil {
		w.logger.ErrorContext(ctx, "metadata enrichment pass failed", "error", err)
		return
	}
	if result.Processed > 0 {
		w.logger.InfoContext(ctx, "metadata enrichment pass complete",
			"processed", result.Processed, "enriched", result.Enriched, "failed", result.Failed)
	}
}

// EnrichOnce enriches every incomplete entity exactly once. A provider error
// for one entity does not stop the pass; the entity stays Incomplete.
func (w *Worker) EnrichOnce(ctx context.Context) (EnrichResult, error) {
	var result EnrichResult

	entities, err := w.store.ListEntities(ctx)
	if err != nil {
		return result, err
	}

	for i := range entities {
		entity := entities[i]
		if entity.Status != library.StatusIncomplete {
			continue
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}
		result.Processed++

		// metadata_latency: how long the provider took, and whether it worked.
		lookupStart := time.Now()
		meta, err := w.provider.FetchMetadata(ctx, &entity)
		outcome := "ok"
		switch {
		case err != nil:
			outcome = "error"
		case meta == nil:
			outcome = "empty"
		}
		w.metrics.ObserveHistogram(
			"astraeus_metadata_lookup_seconds",
			"Time taken to retrieve and process a MetadataSet for one entity.",
			time.Since(lookupStart).Seconds(),
			map[string]string{"provider": w.provider.Name(), "outcome": outcome},
		)

		if err != nil {
			result.Failed++
			w.logger.WarnContext(ctx, "metadata lookup failed",
				"entity_id", entity.ID, "name", entity.Name, "provider", w.provider.Name(), "error", err)
			continue
		}
		if meta == nil {
			result.Failed++
			w.logger.WarnContext(ctx, "metadata provider returned no result",
				"entity_id", entity.ID, "name", entity.Name, "provider", w.provider.Name())
			continue
		}

		entity.Metadata = meta
		entity.Status = library.StatusComplete
		entity.UpdatedAt = w.now()
		if err := w.store.UpdateEntity(ctx, &entity); err != nil {
			return result, err
		}
		result.Enriched++
		w.logger.DebugContext(ctx, "entity enriched", "entity_id", entity.ID, "title", meta.Title)
	}

	return result, nil
}
