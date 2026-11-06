package web

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Sammy-77A/tgvault/internal/backup"
	"github.com/Sammy-77A/tgvault/internal/media"
	"github.com/Sammy-77A/tgvault/internal/subs"
	"github.com/Sammy-77A/tgvault/internal/tgclient"
	"github.com/Sammy-77A/tgvault/internal/tmdb"
)

// The movie app (the library app): browsing TMDB, asking for titles, and
// watching what's been downloaded to the phone. Everything the app shows about
// a title comes through here so the TMDB key stays in the Go side.

type mbState struct {
	mu       sync.Mutex
	lastSync time.Time
}

func (s *Server) mbTMDB() (*tmdb.Client, error) {
	key := strings.TrimSpace(s.st.Get(backup.SettingTMDBKey))
	if key == "" {
		return nil, tmdb.ErrNoKey
	}
	return &tmdb.Client{Key: key, Base: s.tmdbBase, ImageBase: s.imageBase}, nil
}

func (s *Server) mbSubs() *subs.Client {
	return &subs.Client{Key: strings.TrimSpace(s.st.Get(backup.SettingSubtitleKey)), Base: s.subsBase, DL: s.subsDL}
}

// ---- shaping TMDB's answers ----

type mbItem struct {
	ID        int     `json:"id"`
	Kind      string  `json:"kind"` // "movie" or "show"
	Title     string  `json:"title"`
	Year      int     `json:"year,omitempty"`
	Poster    string  `json:"poster,omitempty"`
	Backdrop  string  `json:"backdrop,omitempty"`
	Rating    float64 `json:"rating,omitempty"`
	Overview  string  `json:"overview,omitempty"`
	InLibrary bool    `json:"inLibrary,omitempty"`
	Request   string  `json:"request,omitempty"` // state of the latest request for it
}

type tmdbRow struct {
	ID        int     `json:"id"`
	Media     string  `json:"media_type"`
	Title     string  `json:"title"`
	Name      string  `json:"name"`
	Release   string  `json:"release_date"`
	First     string  `json:"first_air_date"`
	Poster    string  `json:"poster_path"`
	Backdrop  string  `json:"backdrop_path"`
	Vote      float64 `json:"vote_average"`
	Overview  string  `json:"overview"`
	Popularty float64 `json:"popularity"`
}

func yearOf(d string) int {
	if len(d) >= 4 {
		y, _ := strconv.Atoi(d[:4])
		return y
	}
	return 0
}

func (r tmdbRow) item(defKind string) (mbItem, bool) {
	kind := defKind
	switch r.Media {
	case "movie":
		kind = "movie"
	case "tv":
		kind = "show"
	case "person":
		return mbItem{}, false
	}
	it := mbItem{ID: r.ID, Kind: kind, Poster: r.Poster, Backdrop: r.Backdrop, Rating: r.Vote, Overview: r.Overview}
	if kind == "movie" {
		it.Title, it.Year = r.Title, yearOf(r.Release)
	} else {
		it.Title, it.Year = r.Name, yearOf(r.First)
	}
	return it, it.Title != ""
}

// lookups of what's in the library and what has been asked for, so that every
// card can say so.
type mbMarks struct {
	lib  map[string][]int // kind|norm -> the years of the library titles with that name (0: no year)
	reqs map[string]string
}

// yearOK says whether a library title (its folder may carry a year) is the
// one TMDB means: two shows can share a name, and only one of them is here.
func yearOK(libYear, year int) bool {
	return libYear == 0 || year == 0 || libYear == year || libYear == year-1 || libYear == year+1
}

func (m mbMarks) has(kind, title string, year int) bool {
	for _, y := range m.lib[kind+"|"+media.Norm(title)] {
		if yearOK(y, year) {
			return true
		}
	}
	return false
}

func (s *Server) marks() mbMarks {
	m := mbMarks{lib: map[string][]int{}, reqs: map[string]string{}}
	if lib, err := s.eng.Library(); err == nil {
		for _, t := range lib {
			k := t.Kind + "|" + t.Norm
			m.lib[k] = append(m.lib[k], t.Year)
		}
	}
	for _, r := range s.eng.Requests() { // newest first: keep the newest state
		k := r.Kind + "|" + strconv.Itoa(r.TMDB)
		if _, ok := m.reqs[k]; !ok {
			m.reqs[k] = r.State
		}
	}
	return m
}

func (m mbMarks) mark(it *mbItem) {
	it.InLibrary = m.has(it.Kind, it.Title, it.Year)
	it.Request = m.reqs[it.Kind+"|"+strconv.Itoa(it.ID)]
}

