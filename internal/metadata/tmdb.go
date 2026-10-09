package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// maxResponseBytes bounds a provider response. TMDB's largest payload for one
// title is a few tens of kilobytes; a megabyte is generous room for a provider
// that sends more than it needs to.
const maxResponseBytes = 1 << 20

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

// redactURLError strips the credential out of a transport error before it is
// wrapped and logged.
//
// net/http reports a failed request as *url.Error, whose Error() string embeds
// the full URL - and this provider's URL carries the API key as a query
// parameter, because that is how the v3 API authenticates. "TMDB is
// unreachable" is therefore also "the key is in the log", which a log
// aggregator then keeps. The wrapped error keeps the cause, the operation and
// the redacted URL, which is everything the message needed.
func redactURLError(err error) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return err
	}
	redacted := *urlErr
	redacted.URL = redactQuery(urlErr.URL)
	return &redacted
}

// redactQuery replaces every query value with "REDACTED", keeping the parameter
// names. The key is not the only secret a query can carry, so the rule is to
// keep none of them.
func redactQuery(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "REDACTED"
	}
	query := parsed.Query()
	for name := range query {
		query.Set(name, "REDACTED")
	}
	parsed.RawQuery = query.Encode()
	return parsed.String()
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
		return nil, fmt.Errorf("tmdb: requesting %s: %w", kind, redactURLError(err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tmdb: %s search for %q returned HTTP %d", kind, title, resp.StatusCode)
	}

	var payload tmdbSearchResponse
	// Bounded, because the body is somebody else's and "somebody else's" is not a
	// size guarantee. A provider that answers with something enormous - a
	// misconfiguration, a proxy's error page, a hostile response - would otherwise
	// be decoded into memory without limit (L-18 of the 2026-10-09 review).
	limited := io.LimitReader(resp.Body, maxResponseBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("tmdb: reading the %s search response: %w", kind, err)
	}
	if int64(len(body)) > maxResponseBytes {
		return nil, fmt.Errorf("tmdb: the %s search response exceeded %d bytes", kind, maxResponseBytes)
	}
	if err := json.Unmarshal(body, &payload); err != nil {
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
