package web

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Sammy-77A/tgvault/internal/backup"
	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/tgclient"
)

// mailTG is just enough Telegram for requests: Saved Messages in memory.
type mailTG struct {
	mu   sync.Mutex
	next int
	self []struct {
		id   int
		text string
	}
}

func (f *mailTG) Info() tgclient.Info                                         { return tgclient.Info{Status: tgclient.StatusReady} }
func (f *mailTG) WaitReady(context.Context) error                             { return nil }
func (f *mailTG) Upload(context.Context, tgclient.UploadRequest) (int, error) { return 0, io.EOF }
func (f *mailTG) Download(context.Context, int, io.Writer) error              { return io.EOF }
func (f *mailTG) ReadBlock(context.Context, int, int64) ([]byte, error)       { return nil, io.EOF }
func (f *mailTG) Thumbs(context.Context, []int) (map[int][]byte, error)       { return nil, nil }
func (f *mailTG) DeleteMessages(context.Context, []int) error                 { return nil }
func (f *mailTG) EditCaption(context.Context, int, string) error              { return nil }
func (f *mailTG) Exists(context.Context, []int) (map[int]tgclient.RemoteFile, error) {
	return nil, nil
}
func (f *mailTG) MessageLink(int) string                                             { return "" }
func (f *mailTG) ScanChannel(context.Context, func(tgclient.RemoteFile) error) error { return nil }
func (f *mailTG) ScanSince(context.Context, int, func(tgclient.RemoteFile) error) error {
	return nil
}
func (f *mailTG) SendSelf(_ context.Context, text string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	f.self = append(f.self, struct {
		id   int
		text string
	}{f.next, text})
	return f.next, nil
}
func (f *mailTG) SelfHistory(_ context.Context, min int, prefix string, fn func(int, string, int64) error) error {
	f.mu.Lock()
	list := append(f.self[:0:0], f.self...)
	f.mu.Unlock()
	for _, m := range list {
		if m.id > min && strings.HasPrefix(m.text, prefix) {
			if err := fn(m.id, m.text, 1700000000); err != nil {
				return err
			}
		}
	}
	return nil
}