func (s *Server) mbList(ctx context.Context, c *tmdb.Client, p string, q url.Values, kind string, marks mbMarks, ttl time.Duration) ([]mbItem, int, error) {
	raw, err := c.Raw(ctx, p, q, ttl)
	if err != nil {
		return nil, 0, err
	}
	var body struct {
		Results []tmdbRow `json:"results"`
		Pages   int       `json:"total_pages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, 0, err
	}
	out := make([]mbItem, 0, len(body.Results))
	for _, r := range body.Results {
		if it, ok := r.item(kind); ok {
			marks.mark(&it)
			out = append(out, it)
		}
	}
	return out, body.Pages, nil
}

func mbErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, tmdb.ErrNoKey):
		fail(w, 400, "The app has no TMDB key. Ask the owner to rebuild it with one.")
	default:
		fail(w, 502, tgclient.Friendly(err))
	}
}

const day = 24 * time.Hour

// mbHome: the rows on the Home tab.
func (s *Server) mbHome(w http.ResponseWriter, r *http.Request) {
	c, err := s.mbTMDB()
	if err != nil {
		mbErr(w, err)
		return
	}
	marks := s.marks()
	type row struct {
		Title string   `json:"title"`
		Key   string   `json:"key"`
		Items []mbItem `json:"items"`
	}
	defs := []struct{ title, key, path, kind string }{
		{"Trending this week", "trending", "/trending/all/week", "movie"},
		{"Popular movies", "popular-movies", "/movie/popular", "movie"},
		{"Popular shows", "popular-shows", "/tv/popular", "show"},
		{"Top rated movies", "top-movies", "/movie/top_rated", "movie"},
		{"Top rated shows", "top-shows", "/tv/top_rated", "show"},
	}
	rows := make([]row, len(defs))
	var wg sync.WaitGroup
	var firstErr error
	var emu sync.Mutex
	for i, d := range defs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			items, _, err := s.mbList(r.Context(), c, d.path, nil, d.kind, marks, 3*time.Hour)
			if err != nil {
				emu.Lock()
				firstErr = err
				emu.Unlock()
				return
			}
			if len(items) > 20 {
				items = items[:20]
			}
			rows[i] = row{d.title, d.key, items}
		}()
	}
	wg.Wait()
	var kept []row
	for _, x := range rows {
		if len(x.Items) > 0 {
			kept = append(kept, x)
		}
	}
	if len(kept) == 0 && firstErr != nil {
		mbErr(w, firstErr)
		return
	}
	type genre struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	genres := map[string][]genre{}
	for kind, p := range map[string]string{"movie": "/genre/movie/list", "show": "/genre/tv/list"} {
		if raw, err := c.Raw(r.Context(), p, nil, 7*day); err == nil {
			var g struct {
				Genres []genre `json:"genres"`
			}
			if json.Unmarshal(raw, &g) == nil {
				genres[kind] = g.Genres
			}
		}
	}
	writeJSON(w, map[string]any{"rows": kept, "genres": genres})
}

// mbBrowse lists a category: popular, top rated or a genre, page by page.
func (s *Server) mbBrowse(w http.ResponseWriter, r *http.Request) {
	c, err := s.mbTMDB()
	if err != nil {
		mbErr(w, err)
		return
	}
	q := r.URL.Query()
	kind := "movie"
	tv := "movie"
	if q.Get("kind") == "show" {
		kind, tv = "show", "tv"
	}
	page := q.Get("page")
	if n, _ := strconv.Atoi(page); n < 1 || n > 20 {
		page = "1"
	}
	v := url.Values{"page": {page}, "sort_by": {"popularity.desc"}}
	if g := q.Get("genre"); g != "" {
		if _, err := strconv.Atoi(g); err != nil {
			fail(w, 400, "bad genre")
			return
		}
		v.Set("with_genres", g)
	}
	items, pages, err := s.mbList(r.Context(), c, "/discover/"+tv, v, kind, s.marks(), 3*time.Hour)
	if err != nil {
		mbErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"items": items, "pages": pages})
}

func (s *Server) mbSearch(w http.ResponseWriter, r *http.Request) {
	c, err := s.mbTMDB()
	if err != nil {
		mbErr(w, err)
		return
	}
	qs := strings.TrimSpace(r.URL.Query().Get("q"))
	if qs == "" {
		writeJSON(w, map[string]any{"items": []mbItem{}})
		return
	}
	items, _, err := s.mbList(r.Context(), c, "/search/multi", url.Values{"query": {qs}, "include_adult": {"false"}}, "movie", s.marks(), time.Hour)
	if err != nil {
		mbErr(w, err)
		return
	}
	writeJSON(w, map[string]any{"items": items})
}

// mbTitle: everything the details page shows.
func (s *Server) mbTitle(w http.ResponseWriter, r *http.Request) {
	c, err := s.mbTMDB()
	if err != nil {
		mbErr(w, err)
		return
	}
	id, _ := strconv.Atoi(r.URL.Query().Get("id"))
	kind := r.URL.Query().Get("kind")
	if id <= 0 || (kind != "movie" && kind != "show") {
		fail(w, 400, "bad request")
		return
	}
	tvPath := map[string]string{"movie": "/movie/", "show": "/tv/"}[kind]
	raw, err := c.Raw(r.Context(), tvPath+strconv.Itoa(id), url.Values{"append_to_response": {"credits,videos"}}, 6*time.Hour)
	if err != nil {
		mbErr(w, err)
		return
	}
	var d struct {
		tmdbRow
		Tagline  string `json:"tagline"`
		Runtime  int    `json:"runtime"`
		Episode  []int  `json:"episode_run_time"`
		Status   string `json:"status"`
		Votes    int    `json:"vote_count"`
		Language string `json:"original_language"`
		Genres   []struct {
			Name string `json:"name"`
		} `json:"genres"`
		Seasons []struct {
			Number   int    `json:"season_number"`
			Name     string `json:"name"`
			Episodes int    `json:"episode_count"`
			Air      string `json:"air_date"`
			Poster   string `json:"poster_path"`
		} `json:"seasons"`
		Credits struct {
			Cast []struct {
				Name      string `json:"name"`
				Character string `json:"character"`
				Photo     string `json:"profile_path"`
			} `json:"cast"`
		} `json:"credits"`
		Videos struct {
			Results []struct {
				Key  string `json:"key"`
				Name string `json:"name"`
				Site string `json:"site"`
				Type string `json:"type"`
				Off  bool   `json:"official"`
			} `json:"results"`
		} `json:"videos"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		fail(w, 502, "TMDB sent something unreadable")
		return
	}
	it, _ := d.item(kind)
	it.ID = id
	it.Kind = kind
	marks := s.marks()
	marks.mark(&it)
	type castM struct {
		Name      string `json:"name"`
		Character string `json:"character"`
		Photo     string `json:"photo,omitempty"`
	}
	var cast []castM
	for i, p := range d.Credits.Cast {
		if i >= 14 {
			break
		}
		cast = append(cast, castM{p.Name, p.Character, p.Photo})
	}
	type trailer struct {
		Key  string `json:"key"`
		Name string `json:"name"`
	}
	var trailers []trailer
	for _, pass := range []bool{true, false} {
		for _, v := range d.Videos.Results {
			if v.Site == "YouTube" && (v.Type == "Trailer" || (!pass && v.Type == "Teaser")) && (!pass || v.Off) {
				trailers = append(trailers, trailer{v.Key, v.Name})
			}
		}
		if len(trailers) > 0 {
			break
		}
	}
	type seasonM struct {
		Number   int    `json:"number"`
		Name     string `json:"name"`
		Episodes int    `json:"episodes"`
		Year     int    `json:"year,omitempty"`
		Poster   string `json:"poster,omitempty"`
		Have     int    `json:"have"` // episodes in the library
		Local    int    `json:"local"`
	}
	var seasons []seasonM
	var lib *backup.LibTitle
	if all, err := s.eng.Library(); err == nil {
		for i := range all {
			if all[i].Kind == kind && all[i].Norm == media.Norm(it.Title) && yearOK(all[i].Year, it.Year) {
				lib = &all[i]
			}
		}
	}
	for _, sn := range d.Seasons {
		if sn.Number == 0 && len(d.Seasons) > 1 {
			continue
		}
		m := seasonM{Number: sn.Number, Name: sn.Name, Episodes: sn.Episodes, Year: yearOf(sn.Air), Poster: sn.Poster}
		if lib != nil {
			for _, ls := range lib.Seasons {
				if ls.Number == sn.Number {
					for _, ep := range ls.Episodes {
						m.Have += len(ep.Episodes)
						if ep.Local {
							m.Local += len(ep.Episodes)
						}
					}
				}
			}
		}
		seasons = append(seasons, m)
	}
	runtime := d.Runtime
	if runtime == 0 && len(d.Episode) > 0 {
		runtime = d.Episode[0]
	}
	var genres []string
	for _, g := range d.Genres {
		genres = append(genres, g.Name)
	}
	writeJSON(w, map[string]any{"item": it, "tagline": d.Tagline, "runtime": runtime, "status": d.Status, "votes": d.Votes,
		"genres": genres, "cast": cast, "trailers": trailers, "seasons": seasons, "library": lib})
}

