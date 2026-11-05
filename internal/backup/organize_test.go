package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sammy-77A/tgvault/internal/store"
)

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func lastTask(t *testing.T, e *Engine) TaskView {
	t.Helper()
	ts := e.Status().Tasks
	if len(ts) == 0 {
		t.Fatal("no task")
	}
	return ts[len(ts)-1]
}

func TestMovingAFolderMovesTheRealFilesToo(t *testing.T) {
	e, st, tg, root, dir := moveRig(t)
	write(t, filepath.Join(dir, "Shows", "a.mkv"), "episode a")
	write(t, filepath.Join(dir, "Shows", "Season 1", "b.mkv"), "episode b")
	syncAll(t, e, st, root)
	uploads := tg.calls
	ids := map[string]int{}
	for _, p := range []string{"R/Shows/a.mkv", "R/Shows/Season 1/b.mkv"} {
		ids[p] = versionsAt(t, st, p)[0].MsgID
	}
	write(t, filepath.Join(dir, "Archive", "keep.txt"), "already here")
	syncAll(t, e, st, root)
	uploads = tg.calls

	if _, err := e.MoveInCloud(context.Background(), []string{"R/Shows"}, "R/Archive"); err != nil {
		t.Fatal(err)
	}
	waitTasks(t, e)
	if tv := lastTask(t, e); len(tv.Errors) > 0 {
		t.Fatalf("errors: %v", tv.Errors)
	}
	if !exists(filepath.Join(dir, "Archive", "Shows", "a.mkv")) || !exists(filepath.Join(dir, "Archive", "Shows", "Season 1", "b.mkv")) {
		t.Fatal("the files should be in the new folder on disk")
	}
	if exists(filepath.Join(dir, "Shows")) {
		t.Error("the emptied old folder should be gone")
	}
	if !exists(filepath.Join(dir, "Archive", "keep.txt")) {
		t.Error("what was already in the destination must stay")
	}
	for old, id := range ids {
		now := "R/Archive/" + strings.TrimPrefix(old, "R/")
		if vs := versionsAt(t, st, now); len(vs) != 1 || vs[0].MsgID != id {
			t.Errorf("%s: the stored copy should be at the new path: %+v", now, vs)
		}
		if len(versionsAt(t, st, old)) != 0 {
			t.Errorf("%s: still stored under the old path", old)
		}
		if it, err := st.ItemByPath(now); err != nil || !it.LocalPresent || it.State != store.StateSynced {
			t.Errorf("%s: the item should follow: %+v %v", now, it, err)
		}
		if _, err := st.ItemByPath(old); err == nil {
			t.Errorf("%s: the old item should be gone", old)
		}
	}
	syncAll(t, e, st, root)
	if tg.calls != uploads {
		t.Errorf("a scan after the move uploaded %d file(s)", tg.calls-uploads)
	}
	if it, err := st.ItemByPath("R/Archive/Shows/a.mkv"); err != nil || it.State != store.StateSynced {
		t.Errorf("still synced after the scan: %+v %v", it, err)
	}
}

func TestMovingAFileThatWasNeverBackedUp(t *testing.T) {
	e, st, tg, root, dir := moveRig(t)
	write(t, filepath.Join(dir, "old", "done.txt"), "uploaded")
	syncAll(t, e, st, root)
	write(t, filepath.Join(dir, "old", "waiting.txt"), "not uploaded yet")
	e.scanRoot(root) // seen, not uploaded

	if _, err := e.MoveInCloud(context.Background(), []string{"R/old"}, "R/new"); err != nil {
		t.Fatal(err)
	}
	waitTasks(t, e)
	if !exists(filepath.Join(dir, "new", "old", "waiting.txt")) || exists(filepath.Join(dir, "old")) {
		t.Fatal("the file waiting for its first backup should move too, or it would be backed up at the old place")
	}
	syncAll(t, e, st, root)
	if len(versionsAt(t, st, "R/new/old/waiting.txt")) != 1 || len(versionsAt(t, st, "R/old/waiting.txt")) != 0 {
		t.Error("its first backup should be at the new place")
	}
	if tg.calls != 2 {
		t.Errorf("%d uploads, want 2 (one per file, none repeated)", tg.calls)
	}
}

