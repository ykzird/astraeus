package naming

import (
	"testing"
	"time"
)

func TestParseEpisodeName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		fileName    string
		wantOK      bool
		wantSeason  int
		wantEpisode int
		wantTitle   string
	}{
		{
			name:        "dotted release name",
			fileName:    "Breaking.Bad.S02E05.720p.WEB.mkv",
			wantOK:      true,
			wantSeason:  2,
			wantEpisode: 5,
			wantTitle:   "720p WEB",
		},
		{
			name:        "dash separated with title",
			fileName:    "S02E05 - The Long Goodbye.mkv",
			wantOK:      true,
			wantSeason:  2,
			wantEpisode: 5,
			wantTitle:   "The Long Goodbye",
		},
		{
			name:        "lowercase single digits",
			fileName:    "show.s1e2.mkv",
			wantOK:      true,
			wantSeason:  1,
			wantEpisode: 2,
			wantTitle:   "", // nothing but the series name follows the marker
		},
		{
			name:        "space separated marker",
			fileName:    "Series S01 E10 Title Here.mp4",
			wantOK:      true,
			wantSeason:  1,
			wantEpisode: 10,
			wantTitle:   "Title Here",
		},
		{
			name:     "no episode marker",
			fileName: "Some Random Movie.mkv",
			wantOK:   false,
		},
		{
			name:     "season without episode",
			fileName: "Show S02.mkv",
			wantOK:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := ParseEpisodeName(tt.fileName)
			if ok != tt.wantOK {
				t.Fatalf("ParseEpisodeName(%q) ok = %v, want %v", tt.fileName, ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if got.Season != tt.wantSeason {
				t.Errorf("season = %d, want %d", got.Season, tt.wantSeason)
			}
			if got.Episode != tt.wantEpisode {
				t.Errorf("episode = %d, want %d", got.Episode, tt.wantEpisode)
			}
			if got.Title != tt.wantTitle {
				t.Errorf("title = %q, want %q", got.Title, tt.wantTitle)
			}
		})
	}
}

func TestParseEpisodePath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		path        string
		wantOK      bool
		wantSeries  string
		wantSeason  int
		wantEpisode int
		wantTitle   string
	}{
		{
			name:        "series and season directory",
			path:        "Breaking Bad/Season 02/Breaking Bad S02E05 - Breakage.mkv",
			wantOK:      true,
			wantSeries:  "Breaking Bad",
			wantSeason:  2,
			wantEpisode: 5,
			wantTitle:   "Breakage",
		},
		{
			name:        "series directory only, season from file name",
			path:        "The Wire/The.Wire.S03E07.mkv",
			wantOK:      true,
			wantSeries:  "The Wire",
			wantSeason:  3,
			wantEpisode: 7,
			wantTitle:   "",
		},
		{
			name:        "season directory wins over file name season",
			path:        "Show/Season 1/Show.S09E01.mkv",
			wantOK:      true,
			wantSeries:  "Show",
			wantSeason:  1, // the explicit Season folder is authoritative
			wantEpisode: 1,
			wantTitle:   "",
		},
		{
			name:   "no series component",
			path:   "S01E01.mkv",
			wantOK: false,
		},
		{
			name:   "not an episode",
			path:   "Movies/Some Film (2019).mkv",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, ok := ParseEpisodePath(tt.path)
			if ok != tt.wantOK {
				t.Fatalf("ParseEpisodePath(%q) ok = %v, want %v", tt.path, ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if got.Series != tt.wantSeries {
				t.Errorf("series = %q, want %q", got.Series, tt.wantSeries)
			}
			if got.Season != tt.wantSeason {
				t.Errorf("season = %d, want %d", got.Season, tt.wantSeason)
			}
			if got.Episode != tt.wantEpisode {
				t.Errorf("episode = %d, want %d", got.Episode, tt.wantEpisode)
			}
			if got.Title != tt.wantTitle {
				t.Errorf("title = %q, want %q", got.Title, tt.wantTitle)
			}
		})
	}
}

func TestParseEpisodePath_SeasonDirectoryTakesPrecedence(t *testing.T) {
	t.Parallel()

	// The explicit folder layout is the stronger signal: a file named
	// "Show.S01E01.mkv" inside "Season 09" belongs to season 9.
	got, ok := ParseEpisodePath("Show/Season 09/Show.S01E01.mkv")
	if !ok {
		t.Fatal("expected the path to parse as an episode")
	}
	if got.Season != 9 {
		t.Errorf("season = %d, want 9 from the Season directory", got.Season)
	}
	if got.Episode != 1 {
		t.Errorf("episode = %d, want 1 from the file name", got.Episode)
	}

	got, ok = ParseEpisodePath("Show/Season 09/pilot.mkv")
	if ok {
		t.Fatalf("expected a file without an episode marker to be rejected, got %+v", got)
	}
}