// mbSeason: a season's episodes, each with what's in the library.
func (s *Server) mbSeason(w http.ResponseWriter, r *http.Request) {
	c, err := s.mbTMDB()
	if err != nil {
		mbErr(w, err)
		return
	}
	id, _ := strconv.Atoi(r.URL.Query().Get("id"))
	n, err2 := strconv.Atoi(r.URL.Query().Get("n"))
	if id <= 0 || err2 != nil || n < 0 {
		fail(w, 400, "bad request")
		return
	}
	raw, err := c.Raw(r.Context(), "/tv/"+strconv.Itoa(id)+"/season/"+strconv.Itoa(n), nil, 6*time.Hour)
	if err != nil {
		mbErr(w, err)
		return
	}
	var d struct {
		Episodes []struct {
			Number   int     `json:"episode_number"`
			Name     string  `json:"name"`
			Overview string  `json:"overview"`
			Still    string  `json:"still_path"`
			Runtime  int     `json:"runtime"`
			Air      string  `json:"air_date"`
			Vote     float64 `json:"vote_average"`
		} `json:"episodes"`
	}
	if err := json.Unmarshal(raw, &d); err != nil {
		fail(w, 502, "TMDB sent something unreadable")
		return
	}
	have := map[int]backup.LibFile{}
	title := r.URL.Query().Get("title")
	year, _ := strconv.Atoi(r.URL.Query().Get("year"))
	if lib, err := s.eng.Library(); err == nil {
		for _, t := range lib {
			if t.Kind != "show" || t.Norm != media.Norm(title) || !yearOK(t.Year, year) {
				continue
			}
			for _, sn := range t.Seasons {
				if sn.Number == n {
					for _, f := range sn.Episodes {
						for _, e := range f.Episodes {
							have[e] = f
						}
					}
				}
			}
		}
	}
	type epM struct {
		Number   int             `json:"number"`
		Name     string          `json:"name"`
		Overview string          `json:"overview,omitempty"`
		Still    string          `json:"still,omitempty"`
		Runtime  int             `json:"runtime,omitempty"`
		Air      string          `json:"air,omitempty"`
		File     *backup.LibFile `json:"file,omitempty"`
		Pos      int64           `json:"pos,omitempty"`
		Dur      int64           `json:"dur,omitempty"`
	}
	out := make([]epM, 0, len(d.Episodes))
	for _, e := range d.Episodes {
		m := epM{e.Number, e.Name, e.Overview, e.Still, e.Runtime, e.Air, nil, 0, 0}
		if f, ok := have[e.Number]; ok {
			f := f
			m.File = &f
			m.Pos, m.Dur = s.eng.Progress(f.Path)
		}
		out = append(out, m)
	}
	writeJSON(w, map[string]any{"episodes": out})
}

