package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/Sammy-77A/tgvault/internal/media"
	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/tmdb"
)

// The movie library: a folder of the backup where files dropped in any
// shape are recognised from their names, checked against TMDB, and filed as
//
//	Movies/Up (2009)/Up (2009).mkv
//	Shows/Shameless/Season 01/Shameless - S01E03.mkv
//
// Sure matches are moved straight away (cloud copy and file on disk together,
// by the same code as any other move). Anything less sure waits in a review
// list; one answer there settles every file with the same name.

const (
	// SettingTMDBKey holds the owner's TMDB key; SettingMediaRoot the folder.
	SettingTMDBKey   = "media.tmdb_key"
	SettingMediaRoot = "media.root"
	// SettingSubtitleKey holds the key of the subtitle search service.
	SettingSubtitleKey = "media.subtitle_key"
	// DefaultMediaRoot is where the library lives unless changed.
	DefaultMediaRoot = "Downloads/Telegram Desktop/tgmoviebox"
)

// MovieDB is the part of TMDB the organizer uses (tests supply a fake).
type MovieDB interface {
	SearchMovie(ctx context.Context, title string, year int) ([]tmdb.Result, error)
	SearchTV(ctx context.Context, title string, year int) ([]tmdb.Result, error)
	SeasonEpisodes(ctx context.Context, tvID, season int) ([]tmdb.Episode, error)
}

// SetMovieDB replaces the TMDB client (for tests).
func (e *Engine) SetMovieDB(db MovieDB) { e.movieDB = db }

func (e *Engine) tmdbClient() (MovieDB, error) {
	if e.movieDB != nil {
		return e.movieDB, nil
	}
	key := strings.TrimSpace(e.st.Get(SettingTMDBKey))
	if key == "" {
		return nil, tmdb.ErrNoKey
	}
	return &tmdb.Client{Key: key}, nil
}

// MediaRoot is the library folder's cloud path.
func (e *Engine) MediaRoot() string {
	if v := strings.Trim(strings.TrimSpace(e.st.Get(SettingMediaRoot)), "/"); v != "" {
		return v
	}
	return DefaultMediaRoot
}

// EnsureMediaFolders makes the library folder, with its Movies and Shows
// folders, on this device and in the backup's folder list.
func (e *Engine) EnsureMediaFolders() error {
	root := e.MediaRoot()
	for _, p := range []string{root, root + "/Movies", root + "/Shows"} {
		if _, err := e.NewFolder(path.Dir(p), path.Base(p)); err != nil {
			return err
		}
	}
	return nil
}

// HasTMDBKey reports whether a key has been entered (never the key itself).
func (e *Engine) HasTMDBKey() bool { return strings.TrimSpace(e.st.Get(SettingTMDBKey)) != "" }

// SetTMDBKey saves the key; an empty one removes it.
func (e *Engine) SetTMDBKey(key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return e.st.Delete(SettingTMDBKey)
	}
	return e.st.Set(SettingTMDBKey, key)
}

// Guess is what the organizer thinks a file is.
type Guess struct {
	Kind     string `json:"kind"` // "movie" or "episode"
	TMDB     int    `json:"tmdb"`
	Title    string `json:"title"`
	Year     int    `json:"year"`
	Season   int    `json:"season,omitempty"`
	Episodes []int  `json:"episodes,omitempty"`
	// Dup is set when other shows share this title, so the folder needs the year.
	Dup bool `json:"dup,omitempty"`
}

// Candidate is a possible answer shown in the review list.
type Candidate struct {
	Guess
	Poster string `json:"poster,omitempty"`
}

// ReviewItem is a file waiting for the owner's decision.
type ReviewItem struct {
	Path       string      `json:"path"`
	Name       string      `json:"name"`
	Reason     string      `json:"reason"`
	Parsed     string      `json:"parsed"` // e.g. "episode S03E03 of Marvels Jessica Jones"
	Kind       string      `json:"kind"`
	Title      string      `json:"title"`
	Year       int         `json:"year"`
	Season     int         `json:"season,omitempty"`
	Episodes   []int       `json:"episodes,omitempty"`
	Candidates []Candidate `json:"candidates,omitempty"`
}

