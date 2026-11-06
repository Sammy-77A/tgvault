package subs

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSRTToVTTWithShift(t *testing.T) {
	srt := "ï»¿1\r\n00:00:01,000 --> 00:00:03,500\r\nHello there\r\n<i>second line</i>\r\n\r\n2\r\n00:01:00,250 --> 00:01:02,000\r\nBye\r\n"
	got := string(ToVTT([]byte(srt), "a.srt", 0))
	for _, want := range []string{"WEBVTT", "00:00:01.000 --> 00:00:03.500\nHello there\n<i>second line</i>", "00:01:00.250 --> 00:01:02.000\nBye"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	late := string(ToVTT([]byte(srt), "a.srt", 2*time.Second+500*time.Millisecond))
	if !strings.Contains(late, "00:00:03.500 --> 00:00:06.000") {
		t.Errorf("shifting later: %s", late)
	}
	early := string(ToVTT([]byte(srt), "a.srt", -2*time.Second))
	if strings.Contains(early, "Hello there") && !strings.Contains(early, "00:00:00.000 --> 00:00:01.500") {
		t.Errorf("a cue pushed before the start is clamped: %s", early)
	}
	gone := string(ToVTT([]byte(srt), "a.srt", -10*time.Second))
	if strings.Contains(gone, "Hello there") {
		t.Errorf("a cue that ends before the start is dropped: %s", gone)
	}
}

func TestASSAndLatin1(t *testing.T) {
	ass := "[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:05.00,0:00:07.50,Default,,0,0,0,,{\\i1}Hi\\Nthere\n"
	got := string(ToVTT([]byte(ass), "a.ass", 0))
	if !strings.Contains(got, "00:00:05.000 --> 00:00:07.500\nHi\nthere") {
		t.Errorf("ass: %s", got)
	}
	latin := []byte("1\n00:00:01,000 --> 00:00:02,000\ncaf\xe9\n")
	if !strings.Contains(string(ToVTT(latin, "a.srt", 0)), "café") {
		t.Errorf("windows-1252 should be read as such")
	}
}

func TestSearchAndFetch(t *testing.T) {
	var asked string
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	w, _ := zw.Create("Show.S01E02.srt")
	w.Write([]byte("1\n00:00:01,000 --> 00:00:02,000\nhi\n"))
	zw.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/dl/") {
			w.Write(zbuf.Bytes())
			return
		}
		asked = r.URL.RawQuery
		w.Write([]byte(`{"status":true,"subtitles":[{"release_name":"Show.S01E02.720p","lang":"EN","author":"me","url":"/dl/1.zip","season":1,"episode":2},{"release_name":"x","url":""}]}`))
	}))
	defer srv.Close()
	c := &Client{Key: "k", Base: srv.URL + "/search", DL: srv.URL}
	rs, err := c.Search(context.Background(), Query{TMDB: 34307, Show: true, Season: 1, Episode: 2, Lang: "en"})
	if err != nil || len(rs) != 1 || rs[0].Lang != "en" || rs[0].Download != "/dl/1.zip" {
		t.Fatalf("search: %v %+v", err, rs)
	}
	for _, want := range []string{"api_key=k", "tmdb_id=34307", "type=tv", "season_number=1", "episode_number=2", "languages=EN"} {
		if !strings.Contains(asked, want) {
			t.Errorf("query lacks %s: %s", want, asked)
		}
	}
	name, data, err := c.Fetch(context.Background(), rs[0].Download)
	if err != nil || !strings.HasSuffix(name, ".srt") || !strings.Contains(string(data), "hi") {
		t.Fatalf("fetch: %v %s %q", err, name, data)
	}
	if _, _, err := c.Fetch(context.Background(), "http://evil/x"); err == nil {
		t.Error("only paths on the service are fetched")
	}
	if _, err := (&Client{}).Search(context.Background(), Query{TMDB: 1}); err != ErrNoKey {
		t.Errorf("want ErrNoKey, got %v", err)
	}
}
