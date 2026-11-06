// Package subs finds subtitles online (SubDL) and turns subtitle files into
// the WebVTT the phone's player reads, shifting their timing on request.
package subs

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ErrNoKey means no subtitle service key has been entered.
var ErrNoKey = errors.New("no subtitle key set: get a free key at subdl.com and enter it in Settings")

// Client talks to SubDL.
type Client struct {
	Key  string
	HTTP *http.Client
	Base string // search endpoint base; tests change it
	DL   string // download base
}

// Result is one subtitle on offer.
type Result struct {
	Name     string `json:"name"`
	Release  string `json:"release"`
	Lang     string `json:"lang"`
	Author   string `json:"author,omitempty"`
	Season   int    `json:"season,omitempty"`
	Episode  int    `json:"episode,omitempty"`
	Download string `json:"download"` // path to hand back to Fetch
	Hearing  bool   `json:"hearing,omitempty"`
}

// Query says what to look for.
type Query struct {
	TMDB    int
	Show    bool
	Season  int
	Episode int
	Lang    string // "EN"
	Title   string // fallback when TMDB has no match on their side
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// Search lists subtitles for a movie or an episode.
func (c *Client) Search(ctx context.Context, q Query) ([]Result, error) {
	if strings.TrimSpace(c.Key) == "" {
		return nil, ErrNoKey
	}
	base := c.Base
	if base == "" {
		base = "https://api.subdl.com/api/v1/subtitles"
	}
	v := url.Values{"api_key": {strings.TrimSpace(c.Key)}, "subs_per_page": {"30"}, "subtitles_per_page": {"30"}}
	if q.TMDB > 0 {
		v.Set("tmdb_id", strconv.Itoa(q.TMDB))
	} else if q.Title != "" {
		v.Set("film_name", q.Title)
	}
	if q.Show {
		v.Set("type", "tv")
		v.Set("season_number", strconv.Itoa(q.Season))
		v.Set("episode_number", strconv.Itoa(q.Episode))
	} else {
		v.Set("type", "movie")
	}
	if q.Lang != "" {
		v.Set("languages", strings.ToUpper(q.Lang))
	}
	req, err := http.NewRequestWithContext(ctx, "GET", base+"?"+v.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return nil, errors.New("the subtitle service rejected the key; check it in Settings")
	}
	if resp.StatusCode == 429 {
		return nil, errors.New("the subtitle service's daily limit is used up; try again tomorrow")
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("the subtitle service answered %d", resp.StatusCode)
	}
	var body struct {
		Status    json.RawMessage `json:"status"`
		Error     string          `json:"error"`
		Subtitles []struct {
			Release string `json:"release_name"`
			Name    string `json:"name"`
			Lang    string `json:"lang"`
			Author  string `json:"author"`
			URL     string `json:"url"`
			Season  int    `json:"season"`
			Episode int    `json:"episode"`
			Hi      bool   `json:"hi"`
		} `json:"subtitles"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return nil, err
	}
	if len(body.Subtitles) == 0 && body.Error != "" {
		return nil, errors.New(body.Error)
	}
	var out []Result
	for _, s := range body.Subtitles {
		if s.URL == "" {
			continue
		}
		name := s.Release
		if name == "" {
			name = s.Name
		}
		out = append(out, Result{Name: name, Release: s.Release, Lang: strings.ToLower(s.Lang), Author: s.Author,
			Season: s.Season, Episode: s.Episode, Download: s.URL, Hearing: s.Hi})
	}
	return out, nil
}

// Fetch downloads a subtitle (SubDL serves a ZIP) and returns the subtitle
// file inside it.
func (c *Client) Fetch(ctx context.Context, download string) (string, []byte, error) {
	if !strings.HasPrefix(download, "/") || strings.Contains(download, "..") {
		return "", nil, errors.New("bad subtitle link")
	}
	base := c.DL
	if base == "" {
		base = "https://dl.subdl.com"
	}
	req, err := http.NewRequestWithContext(ctx, "GET", base+download, nil)
	if err != nil {
		return "", nil, err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", nil, fmt.Errorf("the subtitle service answered %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", nil, err
	}
	return Extract(data)
}

// Extract returns the subtitle inside a ZIP, or the data itself when it isn't one.
func Extract(data []byte) (string, []byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "subtitle.srt", data, nil
	}
	var best *zip.File
	for _, f := range zr.File {
		n := strings.ToLower(f.Name)
		if strings.HasSuffix(n, ".srt") || strings.HasSuffix(n, ".vtt") || strings.HasSuffix(n, ".ass") || strings.HasSuffix(n, ".ssa") {
			if best == nil || f.UncompressedSize64 > best.UncompressedSize64 {
				best = f
			}
		}
	}
	if best == nil {
		return "", nil, errors.New("that download had no subtitle file in it")
	}
	rc, err := best.Open()
	if err != nil {
		return "", nil, err
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, 8<<20))
	return best.Name, b, err
}

// ---- conversion ----

type cue struct {
	start, end time.Duration
	text       string
}

var (
	reTime  = regexp.MustCompile(`(?:(\d+):)?(\d{1,2}):(\d{2})[,.](\d{1,3})\s*-->\s*(?:(\d+):)?(\d{1,2}):(\d{2})[,.](\d{1,3})`)
	reTag   = regexp.MustCompile(`\{[^}]*\}`)
	reAssDi = regexp.MustCompile(`^Dialogue:\s*[^,]*,([^,]*),([^,]*),[^,]*,[^,]*,[^,]*,[^,]*,[^,]*,[^,]*,(.*)$`)
)

func dur(h, m, s, ms string) time.Duration {
	hh, _ := strconv.Atoi(h)
	mm, _ := strconv.Atoi(m)
	ss, _ := strconv.Atoi(s)
	for len(ms) < 3 {
		ms += "0"
	}
	mi, _ := strconv.Atoi(ms[:3])
	return time.Duration(hh)*time.Hour + time.Duration(mm)*time.Minute + time.Duration(ss)*time.Second + time.Duration(mi)*time.Millisecond
}

func assTime(s string) time.Duration {
	// 0:01:02.34 (centiseconds)
	p := strings.Split(strings.TrimSpace(s), ":")
	if len(p) != 3 {
		return 0
	}
	sec := strings.SplitN(p[2], ".", 2)
	cs := "0"
	if len(sec) == 2 {
		cs = sec[1]
	}
	for len(cs) < 2 {
		cs += "0"
	}
	return dur(p[0], p[1], sec[0], cs[:2]+"0")
}

// text from a file in whatever encoding it came in: UTF-8 (with or without a
// BOM), UTF-16, or else Windows-1252.
func toUTF8(b []byte) string {
	if len(b) >= 2 && ((b[0] == 0xFF && b[1] == 0xFE) || (b[0] == 0xFE && b[1] == 0xFF)) {
		le := b[0] == 0xFF
		var rs []rune
		for i := 2; i+1 < len(b); i += 2 {
			var u uint16
			if le {
				u = uint16(b[i]) | uint16(b[i+1])<<8
			} else {
				u = uint16(b[i])<<8 | uint16(b[i+1])
			}
			rs = append(rs, rune(u))
		}
		return string(rs)
	}
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	for _, c := range b {
		if c >= 0x80 && c < 0xA0 {
			sb.WriteRune([]rune("€\u0081‚ƒ„…†‡ˆ‰Š‹Œ\u008dŽ\u008f\u0090‘’“”•–—˜™š›œ\u009džŸ")[c-0x80])
		} else {
			sb.WriteRune(rune(c))
		}
	}
	return sb.String()
}

func parseCues(text string, ass bool) []cue {
	text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\r", "\n")
	var cues []cue
	if ass {
		for _, line := range strings.Split(text, "\n") {
			if m := reAssDi.FindStringSubmatch(line); m != nil {
				t := reTag.ReplaceAllString(m[3], "")
				t = strings.NewReplacer(`\N`, "\n", `\n`, "\n", `\h`, " ").Replace(t)
				cues = append(cues, cue{assTime(m[1]), assTime(m[2]), strings.TrimSpace(t)})
			}
		}
		sort.SliceStable(cues, func(i, j int) bool { return cues[i].start < cues[j].start })
		return cues
	}
	var cur *cue
	for _, line := range strings.Split(text, "\n") {
		if m := reTime.FindStringSubmatch(line); m != nil {
			if cur != nil {
				cues = append(cues, *cur)
			}
			cur = &cue{start: dur(m[1], m[2], m[3], m[4]), end: dur(m[5], m[6], m[7], m[8])}
			continue
		}
		if cur == nil {
			continue
		}
		if strings.TrimSpace(line) == "" {
			cues = append(cues, *cur)
			cur = nil
			continue
		}
		if cur.text != "" {
			cur.text += "\n"
		}
		cur.text += line
	}
	if cur != nil {
		cues = append(cues, *cur)
	}
	return cues
}

func stamp(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return fmt.Sprintf("%02d:%02d:%02d.%03d", int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60, int(d.Milliseconds())%1000)
}

// ToVTT converts SRT, WebVTT or ASS subtitles to WebVTT, moving every cue by
// shift (positive makes them appear later).
func ToVTT(data []byte, name string, shift time.Duration) []byte {
	ext := strings.ToLower(name)
	cues := parseCues(toUTF8(data), strings.HasSuffix(ext, ".ass") || strings.HasSuffix(ext, ".ssa"))
	var b strings.Builder
	b.WriteString("WEBVTT\n\n")
	for _, c := range cues {
		if strings.TrimSpace(c.text) == "" {
			continue
		}
		start, end := c.start+shift, c.end+shift
		if end <= 0 {
			continue
		}
		fmt.Fprintf(&b, "%s --> %s\n%s\n\n", stamp(start), stamp(end), c.text)
	}
	return []byte(b.String())
}
