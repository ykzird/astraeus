package library

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// This file holds the pure filename/path parsing rules used by the Scanner. It
// has no I/O so the rules can be tested directly.

var (
	// episodeRe matches S01E02, s1e2, S01.E02, S01 E02 and similar forms.
	episodeRe = regexp.MustCompile(`(?i)\bs(\d{1,2})[\s._-]*e(\d{1,3})\b`)
	// seasonDirRe matches a directory named "Season 1", "season.01", "S1", ...
	seasonDirRe = regexp.MustCompile(`(?i)^(?:season|series|s)[\s._-]*(\d{1,2})$`)
	// yearRe captures a trailing (2020) / .2020. release year.
	yearRe = regexp.MustCompile(`[\(\[\.\s_-]((?:19|20)\d{2})[\)\]\.\s_-]*$`)
	// junkRe collapses separators commonly used in release names.
	junkRe = regexp.MustCompile(`[\s._]+`)
)

// VideoExtensions is the set of file extensions the Scanner treats as media.
var VideoExtensions = map[string]bool{
	".mp4":  true,
	".mkv":  true,
	".m4v":  true,
	".avi":  true,
	".webm": true,
	".mov":  true,
}

// MimeTypeForExt maps a lowercase file extension to a MIME type.
func MimeTypeForExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".mp4", ".m4v", ".mov":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".webm":
		return "video/webm"
	case ".avi":
		return "video/x-msvideo"
	default:
		return "application/octet-stream"
	}
}

// IsVideoFile reports whether the path has a recognised video extension.
func IsVideoFile(path string) bool {
	return VideoExtensions[strings.ToLower(filepath.Ext(path))]
}

// IsIgnored reports whether a path component should be skipped during a scan.
// Hidden entries, sample folders and metadata sidecar directories are excluded.
func IsIgnored(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	switch strings.ToLower(name) {
	case "sample", "samples", "extras", "featurettes", "behind the scenes":
		return true
	default:
		return false
	}
}

// EpisodeInfo is the result of parsing an episode file name or path.
type EpisodeInfo struct {
	Series  string // series name, empty when not derivable
	Season  int    // season number, 0 when unknown
	Episode int    // episode number, 0 when unknown
	Title   string // episode title, empty when not derivable
}

// ParseEpisodeName extracts season/episode numbers and a title from a file name
// such as "S02E05 - The Long Goodbye.mkv".
func ParseEpisodeName(fileName string) (EpisodeInfo, bool) {
	base := strings.TrimSuffix(fileName, filepath.Ext(fileName))

	m := episodeRe.FindStringSubmatchIndex(base)
	if m == nil {
		return EpisodeInfo{}, false
	}

	season, errS := strconv.Atoi(base[m[2]:m[3]])
	episode, errE := strconv.Atoi(base[m[4]:m[5]])
	if errS != nil || errE != nil {
		return EpisodeInfo{}, false
	}

	// Only the text after the SxxExx marker is a plausible episode title. The
	// text before it is the series name, which the caller already has, so an
	// absent title is returned as empty rather than guessed at.
	title := cleanTitle(strings.Trim(base[m[1]:], " ._-"))

	return EpisodeInfo{Season: season, Episode: episode, Title: title}, true
}

// ParseEpisodePath derives the full placement of an episode file from its path
// relative to the library root, e.g.
//
//	"Breaking Bad/Season 02/Breaking Bad S02E05 - Breakage.mkv"
//
// yields Series "Breaking Bad", Season 2, Episode 5, Title "Breakage". The
// series name always comes from the first path component; this is what keeps a
// "Season 1" of one series from being conflated with a "Season 1" of another.
//
// A path with no directory component has no series to attach to, so it is
// rejected; ParseEpisodeName handles bare file names.
func ParseEpisodePath(relPath string) (EpisodeInfo, bool) {
	parts := splitPath(relPath)
	if len(parts) < 2 {
		return EpisodeInfo{}, false
	}

	fileName := parts[len(parts)-1]
	info, ok := ParseEpisodeName(fileName)
	if !ok {
		return EpisodeInfo{}, false
	}

	info.Series = cleanTitle(parts[0])

	// A "Season NN" directory wins over the number embedded in the file name:
	// it is the more explicit signal and is what the folder layout encodes.
	if len(parts) >= 3 {
		if n, ok := parseSeasonDir(parts[len(parts)-2]); ok {
			info.Season = n
		}
	}

	return info, true
}

// parseSeasonDir extracts a season number from a directory name.
func parseSeasonDir(dir string) (int, bool) {
	m := seasonDirRe.FindStringSubmatch(dir)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// SeasonDirName renders the canonical directory/entity name for a season.
func SeasonDirName(season int) string {
	return "Season " + strconv.Itoa(season)
}

// ParseMovieName derives a movie title and optional year from a file name or
// folder name such as "Blade Runner 2049 (2017).mkv".
func ParseMovieName(name string) (title string, year int) {
	base := strings.TrimSuffix(name, filepath.Ext(name))
	base = strings.TrimSpace(base)

	if m := yearRe.FindStringSubmatch(base); m != nil {
		if y, err := strconv.Atoi(m[1]); err == nil {
			year = y
			base = strings.TrimSpace(base[:len(base)-len(m[0])])
		}
	}

	return cleanTitle(base), year
}

// cleanTitle turns separator-heavy release names into readable titles.
func cleanTitle(s string) string {
	s = strings.Trim(s, " ._-")
	s = junkRe.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	return s
}

func splitPath(p string) []string {
	p = filepath.ToSlash(filepath.Clean(p))
	if p == "." || p == "/" {
		return nil
	}
	p = strings.TrimPrefix(p, "/")
	parts := strings.Split(p, "/")

	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			continue
		}
		out = append(out, part)
	}
	return out
}
