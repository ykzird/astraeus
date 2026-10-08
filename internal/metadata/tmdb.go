package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/ykzird/astraeus/internal/library"
)

// TMDBBaseURL is the public TMDB API endpoint. It is a field on the provider so
// tests can point it at an httptest server.
const TMDBBaseURL = "https://api.themoviedb.org/3"

// TMDB implements Provider using the TMDB API.
type TMDB struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

// NewTMDB creates a TMDB provider. An empty apiKey is a programming error at
// this level; callers should select a Mock instead.
func NewTMDB(apiKey string) *TMDB {
	return &TMDB{
		apiKey:     apiKey,
		baseURL:    TMDBBaseURL,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// Name reports the provider identity.
func (p *TMDB) Name() string { return "tmdb" }

// tmdbResult covers the fields shared by TMDB's movie and TV search results.
type tmdbResult struct {
	ID           int    `json:"id"`
	Title        string `json:"title"` // movies
	Name         string `json:"name"`  // TV
	Overview     string `json:"overview"`
	PosterPath   string `json:"poster_path"`
	BackdropPath string `json:"backdrop_path"`
	ReleaseDate  string `json:"release_date"`   // movies
	FirstAirDate string `json:"first_air_date"` // TV
}

type tmdbSearchResponse struct {
	Results []tmdbResult `json:"results"`
}

// FetchMetadata looks the entity up on TMDB.
func (p *TMDB) FetchMetadata(ctx context.Context, entity *library.MediaEntity) (*library.MetadataSet, error) {
	if p.apiKey == "" {
		return nil, fmt.Errorf("tmdb: no API key configured")
	}

	switch entity.Type {
	case library.MovieEntity:
		return p.search(ctx, "movie", entity)
	case library.SeriesEntity:
		return p.search(ctx, "tv", entity)
	case library.SeasonEntity, library.EpisodeEntity:
		// Episode- and season-level lookups would multiply API calls for
		// little gain at this stage; the parent series carries the imagery
		// and these entities keep the numbers the scanner derived.
		meta := fromEntity(entity)
		derived := &library.MetadataSet{
			Title:    entity.Name,
			Provider: p.Name(),
			Extra:    map[string]string{},
		}
		for k, v := range meta.Extra {
			derived.Extra[k] = v
		}
		return derived, nil
	default:
		return nil, fmt.Errorf("tmdb: unsupported entity type %q", entity.Type)
	}
}

func (p *TMDB) search(ctx context.Context, kind string, entity *library.MediaEntity) (*library.MetadataSet, error) {
	title := entity.DisplayTitle()
	if title == "" {
		return nil, fmt.Errorf("tmdb: entity %s has no title to search for", entity.ID)
	}

	endpoint, err := url.Parse(p.baseURL + "/search/" + kind)
	if err != nil {
		return nil, fmt.Errorf("tmdb: building request URL: %w", err)
	}
	query := endpoint.Query()
	query.Set("query", title)
	query.Set("api_key", p.apiKey)
	if year := fromEntity(entity).Extra["year"]; year != "" && kind == "movie" {
		query.Set("year", year)
	}
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("tmdb: building request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tmdb: requesting %s: %w", kind, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tmdb: %s search for %q returned HTTP %d", kind, title, resp.StatusCode)
	}

	var payload tmdbSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("tmdb: decoding %s search response: %w", kind, err)
	}
	if len(payload.Results) == 0 {
		return nil, fmt.Errorf("tmdb: no %s result for %q: %w", kind, title, library.ErrNotFound)
	}

	best := payload.Results[0]
	displayTitle := best.Title
	date := best.ReleaseDate
	if kind == "tv" {
		displayTitle = best.Name
		date = best.FirstAirDate
	}
	if displayTitle == "" {
		displayTitle = title
	}

	extra := map[string]string{"tmdb_id": strconv.Itoa(best.ID)}
	if len(date) >= 4 {
		extra["year"] = date[:4]
	}

	return &library.MetadataSet{
		Title:        displayTitle,
		Description:  best.Overview,
		PosterPath:   best.PosterPath,
		BackdropPath: best.BackdropPath,
		Provider:     p.Name(),
		Extra:        extra,
	}, nil
}
