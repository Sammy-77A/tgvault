package notify

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/tgclient"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func setup(t *testing.T) (*Watcher, *store.Store, *fakeClock, *[]string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	clock := &fakeClock{t: time.Now()}
	var sent []string
	w := New(st, tgclient.New(st, dir, "test"), "http://x")
	w.now = clock.now
	w.send = func(title, body, _ string) error {
		sent = append(sent, title+": "+body)
		return nil
	}
	return w, st, clock, &sent
}

func TestStaleFailuresNotifyOncePerDay(t *testing.T) {
	w, st, clock, sent := setup(t)
	root, _ := st.AddRoot(t.TempDir(), "R")
	st.UpsertSeen(store.Item{Kind: store.KindFile, CloudPath: "R/a.txt", RootID: root.ID, Rel: "a.txt", Size: 1, Mtime: 1})
	it, _, _ := st.ClaimNext(0, 0)
	st.FinishItem(it.ID, store.StateFailed, "boom", 1, 0, time.Now().Unix())

	w.Check()
	if len(*sent) != 0 {
		t.Fatalf("a fresh failure must not notify: %v", *sent)
	}
	clock.t = clock.t.Add(25 * time.Hour)
	w.Check()
	w.Check()
	if len(*sent) != 1 || !strings.Contains((*sent)[0], "R/a.txt") {
		t.Fatalf("want one notification about R/a.txt, got %v", *sent)
	}
	clock.t = clock.t.Add(25 * time.Hour)
	w.Check()
	if len(*sent) != 2 {
		t.Fatalf("want a daily reminder, got %v", *sent)
	}
}

func TestMissingFolderAfterGracePeriod(t *testing.T) {
	w, st, clock, sent := setup(t)
	dir := filepath.Join(t.TempDir(), "USB")
	os.MkdirAll(dir, 0o755)
	root, _ := st.AddRoot(dir, "USB")
	st.SetRootScan(root.ID, "folder not found; is the drive connected?")
	w.Check()
	clock.t = clock.t.Add(10 * time.Minute)
	w.Check()
	if len(*sent) != 0 {
		t.Fatal("brief absence must not notify")
	}
	clock.t = clock.t.Add(30 * time.Minute)
	w.Check()
	if len(*sent) != 1 {
		t.Fatalf("want one notification, got %v", *sent)
	}
}

func TestProjectsAndDisabled(t *testing.T) {
	w, st, _, sent := setup(t)
	root, _ := st.AddRoot(t.TempDir(), "R")
	st.SuggestProject(root.ID, "app", "package.json")
	st.SuggestProject(root.ID, "site", ".git")
	w.Check()
	w.Check()
	if len(*sent) != 1 || !strings.Contains((*sent)[0], "2 folders") {
		t.Fatalf("want one batched notification, got %v", *sent)
	}
	st.SuggestProject(root.ID, "new", "go.mod")
	st.Set(SettingKey, "off")
	w.Check()
	if len(*sent) != 1 {
		t.Fatal("notifications were turned off")
	}
	st.Set(SettingKey, "on")
	w.Check()
	if len(*sent) != 2 {
		t.Fatalf("a newly found project should notify, got %v", *sent)
	}
}