func fakeTMDB(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/trending/all/week"):
			w.Write([]byte(`{"results":[{"id":14160,"media_type":"movie","title":"Up","release_date":"2009-05-28","poster_path":"/up.jpg","vote_average":8.3},{"id":99,"media_type":"person","name":"Someone"},{"id":34307,"media_type":"tv","name":"Shameless","first_air_date":"2011-01-09","poster_path":"/sh.jpg"}]}`))
		case strings.HasPrefix(r.URL.Path, "/movie/popular"), strings.HasPrefix(r.URL.Path, "/movie/top_rated"):
			w.Write([]byte(`{"results":[{"id":603,"title":"The Matrix","release_date":"1999-03-30"}]}`))
		case strings.HasPrefix(r.URL.Path, "/tv/popular"), strings.HasPrefix(r.URL.Path, "/tv/top_rated"):
			w.Write([]byte(`{"results":[{"id":1396,"name":"Breaking Bad","first_air_date":"2008-01-20"}]}`))
		case strings.HasPrefix(r.URL.Path, "/genre/"):
			w.Write([]byte(`{"genres":[{"id":28,"name":"Action"}]}`))
		case r.URL.Path == "/tv/34307":
			w.Write([]byte(`{"id":34307,"name":"Shameless","first_air_date":"2011-01-09","overview":"A family.","vote_average":8.1,"episode_run_time":[50],
				"seasons":[{"season_number":0,"name":"Specials","episode_count":3},{"season_number":1,"name":"Season 1","episode_count":12,"air_date":"2011-01-09"},{"season_number":2,"name":"Season 2","episode_count":12}],
				"genres":[{"name":"Drama"}],"credits":{"cast":[{"name":"William H. Macy","character":"Frank","profile_path":"/m.jpg"}]},
				"videos":{"results":[{"key":"abc123","name":"Official Trailer","site":"YouTube","type":"Trailer","official":true},{"key":"zzz","name":"Clip","site":"YouTube","type":"Clip"}]}}`))
		case r.URL.Path == "/tv/34307/season/1":
			w.Write([]byte(`{"episodes":[{"episode_number":1,"name":"Pilot","runtime":50},{"episode_number":2,"name":"Frances"},{"episode_number":3,"name":"Casey"}]}`))
		case r.URL.Path == "/search/movie":
			w.Write([]byte(`{"results":[{"id":1,"title":"Up Again","poster_path":"/no.jpg"},{"id":14160,"title":"Up","release_date":"2009-05-28","poster_path":"/up.jpg"}]}`))
		case r.URL.Path == "/search/tv":
			w.Write([]byte(`{"results":[{"id":34307,"name":"Shameless","poster_path":"/sh.jpg"}]}`))
		case r.URL.Path == "/search/multi":
			w.Write([]byte(`{"results":[{"id":14160,"media_type":"movie","title":"Up","release_date":"2009-05-28"}]}`))
		case r.URL.Path == "/t/p/w342/up.jpg":
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write([]byte("JPEGDATA"))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// mbRig is a movie app server over an in-memory library of two files.
func mbRig(t *testing.T) (*Server, *backup.Engine, *mailTG) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	tg := &mailTG{}
	eng := backup.NewWith(st, tg, dir)
	root := "Downloads/Telegram Desktop/tgmoviebox"
	st.Set(backup.SettingMediaRoot, root)
	st.Set(backup.SettingTMDBKey, "k")
	for id, p := range map[int]string{
		1: root + "/Shows/Shameless/Season 01/Shameless - S01E01.mkv",
		2: root + "/Shows/Shameless/Season 01/Shameless - S01E02.mkv",
		3: root + "/Movies/Up (2009)/Up (2009).mkv",
		4: root + "/Movies/Up (2009)/Up (2009).en.srt",
		5: "Documents/private.pdf",
	} {
		if err := st.AddVersion(store.Version{MsgID: id, CloudPath: p, Kind: store.KindFile, Size: 8, Hash: "h", UploadedAt: int64(id)}); err != nil {
			t.Fatal(err)
		}
	}
	fake := fakeTMDB(t)
	s := &Server{st: st, eng: eng, addr: "127.0.0.1:1", tmdbBase: fake.URL, imageBase: fake.URL + "/t/p"}
	s.SetLibrary(root)
	return s, eng, tg
}

func get(t *testing.T, h http.HandlerFunc, url string, into any) int {
	t.Helper()
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest("GET", url, nil))
	if into != nil && w.Code == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), into); err != nil {
			t.Fatalf("%s: %v\n%s", url, err, w.Body)
		}
	}
	return w.Code
}

func post(t *testing.T, h http.HandlerFunc, url, body string, into any) int {
	t.Helper()
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest("POST", url, strings.NewReader(body)))
	if into != nil && w.Code == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), into); err != nil {
			t.Fatalf("%s: %v\n%s", url, err, w.Body)
		}
	}
	return w.Code
}

func TestHomeRowsMarkWhatIsInTheLibrary(t *testing.T) {
	s, _, _ := mbRig(t)
	var home struct {
		Rows []struct {
			Key   string
			Items []mbItem
		}
		Genres map[string][]struct{ Name string }
	}
	if code := get(t, s.mbHome, "/api/mb/home", &home); code != 200 {
		t.Fatalf("home: %d", code)
	}
	if len(home.Rows) != 5 || home.Rows[0].Key != "trending" {
		t.Fatalf("rows: %+v", home.Rows)
	}
	trending := home.Rows[0].Items
	if len(trending) != 2 { // the person is not a title
		t.Fatalf("trending: %+v", trending)
	}
	var up, shameless mbItem
	for _, it := range trending {
		if it.Title == "Up" {
			up = it
		}
		if it.Title == "Shameless" {
			shameless = it
		}
	}
	if up.Kind != "movie" || up.Year != 2009 || !up.InLibrary || shameless.Kind != "show" || !shameless.InLibrary {
		t.Errorf("titles in the library should say so: %+v %+v", up, shameless)
	}
	if len(home.Genres["movie"]) == 0 {
		t.Error("genres missing")
	}
}

