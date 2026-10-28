package web

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

type browseReply struct {
	Path    string   `json:"path"`
	Top     bool     `json:"top"`
	Parent  string   `json:"parent"`
	Dirs    []string `json:"dirs"`
	Volumes []Volume `json:"volumes"`
	Volume  *Volume  `json:"volume"`
}

func browseOf(t *testing.T, s *Server, path string, hidden bool) browseReply {
	t.Helper()
	q := url.Values{"path": {path}}
	if hidden {
		q.Set("hidden", "1")
	}
	w := httptest.NewRecorder()
	s.browse(w, httptest.NewRequest("GET", "/api/browse?"+q.Encode(), nil))
	if w.Code != 200 {
		t.Fatalf("browse %q: %d %s", path, w.Code, w.Body)
	}
	var r browseReply
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestBrowseStoragesAndFolders(t *testing.T) {
	internal, card := t.TempDir(), t.TempDir()
	for _, d := range []string{"DCIM/Camera", "Download", ".hidden", "Android/media"} {
		if err := os.MkdirAll(filepath.Join(internal, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(card, "Music"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := &Server{phone: &Phone{StorageRoot: internal}}
	s.phone.SetVolumes([]Volume{{Label: "Internal storage", Path: internal, Free: 1, Total: 2}, {Label: "SD card", Path: card}})

	top := browseOf(t, s, "", false)
	if !top.Top || len(top.Volumes) != 2 || top.Volumes[1].Label != "SD card" || len(top.Dirs) != 0 {
		t.Fatalf("the top lists the storages: %+v", top)
	}

	root := browseOf(t, s, internal, false)
	if root.Parent != "" || root.Volume == nil || root.Volume.Label != "Internal storage" {
		t.Fatalf("a storage's top goes up to the list of storages: %+v", root)
	}
	if want := []string{"Android", "DCIM", "Download"}; !equal(root.Dirs, want) {
		t.Fatalf("folders (hidden ones left out): %v, want %v", root.Dirs, want)
	}
	if got := browseOf(t, s, internal, true).Dirs; !equal(got, []string{".hidden", "Android", "DCIM", "Download"}) {
		t.Fatalf("hidden folders listed on request: %v", got)
	}

	sub := browseOf(t, s, filepath.Join(internal, "DCIM"), false)
	if sub.Parent != internal || sub.Volume == nil || sub.Volume.Path != internal || !equal(sub.Dirs, []string{"Camera"}) {
		t.Fatalf("inside a storage: %+v", sub)
	}
	if on := browseOf(t, s, card, false); on.Volume == nil || on.Volume.Label != "SD card" || on.Parent != "" || !equal(on.Dirs, []string{"Music"}) {
		t.Fatalf("the memory card: %+v", on)
	}
}

func TestBrowseFallsBackToStorageRoot(t *testing.T) {
	dir := t.TempDir()
	s := &Server{phone: &Phone{StorageRoot: dir}}
	top := browseOf(t, s, "", false)
	if len(top.Volumes) == 0 || top.Volumes[0].Path != dir {
		t.Fatalf("without a report from Android the shared storage is still offered: %+v", top.Volumes)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestShareDirsKeepRunningRequestsFiles(t *testing.T) {
	base := t.TempDir()
	s := &Server{phone: &Phone{ShareDir: base}}
	if err := os.WriteFile(filepath.Join(base, "old-leftover.mp4"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	first, releaseFirst, err := s.newShareDir()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "old-leftover.mp4")); err == nil {
		t.Fatal("leftovers of older versions should be cleaned up")
	}
	tmp := filepath.Join(first, ".restore-1.tgvault-tmp")
	if err := os.WriteFile(tmp, []byte("half a download"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A second tap while the first file is still being prepared.
	second, releaseSecond, err := s.newShareDir()
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("each request needs its own folder")
	}
	if _, err := os.Stat(tmp); err != nil {
		t.Fatalf("the running download's file was deleted: %v", err)
	}

	// Once both are done, the next request keeps only itself.
	releaseFirst()
	releaseFirst() // releasing twice is harmless
	releaseSecond()
	third, release, err := s.newShareDir()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	entries, _ := os.ReadDir(base)
	if len(entries) != 1 || filepath.Join(base, entries[0].Name()) != third {
		t.Fatalf("only the latest folder should remain, got %v", entries)
	}
}