// mbImg serves a TMDB picture, keeping copies so the app's screens still have
// their pictures without a connection.
func (s *Server) mbImg(w http.ResponseWriter, r *http.Request) {
	size := r.URL.Query().Get("s")
	switch size {
	case "w92", "w154", "w185", "w300", "w342", "w500", "w780", "w1280", "original":
	default:
		size = "w342"
	}
	p := r.URL.Query().Get("p")
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "?#\\") || strings.Contains(p, "..") || len(p) > 100 {
		fail(w, 400, "bad image")
		return
	}
	sum := sha1.Sum([]byte(size + p))
	dir := filepath.Join(s.eng.DataDir(), "img")
	file := filepath.Join(dir, hex.EncodeToString(sum[:])+path.Ext(p))
	if _, err := os.Stat(file); err != nil {
		c, err := s.mbTMDB()
		if err != nil {
			fail(w, 404, "no image")
			return
		}
		b, _, err := c.Image(r.Context(), size, p)
		if err != nil {
			fail(w, 404, "no image")
			return
		}
		_ = os.MkdirAll(dir, 0o700)
		tmp := file + ".tmp"
		if os.WriteFile(tmp, b, 0o600) == nil {
			_ = os.Rename(tmp, file)
		}
	}
	w.Header().Set("Cache-Control", "private, max-age=2592000")
	http.ServeFile(w, r, file)
}