func TestDetailsShowCastTrailersSeasonsAndWhatWeHave(t *testing.T) {
	s, _, _ := mbRig(t)
	var d struct {
		Item     mbItem
		Runtime  int
		Genres   []string
		Cast     []struct{ Name, Character string }
		Trailers []struct{ Key string }
		Seasons  []struct {
			Number, Episodes, Have int
		}
	}
	if code := get(t, s.mbTitle, "/api/mb/title?kind=show&id=34307", &d); code != 200 {
		t.Fatalf("title: %d", code)
	}
	if d.Item.Title != "Shameless" || d.Runtime != 50 || len(d.Cast) != 1 || d.Cast[0].Character != "Frank" {
		t.Errorf("details: %+v", d)
	}
	if len(d.Trailers) != 1 || d.Trailers[0].Key != "abc123" {
		t.Errorf("only real trailers: %+v", d.Trailers)
	}
	if len(d.Seasons) != 2 || d.Seasons[0].Number != 1 || d.Seasons[0].Have != 2 || d.Seasons[1].Have != 0 {
		t.Errorf("seasons (specials hidden, 2 of 12 present in season 1): %+v", d.Seasons)
	}
	var sn struct {
		Episodes []struct {
			Number int
			Name   string
			File   *backup.LibFile
		}
	}
	if code := get(t, s.mbSeason, "/api/mb/season?id=34307&n=1&title=Shameless", &sn); code != 200 || len(sn.Episodes) != 3 {
		t.Fatalf("season: %d %+v", code, sn)
	}
	if sn.Episodes[0].File == nil || sn.Episodes[1].File == nil || sn.Episodes[2].File != nil {
		t.Errorf("episodes 1 and 2 are in the library, 3 is missing: %+v", sn.Episodes)
	}
	if get(t, s.mbTitle, "/api/mb/title?kind=game&id=1", nil) != 400 {
		t.Error("bad kind refused")
	}
}

func TestRequestFromTheAppReachesSavedMessages(t *testing.T) {
	s, _, tg := mbRig(t)
	var out struct{ Request backup.Request }
	body := `{"kind":"show","tmdb":34307,"title":"Shameless","year":2011,"poster":"/sh.jpg","seasons":[3,4]}`
	if code := post(t, s.mbRequest, "/api/mb/request", body, &out); code != 200 || out.Request.ID == "" {
		t.Fatalf("request: %d %+v", code, out)
	}
	if len(tg.self) != 1 || !strings.HasPrefix(tg.self[0].text, "#tgmb-request ") || !strings.Contains(tg.self[0].text, "Shameless") {
		t.Fatalf("Saved Messages: %+v", tg.self)
	}
	if code := post(t, s.mbRequest, "/api/mb/request", body, nil); code != 400 {
		t.Errorf("asking twice: %d", code)
	}
	var list struct {
		Requests []struct {
			Title, State string
			Available    bool
		}
	}
	if get(t, s.mbRequests, "/api/mb/requests", &list) != 200 || len(list.Requests) != 1 || list.Requests[0].State != "requested" {
		t.Fatalf("list: %+v", list)
	}
	if !list.Requests[0].Available {
		t.Error("Shameless is in the library, so it can be marked available")
	}
}

