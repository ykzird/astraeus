package naming

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// This file holds the pure filename/path parsing rules used by the Scanner. It
// has no I/O so the rules can be tested directly.

var (
	// episodeRe matches S01E02, s1e2, S01.E02, S01 E02 and similar forms.
	episodeRe = regexp.MustCompile(`(?i)\bs(\d{1,2})[\s._-]*e(\d{1,3})\b`)
	// seasonDirRe matches a directory named "Season 1", "season.01", "S1", ...
	seasonDirRe = regexp.MustCompile(`(?i)^(?:season|series|s)[\s._-]*(\d{1,2})$`)
	// junkRe collapses separators commonly used in release names.
	junkRe = regexp.MustCompile(`[\s._]+`)
	// bracketedYearRe finds a year a name *declares*: (2017), [2010], {1995}.
	//
	// A declared year is the strongest signal there is, and looking for it first is
	// what stops "Blade Runner 2049 (2017)" taking 2049 - a number in the title -
	// when the name says 2017 in brackets.
	bracketedYearRe = regexp.MustCompile(`[\[(]\s*((?:19|20)\d{2})\s*[\])]`)
	// releaseHyphenRe matches a hyphen that separates tokens rather than sitting
	// inside a word: space, hyphen, space. A one-sided test split "WEB-DL" into two
	// tokens, which the noise list then failed to recognise, and left the second
	// half in the title.
	releaseHyphenRe = regexp.MustCompile(`\s+-\s+`)
	// resolutionLikeRe matches a bare channel layout such as "7" left from "7.1".
	resolutionLikeRe = regexp.MustCompile(`^\d$|^\d\.\d$`)
	// trailingGroupRe matches a channel count with a release group attached, which
	// is what "1-DTOne" is: "7.1-DTOne" loses its dot to the separator pass and
	// arrives as "7" then "1-DTOne". The digits are a channel count and everything
	// after the hyphen is the group's own name, so neither is a title word.
	trailingGroupRe = regexp.MustCompile(`^\d+-.+$`)
)

// releaseNoiseTokens is the vocabulary a release name uses after the title. It is
// a set rather than a regular expression because the decision is per token: the
// first version of this matched over the whole string and needed the separators,
// the surrounding tokens and the brackets to all be right at once.
var releaseNoiseTokens = map[string]bool{
	// Resolution and source.
	"2160p": true, "1080p": true, "720p": true, "576p": true, "480p": true,
	"4k": true, "uhd": true, "hd": true, "sd": true, "hq": true,
	"bluray": true, "blu-ray": true, "brrip": true, "bdrip": true, "bdremux": true, "remux": true,
	"web": true, "webdl": true, "web-dl": true, "webrip": true, "web-rip": true,
	"hdrip": true, "dvdrip": true,
	"dvd": true, "hdtv": true, "pdtv": true, "tvrip": true,
	// Video codecs and bit depths.
	"x264": true, "x265": true, "h264": true, "h265": true, "hevc": true,
	"avc": true, "xvid": true, "divx": true, "av1": true, "vp9": true,
	"10bit": true, "8bit": true, "hi10p": true,
	// Audio.
	"aac": true, "ac3": true, "eac3": true, "dd": true, "ddp": true,
	"dts": true, "dtshd": true, "dtshdma": true, "dts-hd": true, "dts-hdma": true,
	"truehd": true, "true-hd": true, "atmos": true,
	"flac": true, "mp3": true, "opus": true, "commentary": true,
	// Dynamic range and edition.
	"hdr": true, "hdr10": true, "hdr10+": true, "dv": true, "dolby": true,
	"vision": true, "sdr": true, "remastered": true, "proper": true,
	"repack": true, "extended": true, "uncut": true, "imax": true, "multi": true,
}

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
	parts := SplitPath(relPath)
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
	return parseMovieNameAt(name, time.Now())
}

