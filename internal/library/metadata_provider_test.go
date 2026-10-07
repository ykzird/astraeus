package library

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// stubProvider is a MetadataProvider test double shared by the provider and
// worker tests.
type stubProvider struct {
	name string
	meta *MetadataSet
	err  error
}

func (s *stubProvider) Name() string { return s.name }

func (s *stubProvider) FetchMetadata(context.Context, *MediaEntity) (*MetadataSet, error) {
	return s.meta, s.err
}

func TestMockProvider_DerivesMetadataFromEntity(t *testing.T) {
	t.Parallel()

	provider := NewMockProvider()
	entity := &MediaEntity{
		ID:       "entity-6",
		Type:     EpisodeEntity,
		Name:     "S01E01",
		Metadata: &MetadataSet{Extra: map[string]string{"season": "1", "episode": "1"}},
	}

	meta, err := provider.FetchMetadata(context.Background(), entity)
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}
	if meta.Title != "S01E01" {
		t.Errorf("title = %q, want S01E01", meta.Title)
	}
	if meta.Provider != "mock" {
		t.Errorf("provider = %q, want mock", meta.Provider)
	}
	if meta.Extra["episode"] != "1" {
		t.Errorf("episode = %q, want the scanner-derived value to survive", meta.Extra["episode"])
	}
	if meta.Extra["synthetic"] != "true" {
		t.Error("mock metadata should be marked synthetic")
	}
}

func TestChainProvider_FallsBackToTheNextProvider(t *testing.T) {
	t.Parallel()

	primary := &stubProvider{name: "primary", err: errors.New("primary is down")}
	secondary := &stubProvider{name: "secondary", meta: &MetadataSet{Title: "From secondary", Provider: "secondary"}}

	chain := NewChainProvider(primary, secondary)
	meta, err := chain.FetchMetadata(context.Background(), &MediaEntity{ID: "e", Type: MovieEntity, Name: "Dune"})
	if err != nil {
		t.Fatalf("FetchMetadata: %v", err)
	}
	if meta.Title != "From secondary" {
		t.Errorf("title = %q, want the secondary provider's result", meta.Title)
	}
}

func TestChainProvider_ReportsEveryFailure(t *testing.T) {
	t.Parallel()

	chain := NewChainProvider(
		&stubProvider{name: "primary", err: errors.New("primary is down")},
		&stubProvider{name: "secondary", err: errors.New("secondary is down")},
	)
	_, err := chain.FetchMetadata(context.Background(), &MediaEntity{ID: "e", Type: MovieEntity, Name: "Dune"})
	if err == nil {
		t.Fatal("expected an error when every provider fails")
	}
	for _, want := range []string{"primary", "secondary"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}
}
