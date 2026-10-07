package library

import (
	"context"
	"fmt"
)

// MetadataProvider defines the interface for external metadata services.
type MetadataProvider interface {
	// Name identifies the provider in a MetadataSet.
	Name() string
	// FetchMetadata retrieves a MetadataSet for a given MediaEntity. Returning
	// an error leaves the entity Incomplete so an administrator can see it.
	FetchMetadata(ctx context.Context, entity *MediaEntity) (*MetadataSet, error)
}

// ChainProvider tries each provider in order and returns the first success.
// This is how the TMDB -> TVDB/IMDB fallback chain is expressed.
type ChainProvider struct {
	providers []MetadataProvider
}

// NewChainProvider creates a ChainProvider over the given providers.
func NewChainProvider(providers ...MetadataProvider) *ChainProvider {
	return &ChainProvider{providers: providers}
}

// Name reports the chain identity.
func (c *ChainProvider) Name() string { return "chain" }

// FetchMetadata returns the first successful result, or the collected errors.
func (c *ChainProvider) FetchMetadata(ctx context.Context, entity *MediaEntity) (*MetadataSet, error) {
	if len(c.providers) == 0 {
		return nil, fmt.Errorf("no metadata providers configured")
	}

	var errs []error
	for _, p := range c.providers {
		meta, err := p.FetchMetadata(ctx, entity)
		if err == nil && meta != nil {
			return meta, nil
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", p.Name(), err))
		}
	}
	return nil, fmt.Errorf("no provider could supply metadata for %q: %w", entity.DisplayTitle(), joinErrors(errs))
}

// MockProvider derives a MetadataSet from the entity itself. It exists so the
// whole library pipeline can be exercised, and demonstrated, without an API
// key or network access.
type MockProvider struct{}

// NewMockProvider creates a MockProvider.
func NewMockProvider() *MockProvider { return &MockProvider{} }

// Name reports the provider identity.
func (m *MockProvider) Name() string { return "mock" }

// FetchMetadata synthesises metadata from the entity's own name.
func (m *MockProvider) FetchMetadata(_ context.Context, entity *MediaEntity) (*MetadataSet, error) {
	extra := map[string]string{}
	for k, v := range metadataFromEntity(entity).Extra {
		extra[k] = v
	}
	extra["synthetic"] = "true"

	return &MetadataSet{
		Title:       entity.Name,
		Description: fmt.Sprintf("%s imported from the library. No metadata provider is configured, so this description is a placeholder.", entity.Name),
		Provider:    m.Name(),
		Extra:       extra,
	}, nil
}

// metadataFromEntity returns the metadata the scanner already knows about an
// entity (season/episode numbers, release year) and is never nil.
func metadataFromEntity(entity *MediaEntity) *MetadataSet {
	if entity.Metadata != nil {
		return entity.Metadata
	}
	return &MetadataSet{Extra: map[string]string{}}
}

// joinErrors flattens a slice of errors without pulling in errors.Join's
// newline formatting, which reads poorly in logs.
func joinErrors(errs []error) error {
	if len(errs) == 1 {
		return errs[0]
	}
	msg := ""
	for i, err := range errs {
		if i > 0 {
			msg += "; "
		}
		msg += err.Error()
	}
	return fmt.Errorf("%s", msg)
}