func TestParseMovieName(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     string
		wantTitle string
		wantYear  int
	}{
		{name: "title with year in parentheses", input: "Blade Runner 2049 (2017).mkv", wantTitle: "Blade Runner 2049", wantYear: 2017},
		{
			// The number in the title is a title; the year is the one the name
			// declares. Reading 2049 as the year both cut the title in half and
			// looked the film up as a 2049 release (L-17).
			name:      "a number in the title is not the year",
			input:     "Blade Runner 2049.mkv",
			wantTitle: "Blade Runner 2049",
			wantYear:  0,
		},
		{
			// The review's example: the resolution follows the year, so a pattern
			// anchored to the end of the name found nothing at all.
			name:      "a dotted release with the year mid-name",
			input:     "Dune.2021.2160p.WEB-DL.mkv",
			wantTitle: "Dune 2021",
			wantYear:  2021,
		},
		{
			// The review's other example: the year is bracketed and something
			// follows it.
			name:      "a bracketed year with noise after it",
			input:     "The.Movie.(2010).[1080p].mkv",
			wantTitle: "The Movie",
			wantYear:  2010,
		},
		{
			// The local database held this one verbatim, release group and all.
			name:      "a full release name leaves a title",
			input:     "Battleship.2012.UHD.BluRay.2160p.x265.HDR.DTS-HDMA.7.1-DTOne.mkv",
			wantTitle: "Battleship 2012",
			wantYear:  2012,
		},
		{
			// A bare year stays in the title. There is no way to tell "Arrival 2016"
			// from "Blade Runner 2049" by shape alone - both are a title and a
			// number - so the rule is one rule: a year the name brackets is a
			// release marker and comes out, and a year the name spells as a word is
			// part of the name and stays. It is also what stops two films of one
			// title losing the only thing that told them apart.
			name:      "a trailing bare year stays in the title",
			input:     "Arrival.2016.mkv",
			wantTitle: "Arrival 2016",
			wantYear:  2016,
		},
		{name: "no year", input: "Whiplash.mkv", wantTitle: "Whiplash", wantYear: 0},
		{name: "year in the middle is not a release year", input: "2001 A Space Odyssey.mkv", wantTitle: "2001 A Space Odyssey", wantYear: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			title, year := parseMovieNameAt(tt.input, time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC))
			if title != tt.wantTitle {
				t.Errorf("title = %q, want %q", title, tt.wantTitle)
			}
			if year != tt.wantYear {
				t.Errorf("year = %d, want %d", year, tt.wantYear)
			}
		})
	}
}

func TestIsVideoFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		path string
		want bool
	}{
		{"movie.mkv", true},
		{"movie.MP4", true},
		{"movie.avi", true},
		{"notes.txt", false},
		{"poster.jpg", false},
		{"noextension", false},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			if got := IsVideoFile(tt.path); got != tt.want {
				t.Errorf("IsVideoFile(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

// TestIsIgnored covers the directory rule.
//
// "extras" must not be in it. Extras is a name an actual series has - Ricky
// Gervais's - and skipping the directory dropped that series from the library with
// no warning at all, which is L-17 of the 2026-10-09 review.
func TestIsIgnored(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want bool
	}{
		{".hidden", true},
		{"Sample", true},
		{"Featurettes", true},
		{"behind the scenes", true},
		// The word that is also a series name.
		{"Extras", false},
		{"extras", false},
		{"Season 01", false},
		{"Breaking Bad", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsIgnored(tt.name); got != tt.want {
				t.Errorf("IsIgnored(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// TestIsIgnoredFile covers the file rule, which is a different list because the
// same word means different things at the two levels.
func TestIsIgnoredFile(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want bool
	}{
		// Samples are named for the title they came from, so a whole-name test
		// matched none of these and the samples stayed in the library.
		{"Dune.2021.2160p.sample.mkv", true},
		{"Dune-sample.mkv", true},
		{"Dune_sample_1080p.mkv", true},
		{"Sample.mkv", true},
		{"sample-1.mkv", true},
		// A word that merely starts the same way is not the token.
		{"Sampler.mkv", false},
		{"Dune.2021.2160p.mkv", false},
		// "The Sample Maker" is skipped, and that is the deliberate cost of the
		// rule: nothing in a name distinguishes it from "Dune sample 1080p". The
		// comment on IsIgnoredFile records the trade.
		{"The Sample Maker.mkv", true},
		// Supplementary files.
		{"Extras.mkv", true},
		{"trailer.mkv", true},
		{".hidden.mkv", true},
		{"S01E01.mkv", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := IsIgnoredFile(tt.name); got != tt.want {
				t.Errorf("IsIgnoredFile(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestMimeTypeForExt(t *testing.T) {
	t.Parallel()

	tests := []struct {
		ext  string
		want string
	}{
		{".mp4", "video/mp4"},
		{".MKV", "video/x-matroska"},
		{".webm", "video/webm"},
		{".avi", "video/x-msvideo"},
		{".xyz", "application/octet-stream"},
	}

	for _, tt := range tests {
		t.Run(tt.ext, func(t *testing.T) {
			t.Parallel()
			if got := MimeTypeForExt(tt.ext); got != tt.want {
				t.Errorf("MimeTypeForExt(%q) = %q, want %q", tt.ext, got, tt.want)
			}
		})
	}
}

func TestSeasonDirName(t *testing.T) {
	t.Parallel()

	if got := SeasonDirName(3); got != "Season 3" {
		t.Errorf("SeasonDirName(3) = %q, want %q", got, "Season 3")
	}
}

// TestParseEpisodeName_FormsTheReviewFoundUnsupported covers the episode half of
// L-17.
//
// The pattern used `\b` at its left edge, and `_` is a word character - so
// "Show_S01E01_Pilot" has no boundary before the S and was rejected outright, which
// is the shape a great many releases use. Four other forms were not recognised at
// all.
func TestParseEpisodeName_FormsTheReviewFoundUnsupported(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		fileName    string
		wantOK      bool
		wantSeason  int
		wantEpisode int
		wantTitle   string
	}{
		{
			// The form the word boundary rejected.
			name:     "underscore separated",
			fileName: "Show_S01E01_Pilot.mkv",
			wantOK:   true, wantSeason: 1, wantEpisode: 1, wantTitle: "Pilot",
		},
		{
			// A two-episode file takes the last number: the season's numbering
			// continues from it, and that is the episode a viewer is looking for
			// next.
			name:     "two episodes in one file",
			fileName: "Show.S01E01E02.mkv",
			wantOK:   true, wantSeason: 1, wantEpisode: 2,
		},
		{
			// The form that predates SxxExx.
			name:     "the 1x02 form",
			fileName: "Show 1x02 - Descent.mkv",
			wantOK:   true, wantSeason: 1, wantEpisode: 2, wantTitle: "Descent",
		},
		{
			// A year used as the season, which is how long-running shows number
			// themselves. The two-digit pattern must not read this as season 24.
			name:     "a four-digit season",
			fileName: "Show.S2024E01.mkv",
			wantOK:   true, wantSeason: 2024, wantEpisode: 1,
		},
		{
			// The revision suffix is not part of the episode number.
			name:     "a revision suffix",
			fileName: "Show.S01E01v2.mkv",
			wantOK:   true, wantSeason: 1, wantEpisode: 1,
		},
		{
			// A season number of more than two digits is not a season, so this is
			// not an episode at all rather than a season 1 episode.
			name:     "a title that only looks like a marker",
			fileName: "Malcolm X.mkv",
			wantOK:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			info, ok := ParseEpisodeName(tt.fileName)
			if ok != tt.wantOK {
				t.Fatalf("ParseEpisodeName(%q) ok = %v, want %v (info %+v)",
					tt.fileName, ok, tt.wantOK, info)
			}
			if !ok {
				return
			}
			if info.Season != tt.wantSeason {
				t.Errorf("season = %d, want %d", info.Season, tt.wantSeason)
			}
			if info.Episode != tt.wantEpisode {
				t.Errorf("episode = %d, want %d", info.Episode, tt.wantEpisode)
			}
			if tt.wantTitle != "" && info.Title != tt.wantTitle {
				t.Errorf("title = %q, want %q", info.Title, tt.wantTitle)
			}
		})
	}
}

// TestParseEpisodeName_StillRejectsWhatItShould guards the widened patterns
// against being widened too far.
func TestParseEpisodeName_StillRejectsWhatItShould(t *testing.T) {
	t.Parallel()

	for _, fileName := range []string{
		"Show.mkv",
		"S01.mkv", // a season with no episode
		"Season 1.mkv",
		"S01E.mkv",
		"Ex01.mkv", // no digits
		"Malcolm X (1992).mkv",
	} {
		if info, ok := ParseEpisodeName(fileName); ok {
			t.Errorf("ParseEpisodeName(%q) matched as %+v, want no match", fileName, info)
		}
	}
}