func TestDownloadsPlayAndProgressStayInsideTheLibrary(t *testing.T) {
	s, eng, _ := mbRig(t)
	for _, p := range []string{`["Documents/private.pdf"]`, `["Downloads/Telegram Desktop/tgmoviebox/../x"]`} {
		if code := post(t, s.mbDownload, "/api/mb/download", `{"paths":`+p+`}`, nil); code != 403 && code != 400 {
			t.Errorf("%s: want a refusal, got %d", p, code)
		}
	}
	if code := post(t, s.mbPlaylist, "/api/mb/playlist", `{"paths":["Downloads/Telegram Desktop/tgmoviebox/Movies/Up (2009)/Up (2009).mkv"]}`, nil); code != 400 {
		t.Errorf("nothing streams: a video that isn't downloaded can't be played, got %d", code)
	}
	// pretend it was downloaded
	up := "Downloads/Telegram Desktop/tgmoviebox/Movies/Up (2009)/Up (2009)"
	local := eng.MediaLocal(up + ".mkv")
	os.MkdirAll(filepath.Dir(local), 0o755)
	os.WriteFile(local, []byte("VIDEODATA"), 0o644)
	os.WriteFile(strings.TrimSuffix(local, ".mkv")+".en.srt", []byte("1\n00:00:01,000 --> 00:00:02,000\nHi\n"), 0o644)
	var resp struct {
		URL      string
		Playlist struct {
			Items []struct {
				Name, URL string
				Subs      []struct{ URL, Lang string }
			}
		}
	}
	if code := post(t, s.mbPlaylist, "/api/mb/playlist", `{"paths":["`+up+`.mkv"],"tmdb":14160}`, &resp); code != 200 || len(resp.Playlist.Items) != 1 || !strings.Contains(resp.URL, "/plist/") {
		t.Fatalf("playlist: %d %+v", code, resp)
	}
	it := resp.Playlist.Items[0]
	if len(it.Subs) != 1 || it.Subs[0].Lang != "en" {
		t.Fatalf("the sidecar subtitle should be offered: %+v", it)
	}
	// the links work, from this device only
	serve := func(h http.HandlerFunc, link string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", strings.TrimPrefix(link, "http://"+s.addr), nil)
		r.RemoteAddr = "127.0.0.1:5555"
		h(w, r)
		return w
	}
	if w := serve(s.play, it.URL); w.Code != 200 || w.Body.String() != "VIDEODATA" {
		t.Errorf("play: %d %q", w.Code, w.Body)
	}
	vtt := serve(s.sub, it.Subs[0].URL+"?shift=1500")
	if vtt.Code != 200 || !strings.Contains(vtt.Body.String(), "WEBVTT") || !strings.Contains(vtt.Body.String(), "00:00:02.500 --> 00:00:03.500") {
		t.Errorf("subtitle shifted: %d %q", vtt.Code, vtt.Body)
	}
	r := httptest.NewRequest("GET", strings.TrimPrefix(it.URL, "http://"+s.addr), nil)
	r.RemoteAddr = "192.168.1.9:5555"
	w := httptest.NewRecorder()
	s.play(w, r)
	if w.Code != 403 {
		t.Errorf("only this device may play: %d", w.Code)
	}
	// progress
	if code := post(t, s.mbProgress, "/api/mb/progress", `{"path":"`+up+`.mkv","pos":60000,"dur":5400000}`, nil); code != 200 {
		t.Fatalf("progress: %d", code)
	}
	if pos, _ := eng.Progress(up + ".mkv"); pos != 60000 {
		t.Errorf("progress saved: %d", pos)
	}
	if code := post(t, s.mbProgress, "/api/mb/progress", `{"path":"Documents/private.pdf","pos":6,"dur":9}`, nil); code != 400 {
		t.Errorf("only library files keep progress: %d", code)
	}
	// removing
	var rm struct{ Removed int }
	if post(t, s.mbRemove, "/api/mb/remove", `{"paths":["`+up+`.mkv"]}`, &rm) != 200 || rm.Removed != 1 {
		t.Errorf("remove: %+v", rm)
	}
}