// parseMovieNameAt is ParseMovieName with the clock supplied.
//
// The plausibility bound depends on the current year, so a test that used the
// real clock would change meaning with the calendar - and one of the cases is a
// title containing 2049, which is implausible now and will be a plausible year
// later.
func parseMovieNameAt(name string, now time.Time) (title string, year int) {
	base := strings.TrimSuffix(name, filepath.Ext(name))

	// A declared year is the name telling us what it means, so it is taken first
	// and taken out: "The Movie (2010) [1080p]" is 2010, whatever else the name
	// holds.
	if m := bracketedYearRe.FindStringSubmatchIndex(base); m != nil {
		if y, err := strconv.Atoi(base[m[2]:m[3]]); err == nil && plausibleYear(y, now.Year()) {
			year = y
			base = base[:m[0]] + " " + base[m[1]:]
		}
	}

	// Then the name is read as tokens. Splitting first and deciding per token is
	// what makes the rest of this predictable: a regular expression over the whole
	// string has to encode the separators, the surrounding tokens and the
	// brackets at once, and gets them wrong in ways that are hard to see. The
	// first version of this removed the separators around the year and took the
	// year with them, which turned "Dune.2021.2160p" into "Dune 2160p".
	tokens := releaseTokens(base)

	kept := make([]string, 0, len(tokens))
	for i, token := range tokens {
		// A bare year counts once something precedes it. "Dune 2021" is 2021;
		// "2001 A Space Odyssey" is a title whose first word is a number, and
		// nothing before it means nothing to attach the year to.
		if year == 0 && i > 0 && isYearToken(token, now.Year()) {
			year, _ = strconv.Atoi(token)
			// The year stays in the title. A bare year is part of the name - it is
			// what tells two films with one title apart - and a declared one has
			// already been taken out above, so reaching here means the name spells
			// it as a word.
		}
		if isReleaseNoise(token) {
			continue
		}
		// "7.1-DTOne" arrives as one token: the channel layout and the release
		// group joined by a hyphen the tokeniser keeps, because that same hyphen is
		// inside "Spider-Man". A token that is a digit, a dot and something after it
		// is the layout, and the layout is noise.
		if trailingGroupRe.MatchString(token) {
			continue
		}
		kept = append(kept, token)
	}

	// Brackets that survived are release tags as well: "The Movie [1080p]" leaves
	// a token "[1080p]" that the noise list sees as a bare resolution.
	return cleanTitle(strings.Join(kept, " ")), year
}

// releaseTokens splits a file name into the words a release name is made of.
//
// Separators are the dots, underscores and bracketed spacing that release names
// use in place of spaces. A hyphen is only a separator when it is not inside a
// word, because "Spider-Man" and "DTS-HDMA" both contain one and they are not the
// same kind of hyphen.
func releaseTokens(base string) []string {
	// A *video* extension goes first. It survives the separator pass as an ordinary
	// word - "Dune.2021.2160p.mkv" tokenises to a trailing "mkv" - and a stray
	// "mkv" in the title is the kind of thing that is obvious only after seeing it.
	//
	// Only a known extension, because filepath.Ext is a dot-separator rather than
	// a file-type test: on a name whose extension has already been removed it
	// answers ".2016" for "Arrival.2016" and strips the year. The caller removes
	// the extension first, so this is a second, narrower pass for direct callers.
	if ext := filepath.Ext(base); VideoExtensions[strings.ToLower(ext)] {
		base = strings.TrimSuffix(base, ext)
	}
	// Brackets become spaces rather than being removed, so "[1080p]" contributes
	// its own token and can be judged like any other.
	base = strings.NewReplacer("(", " ", ")", " ", "[", " ", "]", " ", "{", " ", "}", " ").Replace(base)
	base = strings.ReplaceAll(base, "_", " ")
	base = strings.ReplaceAll(base, ".", " ")
	base = releaseHyphenRe.ReplaceAllString(base, " ")
	return strings.Fields(base)
}

// isYearToken reports whether a token is a four-digit year that could be one.
func isYearToken(token string, currentYear int) bool {
	if len(token) != 4 {
		return false
	}
	y, err := strconv.Atoi(token)
	if err != nil {
		return false
	}
	return plausibleYear(y, currentYear)
}

// isReleaseNoise reports whether a token is a release tag rather than a title
// word. The list is the vocabulary release names actually use; anything else is
// kept, because a title word wrongly dropped is worse than a tag wrongly kept.
func isReleaseNoise(token string) bool {
	lower := strings.ToLower(token)
	if releaseNoiseTokens[lower] {
		return true
	}
	// A bare "7.1" or "5 1" survived tokenising as digits and a dot.
	if resolutionLikeRe.MatchString(lower) {
		return true
	}
	// A release group lands as the last hyphen-separated word, which tokenising
	// leaves attached: "x265-DTOne" contributes "DTOne".
	return false
}

// plausibleYear reports whether a four-digit number can be a release year.
//
// The upper bound is next year rather than this one, because a film announced for
// next year is released and named before the year arrives. The lower bound is the
// start of the twentieth century in the pattern, which is as far back as a film
// title is worth guessing at.
func plausibleYear(year, currentYear int) bool {
	return year >= 1900 && year <= currentYear+1
}

// TitleFromPath derives a readable display name from a media file path: the
// base name without its extension, and without release-name noise such as
// "1080p" or "x265".
//
// The scanner names new entities with it, and the migration that backfills
// names for entities written by earlier versions uses it too, so an entity
// renamed by a migration ends up with the same name a scan would give it.
func TitleFromPath(path string) string {
	base := filepath.Base(path)
	return cleanTitle(strings.TrimSuffix(base, filepath.Ext(base)))
}

// cleanTitle turns separator-heavy release names into readable titles.
func cleanTitle(s string) string {
	s = strings.Trim(s, " ._-")
	s = junkRe.ReplaceAllString(s, " ")
	s = strings.TrimSpace(s)
	return s
}

// SplitPath breaks a path into its meaningful components, dropping empty
// segments and "." / ".." so that a relative path and an absolute one decompose
// the same way. Callers use it to reason about where a file sits - for example
// which folder names wrap it - without depending on the separator or on how the
// path was spelled.
func SplitPath(p string) []string {
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