// ---- requests ----

func (s *Server) mbRequest(w http.ResponseWriter, r *http.Request) {
	var body backup.Request
	if err := readJSON(r, &body); err != nil {
		fail(w, 400, "bad request")
		return
	}
	body.ID, body.State, body.MsgID = "", "", 0
	req, err := s.eng.MakeRequest(r.Context(), body)
	if err != nil {
		fail(w, 400, tgclient.Friendly(err))
		return
	}
	writeJSON(w, map[string]any{"request": req})
}

func (s *Server) mbRequests(w http.ResponseWriter, r *http.Request) {
	s.mb.mu.Lock()
	stale := time.Since(s.mb.lastSync) > 8*time.Second
	if stale {
		s.mb.lastSync = time.Now()
	}
	s.mb.mu.Unlock()
	if stale {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		_, _ = s.eng.SyncRequests(ctx) // offline is fine: show what we know
		cancel()
	}
	type reqM struct {
		backup.Request
		Available bool `json:"available"`
	}
	marks := s.marks()
	out := []reqM{}
	for _, q := range s.eng.Requests() {
		kind := "movie"
		if q.Kind == "show" {
			kind = "show"
		}
		out = append(out, reqM{q, marks.has(kind, q.Title, q.Year)})
	}
	writeJSON(w, map[string]any{"requests": out})
}

func (s *Server) mbRequestsClear(w http.ResponseWriter, r *http.Request) {
	n, err := s.eng.ClearAnswered()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"cleared": n})
}

// ---- the library ----

// resolve finds a library title on TMDB, so that it can have a poster and open
// the same details page as anything else. Answers are kept a week.
func (s *Server) resolve(ctx context.Context, c *tmdb.Client, t backup.LibTitle) (id int, poster string) {
	p, k := "/search/movie", "title"
	if t.Kind == "show" {
		p, k = "/search/tv", "name"
	}
	q := url.Values{"query": {t.Title}}
	if t.Year > 0 && t.Kind == "movie" {
		q.Set("year", strconv.Itoa(t.Year))
	}
	raw, err := c.Raw(ctx, p, q, 7*day)
	if err != nil {
		return 0, ""
	}
	var body struct {
		Results []map[string]any `json:"results"`
	}
	if json.Unmarshal(raw, &body) != nil || len(body.Results) == 0 {
		return 0, ""
	}
	pick := body.Results[0]
	for _, r := range body.Results {
		if n, _ := r[k].(string); media.Norm(n) == t.Norm {
			pick = r
			break
		}
	}
	idf, _ := pick["id"].(float64)
	pp, _ := pick["poster_path"].(string)
	return int(idf), pp
}

func (s *Server) mbLibrary(w http.ResponseWriter, r *http.Request) {
	lib, err := s.eng.Library()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	type titleM struct {
		backup.LibTitle
		TMDB   int    `json:"tmdb,omitempty"`
		Poster string `json:"poster,omitempty"`
		Local  int    `json:"local"` // files on this phone
		Total  int    `json:"total"`
	}
	out := make([]titleM, len(lib))
	c, cerr := s.mbTMDB()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	for i, t := range lib {
		m := titleM{LibTitle: t}
		count := func(f backup.LibFile) {
			m.Total++
			if f.Local {
				m.Local++
			}
		}
		for _, f := range t.Files {
			count(f)
		}
		for _, sn := range t.Seasons {
			for _, f := range sn.Episodes {
				count(f)
			}
		}
		out[i] = m
		if cerr == nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				out[i].TMDB, out[i].Poster = s.resolve(ctx, c, t)
			}()
		}
	}
	wg.Wait()
	writeJSON(w, map[string]any{"titles": out, "root": s.eng.MediaRoot()})
}

func (s *Server) mbPaths(w http.ResponseWriter, r *http.Request) ([]string, bool) {
	var body struct {
		Paths []string `json:"paths"`
	}
	if err := readJSON(r, &body); err != nil || len(body.Paths) == 0 {
		fail(w, 400, "bad request")
		return nil, false
	}
	var out []string
	for _, p := range body.Paths {
		cp, ok := cleanCloudPath(p)
		if !ok || !strings.HasPrefix(cp, s.eng.MediaRoot()+"/") {
			fail(w, 403, "that isn't in the library")
			return nil, false
		}
		out = append(out, cp)
	}
	return out, true
}