func TestMovieAppRoutesPassTheLibraryGate(t *testing.T) {
	s, _, _ := mbRig(t)
	for _, c := range []struct {
		method, url, body string
		want              int
	}{
		{"GET", "/api/mb/home", "", 200},
		{"GET", "/api/mb/title?kind=movie&id=1", "", 200},
		{"POST", "/api/mb/request", "{}", 200},
		{"POST", "/api/mb/playlist", "{}", 200},
		{"GET", "/api/mb/subs/search?tmdb=1", "", 200},
		{"POST", "/api/media/answer", "{}", 403}, // answering requests is the PC's
		{"POST", "/api/media/organize", "{}", 403},
		{"POST", "/api/bulk", `{"action":"delete-cloud","paths":["x"]}`, 403},
	} {
		if got := gate(s, c.method, c.url, c.body); got != c.want {
			t.Errorf("%s %s: got %d, want %d", c.method, c.url, got, c.want)
		}
	}
}

func TestPicturesAreKeptForOffline(t *testing.T) {
	s, _, _ := mbRig(t)
	w := httptest.NewRecorder()
	s.mbImg(w, httptest.NewRequest("GET", "/api/mb/img?p=/up.jpg&s=w342", nil))
	if w.Code != 200 || w.Body.String() != "JPEGDATA" {
		t.Fatalf("image: %d %q", w.Code, w.Body)
	}
	s.tmdbBase, s.imageBase = "http://127.0.0.1:1", "http://127.0.0.1:1" // now offline
	w = httptest.NewRecorder()
	s.mbImg(w, httptest.NewRequest("GET", "/api/mb/img?p=/up.jpg&s=w342", nil))
	if w.Code != 200 || w.Body.String() != "JPEGDATA" {
		t.Errorf("the saved copy should still show: %d", w.Code)
	}
	w = httptest.NewRecorder()
	s.mbImg(w, httptest.NewRequest("GET", "/api/mb/img?p=/../secret&s=w342", nil))
	if w.Code != 400 {
		t.Errorf("bad path: %d", w.Code)
	}
}

