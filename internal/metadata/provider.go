// Package metadata retrieves descriptive metadata for library entities from
// external services, and attaches it to them.
//
// It is a service over the library domain rather than part of it: the domain
// types and their persistence live in internal/library, and this package
// depends on them. Nothing in internal/library depends on this package, which
// is what keeps the dependency pointing one way and the library usable without
// a metadata provider at all.
package metadata

import (
	"context"
	"fmt"

	"github.com/ykzird/astraeus/internal/library"
)

// Provider defines the interface for external metadata services.
type Provider interface {
	// Name identifies the provider in a MetadataSet.
	Name() string
	// FetchMetadata retrieves a MetadataSet for a given entity. Returning an
	// error leaves the entity Incomplete so an administrator can see it.
	FetchMetadata(ctx context.Context, entity *library.MediaEntity) (*library.MetadataSet, error)
}

// Chain tries each provider in order and returns the first success. This is how
// a TMDB -> TVDB/IMDB fallback chain is expressed.
type Chain struct {
	providers []Provider
}

// NewChain creates a Chain over the given providers.
func NewChain(providers ...Provider) *Chain {
	return &Chain{providers: providers}
}

// Name reports the chain identity.
func (c *Chain) Name() string { return "chain" }

// FetchMetadata returns the first successful result, or the collected errors.
func (c *Chain) FetchMetadata(ctx context.Context, entity *library.MediaEntity) (*library.MetadataSet, error) {
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

// Mock derives a MetadataSet from the entity itself. It exists so the whole
// library pipeline can be exercised, and demonstrated, without an API key or
// network access.
type Mock struct{}

// NewMock creates a Mock provider.
func NewMock() *Mock { return &Mock{} }

// Name reports the provider identity.
func (m *Mock) Name() string { return "mock" }

// FetchMetadata synthesises metadata from the entity's own name.
func (m *Mock) FetchMetadata(_ context.Context, entity *library.MediaEntity) (*library.MetadataSet, error) {
	extra := map[string]string{}
	for k, v := range fromEntity(entity).Extra {
		extra[k] = v
	}
	extra["synthetic"] = "true"

	return &library.MetadataSet{
		Title:       entity.Name,
		Description: fmt.Sprintf("%s imported from the library. No metadata provider is configured, so this description is a placeholder.", entity.Name),
		Provider:    m.Name(),
		Extra:       extra,
	}, nil
}

// fromEntity returns the metadata the scanner already knows about an entity
// (season/episode numbers, release year) and is never nil.
func fromEntity(entity *library.MediaEntity) *library.MetadataSet {
	if entity.Metadata != nil {
		return entity.Metadata
	}
	return &library.MetadataSet{Extra: map[string]string{}}
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