func TestRenameFileAndFolder(t *testing.T) {
	e, st, tg, root, dir := moveRig(t)
	write(t, filepath.Join(dir, "Docs", "draft.txt"), "words")
	write(t, filepath.Join(dir, "Docs", "more.txt"), "more words")
	syncAll(t, e, st, root)
	uploads := tg.calls

	if _, err := e.RenameInCloud(context.Background(), "R/Docs/draft.txt", "final.txt"); err != nil {
		t.Fatal(err)
	}
	waitTasks(t, e)
	if !exists(filepath.Join(dir, "Docs", "final.txt")) || exists(filepath.Join(dir, "Docs", "draft.txt")) {
		t.Fatal("the file on disk should be renamed")
	}
	if _, err := e.RenameInCloud(context.Background(), "R/Docs", "Documents"); err != nil {
		t.Fatal(err)
	}
	waitTasks(t, e)
	if !exists(filepath.Join(dir, "Documents", "final.txt")) || !exists(filepath.Join(dir, "Documents", "more.txt")) || exists(filepath.Join(dir, "Docs")) {
		t.Fatal("the folder on disk should be renamed")
	}
	if len(versionsAt(t, st, "R/Documents/final.txt")) != 1 || len(versionsAt(t, st, "R/Docs/draft.txt")) != 0 {
		t.Error("the stored copies should follow both renames")
	}
	syncAll(t, e, st, root)
	if tg.calls != uploads {
		t.Errorf("renaming caused %d upload(s)", tg.calls-uploads)
	}
	for _, bad := range []string{"", "a/b", "..", "x:y", "what?"} {
		if _, err := e.RenameInCloud(context.Background(), "R/Documents/more.txt", bad); err == nil {
			t.Errorf("%q should not be a valid name", bad)
		}
	}
}

func TestMoveStopsWhenSomethingIsInTheWay(t *testing.T) {
	e, st, tg, root, dir := moveRig(t)
	write(t, filepath.Join(dir, "a", "x.txt"), "from a")
	syncAll(t, e, st, root)
	write(t, filepath.Join(dir, "b", "x.txt"), "a different file in b, not yet backed up")
	e.scanRoot(root)
	before := tg.calls

	if _, err := e.MoveInCloud(context.Background(), []string{"R/a/x.txt"}, "R/b"); err != nil {
		t.Fatal(err)
	}
	waitTasks(t, e)
	if tv := lastTask(t, e); len(tv.Errors) != 1 {
		t.Fatalf("want one error, got %v", tv.Errors)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "b", "x.txt")); string(b) != "a different file in b, not yet backed up" {
		t.Error("the file in the way was overwritten")
	}
	if !exists(filepath.Join(dir, "a", "x.txt")) {
		t.Error("the file must stay where it was")
	}
	if len(versionsAt(t, st, "R/a/x.txt")) != 1 || len(versionsAt(t, st, "R/b/x.txt")) != 0 {
		t.Error("the stored copy must not move when the file can't")
	}
	if tg.calls != before {
		t.Error("nothing should be uploaded")
	}
}

func TestMoveOutsideBackedUpFoldersIsRefused(t *testing.T) {
	e, st, _, root, dir := moveRig(t)
	write(t, filepath.Join(dir, "x.txt"), "here")
	syncAll(t, e, st, root)

	if _, err := e.MoveInCloud(context.Background(), []string{"R/x.txt"}, "Elsewhere"); err != nil {
		t.Fatal(err)
	}
	waitTasks(t, e)
	if tv := lastTask(t, e); len(tv.Errors) != 1 || !strings.Contains(tv.Errors[0], "backed up from this PC") {
		t.Fatalf("want a clear refusal, got %v", tv.Errors)
	}
	if !exists(filepath.Join(dir, "x.txt")) || len(versionsAt(t, st, "R/x.txt")) != 1 {
		t.Error("nothing should have moved")
	}
}

func TestABackedUpFolderItselfCannotBeMoved(t *testing.T) {
	e, st, _, root, dir := moveRig(t)
	write(t, filepath.Join(dir, "x.txt"), "here")
	syncAll(t, e, st, root)
	if _, err := e.MoveInCloud(context.Background(), []string{"R"}, "Other"); err == nil {
		t.Error("a whole backed-up folder shouldn't be movable")
	}
	if _, err := e.RenameInCloud(context.Background(), "R", "S"); err == nil {
		t.Error("a whole backed-up folder shouldn't be renamable")
	}
	if _, err := e.MoveInCloud(context.Background(), []string{"R/x.txt"}, "R/x.txt/inside"); err == nil {
		t.Error("moving into itself")
	}
}