func TestNativePlayerDoor(t *testing.T) {
	s, eng, _ := mbRig(t)
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	f, _ := zw.Create("Up.srt")
	f.Write([]byte("1\n00:00:01,000 --> 00:00:02,000\nFrom the internet\n"))
	zw.Close()
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/dl/") {
			w.Write(zbuf.Bytes())
			return
		}
		w.Write([]byte(`{"status":true,"subtitles":[{"release_name":"Up.2009.720p","lang":"EN","url":"/dl/up.zip"}]}`))
	}))
	defer fake.Close()
	s.subsBase, s.subsDL = fake.URL+"/search", fake.URL
	s.st.Set(backup.SettingSubtitleKey, "subkey")

	up := "Downloads/Telegram Desktop/tgmoviebox/Movies/Up (2009)/Up (2009).mkv"
	local := eng.MediaLocal(up)
	os.MkdirAll(filepath.Dir(local), 0o755)
	os.WriteFile(local, []byte("VIDEO"), 0o644)
	var resp struct{ URL string }
	if post(t, s.mbPlaylist, "/api/mb/playlist", `{"paths":["`+up+`"],"tmdb":14160}`, &resp) != 200 {
		t.Fatal("playlist")
	}
	door := func(method, sub, query, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(method, strings.TrimPrefix(resp.URL, "http://"+s.addr)+sub+query, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:4000"
		s.plist(w, r)
		return w
	}
	if w := door("GET", "", "", ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"Up (2009).mkv"`) {
		t.Fatalf("the player reads the playlist: %d %s", w.Code, w.Body)
	}
	if w := door("POST", "/progress", "?i=0&pos=120000&dur=5400000", ""); w.Code != 200 {
		t.Fatalf("progress: %d %s", w.Code, w.Body)
	}
	if pos, _ := eng.Progress(up); pos != 120000 {
		t.Errorf("progress saved: %d", pos)
	}
	if w := door("POST", "/progress", "?i=7&pos=1&dur=2", ""); w.Code != 400 {
		t.Errorf("a made-up item: %d", w.Code)
	}
	w := door("GET", "/subs", "?i=0&lang=en", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "/dl/up.zip") {
		t.Fatalf("subtitle search: %d %s", w.Code, w.Body)
	}
	w = door("POST", "/subget", "?i=0", `{"download":"/dl/up.zip","lang":"en"}`)
	if w.Code != 200 {
		t.Fatalf("subtitle get: %d %s", w.Code, w.Body)
	}
	saved, err := os.ReadFile(strings.TrimSuffix(local, ".mkv") + ".en.srt")
	if err != nil || !strings.Contains(string(saved), "From the internet") {
		t.Errorf("the subtitle should sit beside the video: %v %q", err, saved)
	}
	// strangers: another device, a wrong key
	r := httptest.NewRequest("GET", strings.TrimPrefix(resp.URL, "http://"+s.addr), nil)
	r.RemoteAddr = "10.0.0.5:4000"
	w = httptest.NewRecorder()
	s.plist(w, r)
	if w.Code != 403 {
		t.Errorf("only this device: %d", w.Code)
	}
	r = httptest.NewRequest("GET", "/plist/nope", nil)
	r.RemoteAddr = "127.0.0.1:4000"
	w = httptest.NewRecorder()
	s.plist(w, r)
	if w.Code != 404 {
		t.Errorf("wrong key: %d", w.Code)
	}
}

func TestLibraryListGetsTitlesPostersAndIDs(t *testing.T) {
	s, _, _ := mbRig(t)
	var lib struct {
		Titles []struct {
			Title  string
			Kind   string
			TMDB   int
			Poster string
			Total  int
			Local  int
		}
	}
	if get(t, s.mbLibrary, "/api/mb/library", &lib) != 200 || len(lib.Titles) != 2 {
		t.Fatalf("library: %+v", lib)
	}
	for _, tl := range lib.Titles {
		switch tl.Title {
		case "Up":
			if tl.TMDB != 14160 || tl.Poster != "/up.jpg" || tl.Total != 1 {
				t.Errorf("Up should be matched by name, not just first result: %+v", tl)
			}
		case "Shameless":
			if tl.TMDB != 34307 || tl.Total != 2 || tl.Local != 0 {
				t.Errorf("Shameless: %+v", tl)
			}
		default:
			t.Errorf("unexpected title %q", tl.Title)
		}
	}
	s.st.Delete(backup.SettingTMDBKey) // no key: still lists, without pictures
	if get(t, s.mbLibrary, "/api/mb/library", &lib) != 200 || len(lib.Titles) != 2 {
		t.Errorf("the library lists offline too")
	}
}

func TestTwoShowsWithOneNameAreToldApartByYear(t *testing.T) {
	s, _, _ := mbRig(t)
	root := "Downloads/Telegram Desktop/tgmoviebox"
	// the library holds the 2011 one, filed with its year because another Shameless exists
	for id, p := range map[int]string{
		20: root + "/Shows/Shameless (2011)/Season 01/Shameless - S01E01.mkv",
	} {
		s.st.AddVersion(store.Version{MsgID: id, CloudPath: p, Kind: store.KindFile, Size: 8, Hash: "h", UploadedAt: int64(id)})
	}
	m := s.marks()
	if !m.has("show", "Shameless", 2011) {
		t.Error("the 2011 show is in the library")
	}
	only2011 := mbMarks{lib: map[string][]int{"show|shameless": {2011}}}
	if only2011.has("show", "Shameless", 2004) {
		t.Error("the 2004 show must not be marked as in the library")
	}
	if !only2011.has("show", "Shameless", 2011) || !only2011.has("show", "Shameless", 2012) {
		t.Error("the year may be a little off between TMDB and the folder")
	}
	if !(mbMarks{lib: map[string][]int{"show|good omens": {0}}}).has("show", "Good Omens", 2019) {
		t.Error("a folder without a year matches any")
	}
}
