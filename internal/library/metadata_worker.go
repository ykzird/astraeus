package library

import (
	"context"
	"log/slog"
	"time"

	"github.com/jok/astraeus-media/internal/observability"
)

// EnrichResult reports the outcome of one enrichment pass.
type EnrichResult struct {
	Processed int `json:"processed"`
	Enriched  int `json:"enriched"`
	Failed    int `json:"failed"`
}

// MetadataWorker periodically attaches MetadataSets to incomplete entities.
// An entity stays Incomplete until a provider succeeds, which is what the
// administrator-facing "needs attention" view reports.
type MetadataWorker struct {
	repo     Repository
	provider MetadataProvider
	interval time.Duration
	logger   *slog.Logger
	now      func() time.Time
	metrics  *observability.Metrics
}

// NewMetadataWorker creates a MetadataWorker.
func NewMetadataWorker(repo Repository, provider MetadataProvider, interval time.Duration, logger *slog.Logger) *MetadataWorker {
	if logger == nil {
		logger = slog.Default()
	}
	if interval <= 0 {
		interval = time.Hour
	}
	return &MetadataWorker{
		repo:     repo,
		provider: provider,
		interval: interval,
		logger:   logger,
		now:      time.Now,
	}
}

// SetMetrics attaches a metrics collector. Passing nil disables instrumentation.
func (w *MetadataWorker) SetMetrics(metrics *observability.Metrics) {
	w.metrics = metrics
}

// Start runs one enrichment pass immediately, then once per interval until the
// context is cancelled. It blocks, so callers usually run it in a goroutine.
func (w *MetadataWorker) Start(ctx context.Context) {
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

func (w *MetadataWorker) runPass(ctx context.Context) {
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
func (w *MetadataWorker) EnrichOnce(ctx context.Context) (EnrichResult, error) {
	var result EnrichResult

	entities, err := w.repo.ListEntities(ctx)
	if err != nil {
		return result, err
	}

	for i := range entities {
		entity := entities[i]
		if entity.Status != StatusIncomplete {
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
		entity.Status = StatusComplete
		entity.UpdatedAt = w.now()
		if err := w.repo.UpdateEntity(ctx, &entity); err != nil {
			return result, err
		}
		result.Enriched++
		w.logger.DebugContext(ctx, "entity enriched", "entity_id", entity.ID, "title", meta.Title)
	}

	return result, nil
}
