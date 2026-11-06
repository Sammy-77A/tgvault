// Package tmdb is a small client for The Movie Database: just what tgvault
// needs to recognise a title and list a season's episodes.
//
// This product uses the TMDB API but is not endorsed or certified by TMDB.
package tmdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const base = "https://api.themoviedb.org/3"

// ErrNoKey means no TMDB key has been entered yet.
var ErrNoKey = errors.New("no TMDB key set: add it in tgvault's settings")

// Client talks to TMDB. Key may be the short API key or the long read access
// token; either works.
type Client struct {
	Key  string
	HTTP *http.Client
	Base string // overridden by tests
	// ImageBase is where pictures come from; tests change it.
	ImageBase string
}

// Result is one search hit.
type Result struct {
	ID         int
	Title      string
	Year       int
	Popularity float64
	Poster     string
	TV         bool
	Countries  []string // where a show comes from ("US", "GB"); empty for movies
}

// Season is a season of a show and how many episodes TMDB lists for it.
type Season struct {
	Number   int
	Episodes int
}

// Episode is one episode of a season.
type Episode struct {
	Number int
	Name   string
}

func (c *Client) get(ctx context.Context, p string, q url.Values, out any) error {
	if strings.TrimSpace(c.Key) == "" {
		return ErrNoKey
	}
	b := c.Base
	if b == "" {
		b = base
	}
	if q == nil {
		q = url.Values{}
	}
	key := strings.TrimSpace(c.Key)
	bearer := strings.HasPrefix(key, "eyJ")
	if !bearer {
		q.Set("api_key", key)
	}
	cl := c.HTTP
	if cl == nil {
		cl = &http.Client{Timeout: 20 * time.Second}
	}
	var last error
	for try := 0; try < 3; try++ {
		req, err := http.NewRequestWithContext(ctx, "GET", b+p+"?"+q.Encode(), nil)
		if err != nil {
			return err
		}
		if bearer {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := cl.Do(req)
		if err != nil {
			last = err
		} else {
			err = decode(resp, out)
			if err == nil {
				return nil
			}
			last = err
			var he *httpError
			if errors.As(err, &he) && he.code != 429 && he.code < 500 {
				return err // a bad key or missing title will not improve
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(try+1) * 700 * time.Millisecond):
		}
	}
	return last
}

type httpError struct{ code int }

func (e *httpError) Error() string {
	if e.code == 401 {
		return "TMDB rejected the key (401): check it in settings"
	}
	return fmt.Sprintf("TMDB answered %d", e.code)
}

func decode(resp *http.Response, out any) error {
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return &httpError{resp.StatusCode}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func year(date string) int {
	if len(date) >= 4 {
		y, _ := strconv.Atoi(date[:4])
		return y
	}
	return 0
}

type searchResp struct {
	Results []struct {
		ID         int      `json:"id"`
		Title      string   `json:"title"`
		Name       string   `json:"name"`
		Release    string   `json:"release_date"`
		First      string   `json:"first_air_date"`
		Popularity float64  `json:"popularity"`
		Poster     string   `json:"poster_path"`
		Countries  []string `json:"origin_country"`
	} `json:"results"`
}

func (c *Client) search(ctx context.Context, kind, title string, yr int) ([]Result, error) {
	q := url.Values{"query": {title}, "include_adult": {"false"}}
	if yr > 0 {
		if kind == "movie" {
			q.Set("year", strconv.Itoa(yr))
		} else {
			q.Set("first_air_date_year", strconv.Itoa(yr))
		}
	}
	var r searchResp
	if err := c.get(ctx, "/search/"+kind, q, &r); err != nil {
		return nil, err
	}
	var out []Result
	for _, x := range r.Results {
		t, d := x.Title, x.Release
		if kind == "tv" {
			t, d = x.Name, x.First
		}
		out = append(out, Result{ID: x.ID, Title: t, Year: year(d), Popularity: x.Popularity, Poster: x.Poster, TV: kind == "tv", Countries: x.Countries})
	}
	return out, nil
}

// SearchMovie looks a movie up by title, and by year when one is known.
func (c *Client) SearchMovie(ctx context.Context, title string, yr int) ([]Result, error) {
	return c.search(ctx, "movie", title, yr)
}

// SearchTV looks a show up by title.
func (c *Client) SearchTV(ctx context.Context, title string, yr int) ([]Result, error) {
	return c.search(ctx, "tv", title, yr)
}

// Seasons lists a show's seasons (specials, season 0, included).
func (c *Client) Seasons(ctx context.Context, tvID int) ([]Season, error) {
	var r struct {
		Seasons []struct {
			Number   int `json:"season_number"`
			Episodes int `json:"episode_count"`
		} `json:"seasons"`
	}
	if err := c.get(ctx, "/tv/"+strconv.Itoa(tvID), nil, &r); err != nil {
		return nil, err
	}
	var out []Season
	for _, s := range r.Seasons {
		out = append(out, Season{s.Number, s.Episodes})
	}
	return out, nil
}

// SeasonEpisodes lists the episodes of one season.
func (c *Client) SeasonEpisodes(ctx context.Context, tvID, season int) ([]Episode, error) {
	var r struct {
		Episodes []struct {
			Number int    `json:"episode_number"`
			Name   string `json:"name"`
		} `json:"episodes"`
	}
	if err := c.get(ctx, fmt.Sprintf("/tv/%d/season/%d", tvID, season), nil, &r); err != nil {
		return nil, err
	}
	var out []Episode
	for _, e := range r.Episodes {
		out = append(out, Episode{e.Number, e.Name})
	}
	return out, nil
}

// ---- raw access with a short-lived cache, for the movie app's pages ----

type cacheEntry struct {
	body    []byte
	expires time.Time
}

var (
	cacheMu sync.Mutex
	cache   = map[string]cacheEntry{}
)

// Raw returns TMDB's JSON for a path such as "/movie/603", cached for ttl so
// that browsing doesn't spend the daily allowance (and works a while offline).
func (c *Client) Raw(ctx context.Context, p string, q url.Values, ttl time.Duration) (json.RawMessage, error) {
	key := p + "?" + q.Encode()
	cacheMu.Lock()
	if e, ok := cache[key]; ok && time.Now().Before(e.expires) {
		cacheMu.Unlock()
		return e.body, nil
	}
	cacheMu.Unlock()
	var raw json.RawMessage
	if err := c.get(ctx, p, cloneValues(q), &raw); err != nil {
		// stale is better than nothing when offline
		cacheMu.Lock()
		e, ok := cache[key]
		cacheMu.Unlock()
		if ok {
			return e.body, nil
		}
		return nil, err
	}
	cacheMu.Lock()
	if len(cache) > 500 {
		cache = map[string]cacheEntry{}
	}
	cache[key] = cacheEntry{raw, time.Now().Add(ttl)}
	cacheMu.Unlock()
	return raw, nil
}

func cloneValues(q url.Values) url.Values {
	out := url.Values{}
	for k, v := range q {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// Image fetches a picture from TMDB's image server. size is like "w342" or
// "w780"; p is the path TMDB gave, such as "/abc.jpg".
func (c *Client) Image(ctx context.Context, size, p string) ([]byte, string, error) {
	if !strings.HasPrefix(p, "/") || strings.ContainsAny(p, "?#\\") || strings.Contains(p, "..") {
		return nil, "", errors.New("bad image path")
	}
	cl := c.HTTP
	if cl == nil {
		cl = &http.Client{Timeout: 30 * time.Second}
	}
	base := c.ImageBase
	if base == "" {
		base = "https://image.tmdb.org/t/p"
	}
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/"+size+p, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, "", &httpError{resp.StatusCode}
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return b, resp.Header.Get("Content-Type"), err
}