func (s *Server) mbDownload(w http.ResponseWriter, r *http.Request) {
	paths, ok := s.mbPaths(w, r)
	if !ok {
		return
	}
	id, err := s.eng.DownloadMedia(r.Context(), paths)
	if err != nil {
		fail(w, 400, tgclient.Friendly(err))
		return
	}
	writeJSON(w, map[string]any{"task": id})
}

func (s *Server) mbRemove(w http.ResponseWriter, r *http.Request) {
	paths, ok := s.mbPaths(w, r)
	if !ok {
		return
	}
	n, err := s.eng.RemoveMedia(paths)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"removed": n})
}

func (s *Server) mbProgress(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
		Pos  int64  `json:"pos"`
		Dur  int64  `json:"dur"`
	}
	if err := readJSON(r, &body); err != nil {
		fail(w, 400, "bad request")
		return
	}
	cp, ok := cleanCloudPath(body.Path)
	if !ok {
		fail(w, 400, "bad path")
		return
	}
	if err := s.eng.SetProgress(cp, body.Pos, body.Dur); err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// ---- playing ----

// plSub, plItem and playlist are what the native player is given.
type plSub struct {
	URL   string `json:"url"`
	Lang  string `json:"lang"`
	Label string `json:"label"`
	Mime  string `json:"mime"`
}

type plItem struct {
	Path    string  `json:"path"`
	Name    string  `json:"name"`
	URL     string  `json:"url"`
	Subs    []plSub `json:"subs"`
	Pos     int64   `json:"pos"`
	Season  int     `json:"season,omitempty"`
	Episode int     `json:"episode,omitempty"`
}

type playlist struct {
	Items []plItem `json:"items"`
	Start int      `json:"start"`
	TMDB  int      `json:"tmdb"`
	Show  bool     `json:"show"`
	Title string   `json:"title"`
	Lang  string   `json:"lang"`
	Base  string   `json:"base"`
	Key   string   `json:"key"`
}

// mbPlaylist prepares a playlist of downloaded videos for the native player
// and returns the link the player reads it from. Nothing streams: a video that
// isn't on this phone is left out.
func (s *Server) mbPlaylist(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Paths []string `json:"paths"`
		Start string   `json:"start"`
		TMDB  int      `json:"tmdb"`
		Show  bool     `json:"show"`
		Lang  string   `json:"lang"`
		Title string   `json:"title"`
	}
	if err := readJSON(r, &body); err != nil || len(body.Paths) == 0 {
		fail(w, 400, "bad request")
		return
	}
	pl := &playlist{TMDB: body.TMDB, Show: body.Show, Lang: body.Lang, Base: "http://" + s.addr}
	for _, p := range body.Paths {
		cp, ok := cleanCloudPath(p)
		if !ok || !strings.HasPrefix(cp, s.eng.MediaRoot()+"/") {
			fail(w, 403, "that isn't in the library")
			return
		}
		local := s.eng.MediaLocal(cp)
		fi, err := os.Stat(local)
		if err != nil || fi.IsDir() {
			continue // not downloaded: nothing streams
		}
		if cp == body.Start {
			pl.Start = len(pl.Items)
		}
		it := plItem{Path: cp, Name: path.Base(cp)}
		info := media.Parse(path.Base(cp))
		if info.HasSeason && len(info.Episodes) > 0 {
			it.Season, it.Episode = info.Season, info.Episodes[0]
		}
		it.URL = s.localURL(local, "/play/")
		it.Pos, _ = s.eng.Progress(cp)
		it.Subs = s.sidecarSubs(local)
		pl.Items = append(pl.Items, it)
	}
	if len(pl.Items) == 0 {
		fail(w, 400, "Download it to this phone first; nothing streams.")
		return
	}
	pl.Title = body.Title
	if pl.Title == "" {
		pl.Title = media.Parse(pl.Items[pl.Start].Name).Title
	}
	pl.Key = s.lists.add(pl)
	writeJSON(w, map[string]any{"url": pl.Base + "/plist/" + pl.Key, "count": len(pl.Items), "playlist": pl})
}

// sidecarSubs lists the subtitle files beside a downloaded video.
func (s *Server) sidecarSubs(local string) []plSub {
	stem := strings.TrimSuffix(filepath.Base(local), filepath.Ext(local))
	entries, _ := os.ReadDir(filepath.Dir(local))
	var out []plSub
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !media.IsSubtitle(path.Ext(n)) || !strings.HasPrefix(strings.TrimSuffix(n, path.Ext(n)), stem) {
			continue
		}
		lang := langFromName(n)
		label := strings.ToUpper(lang)
		if label == "" {
			label = "Subtitles"
		}
		out = append(out, plSub{URL: s.localURL(filepath.Join(filepath.Dir(local), n), "/sub/"), Lang: lang, Label: label, Mime: "text/vtt"})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Lang == "en" && out[j].Lang != "en" })
	return out
}