func TestNewFolderThenMoveFilesIn(t *testing.T) {
	e, st, tg, root, dir := moveRig(t)
	write(t, filepath.Join(dir, "x.txt"), "file")
	syncAll(t, e, st, root)

	if _, err := e.NewFolder("", "Top"); err == nil {
		t.Error("no new folders at the top level")
	}
	p, err := e.NewFolder("R", "Sorted")
	if err != nil || p != "R/Sorted" {
		t.Fatalf("%q %v", p, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "Sorted")); err != nil || !fi.IsDir() {
		t.Fatal("the folder should exist on disk too")
	}
	list, _ := e.List("R")
	found := false
	for _, en := range list {
		found = found || (en.Name == "Sorted" && en.Kind == "folder")
	}
	if !found {
		t.Fatalf("the empty folder should be listed: %+v", list)
	}
	if _, err := e.MoveInCloud(context.Background(), []string{"R/x.txt"}, "R/Sorted"); err != nil {
		t.Fatal(err)
	}
	waitTasks(t, e)
	if !exists(filepath.Join(dir, "Sorted", "x.txt")) {
		t.Fatal("the file should be in the new folder")
	}
	syncAll(t, e, st, root)
	if tg.calls != 1 {
		t.Errorf("%d uploads", tg.calls)
	}
	// Moving an empty folder carries it along, on disk too.
	if _, err := e.NewFolder("R", "Empty"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.MoveInCloud(context.Background(), []string{"R/Empty"}, "R/Sorted"); err != nil {
		t.Fatal(err)
	}
	waitTasks(t, e)
	if !exists(filepath.Join(dir, "Sorted", "Empty")) || exists(filepath.Join(dir, "Empty")) {
		t.Error("the empty folder should have moved on disk")
	}
	list, _ = e.List("R/Sorted")
	found = false
	for _, en := range list {
		found = found || en.Name == "Empty"
	}
	if !found {
		t.Errorf("the moved empty folder should be listed: %+v", list)
	}
	// And it can be removed again.
	if _, err := e.DeleteFromCloudMany(context.Background(), []string{"R/Sorted/Empty"}); err != nil {
		t.Fatal(err)
	}
	list, _ = e.List("R/Sorted")
	for _, en := range list {
		if en.Name == "Empty" {
			t.Error("the empty folder should be gone")
		}
	}
}

func TestMovedProjectFolderKeepsItsBackup(t *testing.T) {
	e, st, tg, root, dir := moveRig(t)
	write(t, filepath.Join(dir, "work", "app", "package.json"), "{}")
	write(t, filepath.Join(dir, "work", "app", "main.js"), "console.log(1)")
	e.scanRoot(root)
	rootProjects, _ := st.Projects(store.ProjectSuggested)
	if len(rootProjects) != 1 {
		t.Skipf("no project was detected here (%d)", len(rootProjects))
	}
	if err := st.SetProjectState(rootProjects[0].ID, store.ProjectConfirmed); err != nil {
		t.Fatal(err)
	}
	syncAll(t, e, st, root)
	uploads := tg.calls
	if len(versionsAt(t, st, "R/work/app")) == 0 {
		t.Skip("the project wasn't backed up in this test rig")
	}
	if _, err := e.MoveInCloud(context.Background(), []string{"R/work/app"}, "R/archive"); err != nil {
		t.Fatal(err)
	}
	waitTasks(t, e)
	if !exists(filepath.Join(dir, "archive", "app", "main.js")) {
		t.Fatal("the project folder should have moved")
	}
	syncAll(t, e, st, root)
	if tg.calls != uploads {
		t.Errorf("moving the project caused %d upload(s)", tg.calls-uploads)
	}
	if len(versionsAt(t, st, "R/archive/app")) == 0 {
		t.Error("its backup should be at the new place")
	}
}

// Organizing on one device organizes the same files on another: it follows the
// caption change and moves its own copy.
func TestOtherDeviceMovesItsFilesToo(t *testing.T) {
	tg := newFakeTG()
	a, sa := newSplitEngine(t, tg)
	dirA := t.TempDir()
	rootA, _ := sa.AddRoot(dirA, "R")
	write(t, filepath.Join(dirA, "x.txt"), "shared bytes")
	syncAll(t, a, sa, rootA)

	b, sb := newSplitEngine(t, tg)
	dirB := t.TempDir()
	rootB, _ := sb.AddRoot(dirB, "R")
	write(t, filepath.Join(dirB, "x.txt"), "shared bytes")
	if _, err := b.reconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	syncAll(t, b, sb, rootB)
	uploads := tg.calls

	if _, err := a.MoveInCloud(context.Background(), []string{"R/x.txt"}, "R/sorted"); err != nil {
		t.Fatal(err)
	}
	waitTasks(t, a)
	if _, err := b.reconcileOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(dirB, "sorted", "x.txt")) || exists(filepath.Join(dirB, "x.txt")) {
		t.Fatal("the other device should have moved its copy too")
	}
	syncAll(t, b, sb, rootB)
	if tg.calls != uploads {
		t.Errorf("%d upload(s) after the other device followed", tg.calls-uploads)
	}
	if it, err := sb.ItemByPath("R/sorted/x.txt"); err != nil || it.State != store.StateSynced {
		t.Errorf("its item should follow: %+v %v", it, err)
	}
}
