package backup

import (
	"archive/zip"
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/Sammy-77A/tgvault/internal/store"
)

func newTestEngine(t *testing.T) (*Engine, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, nil, dir), st
}

func write(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func paths(t *testing.T, st *store.Store, rootID int64) []string {
	t.Helper()
	items, err := st.ItemsByRoot(rootID)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for p, it := range items {
		if it.LocalPresent {
			out = append(out, p+"="+it.State)
		}
	}
	sort.Strings(out)
	return out
}

func TestScanQueuesFilesKeepsFoldersAndHoldsProjects(t *testing.T) {
	e, st := newTestEngine(t)
	docs := filepath.Join(t.TempDir(), "Documents")
	write(t, filepath.Join(docs, "a.txt"), "a")
	write(t, filepath.Join(docs, "Taxes", "2025", "return.pdf"), "pdf")
	write(t, filepath.Join(docs, "empty.txt"), "")
	write(t, filepath.Join(docs, "~$lock.docx"), "x")
	write(t, filepath.Join(docs, "node_modules", "junk.js"), "x")
	write(t, filepath.Join(docs, "shop", "package.json"), "{}")
	write(t, filepath.Join(docs, "shop", "index.js"), "x")

	root, err := st.AddRoot(docs, "Documents")
	if err != nil {
		t.Fatal(err)
	}
	e.scanRoot(root)

	got := paths(t, st, root.ID)
	want := []string{
		"Documents/Taxes/2025/return.pdf=pending",
		"Documents/a.txt=pending",
		"Documents/empty.txt=skipped",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v\nwant %v", got, want)
		}
	}
	projects, _ := st.Projects(store.ProjectSuggested)
	if len(projects) != 1 || projects[0].Rel != "shop" {
		t.Fatalf("expected shop to be suggested as a project, got %+v", projects)
	}

	// Confirming it makes the whole folder one project item.
	if err := st.SetProjectState(projects[0].ID, store.ProjectConfirmed); err != nil {
		t.Fatal(err)
	}
	e.scanRoot(root)
	it, err := st.ItemByPath("Documents/shop")
	if err != nil || it.Kind != store.KindProject || it.State != store.StatePending {
		t.Fatalf("project item = %+v, %v", it, err)
	}
	if _, err := st.ItemByPath("Documents/shop/index.js"); err == nil {
		t.Fatal("files inside a project must not be tracked individually")
	}
}