type reviewBody struct {
	Kind       string      `json:"kind"`
	Title      string      `json:"title"`
	Year       int         `json:"year"`
	Season     int         `json:"season"`
	Episodes   []int       `json:"episodes"`
	Candidates []Candidate `json:"candidates"`
}

// Review lists the files waiting for the owner.
func (e *Engine) Review() ([]ReviewItem, error) {
	notes, err := e.st.MediaNotes("review")
	if err != nil {
		return nil, err
	}
	out := []ReviewItem{}
	for _, n := range notes {
		// a file that was deleted or moved away no longer needs a decision
		if vs, _ := e.st.Versions(n.CloudPath); len(vs) == 0 {
			if it, err := e.st.ItemByPath(n.CloudPath); err != nil || !it.LocalPresent {
				_ = e.st.MediaDrop(n.CloudPath)
				continue
			}
		}
		var b reviewBody
		_ = json.Unmarshal([]byte(n.Body), &b)
		it := ReviewItem{Path: n.CloudPath, Name: path.Base(n.CloudPath), Reason: n.Reason,
			Kind: b.Kind, Title: b.Title, Year: b.Year, Season: b.Season, Episodes: b.Episodes, Candidates: b.Candidates}
		it.Parsed = describeParse(b)
		out = append(out, it)
	}
	return out, nil
}

func describeParse(b reviewBody) string {
	switch b.Kind {
	case "episode":
		return fmt.Sprintf("episode %s of %s", epLabel(b.Season, b.Episodes), b.Title)
	case "movie":
		if b.Year > 0 {
			return fmt.Sprintf("movie %s (%d)", b.Title, b.Year)
		}
		return "movie " + b.Title
	}
	return "unknown"
}

func epLabel(season int, eps []int) string {
	if len(eps) == 0 {
		return fmt.Sprintf("S%02d", season)
	}
	if len(eps) == 1 {
		return fmt.Sprintf("S%02dE%02d", season, eps[0])
	}
	return fmt.Sprintf("S%02dE%02d-E%02d", season, eps[0], eps[len(eps)-1])
}

// ---- finding what to sort ----