// plist is the native player's own door: it reads the playlist, reports how
// far it got, and asks for subtitles. A key opens exactly one playlist, from
// this device only.
func (s *Server) plist(w http.ResponseWriter, r *http.Request) {
	if !loopback(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/plist/"), "/"), "/")
	pl, ok := s.lists.get(parts[0])
	if !ok {
		http.Error(w, "this link has expired", http.StatusNotFound)
		return
	}
	sub := ""
	if len(parts) > 1 {
		sub = parts[1]
	}
	item := func() (int, bool) {
		i, err := strconv.Atoi(r.URL.Query().Get("i"))
		if err != nil || i < 0 || i >= len(pl.Items) {
			fail(w, 400, "bad item")
			return 0, false
		}
		return i, true
	}
	switch {
	case sub == "" && r.Method == http.MethodGet:
		writeJSON(w, pl)
	case sub == "progress" && r.Method == http.MethodPost:
		i, ok := item()
		if !ok {
			return
		}
		pos, _ := strconv.ParseInt(r.URL.Query().Get("pos"), 10, 64)
		dur, _ := strconv.ParseInt(r.URL.Query().Get("dur"), 10, 64)
		if err := s.eng.SetProgress(pl.Items[i].Path, pos, dur); err != nil {
			fail(w, 400, err.Error())
			return
		}
		writeJSON(w, map[string]any{"ok": true})
	case sub == "subs" && r.Method == http.MethodGet:
		i, ok := item()
		if !ok {
			return
		}
		it := pl.Items[i]
		lang := r.URL.Query().Get("lang")
		if lang == "" {
			lang = "en"
		}
		res, err := s.mbSubs().Search(r.Context(), subs.Query{TMDB: pl.TMDB, Show: pl.Show, Season: it.Season, Episode: it.Episode, Lang: lang, Title: pl.Title})
		if err != nil {
			code := 502
			if errors.Is(err, subs.ErrNoKey) {
				code = 400
			}
			fail(w, code, tgclient.Friendly(err))
			return
		}
		if res == nil {
			res = []subs.Result{}
		}
		writeJSON(w, map[string]any{"results": res})
	case sub == "subget" && r.Method == http.MethodPost:
		i, ok := item()
		if !ok {
			return
		}
		var body struct {
			Download string `json:"download"`
			Lang     string `json:"lang"`
		}
		if err := readJSON(r, &body); err != nil {
			fail(w, 400, "bad request")
			return
		}
		got, err := s.saveSubtitle(r.Context(), pl.Items[i].Path, body.Download, body.Lang)
		if err != nil {
			fail(w, 502, tgclient.Friendly(err))
			return
		}
		writeJSON(w, got)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

type listKeys struct {
	mu sync.Mutex
	m  map[string]*playlist
	at map[string]time.Time
}

func (l *listKeys) add(p *playlist) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	key := hex.EncodeToString(b)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m, l.at = map[string]*playlist{}, map[string]time.Time{}
	}
	now := time.Now()
	for k, t := range l.at {
		if now.Sub(t) > playKeyLife {
			delete(l.m, k)
			delete(l.at, k)
		}
	}
	l.m[key], l.at[key] = p, now
	return key
}

func (l *listKeys) get(key string) (*playlist, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.m[key]
	if !ok || time.Since(l.at[key]) > playKeyLife {
		return nil, false
	}
	return p, true
}

func langFromName(n string) string {
	stem := strings.TrimSuffix(n, path.Ext(n))
	if i := strings.LastIndex(stem, "."); i >= 0 {
		l := strings.ToLower(stem[i+1:])
		if len(l) >= 2 && len(l) <= 3 {
			return l
		}
	}
	return ""
}

func (s *Server) localURL(file, prefix string) string {
	return "http://" + s.addr + prefix + s.plays.addLocal(file) + "/" + url.PathEscape(filepath.Base(file))
}

// ---- subtitles from the internet ----

func (s *Server) mbSubsSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tmdbID, _ := strconv.Atoi(q.Get("tmdb"))
	season, _ := strconv.Atoi(q.Get("season"))
	episode, _ := strconv.Atoi(q.Get("episode"))
	lang := q.Get("lang")
	if lang == "" {
		lang = "en"
	}
	res, err := s.mbSubs().Search(r.Context(), subs.Query{TMDB: tmdbID, Show: q.Get("show") == "1", Season: season, Episode: episode, Lang: lang, Title: q.Get("title")})
	if err != nil {
		if errors.Is(err, subs.ErrNoKey) {
			fail(w, 400, err.Error())
			return
		}
		fail(w, 502, tgclient.Friendly(err))
		return
	}
	if res == nil {
		res = []subs.Result{}
	}
	writeJSON(w, map[string]any{"results": res})
}

