package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sammy-77A/tgvault/internal/backup"
	"github.com/Sammy-77A/tgvault/internal/store"
)

func libraryServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for id, p := range map[int]string{1: "Movies/Action/a.mkv", 2: "Movies2/b.mkv", 3: "Documents/tax.pdf", 4: "Movies/c.mp4"} {
		if err := st.AddVersion(store.Version{MsgID: id, CloudPath: p, Kind: store.KindFile, Size: 1, Hash: "h", UploadedAt: int64(id)}); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{eng: backup.New(st, nil, dir)}
	s.SetLibrary("Movies/")
	return s
}

func gate(s *Server, method, url, body string) int {
	r := httptest.NewRequest(method, url, strings.NewReader(body))
	w := httptest.NewRecorder()
	if s.libraryGate(w, r) {
		return http.StatusOK
	}
	return w.Code
}

func TestLibraryOnlyServesItsFolder(t *testing.T) {
	s := libraryServer(t)
	for _, c := range []struct {
		method, url, body string
		want              int
	}{
		// What the app is for.
		{"GET", "/", "", 200},
		{"GET", "/app.js", "", 200},
		{"GET", "/api/state", "", 200},
		{"POST", "/api/login/phone", "{}", 200},
		{"GET", "/api/cloud?path=Movies", "", 200},
		{"GET", "/api/cloud?path=Movies/Action", "", 200},
		{"GET", "/api/search?path=Movies&q=a", "", 200},
		{"GET", "/api/download?msg=1", "", 200},
		{"GET", "/api/preview?msg=4", "", 200},
		{"GET", "/api/download?path=Movies/c.mp4", "", 200},
		{"GET", "/api/thumbs?ids=1,4", "", 200},
		{"POST", "/api/play-link", `{"msg":1}`, 200},
		{"POST", "/api/bulk", `{"action":"download","paths":["Movies/Action","Movies/c.mp4"]}`, 200},
		{"POST", "/api/tasks/7/dismiss", "", 200},
		// Other folders, including one that only starts with the same letters.
		{"GET", "/api/cloud?path=", "", 403},
		{"GET", "/api/cloud?path=Documents", "", 403},
		{"GET", "/api/cloud?path=Movies2", "", 403},
		{"GET", "/api/cloud?path=Movies/../Documents", "", 403},
		{"GET", "/api/search?path=&q=tax", "", 403},
		{"GET", "/api/download?msg=3", "", 403},
		{"GET", "/api/download?msg=2", "", 403},
		{"GET", "/api/preview?msg=999", "", 403},
		{"GET", "/api/download?path=Documents/tax.pdf", "", 403},
		{"GET", "/api/thumbs?ids=1,3", "", 403},
		{"GET", "/api/thumb?msg=3", "", 403},
		{"POST", "/api/play-link", `{"msg":3}`, 403},
		{"POST", "/api/bulk", `{"action":"download","paths":["Movies/c.mp4","Documents"]}`, 403},
		{"POST", "/api/bulk", `{"action":"download","paths":[]}`, 403},
		{"GET", "/api/download-zip?path=", "", 403},
		// Everything that changes or reveals more than the folder.
		{"POST", "/api/bulk", `{"action":"delete-cloud","paths":["Movies/c.mp4"]}`, 403},
		{"POST", "/api/bulk", `{"action":"move","paths":["Movies/c.mp4"],"to":"Other"}`, 403},
		{"POST", "/api/bulk", `{"action":"move","paths":["Movies/c.mp4"],"to":"Movies/Action"}`, 403},
		{"POST", "/api/bulk", `{"action":"rename","paths":["Movies/c.mp4"],"name":"d.mp4"}`, 403},
		{"POST", "/api/new-folder", `{"parent":"Movies","name":"New"}`, 403},
		{"GET", "/api/media", "", 403},
		{"POST", "/api/media/key", `{"key":"x"}`, 403},
		{"POST", "/api/media/organize", `{}`, 403},
		{"POST", "/api/media/resolve", `{}`, 403},
		{"POST", "/api/media/skip", `{"path":"Movies/c.mp4"}`, 403},
		{"GET", "/api/media/search?q=up", "", 403},
		{"POST", "/api/bulk", `{"action":"restore","paths":["Movies/c.mp4"],"dest":"/tmp"}`, 403},
		{"POST", "/api/bulk", `not json`, 403},
		{"POST", "/api/delete-cloud", `{"path":"Movies/c.mp4"}`, 403},
		{"POST", "/api/restore", `{"path":"Movies/c.mp4","dest":"/tmp"}`, 403},
		{"POST", "/api/free", `{"path":"Movies"}`, 403},
		{"POST", "/api/roots", `{}`, 403},
		{"POST", "/api/pick-folder", `{}`, 403},
		{"GET", "/api/browse?path=/", "", 403},
		{"POST", "/api/zip-extract", "", 403},
		{"GET", "/api/zip?msg=1", "", 403},
		{"POST", "/api/quit", "", 403},
		{"POST", "/api/pair-phone", `{}`, 403},
		{"POST", "/api/phone-settings", `{}`, 403},
		{"POST", "/", "", 403},
	} {
		if got := gate(s, c.method, c.url, c.body); got != c.want {
			t.Errorf("%s %s %s: got %d, want %d", c.method, c.url, c.body, got, c.want)
		}
	}
}

func TestLibraryGateKeepsTheBodyForTheHandler(t *testing.T) {
	s := libraryServer(t)
	r := httptest.NewRequest("POST", "/api/play-link", strings.NewReader(`{"msg":1}`))
	if !s.libraryGate(httptest.NewRecorder(), r) {
		t.Fatal("refused")
	}
	var body struct{ Msg int }
	if err := readJSON(r, &body); err != nil || body.Msg != 1 {
		t.Fatalf("the handler can't read the body any more: %+v %v", body, err)
	}
}