func (e *Engine) inbox() ([]string, error) {
	root := e.MediaRoot()
	set := map[string]bool{}
	vs, err := e.st.VersionsUnder(root)
	if err != nil {
		return nil, err
	}
	for _, v := range vs {
		set[v.CloudPath] = true
	}
	items, err := e.st.ItemsUnder(root)
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		if it.LocalPresent {
			set[it.CloudPath] = true
		}
	}
	skipped := map[string]bool{}
	if ns, err := e.st.MediaNotes("skipped"); err == nil {
		for _, n := range ns {
			skipped[n.CloudPath] = true
		}
	}
	var out []string
	for p := range set {
		if underPrefix(p, root+"/Movies") || underPrefix(p, root+"/Shows") || skipped[p] {
			continue
		}
		ext := path.Ext(p)
		if !media.IsVideo(ext) && !media.IsSubtitle(ext) {
			continue
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// ---- matching ----

type lookup struct {
	db       MovieDB
	movies   map[string][]tmdb.Result
	shows    map[string][]tmdb.Result
	episodes map[string][]tmdb.Episode
}

func newLookup(db MovieDB) *lookup {
	return &lookup{db: db, movies: map[string][]tmdb.Result{}, shows: map[string][]tmdb.Result{}, episodes: map[string][]tmdb.Episode{}}
}

func (l *lookup) movie(ctx context.Context, title string, year int) ([]tmdb.Result, error) {
	k := fmt.Sprintf("%s|%d", media.Norm(title), year)
	if r, ok := l.movies[k]; ok {
		return r, nil
	}
	r, err := l.db.SearchMovie(ctx, title, year)
	if err == nil && len(r) == 0 && year > 0 {
		r, err = l.db.SearchMovie(ctx, title, 0) // the year in the name may be off
	}
	if err == nil {
		l.movies[k] = r
	}
	return r, err
}

func (l *lookup) show(ctx context.Context, title string, year int) ([]tmdb.Result, error) {
	k := fmt.Sprintf("%s|%d", media.Norm(title), year)
	if r, ok := l.shows[k]; ok {
		return r, nil
	}
	r, err := l.db.SearchTV(ctx, title, year)
	if err == nil && len(r) == 0 && year > 0 {
		r, err = l.db.SearchTV(ctx, title, 0)
	}
	if err == nil {
		l.shows[k] = r
	}
	return r, err
}

func (l *lookup) season(ctx context.Context, id, season int) ([]tmdb.Episode, error) {
	k := fmt.Sprintf("%d|%d", id, season)
	if r, ok := l.episodes[k]; ok {
		return r, nil
	}
	r, err := l.db.SeasonEpisodes(ctx, id, season)
	if err == nil {
		l.episodes[k] = r
	}
	return r, err
}

// decision is the outcome for one file: a destination, or a reason to ask.
type decision struct {
	from   string
	to     string // empty when it needs review
	reason string
	body   reviewBody
	guess  Guess
	// retry: a network or key problem, so leave the file alone for next time
	retry error
}

func candidates(rs []tmdb.Result, tv bool) []Candidate {
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].Popularity > rs[j].Popularity })
	var out []Candidate
	for i, r := range rs {
		if i >= 6 {
			break
		}
		g := Guess{TMDB: r.ID, Title: r.Title, Year: r.Year, Kind: "movie"}
		if tv {
			g.Kind = "episode"
		}
		out = append(out, Candidate{Guess: g, Poster: r.Poster})
	}
	return out
}

// pick chooses the one result a name clearly means, or reports why not.
func pick(info media.Info, rs []tmdb.Result) (tmdb.Result, bool) {
	var exact []tmdb.Result
	for _, r := range rs {
		if media.Norm(r.Title) == media.Norm(info.Title) {
			exact = append(exact, r)
		}
	}
	if info.Country != "" && len(exact) > 1 {
		var byCountry []tmdb.Result
		for _, r := range exact {
			for _, c := range r.Countries {
				if c == info.Country {
					byCountry = append(byCountry, r)
					break
				}
			}
		}
		if len(byCountry) > 0 {
			exact = byCountry
		}
	}
	if info.Year > 0 {
		var byYear []tmdb.Result
		for _, r := range exact {
			if r.Year == info.Year || r.Year == info.Year-1 || r.Year == info.Year+1 {
				byYear = append(byYear, r)
			}
		}
		exact = byYear
	}
	if len(exact) == 1 {
		return exact[0], true
	}
	if len(exact) > 1 && (info.Year > 0 || info.Country != "") {
		// several with the same title and year (or country): the famous one
		sort.SliceStable(exact, func(i, j int) bool { return exact[i].Popularity > exact[j].Popularity })
		if exact[0].Popularity >= 3*exact[1].Popularity {
			return exact[0], true
		}
	}
	return tmdb.Result{}, false
}

func dupTitle(rs []tmdb.Result, chosen tmdb.Result) bool {
	for _, r := range rs {
		if r.ID != chosen.ID && media.Norm(r.Title) == media.Norm(chosen.Title) {
			return true
		}
	}
	return false
}

