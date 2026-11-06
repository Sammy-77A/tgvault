package tmdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fake(t *testing.T) (*Client, *[]string) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path+"?"+r.URL.RawQuery+" auth="+r.Header.Get("Authorization"))
		switch {
		case r.URL.Path == "/search/movie":
			w.Write([]byte(`{"results":[{"id":14160,"title":"Up","release_date":"2009-05-28","popularity":50.5,"poster_path":"/up.jpg"}]}`))
		case r.URL.Path == "/search/tv":
			w.Write([]byte(`{"results":[{"id":34307,"name":"Shameless","first_air_date":"2011-01-09","popularity":90}]}`))
		case r.URL.Path == "/tv/34307":
			w.Write([]byte(`{"seasons":[{"season_number":0,"episode_count":2},{"season_number":1,"episode_count":12}]}`))
		case r.URL.Path == "/tv/34307/season/1":
			w.Write([]byte(`{"episodes":[{"episode_number":1,"name":"Pilot"},{"episode_number":2,"name":"Frances"}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return &Client{Key: "shortkey", Base: srv.URL}, &seen
}

func TestSearchAndSeasons(t *testing.T) {
	c, seen := fake(t)
	ctx := context.Background()
	m, err := c.SearchMovie(ctx, "Up", 2009)
	if err != nil || len(m) != 1 || m[0].ID != 14160 || m[0].Year != 2009 || m[0].TV {
		t.Fatalf("movie: %v %+v", err, m)
	}
	tv, err := c.SearchTV(ctx, "Shameless", 0)
	if err != nil || len(tv) != 1 || !tv[0].TV || tv[0].Title != "Shameless" {
		t.Fatalf("tv: %v %+v", err, tv)
	}
	s, _ := c.Seasons(ctx, 34307)
	if len(s) != 2 || s[1].Number != 1 || s[1].Episodes != 12 {
		t.Fatalf("seasons: %+v", s)
	}
	e, _ := c.SeasonEpisodes(ctx, 34307, 1)
	if len(e) != 2 || e[1].Name != "Frances" {
		t.Fatalf("episodes: %+v", e)
	}
	if !strings.Contains((*seen)[0], "api_key=shortkey") || !strings.Contains((*seen)[0], "year=2009") {
		t.Fatalf("request: %v", *seen)
	}
}

func TestLongTokenIsSentAsBearer(t *testing.T) {
	c, seen := fake(t)
	c.Key = "eyJhbGciOi.fake.token"
	if _, err := c.SearchMovie(context.Background(), "Up", 0); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains((*seen)[0], "auth=Bearer eyJhbGciOi.fake.token") || strings.Contains((*seen)[0], "api_key") {
		t.Fatalf("request: %v", *seen)
	}
}

func TestNoKeyAndBadKey(t *testing.T) {
	c, _ := fake(t)
	c.Key = " "
	if _, err := c.SearchMovie(context.Background(), "Up", 0); err != ErrNoKey {
		t.Fatalf("want ErrNoKey, got %v", err)
	}
	c.Key = "x"
	if _, err := c.Seasons(context.Background(), 1); err == nil { // the fake answers 404
		t.Fatal("want an error")
	}
}