// saveSubtitle downloads a chosen subtitle beside the downloaded video.
func (s *Server) saveSubtitle(ctx context.Context, videoPath, download, langIn string) (map[string]any, error) {
	local := s.eng.MediaLocal(videoPath)
	if _, err := os.Stat(local); err != nil {
		return nil, errors.New("download the video first")
	}
	name, data, err := s.mbSubs().Fetch(ctx, download)
	if err != nil {
		return nil, err
	}
	lang := strings.ToLower(strings.TrimSpace(langIn))
	if lang == "" || len(lang) > 3 || strings.ContainsAny(lang, "./\\ ") {
		lang = "en"
	}
	ext := strings.ToLower(path.Ext(name))
	if ext != ".srt" && ext != ".vtt" && ext != ".ass" && ext != ".ssa" {
		ext = ".srt"
	}
	file := strings.TrimSuffix(local, filepath.Ext(local)) + "." + lang + ext
	if err := os.WriteFile(file, data, 0o600); err != nil {
		return nil, err
	}
	return map[string]any{"url": s.localURL(file, "/sub/"), "lang": lang, "label": strings.ToUpper(lang), "mime": "text/vtt"}, nil
}

// mbSubsGet is the page's way to the same thing.
func (s *Server) mbSubsGet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path     string `json:"path"`
		Download string `json:"download"`
		Lang     string `json:"lang"`
	}
	if err := readJSON(r, &body); err != nil {
		fail(w, 400, "bad request")
		return
	}
	cp, ok := cleanCloudPath(body.Path)
	if !ok || !strings.HasPrefix(cp, s.eng.MediaRoot()+"/") {
		fail(w, 403, "that isn't in the library")
		return
	}
	got, err := s.saveSubtitle(r.Context(), cp, body.Download, body.Lang)
	if err != nil {
		fail(w, 502, tgclient.Friendly(err))
		return
	}
	writeJSON(w, got)
}

func (s *Server) mbSubsKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key string `json:"key"`
	}
	if err := readJSON(r, &body); err != nil {
		fail(w, 400, "bad request")
		return
	}
	if k := strings.TrimSpace(body.Key); k == "" {
		_ = s.st.Delete(backup.SettingSubtitleKey)
	} else {
		_ = s.st.Set(backup.SettingSubtitleKey, k)
	}
	go func() { // share it with the phone app
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = s.eng.PublishConfig(ctx)
	}()
	writeJSON(w, map[string]any{"hasKey": strings.TrimSpace(s.st.Get(backup.SettingSubtitleKey)) != ""})
}

// mbInfo: what the app needs to know about itself.
func (s *Server) mbInfo(w http.ResponseWriter, r *http.Request) {
	if !s.eng.HasTMDBKey() {
		// the PC shares its keys through Saved Messages; look for them
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		_, _ = s.eng.SyncRequests(ctx)
		cancel()
	}
	writeJSON(w, map[string]any{
		"hasTMDB": s.eng.HasTMDBKey(), "hasSubs": strings.TrimSpace(s.st.Get(backup.SettingSubtitleKey)) != "",
		"root": s.eng.MediaRoot(), "library": s.library != "", "used": s.eng.MediaDiskUsage(),
	})
}

// sub serves a downloaded subtitle file as WebVTT, moved by ?shift= milliseconds.
func (s *Server) sub(w http.ResponseWriter, r *http.Request) {
	if !loopback(r) || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/sub/"), "/", 2)
	k, ok := s.plays.get(parts[0])
	if !ok || k.local == "" {
		http.Error(w, "this link has expired", http.StatusNotFound)
		return
	}
	data, err := os.ReadFile(k.local)
	if err != nil {
		http.Error(w, "gone", http.StatusNotFound)
		return
	}
	shift, _ := strconv.Atoi(r.URL.Query().Get("shift"))
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(subs.ToVTT(data, k.local, time.Duration(shift)*time.Millisecond))
}
