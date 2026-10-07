package library

import "testing"

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
		{name: "dotted release", input: "Dune.2021.2160p.WEB-DL.mkv", wantTitle: "Dune 2021 2160p WEB-DL", wantYear: 0},
		{name: "trailing year only", input: "Arrival.2016.mkv", wantTitle: "Arrival", wantYear: 2016},
		{name: "no year", input: "Whiplash.mkv", wantTitle: "Whiplash", wantYear: 0},
		{name: "year in the middle is not a release year", input: "2001 A Space Odyssey.mkv", wantTitle: "2001 A Space Odyssey", wantYear: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			title, year := ParseMovieName(tt.input)
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

func TestIsIgnored(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		want bool
	}{
		{".hidden", true},
		{"Sample", true},
		{"extras", true},
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
