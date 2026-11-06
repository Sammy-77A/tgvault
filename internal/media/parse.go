// Package media works out what a downloaded video file is: a movie or an
// episode, which title, which season and episode, from its name and folders.
package media

import (
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Kind says what a file looks like.
type Kind int

const (
	Unknown Kind = iota
	Movie
	Episode
)

func (k Kind) String() string {
	switch k {
	case Movie:
		return "movie"
	case Episode:
		return "episode"
	}
	return "unknown"
}

// Info is what the name of one file tells us.
type Info struct {
	Kind     Kind
	Title    string // as written, separators turned into spaces
	Year     int    // 0 when absent
	Season   int    // meaningful when HasSeason
	Episodes []int  // more than one for a file holding several episodes
	Ext      string // lower case, with the dot
	Subtitle bool   // a .srt/.vtt/... file that belongs next to a video
	// Weak is set when part of the answer was guessed from a folder name or
	// from very little text, so the caller should not trust it blindly.
	Weak      bool
	HasSeason bool
}

var videoExt = map[string]bool{".mkv": true, ".mp4": true, ".avi": true, ".mov": true, ".m4v": true,
	".webm": true, ".ts": true, ".wmv": true, ".mpg": true, ".mpeg": true, ".flv": true}
var subExt = map[string]bool{".srt": true, ".vtt": true, ".ass": true, ".ssa": true, ".sub": true}

// IsVideo reports whether the extension is one we organize.
func IsVideo(ext string) bool { return videoExt[strings.ToLower(ext)] }

// IsSubtitle reports whether the extension is a sidecar subtitle.
func IsSubtitle(ext string) bool { return subExt[strings.ToLower(ext)] }

var (
	// S01E02, S01E02E03, S01E02-E03, S01E02-03, s1.e2
	reSxE = regexp.MustCompile(`(?i)\bS(\d{1,2})[ ._-]?E(\d{1,3})((?:[ ._-]?-?[ ._-]?E?\d{1,3}\b)*)`)
	// 1x02, 1x02-03
	reNxM = regexp.MustCompile(`(?i)\b(\d{1,2})x(\d{2,3})(-\d{2,3})?\b`)
	// Season 1 Episode 2, Season 1 Ep 2
	reWords = regexp.MustCompile(`(?i)\bSeason[ ._-]?(\d{1,2})[ ._-]+(?:Episode|Ep)[ ._-]?(\d{1,3})\b`)
	// a lone episode marker: "Episode 03", "Ep03", "E03"
	reEpOnly = regexp.MustCompile(`(?i)^(?:.*?[ ._-])?(?:Episode|Ep|E)[ ._-]?(\d{1,3})(?:[ ._-].*)?$`)
	// season words in a folder name: "Season 01", "Shameless Season 11", "S02"
	reSeasonDir = regexp.MustCompile(`(?i)^(.*?)[ ._-]*\b(?:Season|Series|S)[ ._-]?(\d{1,2})\b`)
	reYear      = regexp.MustCompile(`(?:^|[\s(\[.])((?:19|20)\d{2})(?:$|[\s)\].])`)
	reBracket   = regexp.MustCompile(`\[[^\]]*\]|\([^)]*\)`)
	reSpace     = regexp.MustCompile(`\s+`)
)

// tags that mark the end of the title and the start of release noise.
var noise = map[string]bool{
	"480p": true, "576p": true, "720p": true, "1080p": true, "1080i": true, "2160p": true, "4k": true, "uhd": true,
	"bluray": true, "blu": true, "brrip": true, "bdrip": true, "webrip": true, "web": true, "webdl": true, "hdtv": true,
	"hdrip": true, "dvdrip": true, "dvdscr": true, "hdcam": true, "cam": true, "hdts": true, "remux": true,
	"x264": true, "x265": true, "h264": true, "h265": true, "hevc": true, "avc": true, "xvid": true, "10bit": true,
	"aac": true, "aac5": true, "ac3": true, "dts": true, "ddp5": true, "dd5": true, "atmos": true, "6ch": true, "5": true,
	"nf": true, "amzn": true, "dsnp": true, "hmax": true, "atvp": true, "hulu": true,
	"yify": true, "yts": true, "rarbg": true, "psa": true, "ettv": true, "eztv": true,
	"repack": true, "proper": true, "unrated": true, "remastered": true, "extended": true, "directors": true,
	"hdr": true, "hdr10": true, "dv": true, "imax": true, "multi": true, "dual": true, "subbed": true, "dubbed": true,
}

// Parse reads one file name (no folders). Use ParseIn when the folders it sits
// in may help.
func Parse(name string) Info {
	base := path.Base(strings.ReplaceAll(name, `\`, "/"))
	ext := strings.ToLower(path.Ext(base))
	stem := strings.TrimSuffix(base, path.Ext(base))
	in := Info{Ext: ext, Subtitle: subExt[ext]}

	// Square brackets and parentheses hold release groups and site tags, but a
	// bracketed year like "(1998)" is a real clue: keep it.
	year := 0
	if m := regexp.MustCompile(`[(\[]((?:19|20)\d{2})[)\]]`).FindStringSubmatch(stem); m != nil {
		year, _ = strconv.Atoi(m[1])
	}
	clean := reBracket.ReplaceAllString(stem, " ")
	clean = strings.NewReplacer("_", " ", ".", " ").Replace(clean)
	clean = reSpace.ReplaceAllString(strings.TrimSpace(clean), " ")

	// Episode markers come first: they end the title.
	var cut int = -1
	if m := reSxE.FindStringSubmatchIndex(clean); m != nil {
		in.Kind = Episode
		in.Season, _ = strconv.Atoi(clean[m[2]:m[3]])
		in.HasSeason = true
		first, _ := strconv.Atoi(clean[m[4]:m[5]])
		in.Episodes = expand(first, clean[m[6]:m[7]])
		cut = m[0]
	} else if m := reWords.FindStringSubmatchIndex(clean); m != nil {
		in.Kind = Episode
		in.Season, _ = strconv.Atoi(clean[m[2]:m[3]])
		in.HasSeason = true
		e, _ := strconv.Atoi(clean[m[4]:m[5]])
		in.Episodes = []int{e}
		cut = m[0]
	} else if m := reNxM.FindStringSubmatchIndex(clean); m != nil {
		in.Kind = Episode
		in.Season, _ = strconv.Atoi(clean[m[2]:m[3]])
		in.HasSeason = true
		first, _ := strconv.Atoi(clean[m[4]:m[5]])
		tail := ""
		if m[6] >= 0 {
			tail = clean[m[6]:m[7]]
		}
		in.Episodes = expand(first, tail)
		cut = m[0]
	}

	title := clean
	if cut >= 0 {
		title = clean[:cut]
	} else {
		title = firstSegment(clean, &year)
	}
	if cut >= 0 {
		// a year may sit between the title and the episode: "Show 2019 S01E02"
		title = firstSegment(title, &year)
	}
	title = strings.Trim(title, " -")
	in.Title = reSpace.ReplaceAllString(title, " ")
	in.Year = year
	if in.Kind != Episode && in.Title != "" && !in.Subtitle {
		in.Kind = Movie
	}
	if in.Subtitle && in.Kind != Episode && in.Title != "" {
		in.Kind = Movie
	}
	if in.Title == "" {
		in.Weak = true
	}
	return in
}

// firstSegment returns the words before the first year or release-noise word
// and records the year it found.
func firstSegment(s string, year *int) string {
	words := strings.Fields(s)
	end := len(words)
	for i, w := range words {
		if noise[strings.ToLower(strings.Trim(w, "-"))] {
			end = i
			break
		}
	}
	// The release year is the last year-like word before the noise; any
	// earlier one belongs to the title ("Blade Runner 2049 2017").
	at := -1
	for i := 1; i < end; i++ {
		if y, err := strconv.Atoi(words[i]); err == nil && y >= 1900 && y <= 2099 {
			at = i
		}
	}
	if at >= 0 {
		if *year == 0 {
			*year, _ = strconv.Atoi(words[at])
		}
		end = at
	}
	return strings.Join(words[:end], " ")
}

// expand turns the text after the first episode number ("-E03", "E03", "-03")
// into the full list.
func expand(first int, tail string) []int {
	eps := []int{first}
	nums := regexp.MustCompile(`\d{1,3}`).FindAllString(tail, -1)
	for _, n := range nums {
		v, _ := strconv.Atoi(n)
		if v > eps[len(eps)-1] && v-eps[len(eps)-1] < 20 {
			// "E01-E03" is a range, "E01E02" is a list: fill any gap
			for x := eps[len(eps)-1] + 1; x <= v; x++ {
				eps = append(eps, x)
			}
		}
	}
	return eps
}

// ParseIn reads a file's name together with the folders it sits in (slash
// separated, outermost first, the file last). Missing pieces are taken from
// the folders: "Shameless/Season 01/Episode 03.mkv" is episode 3 of season 1.
func ParseIn(rel string) Info {
	rel = strings.ReplaceAll(rel, `\`, "/")
	parts := strings.Split(rel, "/")
	in := Parse(parts[len(parts)-1])
	dirs := parts[:len(parts)-1]
	if in.Kind == Unknown || in.Title == "" || (in.Kind == Episode && !in.HasSeason) || looksLikeEpisodeOnly(in) {
		// a bare "Episode 03.mkv": look at the folders
		if m := reEpOnly.FindStringSubmatch(strings.TrimSuffix(parts[len(parts)-1], path.Ext(parts[len(parts)-1]))); m != nil && in.Kind != Episode {
			n, _ := strconv.Atoi(m[1])
			in.Kind = Episode
			in.Episodes = []int{n}
			in.Title = ""
		}
	}
	if in.Kind == Episode || in.Kind == Unknown {
		for i := len(dirs) - 1; i >= 0; i-- {
			d := strings.ReplaceAll(strings.ReplaceAll(dirs[i], "_", " "), ".", " ")
			if m := reSeasonDir.FindStringSubmatch(d); m != nil {
				if !in.HasSeason {
					in.Season, _ = strconv.Atoi(m[2])
					in.HasSeason = true
					in.Weak = true
				}
				if in.Title == "" {
					if t := strings.TrimSpace(m[1]); t != "" {
						in.Title = reSpace.ReplaceAllString(t, " ")
						in.Weak = true
					}
				}
			} else if in.Title == "" && !strings.EqualFold(strings.TrimSpace(d), "") {
				in.Title = strings.TrimSpace(firstSegment(d, new(int)))
				in.Weak = true
			}
			if in.Title != "" && in.HasSeason {
				break
			}
		}
		if in.Kind == Unknown && in.HasSeason && len(in.Episodes) > 0 {
			in.Kind = Episode
		}
	}
	if in.Kind == Unknown && in.Title != "" {
		in.Kind = Movie
	}
	return in
}

func looksLikeEpisodeOnly(in Info) bool {
	return in.Kind == Movie && len(strings.Fields(in.Title)) <= 2 && reEpOnly.MatchString(in.Title)
}

// Norm reduces a title to lower-case letters and digits so that
// "Marvel's Jessica Jones" and "marvels jessica jones" compare equal.
func Norm(s string) string {
	var b strings.Builder
	space := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if space && b.Len() > 0 {
				b.WriteByte(' ')
			}
			space = false
			b.WriteRune(r)
		case r == '\'' || r == '’':
			// apostrophes vanish: Marvel's -> marvels
		default:
			space = true
		}
	}
	return b.String()
}
