package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sammy-77A/tgvault/internal/meta"
	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/tgclient"
)

// Tiny limits so a 10.5 KB file splits into 3 parts: 4096 + 4096 + 2308.
const (
	testSplitAt  = 10_000
	testPartSize = 4096
	testBigSize  = 10_500
)

func newSplitEngine(t *testing.T, tg *fakeTG) (*Engine, *store.Store) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := newEngine(st, dir)
	e.tg = tg
	e.splitAt, e.partSize = testSplitAt, testPartSize
	if err := os.MkdirAll(e.tmpDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return e, st
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// bigFile writes a file over the test split size into a backup folder and
// returns its tracked item and path.
func bigFile(t *testing.T, e *Engine, st *store.Store, data []byte) (store.Item, string) {
	t.Helper()
	docs := filepath.Join(t.TempDir(), "Docs")
	abs := filepath.Join(docs, "movie.mkv")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, data, 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := st.AddRoot(docs, "Docs")
	if err != nil {
		t.Fatal(err)
	}
	e.scanRoot(root)
	it, err := st.ItemByPath("Docs/movie.mkv")
	if err != nil {
		t.Fatal(err)
	}
	return it, abs
}

func latest(t *testing.T, st *store.Store, p string) store.Version {
	t.Helper()
	v, ok, err := st.LatestVersion(p)
	if err != nil || !ok {
		t.Fatalf("no version of %s: %v", p, err)
	}
	return v
}

func TestSplitUploadAndRestore(t *testing.T) {
	tg := newFakeTG()
	e, st := newSplitEngine(t, tg)
	data := randomBytes(t, testBigSize)
	it, abs := bigFile(t, e, st, data)

	if out := e.uploadFile(context.Background(), it, abs); out.state != store.StateSynced {
		t.Fatalf("upload: %+v", out)
	}
	v := latest(t, st, "Docs/movie.mkv")
	if v.Parts != 3 || v.Size != testBigSize {
		t.Fatalf("version = %+v, want 3 parts of a %d-byte file", v, testBigSize)
	}
	if tg.count() != 3 {
		t.Fatalf("channel holds %d messages, want 3", tg.count())
	}
	parts, _ := st.Parts(v.MsgID)
	wantSizes := []int64{4096, 4096, 2308}
	for i, p := range parts {
		m := tg.msgs[p.MsgID]
		in, ok := meta.Parse(m.caption)
		if !ok || !in.Split() || in.Part != i+1 || in.Parts != 3 || in.PartSize != wantSizes[i] || in.Hash != v.Hash {
			t.Fatalf("part %d caption = %+v (%v)", i+1, in, ok)
		}
		if !strings.Contains(m.caption, `"v":2`) || !strings.Contains(m.caption, "(part ") {
			t.Fatalf("part %d caption isn't version 2:\n%s", i+1, m.caption)
		}
		if want := "movie.mkv.part" + string(rune('1'+i)) + "of3"; m.name != want {
			t.Fatalf("part file name %q, want %q", m.name, want)
		}
	}
	if pus, _ := st.PartUploads(""); len(pus) != 0 {
		t.Fatalf("unfinished-upload rows left behind: %+v", pus)
	}

	// Restoring joins the parts back into the original bytes.
	if err := os.Remove(abs); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.restoreFile(context.Background(), v, abs); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(abs)
	if !bytes.Equal(got, data) {
		t.Fatal("restored file differs from the original")
	}

	// Streaming (browser download, ZIP) gives the same bytes.
	var buf bytes.Buffer
	if err := e.DownloadTo(context.Background(), v, &buf); err != nil || !bytes.Equal(buf.Bytes(), data) {
		t.Fatalf("stream: %v", err)
	}
}

func TestSmallFilesStayOneMessage(t *testing.T) {
	tg := newFakeTG()
	e, st := newSplitEngine(t, tg)
	it, abs := bigFile(t, e, st, randomBytes(t, testSplitAt))
	if out := e.uploadFile(context.Background(), it, abs); out.state != store.StateSynced {
		t.Fatalf("upload: %+v", out)
	}
	if v := latest(t, st, "Docs/movie.mkv"); v.Parts != 0 || tg.count() != 1 {
		t.Fatalf("a file at the limit must stay one message: %+v, %d messages", v, tg.count())
	}
}

func TestSplitUploadContinuesAfterInterruption(t *testing.T) {
	tg := newFakeTG()
	e, st := newSplitEngine(t, tg)
	data := randomBytes(t, testBigSize)
	it, abs := bigFile(t, e, st, data)

	tg.beforeUpload = func(n int, _ tgclient.UploadRequest) error {
		if n == 2 {
			return errors.New("connection lost")
		}
		return nil
	}
	if out := e.uploadFile(context.Background(), it, abs); out.state == store.StateSynced {
		t.Fatal("upload should have failed on part 2")
	}
	pus, _ := st.PartUploads("Docs/movie.mkv")
	if len(pus) != 1 || pus[0].Index != 1 {
		t.Fatalf("after the failure, posted parts = %+v, want part 1", pus)
	}
	if _, ok, _ := st.LatestVersion("Docs/movie.mkv"); ok {
		t.Fatal("an unfinished split file must not count as backed up")
	}
	firstPart := pus[0].MsgID

	tg.beforeUpload = nil
	before := tg.calls
	if out := e.uploadFile(context.Background(), it, abs); out.state != store.StateSynced {
		t.Fatalf("second try: %+v", out)
	}
	if n := tg.calls - before; n != 2 {
		t.Fatalf("second try uploaded %d parts, want only the 2 missing ones", n)
	}
	v := latest(t, st, "Docs/movie.mkv")
	if v.MsgID != firstPart || tg.count() != 3 {
		t.Fatalf("version %+v should start at the part posted earlier (%d); %d messages", v, firstPart, tg.count())
	}
	var buf bytes.Buffer
	if err := e.DownloadTo(context.Background(), v, &buf); err != nil || !bytes.Equal(buf.Bytes(), data) {
		t.Fatalf("download after resume: %v", err)
	}
}

func TestSplitUploadDroppedWhenFileChanges(t *testing.T) {
	tg := newFakeTG()
	e, st := newSplitEngine(t, tg)
	it, abs := bigFile(t, e, st, randomBytes(t, testBigSize))
	tg.afterUpload = func(n int) {
		if n == 1 {
			later := time.Now().Add(time.Minute)
			_ = os.Chtimes(abs, later, later)
		}
	}
	out := e.uploadFile(context.Background(), it, abs)
	if out.state != store.StateFailed {
		t.Fatalf("upload of a changing file: %+v", out)
	}
	if tg.count() != 0 {
		t.Fatalf("%d part(s) of the inconsistent upload were left in Telegram", tg.count())
	}
	if pus, _ := st.PartUploads(""); len(pus) != 0 {
		t.Fatalf("rows left: %+v", pus)
	}
}

func TestChangedContentDropsEarlierUnfinishedUpload(t *testing.T) {
	tg := newFakeTG()
	e, st := newSplitEngine(t, tg)
	it, abs := bigFile(t, e, st, randomBytes(t, testBigSize))
	tg.beforeUpload = func(n int, _ tgclient.UploadRequest) error {
		if n == 2 {
			return errors.New("connection lost")
		}
		return nil
	}
	e.uploadFile(context.Background(), it, abs)
	tg.beforeUpload = nil
	if err := os.WriteFile(abs, randomBytes(t, testBigSize), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := e.uploadFile(context.Background(), it, abs); out.state != store.StateSynced {
		t.Fatalf("upload of new content: %+v", out)
	}
	if tg.count() != 3 {
		t.Fatalf("channel holds %d messages; the old content's part should have been removed", tg.count())
	}
}

func TestSplitDownloadRetriesABadPart(t *testing.T) {
	tg := newFakeTG()
	e, st := newSplitEngine(t, tg)
	data := randomBytes(t, testBigSize)
	it, abs := bigFile(t, e, st, data)
	e.uploadFile(context.Background(), it, abs)
	v := latest(t, st, "Docs/movie.mkv")
	parts, _ := st.Parts(v.MsgID)

	// Part 2 arrives corrupted once: it's fetched again and the restore works.
	tg.badDownloads[parts[1].MsgID] = 1
	dest := filepath.Join(t.TempDir(), "out.mkv")
	if _, _, err := e.restoreFile(context.Background(), v, dest); err != nil {
		t.Fatalf("restore with one bad download: %v", err)
	}
	if got, _ := os.ReadFile(dest); !bytes.Equal(got, data) {
		t.Fatal("restored bytes differ")
	}

	// Corrupted every time: the restore fails and leaves nothing behind.
	tg.badDownloads[parts[2].MsgID] = 100
	dir := t.TempDir()
	if _, _, err := e.restoreFile(context.Background(), v, filepath.Join(dir, "bad.mkv")); err == nil ||
		!strings.Contains(err.Error(), "part 3 of 3") {
		t.Fatalf("restore with a bad part: %v", err)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("files left after a failed restore: %v", left)
	}

	// A browser download stops with an error instead of finishing silently.
	if err := e.DownloadTo(context.Background(), v, io.Discard); err == nil {
		t.Fatal("stream of a bad part should fail")
	}
}

func TestReconcileFindsSplitFilesAndNoticesMissingParts(t *testing.T) {
	tg := newFakeTG()
	e, st := newSplitEngine(t, tg)
	it, abs := bigFile(t, e, st, randomBytes(t, testBigSize))
	e.uploadFile(context.Background(), it, abs)
	v := latest(t, st, "Docs/movie.mkv")

	// Another device (or a reinstall) rebuilds the list from Telegram alone.
	other, otherSt := newSplitEngine(t, tg)
	if _, err := other.reconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := latest(t, otherSt, "Docs/movie.mkv")
	if got.MsgID != v.MsgID || got.Parts != 3 || got.Hash != v.Hash {
		t.Fatalf("rebuilt version %+v, want %+v", got, v)
	}

	// A part deleted inside Telegram makes the whole file count as missing.
	parts, _ := st.Parts(v.MsgID)
	_ = tg.DeleteMessages(context.Background(), []int{parts[1].MsgID})
	if _, err := other.reconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := otherSt.LatestVersion("Docs/movie.mkv"); ok {
		t.Fatal("a split file with a missing part must not count as backed up")
	}
	// The remaining parts are recent and might be another device's upload in
	// progress, so they're left alone.
	if tg.count() != 2 {
		t.Fatalf("recent parts were removed: %d left", tg.count())
	}
}

func TestReconcileRemovesAbandonedParts(t *testing.T) {
	tg := newFakeTG()
	e, st := newSplitEngine(t, tg)
	it, abs := bigFile(t, e, st, randomBytes(t, testBigSize))
	tg.beforeUpload = func(n int, _ tgclient.UploadRequest) error {
		if n == 2 {
			return errors.New("connection lost")
		}
		return nil
	}
	e.uploadFile(context.Background(), it, abs)

	other, _ := newSplitEngine(t, tg)
	if _, err := other.reconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tg.count() != 1 {
		t.Fatal("a fresh unfinished upload must be left alone")
	}
	// Make it look abandoned for over a week.
	for _, m := range tg.msgs {
		m.date -= int((orphanAge + time.Hour).Seconds())
	}
	// The device doing the upload still keeps it...
	if _, err := e.reconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tg.count() != 1 {
		t.Fatal("this device's own unfinished upload must be kept")
	}
	// ...but for everyone else it's abandoned.
	if _, err := other.reconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if tg.count() != 0 {
		t.Fatalf("abandoned parts left: %d", tg.count())
	}
}

func TestSyncPicksUpSplitFileFromAnotherDevice(t *testing.T) {
	tg := newFakeTG()
	e, st := newSplitEngine(t, tg)
	it, abs := bigFile(t, e, st, randomBytes(t, testBigSize))
	e.uploadFile(context.Background(), it, abs)
	v := latest(t, st, "Docs/movie.mkv")

	other, otherSt := newSplitEngine(t, tg)
	n, err := other.SyncNew(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("sync added %d, %v; want 1", n, err)
	}
	if got := latest(t, otherSt, "Docs/movie.mkv"); got.MsgID != v.MsgID || got.Parts != 3 {
		t.Fatalf("synced version %+v", got)
	}
	// The uploading device doesn't count its own file as new.
	if n, _ := e.SyncNew(context.Background()); n != 0 {
		t.Fatalf("own upload counted as new: %d", n)
	}
}

func TestMoveSplitFileRollsBackOnFailure(t *testing.T) {
	tg := newFakeTG()
	e, st := newSplitEngine(t, tg)
	it, abs := bigFile(t, e, st, randomBytes(t, testBigSize))
	e.uploadFile(context.Background(), it, abs)
	v := latest(t, st, "Docs/movie.mkv")
	parts, _ := st.Parts(v.MsgID)

	tg.editFails[parts[2].MsgID] = errors.New("MESSAGE_EDIT_TIME_EXPIRED")
	if err := e.moveVersion(context.Background(), v, "Docs/Films/movie.mkv", nil); err == nil {
		t.Fatal("move should fail when a part can't be edited")
	}
	for _, p := range parts {
		in, _ := meta.Parse(tg.msgs[p.MsgID].caption)
		if in.Path != "Docs/movie.mkv" {
			t.Fatalf("part %d was left at %s after a failed move", p.Index, in.Path)
		}
	}

	delete(tg.editFails, parts[2].MsgID)
	if err := e.moveVersion(context.Background(), v, "Docs/Films/movie.mkv", nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range parts {
		in, _ := meta.Parse(tg.msgs[p.MsgID].caption)
		if in.Path != "Docs/Films/movie.mkv" {
			t.Fatalf("part %d is at %s after the move", p.Index, in.Path)
		}
	}
	// Telegram alone still describes a complete file at the new place.
	other, otherSt := newSplitEngine(t, tg)
	if _, err := other.reconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := latest(t, otherSt, "Docs/Films/movie.mkv"); got.Parts != 3 {
		t.Fatalf("after the move, reconcile found %+v", got)
	}
}

func TestPruneAndCheckCoverEveryPart(t *testing.T) {
	tg := newFakeTG()
	e, st := newSplitEngine(t, tg)
	it, abs := bigFile(t, e, st, randomBytes(t, testBigSize))
	for i := 0; i < keepVersions+1; i++ {
		if err := os.WriteFile(abs, randomBytes(t, testBigSize), 0o644); err != nil {
			t.Fatal(err)
		}
		if out := e.uploadFile(context.Background(), it, abs); out.state != store.StateSynced {
			t.Fatalf("upload %d: %+v", i, out)
		}
	}
	vs, _ := st.Versions("Docs/movie.mkv")
	if len(vs) != keepVersions || tg.count() != keepVersions*3 {
		t.Fatalf("%d versions and %d messages; want %d and %d", len(vs), tg.count(), keepVersions, keepVersions*3)
	}

	// The check before freeing space notices one missing part.
	problems, err := e.checkInTelegram(context.Background(), vs[:1])
	if err != nil || len(problems) != 0 {
		t.Fatalf("complete file reported %v, %v", problems, err)
	}
	parts, _ := st.Parts(vs[0].MsgID)
	_ = tg.DeleteMessages(context.Background(), []int{parts[2].MsgID})
	problems, _ = e.checkInTelegram(context.Background(), vs[:1])
	if problems[vs[0].MsgID] == "" {
		t.Fatal("a missing part must block freeing space")
	}
}

func TestDuplicatePartIsTolerated(t *testing.T) {
	tg := newFakeTG()
	e, st := newSplitEngine(t, tg)
	it, abs := bigFile(t, e, st, randomBytes(t, testBigSize))
	e.uploadFile(context.Background(), it, abs)
	v := latest(t, st, "Docs/movie.mkv")
	parts, _ := st.Parts(v.MsgID)

	// Part 2 posted twice, as after a crash right after posting it.
	m := tg.msgs[parts[1].MsgID]
	tg.next++
	tg.msgs[tg.next] = &fakeMsg{caption: m.caption, data: m.data, date: m.date, name: m.name}

	other, otherSt := newSplitEngine(t, tg)
	if _, err := other.reconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := latest(t, otherSt, "Docs/movie.mkv"); got.Parts != 3 {
		t.Fatalf("with a duplicate part, found %+v", got)
	}
	// The device that recorded the file keeps its parts; the spare copy goes.
	if _, err := e.reconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := tg.msgs[parts[1].MsgID]; !ok || tg.count() != 3 {
		t.Fatalf("expected the recorded part kept and the duplicate removed; %d messages", tg.count())
	}
}

func TestRestoreRefusesWhenDiskIsTooSmall(t *testing.T) {
	if err := checkFreeSpace(t.TempDir(), 1<<62); err == nil || !strings.Contains(err.Error(), "not enough free space") {
		t.Fatalf("got %v", err)
	}
	if err := checkFreeSpace(t.TempDir(), 1024); err != nil {
		t.Fatal(err)
	}
}

func TestSkippedBigFilesAreQueuedAgain(t *testing.T) {
	_, st := newSplitEngine(t, newFakeTG())
	root, _ := st.AddRoot(t.TempDir(), "Docs")
	if err := st.UpsertSeen(store.Item{Kind: store.KindFile, CloudPath: "Docs/huge.iso", RootID: root.ID, Rel: "huge.iso",
		Size: 5 << 30, State: store.StateSkipped, Note: "over 2 GB; Telegram can't store files this big yet"}); err != nil {
		t.Fatal(err)
	}
	if err := st.ResetUploading(); err != nil {
		t.Fatal(err)
	}
	it, _ := st.ItemByPath("Docs/huge.iso")
	if it.State != store.StatePending || it.Note != "" {
		t.Fatalf("item = %+v, want pending", it)
	}
}

func TestOnlyOneBigUploadAtATime(t *testing.T) {
	tg := newFakeTG()
	e, st := newSplitEngine(t, tg)
	it, abs := bigFile(t, e, st, randomBytes(t, testBigSize))
	e.bigSlot <- struct{}{} // another big file is uploading
	out := e.uploadFile(context.Background(), it, abs)
	if out.wait.IsZero() || tg.count() != 0 {
		t.Fatalf("second big file should wait: %+v, %d messages", out, tg.count())
	}
	<-e.bigSlot
	if out := e.uploadFile(context.Background(), it, abs); out.state != store.StateSynced {
		t.Fatalf("after the first finished: %+v", out)
	}
	if len(e.bigSlot) != 0 {
		t.Fatal("the slot must be released after an upload")
	}
}

// Task IDs reach the browser as JSON numbers; above 2^53 JavaScript rounds
// them and Dismiss sends an ID that matches nothing.
func TestTaskIDsSurviveJavaScriptAndDismiss(t *testing.T) {
	e, _ := newSplitEngine(t, newFakeTG())
	a, b := e.newTask("restore", "a"), e.newTask("restore", "b")
	if a.ID == b.ID {
		t.Fatal("two tasks got the same ID")
	}
	for _, task := range []*Task{a, b} {
		if task.ID > 1<<53 {
			t.Fatalf("task ID %d is too big for JavaScript", task.ID)
		}
	}
	a.update(func(t *Task) { t.Finished = true })
	e.DismissTask(a.ID)
	tasks := e.Status().Tasks
	if len(tasks) != 1 || tasks[0].ID != b.ID {
		t.Fatalf("after dismissing a, tasks = %+v", tasks)
	}
}
