package backup

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sammy-77A/tgvault/internal/store"
)

func addVersion(t *testing.T, st *store.Store, id int, p string, size, mtime int64) {
	t.Helper()
	if err := st.AddVersion(store.Version{MsgID: id, CloudPath: p, Kind: store.KindFile, Size: size, Mtime: mtime, Hash: "h", UploadedAt: int64(id)}); err != nil {
		t.Fatal(err)
	}
}

func TestSearchAcrossSubfolders(t *testing.T) {
	e, st := newTestEngine(t)
	day := time.Date(2026, 10, 1, 12, 0, 0, 0, time.Local).Unix()
	addVersion(t, st, 1, "Downloads/Music/Burna Boy - Last Last.mp3", 5_000_000, day)
	addVersion(t, st, 2, "Downloads/Music/cover.jpg", 100, day)
	addVersion(t, st, 3, "Documents/last-year.pdf", 300, day-90*86400)
	addVersion(t, st, 4, "Downloads/setup.exe", 400, day)

	names := func(q Query) []string {
		es, total, err := e.Search(q)
		if err != nil {
			t.Fatal(err)
		}
		if total != len(es) {
			t.Fatalf("total %d but %d entries", total, len(es))
		}
		var out []string
		for _, en := range es {
			out = append(out, en.Name)
		}
		return out
	}
	if got := names(Query{Text: "LAST"}); len(got) != 2 {
		t.Fatalf("name search should ignore case and look everywhere: %v", got)
	}
	if got := names(Query{Prefix: "Downloads", Type: "audio"}); len(got) != 1 || !strings.HasSuffix(got[0], ".mp3") {
		t.Fatalf("audio under Downloads: %v", got)
	}
	if got := names(Query{From: day - 86400}); len(got) != 3 {
		t.Fatalf("modified since yesterday: %v", got)
	}
	if got := names(Query{To: day - 86400}); len(got) != 1 || got[0] != "last-year.pdf" {
		t.Fatalf("modified before yesterday: %v", got)
	}
	es, total, _ := e.Search(Query{Limit: 2})
	if len(es) != 2 || total != 4 {
		t.Fatalf("limit: %d entries, total %d", len(es), total)
	}
}

func TestRecommendationsOnlyUpToDateFiles(t *testing.T) {
	e, st := newTestEngine(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "big.mp4"), strings.Repeat("v", 3000))
	write(t, filepath.Join(dir, "small.jpg"), "jpg")
	write(t, filepath.Join(dir, "changed.txt"), "new content")
	write(t, filepath.Join(dir, "never.txt"), "not uploaded")
	root, _ := st.AddRoot(dir, "R")
	e.scanRoot(root)
	later := time.Now().Unix() + 10
	for i, name := range []string{"big.mp4", "small.jpg", "changed.txt"} {
		it, _ := st.ItemByPath("R/" + name)
		size := it.Size
		if name == "changed.txt" {
			size = 3 // the backup is of an older, different version
		}
		addVersion(t, st, 10+i, it.CloudPath, size, it.Mtime)
		st.FinishItem(it.ID, store.StateSynced, "", 0, 0, later)
	}
	recs, err := e.Recommendations(0, "", false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].CloudPath != "R/big.mp4" || recs[1].CloudPath != "R/small.jpg" {
		t.Fatalf("want big.mp4 then small.jpg, got %+v", recs)
	}
	if recs, _ := e.Recommendations(1000, "", false, 0, 0); len(recs) != 1 {
		t.Fatalf("minimum size: %+v", recs)
	}
	if recs, _ := e.Recommendations(0, "image", false, 0, 0); len(recs) != 1 || recs[0].Type != "image" {
		t.Fatalf("type filter: %+v", recs)
	}
	// Date range: by the file's last modified time; "to" is exclusive.
	has := func(from, to int64, path string) bool {
		rs, err := e.Recommendations(0, "", false, from, to)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rs {
			if r.CloudPath == path {
				return true
			}
		}
		return false
	}
	mt := recs[0].Mtime
	if !has(mt, mt+1, recs[0].CloudPath) || !has(0, 0, recs[0].CloudPath) {
		t.Fatal("a range around the file's time must include it")
	}
	if has(mt+1, 0, recs[0].CloudPath) {
		t.Fatal("from: a file modified before it must be left out")
	}
	if has(0, mt, recs[0].CloudPath) {
		t.Fatal("to is exclusive: a file modified at it must be left out")
	}
	plan, err := e.PlanFreeMany([]string{"R/big.mp4", "R/small.jpg", "R"})
	if err != nil || len(plan.Candidates) != 2 {
		t.Fatalf("overlapping selections must count each file once: %+v %v", plan, err)
	}
}

func TestMoveRefusesFilesStillBackedUpHere(t *testing.T) {
	e, st := newTestEngine(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.txt"), "a")
	root, _ := st.AddRoot(dir, "R")
	e.scanRoot(root)
	addVersion(t, st, 1, "R/a.txt", 1, 1)
	if _, err := e.MoveInCloud(context.Background(), []string{"R/a.txt"}, "Archive"); err == nil {
		t.Fatal("moving a file that's still backed up from here must be refused")
	}
	addVersion(t, st, 2, "Phone/Camera/x.jpg", 1, 1)
	if _, err := e.MoveInCloud(context.Background(), []string{"Phone/Camera/x.jpg"}, "Archive"); err == nil ||
		!strings.Contains(err.Error(), "between") {
		t.Fatalf("moving between devices' parts must be refused, got %v", err)
	}
}