func TestLocalDeleteKeepsCloudCopy(t *testing.T) {
	e, st := newTestEngine(t)
	docs := filepath.Join(t.TempDir(), "Docs")
	write(t, filepath.Join(docs, "keep.txt"), "k")
	write(t, filepath.Join(docs, "new.txt"), "n")
	root, _ := st.AddRoot(docs, "Docs")
	e.scanRoot(root)

	// Pretend keep.txt was uploaded.
	it, _ := st.ItemByPath("Docs/keep.txt")
	if err := st.AddVersion(store.Version{MsgID: 10, CloudPath: it.CloudPath, Kind: store.KindFile, Size: 1, Hash: "h", UploadedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := st.FinishItem(it.ID, store.StateSynced, "", 0, 0, time.Now().Unix()+10); err != nil {
		t.Fatal(err)
	}

	os.Remove(filepath.Join(docs, "keep.txt"))
	os.Remove(filepath.Join(docs, "new.txt"))
	e.scanRoot(root)

	it, err := st.ItemByPath("Docs/keep.txt")
	if err != nil || it.LocalPresent || it.State != store.StateSynced {
		t.Fatalf("deleted-but-backed-up file should stay as cloud-only, got %+v %v", it, err)
	}
	if vs, _ := st.Versions("Docs/keep.txt"); len(vs) != 1 {
		t.Fatal("the cloud version must be kept")
	}
	if _, err := st.ItemByPath("Docs/new.txt"); err == nil {
		t.Fatal("a never-uploaded deleted file should be forgotten")
	}
}

func TestMissingRootIsNotTreatedAsDeletion(t *testing.T) {
	e, st := newTestEngine(t)
	docs := filepath.Join(t.TempDir(), "USB")
	write(t, filepath.Join(docs, "a.txt"), "a")
	root, _ := st.AddRoot(docs, "USB")
	e.scanRoot(root)
	os.RemoveAll(docs) // drive unplugged
	e.scanRoot(root)
	it, err := st.ItemByPath("USB/a.txt")
	if err != nil || !it.LocalPresent {
		t.Fatalf("items must survive a missing root: %+v %v", it, err)
	}
	r, _ := st.Root(root.ID)
	if r.ScanError == "" {
		t.Fatal("missing root should report a scan error")
	}
}

func TestClaimNextWaitsForQuietPeriodAndFailureDoesNotBlock(t *testing.T) {
	_, st := newTestEngine(t)
	root, _ := st.AddRoot(t.TempDir(), "R")
	for _, p := range []string{"R/a", "R/b"} {
		if err := st.UpsertSeen(store.Item{Kind: store.KindFile, CloudPath: p, RootID: root.ID, Rel: p[2:], Size: 1, Mtime: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok, _ := st.ClaimNext(time.Hour, time.Hour); ok {
		t.Fatal("items changed just now must wait for the quiet period")
	}
	a, ok, _ := st.ClaimNext(0, 0)
	if !ok {
		t.Fatal("expected an item")
	}
	// a fails and is scheduled far in the future; b must still be served.
	if err := st.FinishItem(a.ID, store.StateFailed, "boom", 1, time.Now().Add(time.Hour).Unix(), time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	b, ok, _ := st.ClaimNext(0, 0)
	if !ok || b.ID == a.ID {
		t.Fatalf("a failing item blocked the queue: %+v %v", b, ok)
	}
	if _, ok, _ := st.ClaimNext(0, 0); ok {
		t.Fatal("nothing else should be eligible")
	}
}

func TestChangeDuringUploadRequeues(t *testing.T) {
	_, st := newTestEngine(t)
	root, _ := st.AddRoot(t.TempDir(), "R")
	st.UpsertSeen(store.Item{Kind: store.KindFile, CloudPath: "R/a", RootID: root.ID, Rel: "a", Size: 1, Mtime: 1})
	it, _, _ := st.ClaimNext(0, 0)
	started := time.Now().Unix() - 5
	// The file changes while uploading.
	st.UpsertSeen(store.Item{Kind: store.KindFile, CloudPath: "R/a", RootID: root.ID, Rel: "a", Size: 2, Mtime: 2})
	st.FinishItem(it.ID, store.StateSynced, "", 0, 0, started)
	got, _ := st.Item(it.ID)
	if got.State != store.StatePending {
		t.Fatalf("state = %s, want pending so the new content gets uploaded", got.State)
	}
}

func TestUploadKeptWhenFileDeletedAfterwards(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a.txt")
	os.WriteFile(p, []byte("hello"), 0o644)
	before, _ := os.Stat(p)

	after, err := os.Stat(p)
	if !uploadStillValid(before, after, err) {
		t.Fatal("an unchanged file must keep its upload")
	}
	os.WriteFile(p, []byte("hello, changed"), 0o644)
	after, err = os.Stat(p)
	if uploadStillValid(before, after, err) {
		t.Fatal("a changed file must be uploaded again")
	}
	os.Remove(p)
	after, err = os.Stat(p)
	if !uploadStillValid(before, after, err) {
		t.Fatal("a file deleted after its upload must keep that upload")
	}
}

func TestProjectZipSkipsRebuildableFolders(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "composer.json"), "{}")
	write(t, filepath.Join(dir, ".env"), "SECRET=1")
	write(t, filepath.Join(dir, "app", "main.php"), "<?php")
	write(t, filepath.Join(dir, "vendor", "lib.php"), "x")
	write(t, filepath.Join(dir, "node_modules", "x.js"), "x")
	write(t, filepath.Join(dir, ".git", "HEAD"), "ref")

	files := projectFiles(dir)
	zipPath := filepath.Join(t.TempDir(), "p.zip")
	if _, err := writeProjectZip(context.Background(), zipPath, files); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	want := []string{".env", ".git/HEAD", "app/main.php", "composer.json"}
	if len(names) != len(want) {
		t.Fatalf("zip has %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("zip has %v, want %v", names, want)
		}
	}

	fp1, _, _ := projectFingerprint(dir)
	write(t, filepath.Join(dir, "vendor", "other.php"), "x") // rebuildable: no change
	fp2, _, _ := projectFingerprint(dir)
	write(t, filepath.Join(dir, "app", "new.php"), "x")
	fp3, _, _ := projectFingerprint(dir)
	if fp1 != fp2 || fp2 == fp3 {
		t.Fatalf("fingerprints: %s %s %s", fp1, fp2, fp3)
	}
}

func TestRemoveEmptyDirsStopsAtRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "Documents")
	deep := filepath.Join(root, "a", "b")
	os.MkdirAll(deep, 0o755)
	removeEmptyDirs([]freedDir{{dir: deep, root: root}})
	if _, err := os.Stat(filepath.Join(root, "a")); !os.IsNotExist(err) {
		t.Fatal("empty subfolders should be removed")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal("the backup folder itself must never be removed")
	}
}

func TestRestoredName(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	write(t, p, "1")
	if got := restoredName(p); filepath.Base(got) != "a (restored).txt" {
		t.Fatalf("got %s", got)
	}
	write(t, filepath.Join(dir, "a (restored).txt"), "2")
	if got := restoredName(p); filepath.Base(got) != "a (restored 2).txt" {
		t.Fatalf("got %s", got)
	}
}

func TestOldRateLimitFailuresBecomeQueued(t *testing.T) {
	_, st := newTestEngine(t)
	root, _ := st.AddRoot(t.TempDir(), "R")
	st.UpsertSeen(store.Item{Kind: store.KindFile, CloudPath: "R/a", RootID: root.ID, Rel: "a", Size: 1, Mtime: 1})
	it, _, _ := st.ClaimNext(0, 0)
	st.FinishItem(it.ID, store.StateFailed, "Telegram asked tgvault to slow down; continuing at 05:56", 3, time.Now().Add(time.Hour).Unix(), time.Now().Unix())
	if err := st.ResetUploading(); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Item(it.ID)
	if got.State != store.StatePending || got.Attempts != 0 || got.NextAttempt != 0 {
		t.Fatalf("rate-limited item should be queued, got %+v", got)
	}
}

func TestStartCooldownKeepsLatestEnd(t *testing.T) {
	e, _ := newTestEngine(t)
	long := e.startCooldown(10 * time.Minute)
	short := e.startCooldown(time.Minute)
	if !short.Equal(long) || time.Until(e.cooldownEnd()) < 9*time.Minute {
		t.Fatal("a shorter wait must not cut an active pause short")
	}
}