func (e *Engine) decide(ctx context.Context, lk *lookup, p string) decision {
	root := e.MediaRoot()
	rel := strings.TrimPrefix(p, root+"/")
	info := media.ParseIn(rel)
	d := decision{from: p, body: reviewBody{Kind: info.Kind.String(), Title: info.Title, Year: info.Year, Season: info.Season, Episodes: info.Episodes}}
	if info.Kind == media.Unknown || info.Title == "" {
		d.reason = "couldn't tell what this file is from its name"
		return d
	}
	tv := info.Kind == media.Episode
	kind := "movie"
	if tv {
		kind = "tv"
		if !info.HasSeason || len(info.Episodes) == 0 {
			d.reason = "looks like an episode, but the season or episode number isn't in the name"
			return d
		}
	}

	var chosen tmdb.Result
	sure := false
	var cands []Candidate
	if a, ok := e.st.MediaGetAlias(aliasKey(info), kind); ok {
		chosen, sure = tmdb.Result{ID: a.TMDBID, Title: a.Title, Year: a.Year, TV: tv}, true
		if tv {
			if rs, err := lk.show(ctx, info.Title, 0); err == nil {
				d.guess.Dup = dupTitle(rs, chosen)
			}
		}
	} else {
		var rs []tmdb.Result
		var err error
		if tv {
			rs, err = lk.show(ctx, info.Title, info.Year)
		} else {
			rs, err = lk.movie(ctx, info.Title, info.Year)
		}
		if err != nil {
			d.retry = err
			return d
		}
		chosen, sure = pick(info, rs)
		cands = candidates(rs, tv)
		if sure && dupTitle(rs, chosen) {
			d.guess.Dup = true
		}
	}
	d.body.Candidates = cands
	if !sure {
		if len(cands) == 0 {
			d.reason = "TMDB has nothing called \"" + info.Title + "\""
		} else {
			d.reason = "not sure which title \"" + info.Title + "\" means"
		}
		return d
	}
	if info.Weak {
		d.reason = "the title or season came from folder names, so please confirm it"
		d.body.Candidates = []Candidate{{Guess: Guess{Kind: kindWord(tv), TMDB: chosen.ID, Title: chosen.Title, Year: chosen.Year}}}
		return d
	}
	g := Guess{Kind: kindWord(tv), TMDB: chosen.ID, Title: chosen.Title, Year: chosen.Year, Dup: d.guess.Dup}
	if tv {
		g.Season, g.Episodes = info.Season, info.Episodes
		eps, err := lk.season(ctx, chosen.ID, info.Season)
		if err != nil {
			d.retry = err
			return d
		}
		have := map[int]bool{}
		for _, ep := range eps {
			have[ep.Number] = true
		}
		for _, n := range info.Episodes {
			if len(have) > 0 && !have[n] {
				d.reason = fmt.Sprintf("TMDB lists no episode %d in season %d of %s", n, info.Season, chosen.Title)
				d.body.Candidates = []Candidate{{Guess: g}}
				return d
			}
		}
	}
	d.guess = g
	d.to = destination(root, g, p)
	return d
}

// aliasKey names what a file name says: the title, and the country tag when
// there is one ("shameless|US" is not "shameless|GB").
func aliasKey(info media.Info) string {
	k := media.Norm(info.Title)
	if info.Country != "" {
		k += "|" + info.Country
	}
	return k
}

func kindWord(tv bool) string {
	if tv {
		return "episode"
	}
	return "movie"
}

var illegal = regexp.MustCompile(`[<>"|?*\x00-\x1f]`)
var spaces = regexp.MustCompile(`\s+`)

func cleanName(s string) string {
	s = strings.NewReplacer("/", " ", "\\", " ", ":", " -").Replace(s)
	s = illegal.ReplaceAllString(s, "")
	s = spaces.ReplaceAllString(strings.TrimSpace(s), " ")
	return strings.TrimRight(s, ". ")
}

var langTags = map[string]bool{"en": true, "eng": true, "fr": true, "fre": true, "es": true, "spa": true, "de": true, "ger": true,
	"it": true, "ita": true, "pt": true, "por": true, "ar": true, "ara": true, "sw": true, "swa": true}

