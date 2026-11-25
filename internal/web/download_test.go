package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Sammy-77A/tgvault/internal/backup"
	"github.com/Sammy-77A/tgvault/internal/store"
)

func TestParseByteRange(t *testing.T) {
	cases := []struct {
		hdr       string
		size      int64
		wantStart int64
		wantEnd   int64
		wantOK    bool
	}{
		{"bytes=0-99", 2000, 0, 99, true},
		{"bytes=100-", 2000, 100, 1999, true},
		{"bytes=-100", 2000, 1900, 1999, true},
		{"bytes=-5000", 2000, 0, 1999, true},
		{"bytes=1900-5000", 2000, 1900, 1999, true},
		{"bytes=5000-", 2000, 0, 0, false},
		{"bytes=2000-2000", 2000, 0, 0, false},
		{"bytes=500-100", 2000, 0, 0, false},
		{"bytes=0-10, 20-30", 2000, 0, 0, false},
		{"bytes=-0", 2000, 0, 0, false},
		{"items=0-99", 2000, 0, 0, false},
		{"bytes=0-0", 0, 0, 0, false},
	}
	for _, c := range cases {
		s, e, ok := parseByteRange(c.hdr, c.size)
		if ok != c.wantOK || (ok && (s != c.wantStart || e != c.wantEnd)) {
			t.Errorf("parseByteRange(%q, %d) = (%d, %d, %v), want (%d, %d, %v)",
				c.hdr, c.size, s, e, ok, c.wantStart, c.wantEnd, c.wantOK)
		}
	}
}

func TestDownloadRangeHeadersAndGate(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.AddVersion(store.Version{
		MsgID: 1, CloudPath: "Movies/a.mkv", Kind: store.KindFile,
		Size: 2000, Hash: "h", UploadedAt: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.AddVersion(store.Version{
		MsgID: 2, CloudPath: "Documents/secret.pdf", Kind: store.KindFile,
		Size: 2000, Hash: "h2", UploadedAt: 2,
	}); err != nil {
		t.Fatal(err)
	}

	s := &Server{eng: backup.New(st, nil, dir)}
	s.SetLibrary("Movies/")

	// 1. Unsatisfiable range -> 416 with Content-Range: bytes */2000
	r := httptest.NewRequest("GET", "/api/download?msg=1", nil)
	r.Header.Set("Range", "bytes=5000-")
	w := httptest.NewRecorder()
	s.download(w, r)
	if w.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("416 status = %d, want 416", w.Code)
	}
	if got := w.Header().Get("Content-Range"); got != "bytes */2000" {
		t.Fatalf("Content-Range = %q, want bytes */2000", got)
	}
	if got := w.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("Accept-Ranges = %q, want bytes", got)
	}

	// 2. HEAD with no Range -> 200, Content-Length == 2000, Accept-Ranges: bytes, empty body
	r = httptest.NewRequest("HEAD", "/api/download?msg=1", nil)
	w = httptest.NewRecorder()
	s.download(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("HEAD status = %d, want 200", w.Code)
	}
	if got := w.Header().Get("Content-Length"); got != "2000" {
		t.Fatalf("HEAD Content-Length = %q, want 2000", got)
	}
	if got := w.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("HEAD Accept-Ranges = %q, want bytes", got)
	}
	if w.Body.Len() != 0 {
		t.Fatalf("HEAD body len = %d, want 0", w.Body.Len())
	}

	// 3. HEAD with Range -> 206, Content-Range, Content-Length == 100, empty body
	r = httptest.NewRequest("HEAD", "/api/download?msg=1", nil)
	r.Header.Set("Range", "bytes=0-99")
	w = httptest.NewRecorder()
	s.download(w, r)
	if w.Code != http.StatusPartialContent {
		t.Fatalf("HEAD Range status = %d, want 206", w.Code)
	}
	if got := w.Header().Get("Content-Range"); got != "bytes 0-99/2000" {
		t.Fatalf("HEAD Range Content-Range = %q, want bytes 0-99/2000", got)
	}
	if got := w.Header().Get("Content-Length"); got != "100" {
		t.Fatalf("HEAD Range Content-Length = %q, want 100", got)
	}
	if w.Body.Len() != 0 {
		t.Fatalf("HEAD Range body len = %d, want 0", w.Body.Len())
	}

	// 4. Range request on a file the caller may not access -> 403
	r = httptest.NewRequest("GET", "/api/download?msg=2", nil)
	r.Header.Set("Range", "bytes=0-99")
	w = httptest.NewRecorder()
	if s.libraryGate(w, r) {
		t.Fatal("libraryGate allowed Range request for Documents/secret.pdf")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("gate status = %d, want 403", w.Code)
	}
}
