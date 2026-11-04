package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sammy-77A/tgvault/internal/store"
)

// syncAll scans the root and uploads whatever is waiting.
func syncAll(t *testing.T, e *Engine, st *store.Store, root store.Root) {
	t.Helper()
	e.scanRoot(root)
	for {
		it, ok, err := st.ClaimNext(0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return
		}
		e.process(context.Background(), it)
	}
}

func moveRig(t *testing.T) (*Engine, *store.Store, *fakeTG, store.Root, string) {
	t.Helper()
	tg := newFakeTG()
	e, st := newSplitEngine(t, tg)
	dir := t.TempDir()
	root, err := st.AddRoot(dir, "R")
	if err != nil {
		t.Fatal(err)
	}
	return e, st, tg, root, dir
}

func versionsAt(t *testing.T, st *store.Store, p string) []store.Version {
	t.Helper()
	vs, err := st.Versions(p)
	if err != nil {
		t.Fatal(err)
	}
	return vs
}

func TestMovedFileIsNotUploadedAgain(t *testing.T) {
	e, st, tg, root, dir := moveRig(t)
	write(t, filepath.Join(dir, "Shows", "ep1.mkv"), "episode one")
	syncAll(t, e, st, root)
	old := versionsAt(t, st, "R/Shows/ep1.mkv")
	if len(old) != 1 || tg.calls != 1 {
		t.Fatalf("setup: %d versions, %d uploads", len(old), tg.calls)
	}
	if err := os.MkdirAll(filepath.Join(dir, "Shows", "Season 1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "Shows", "ep1.mkv"), filepath.Join(dir, "Shows", "Season 1", "ep1.mkv")); err != nil {
		t.Fatal(err)
	}
	syncAll(t, e, st, root)

	if tg.calls != 1 {
		t.Fatalf("the moved file was uploaded again (%d uploads)", tg.calls)
	}
	now := versionsAt(t, st, "R/Shows/Season 1/ep1.mkv")
	if len(now) != 1 || now[0].MsgID != old[0].MsgID {
		t.Fatalf("the stored copy should now be at the new path: %+v", now)
	}
	if left := versionsAt(t, st, "R/Shows/ep1.mkv"); len(left) != 0 {
		t.Fatalf("nothing should stay under the old name: %+v", left)
	}
	if c := tg.msgs[old[0].MsgID].caption; !strings.Contains(c, "Season 1/ep1.mkv") {
		t.Errorf("the Telegram caption should name the new path: %s", c)
	}
	if it, err := st.ItemByPath("R/Shows/Season 1/ep1.mkv"); err != nil || it.State != store.StateSynced {
		t.Fatalf("the new item: %+v %v", it, err)
	}
	if _, err := st.ItemByPath("R/Shows/ep1.mkv"); err == nil {
		t.Error("the old item should be forgotten")
	}
}

func TestRenameAndOlderVersionsFollow(t *testing.T) {
	e, st, tg, root, dir := moveRig(t)
	p := filepath.Join(dir, "notes.txt")
	write(t, p, "first draft")
	syncAll(t, e, st, root)
	write(t, p, "second draft, a bit longer")
	syncAll(t, e, st, root)
	if n := len(versionsAt(t, st, "R/notes.txt")); n != 2 {
		t.Fatalf("setup: %d versions", n)
	}
	if err := os.Rename(p, filepath.Join(dir, "final.txt")); err != nil {
		t.Fatal(err)
	}
	syncAll(t, e, st, root)
	if tg.calls != 2 {
		t.Fatalf("rename caused an upload (%d uploads)", tg.calls)
	}
	if n := len(versionsAt(t, st, "R/final.txt")); n != 2 {
		t.Fatalf("both versions should follow the rename, got %d", n)
	}
	if n := len(versionsAt(t, st, "R/notes.txt")); n != 0 {
		t.Fatalf("%d versions left under the old name", n)
	}
}

func TestCopiedFileIsStillUploaded(t *testing.T) {
	e, st, tg, root, dir := moveRig(t)
	write(t, filepath.Join(dir, "a.txt"), "same bytes")
	syncAll(t, e, st, root)
	write(t, filepath.Join(dir, "copy of a.txt"), "same bytes") // the original stays
	syncAll(t, e, st, root)
	if tg.calls != 2 || len(versionsAt(t, st, "R/a.txt")) != 1 || len(versionsAt(t, st, "R/copy of a.txt")) != 1 {
		t.Fatalf("a copy is a new file: %d uploads", tg.calls)
	}
}

func TestChangedAndMovedIsUploaded(t *testing.T) {
	e, st, tg, root, dir := moveRig(t)
	write(t, filepath.Join(dir, "a.txt"), "one")
	syncAll(t, e, st, root)
	if err := os.Remove(filepath.Join(dir, "a.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "b.txt"), "different content")
	syncAll(t, e, st, root)
	if tg.calls != 2 {
		t.Fatalf("different content is a new file: %d uploads", tg.calls)
	}
	if len(versionsAt(t, st, "R/a.txt")) != 1 {
		t.Error("the deleted file's backup must stay")
	}
}

func TestFreedFileIsNotTakenForAMove(t *testing.T) {
	e, st, tg, root, dir := moveRig(t)
	write(t, filepath.Join(dir, "movie.mkv"), "frames and frames")
	syncAll(t, e, st, root)
	if _, err := e.FreeSpace(context.Background(), "R/movie.mkv"); err != nil {
		t.Fatal(err)
	}
	waitTasks(t, e)
	if _, err := os.Stat(filepath.Join(dir, "movie.mkv")); !os.IsNotExist(err) {
		t.Fatal("setup: the local copy should be gone")
	}
	// The same file comes back under another name: the freed backup stays where it is.
	write(t, filepath.Join(dir, "again.mkv"), "frames and frames")
	syncAll(t, e, st, root)
	if tg.calls != 2 || len(versionsAt(t, st, "R/movie.mkv")) != 1 || len(versionsAt(t, st, "R/again.mkv")) != 1 {
		t.Fatalf("a freed file's backup must not be renamed away: %d uploads", tg.calls)
	}
}

func TestTwoMovedCopiesDoNotShareOneBackup(t *testing.T) {
	e, st, tg, root, dir := moveRig(t)
	write(t, filepath.Join(dir, "a.bin"), "payload")
	syncAll(t, e, st, root)
	_ = os.Remove(filepath.Join(dir, "a.bin"))
	write(t, filepath.Join(dir, "x", "a.bin"), "payload")
	write(t, filepath.Join(dir, "y", "a.bin"), "payload")
	syncAll(t, e, st, root)
	x, y := versionsAt(t, st, "R/x/a.bin"), versionsAt(t, st, "R/y/a.bin")
	if len(x) != 1 || len(y) != 1 || x[0].MsgID == y[0].MsgID || tg.calls != 2 {
		t.Fatalf("each present file needs its own backup: x=%+v y=%+v uploads=%d", x, y, tg.calls)
	}
}

func TestMovedBigFileIsNotUploadedAgain(t *testing.T) {
	e, st, tg, root, dir := moveRig(t)
	big := string(randomBytes(t, testSplitAt*2+10))
	write(t, filepath.Join(dir, "big.mkv"), big)
	syncAll(t, e, st, root)
	before := tg.calls
	old := versionsAt(t, st, "R/big.mkv")
	if len(old) != 1 || old[0].Parts < 2 {
		t.Fatalf("setup: %+v", old)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sorted"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "big.mkv"), filepath.Join(dir, "sorted", "big.mkv")); err != nil {
		t.Fatal(err)
	}
	syncAll(t, e, st, root)
	if tg.calls != before {
		t.Fatalf("the big file was uploaded again (%d new uploads)", tg.calls-before)
	}
	now := versionsAt(t, st, "R/sorted/big.mkv")
	if len(now) != 1 || now[0].MsgID != old[0].MsgID || now[0].Parts != old[0].Parts {
		t.Fatalf("%+v", now)
	}
	for _, p := range mustPieces(t, e, now[0]) {
		if c := tg.msgs[p.MsgID].caption; !strings.Contains(c, "sorted/big.mkv") {
			t.Errorf("part %d still names the old path: %s", p.Index, c)
		}
	}
}

func mustPieces(t *testing.T, e *Engine, v store.Version) []store.Part {
	t.Helper()
	ps, err := e.pieces(v)
	if err != nil {
		t.Fatal(err)
	}
	return ps
}

// waitTasks waits for background tasks (freeing space and the like) to finish.
func waitTasks(t *testing.T, e *Engine) {
	t.Helper()
	for i := 0; i < 400; i++ {
		done := true
		for _, tv := range e.Status().Tasks {
			if !tv.Finished {
				done = false
			}
		}
		if done {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("a task did not finish")
}

func TestReadOnlyEngineCannotChangeTelegram(t *testing.T) {
	tg := newFakeTG()
	e, st := newSplitEngine(t, tg)
	dir := t.TempDir()
	root, _ := st.AddRoot(dir, "R")
	write(t, filepath.Join(dir, "a.txt"), "hello")
	syncAll(t, e, st, root)
	v := versionsAt(t, st, "R/a.txt")[0]

	e.ReadOnly()
	ctx := context.Background()
	if err := e.tg.DeleteMessages(ctx, []int{v.MsgID}); err == nil {
		t.Error("deleting must fail")
	}
	if err := e.tg.EditCaption(ctx, v.MsgID, "x"); err == nil {
		t.Error("editing must fail")
	}
	write(t, filepath.Join(dir, "b.txt"), "new")
	syncAll(t, e, st, root)
	if tg.calls != 1 {
		t.Errorf("uploading must fail, but %d uploads happened", tg.calls)
	}
	if _, ok := tg.msgs[v.MsgID]; !ok {
		t.Error("the stored file must still be in Telegram")
	}
	var sb strings.Builder
	if err := e.DownloadTo(ctx, v, &sb); err != nil || sb.String() != "hello" {
		t.Errorf("reading must still work: %q %v", sb.String(), err)
	}
}

// A second device (its own database, the same channel) follows a move when it
// next checks Telegram, for ordinary and split files.
func TestOtherDeviceFollowsAMove(t *testing.T) {
	tg := newFakeTG()
	a, st := newSplitEngine(t, tg)
	dir := t.TempDir()
	root, _ := st.AddRoot(dir, "R")
	write(t, filepath.Join(dir, "small.txt"), "small file")
	write(t, filepath.Join(dir, "big.mkv"), string(randomBytes(t, testSplitAt*2+10)))
	syncAll(t, a, st, root)

	b, _ := newSplitEngine(t, tg) // the other device
	if _, err := b.reconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(versionsAt(t, b.st, "R/small.txt")) != 1 || len(versionsAt(t, b.st, "R/big.mkv")) != 1 {
		t.Fatal("setup: the other device should know both files")
	}
	big := versionsAt(t, b.st, "R/big.mkv")[0]

	if err := os.MkdirAll(filepath.Join(dir, "sorted"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"small.txt", "big.mkv"} {
		if err := os.Rename(filepath.Join(dir, n), filepath.Join(dir, "sorted", n)); err != nil {
			t.Fatal(err)
		}
	}
	syncAll(t, a, st, root) // relabelled, not uploaded again

	if _, err := b.reconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"small.txt", "big.mkv"} {
		if len(versionsAt(t, b.st, "R/"+n)) != 0 || len(versionsAt(t, b.st, "R/sorted/"+n)) != 1 {
			t.Errorf("%s: the other device should now list it under sorted/", n)
		}
	}
	if got := versionsAt(t, b.st, "R/sorted/big.mkv")[0]; got.MsgID != big.MsgID || got.Parts != big.Parts {
		t.Errorf("the split file changed identity: %+v vs %+v", got, big)
	}
}