// destination builds the library path for a guess, keeping the extension (and
// a language tag on a subtitle file).
func destination(root string, g Guess, from string) string {
	ext := path.Ext(from)
	stem := strings.TrimSuffix(path.Base(from), ext)
	suffix := ""
	if media.IsSubtitle(ext) {
		if i := strings.LastIndex(stem, "."); i >= 0 && langTags[strings.ToLower(stem[i+1:])] {
			suffix = "." + strings.ToLower(stem[i+1:])
		}
	}
	title := cleanName(g.Title)
	if g.Kind == "movie" {
		name := title
		if g.Year > 0 {
			name = fmt.Sprintf("%s (%d)", title, g.Year)
		}
		return path.Join(root, "Movies", name, name+suffix+ext)
	}
	show := title
	if g.Dup && g.Year > 0 {
		show = fmt.Sprintf("%s (%d)", title, g.Year)
	}
	code := fmt.Sprintf("S%02dE%02d", g.Season, g.Episodes[0])
	if len(g.Episodes) > 1 {
		code += fmt.Sprintf("-E%02d", g.Episodes[len(g.Episodes)-1])
	}
	return path.Join(root, "Shows", show, fmt.Sprintf("Season %02d", g.Season), title+" - "+code+suffix+ext)
}

// ---- running it ----

// OrganizeMedia sorts whatever is loose in the library folder. It returns the
// task that shows progress; files it can't place go to the review list.
func (e *Engine) OrganizeMedia(ctx context.Context) (int64, error) {
	db, err := e.tmdbClient()
	if err != nil {
		return 0, err
	}
	files, err := e.inbox()
	if err != nil {
		return 0, err
	}
	if len(files) == 0 {
		return 0, errors.New("nothing to sort: every video in the library folder is already filed")
	}
	t := e.newTask("organize", "Sorting movies and shows")
	t.update(func(t *Task) { t.Total = len(files) })
	go func() {
		ctx := context.WithoutCancel(ctx)
		lk := newLookup(db)
		var moves []relocation
		taken := map[string]string{} // destination -> file claiming it
		sorted, asked, waiting := 0, 0, 0
		for _, p := range files {
			t.update(func(t *Task) { t.Current = path.Base(p) })
			d := e.decide(ctx, lk, p)
			switch {
			case d.retry != nil:
				waiting++
				t.update(func(t *Task) { t.Errors = append(t.Errors, path.Base(p)+": "+d.retry.Error()) })
			case d.to != "" && d.to == p:
				_ = e.st.MediaDrop(p) // already where it belongs
			case d.to != "":
				if other, dup := taken[d.to]; dup {
					d.reason = "another file (" + path.Base(other) + ") is already going to " + path.Base(d.to) + ": a duplicate?"
					d.to = ""
				} else if vs, _ := e.st.Versions(d.to); len(vs) > 0 {
					d.reason = "your library already has " + path.Base(d.to) + ": a duplicate?"
					d.to = ""
				}
				if d.to != "" {
					taken[d.to] = p
					moves = append(moves, relocation{p, d.to})
					_ = e.st.MediaDrop(p)
					sorted++
					break
				}
				fallthrough
			default:
				body, _ := json.Marshal(d.body)
				_ = e.st.MediaPut(p, "review", d.reason, string(body))
				asked++
			}
			t.update(func(t *Task) { t.Done++ })
		}
		t.update(func(t *Task) {
			t.Notes = append(t.Notes, fmt.Sprintf("%d sorted, %d need your decision", sorted, asked))
			if waiting > 0 {
				t.Notes = append(t.Notes, fmt.Sprintf("%d left for next time (TMDB wasn't reachable)", waiting))
			}
		})
		if len(moves) > 0 {
			if _, err := e.relocate(ctx, fmt.Sprintf("Filing %d video(s) into the library", len(moves)), moves); err != nil {
				t.update(func(t *Task) { t.Errors = append(t.Errors, err.Error()) })
			}
		}
		t.update(func(t *Task) { t.Finished, t.Current = true, "" })
	}()
	return t.ID, nil
}

// ResolveMedia applies the owner's decision for a file in the review list.
// The choice becomes an alias so that other files named the same way follow.
func (e *Engine) ResolveMedia(ctx context.Context, cloudPath string, g Guess) error {
	if _, ok := e.st.MediaNote(cloudPath); !ok {
		return errors.New("that file isn't waiting for a decision")
	}
	if g.Title == "" || (g.Kind != "movie" && g.Kind != "episode") {
		return errors.New("choose a title")
	}
	info := media.ParseIn(strings.TrimPrefix(cloudPath, e.MediaRoot()+"/"))
	if g.Kind == "episode" {
		if g.Season == 0 && !info.HasSeason {
			return errors.New("which season is it?")
		}
		if g.Season == 0 {
			g.Season = info.Season
		}
		if len(g.Episodes) == 0 {
			g.Episodes = info.Episodes
		}
		if len(g.Episodes) == 0 {
			return errors.New("which episode is it?")
		}
	}
	var body reviewBody
	if n, ok := e.st.MediaNote(cloudPath); ok {
		_ = json.Unmarshal([]byte(n.Body), &body)
	}
	for _, c := range body.Candidates {
		if c.TMDB != g.TMDB && media.Norm(c.Title) == media.Norm(g.Title) {
			g.Dup = true // another show shares the name: the folder needs the year
		}
	}
	if g.TMDB > 0 && info.Title != "" {
		k := "movie"
		if g.Kind == "episode" {
			k = "tv"
		}
		_ = e.st.MediaSetAlias(store.MediaAlias{Norm: aliasKey(info), Kind: k, TMDBID: g.TMDB, Title: g.Title, Year: g.Year})
	}
	to := destination(e.MediaRoot(), g, cloudPath)
	if to == cloudPath {
		return e.st.MediaDrop(cloudPath)
	}
	if vs, _ := e.st.Versions(to); len(vs) > 0 {
		return errors.New("your library already has " + path.Base(to))
	}
	moves := []relocation{{cloudPath, to}}
	taken := map[string]bool{to: true}
	// Every other file waiting under the same name follows the same answer, so
	// a whole season is one decision, not one per episode.
	if notes, err := e.st.MediaNotes("review"); err == nil && g.TMDB > 0 && info.Title != "" {
		for _, n := range notes {
			if n.CloudPath == cloudPath {
				continue
			}
			oi := media.ParseIn(strings.TrimPrefix(n.CloudPath, e.MediaRoot()+"/"))
			if aliasKey(oi) != aliasKey(info) || (oi.Kind == media.Episode) != (g.Kind == "episode") {
				continue
			}
			og := g
			if g.Kind == "episode" {
				if !oi.HasSeason || len(oi.Episodes) == 0 {
					continue
				}
				og.Season, og.Episodes = oi.Season, oi.Episodes
			}
			dst := destination(e.MediaRoot(), og, n.CloudPath)
			if dst == n.CloudPath || taken[dst] {
				continue
			}
			if vs, _ := e.st.Versions(dst); len(vs) > 0 {
				continue
			}
			taken[dst] = true
			moves = append(moves, relocation{n.CloudPath, dst})
		}
	}
	title := "Filing " + path.Base(cloudPath)
	if len(moves) > 1 {
		title = fmt.Sprintf("Filing %d files as %s", len(moves), g.Title)
	}
	if _, err := e.relocate(ctx, title, moves); err != nil {
		return err
	}
	for _, m := range moves {
		_ = e.st.MediaDrop(m.from)
	}
	return nil
}

// SkipMedia tells the organizer to leave a file where it is.
func (e *Engine) SkipMedia(cloudPath string) error {
	return e.st.MediaPut(cloudPath, "skipped", "left alone on purpose", "")
}

// SearchMedia looks titles up so a wrong guess can be corrected by hand.
func (e *Engine) SearchMedia(ctx context.Context, kind, q string, year int) ([]Candidate, error) {
	db, err := e.tmdbClient()
	if err != nil {
		return nil, err
	}
	var rs []tmdb.Result
	if kind == "episode" {
		rs, err = db.SearchTV(ctx, q, year)
	} else {
		rs, err = db.SearchMovie(ctx, q, year)
	}
	if err != nil {
		return nil, err
	}
	return candidates(rs, kind == "episode"), nil
}
