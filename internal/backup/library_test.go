package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Sammy-77A/tgvault/internal/media"
	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/tmdb"
)

type fakeDB struct {
	calls int
	down  bool
}

func (f *fakeDB) SearchMovie(_ context.Context, title string, year int) ([]tmdb.Result, error) {
	f.calls++
	if f.down {
		return nil, errors.New("offline")
	}
	all := []tmdb.Result{
		{ID: 14160, Title: "Up", Year: 2009, Popularity: 80},
		{ID: 5, Title: "Seed of Chucky", Year: 2004, Popularity: 20},
		{ID: 6, Title: "Bride of Chucky", Year: 1998, Popularity: 25},
	}
	var out []tmdb.Result
	for _, r := range all {
		if media.Norm(r.Title) == media.Norm(title) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeDB) SearchTV(_ context.Context, title string, year int) ([]tmdb.Result, error) {
	f.calls++
	if f.down {
		return nil, errors.New("offline")
	}
	switch media.Norm(title) {
	case "marvels jessica jones":
		return []tmdb.Result{{ID: 61889, Title: "Marvel's Jessica Jones", Year: 2015, Popularity: 60, TV: true}}, nil
	case "shameless":
		return []tmdb.Result{
			{ID: 34307, Title: "Shameless", Year: 2011, Popularity: 90, TV: true},
			{ID: 2200, Title: "Shameless", Year: 2004, Popularity: 30, TV: true},
		}, nil
	}
	return nil, nil
}

func (f *fakeDB) SeasonEpisodes(_ context.Context, id, season int) ([]tmdb.Episode, error) {
	var out []tmdb.Episode
	for i := 1; i <= 13; i++ {
		out = append(out, tmdb.Episode{Number: i})
	}
	return out, nil
}

func libRig(t *testing.T) (*Engine, string, *fakeDB) {
	e, st, _, root, dir := moveRig(t)
	st.Set(SettingMediaRoot, "R/tgmoviebox")
	db := &fakeDB{}
	e.SetMovieDB(db)
	lib := filepath.Join(dir, "tgmoviebox")
	for _, f := range []string{
		"Marvels_Jessica_Jones_S03E03_A_K_A_I_Have_No_Spleen_720p_NF_WEB.mkv",
		"Marvels_Jessica_Jones_S03E04_A_K_A_Customer_Service_is_Standing.mkv",
		"Up.2009.1080p.BluRay.x264.mkv",
		"Seed.of.Chucky.2004.UNRATED.REMASTERED.1080p.BluRay.x264.mkv",
		"Seed.of.Chucky.2004.UNRATED.REMASTERED.1080p.BluRay.x264.en.srt",
		"Shameless.S01E01.720p.mkv",
		"Shameless.S01E02.720p.mkv",
		"random holiday video.mp4",
		"notes.txt",
	} {
		write(t, filepath.Join(lib, f), "content of "+f)
	}
	syncAll(t, e, st, root)
	return e, lib, db
}

func organize(t *testing.T, e *Engine) {
	t.Helper()
	if _, err := e.OrganizeMedia(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitTasks(t, e)
}

func TestOrganizerSortsSureMatchesAndAsksAboutTheRest(t *testing.T) {
	e, lib, _ := libRig(t)
	organize(t, e)
	for _, ts := range e.Status().Tasks {
		if len(ts.Errors) > 0 {
			t.Fatalf("task %q errors: %v", ts.Title, ts.Errors)
		}
	}
	for _, p := range []string{
		"Shows/Marvel's Jessica Jones/Season 03/Marvel's Jessica Jones - S03E03.mkv",
		"Shows/Marvel's Jessica Jones/Season 03/Marvel's Jessica Jones - S03E04.mkv",
		"Movies/Up (2009)/Up (2009).mkv",
		"Movies/Seed of Chucky (2004)/Seed of Chucky (2004).mkv",
		"Movies/Seed of Chucky (2004)/Seed of Chucky (2004).en.srt",
	} {
		if !exists(filepath.Join(lib, filepath.FromSlash(p))) {
			t.Errorf("missing on disk: %s", p)
		}
	}
	if !exists(filepath.Join(lib, "notes.txt")) {
		t.Error("a text file is not a video and must stay put")
	}
	rv, err := e.Review()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]ReviewItem{}
	for _, r := range rv {
		names[r.Name] = r
	}
	if _, ok := names["Shameless.S01E01.720p.mkv"]; !ok || len(names) != 3 {
		t.Fatalf("expected Shameless x2 and the holiday video in review, got %v", names)
	}
	if r := names["Shameless.S01E01.720p.mkv"]; len(r.Candidates) != 2 || r.Reason == "" || !strings.Contains(r.Parsed, "S01E01") {
		t.Errorf("review item should show candidates and what was read: %+v", r)
	}
	if r := names["random holiday video.mp4"]; len(r.Candidates) != 0 {
		t.Errorf("nothing to suggest for the holiday video: %+v", r)
	}
	// files being reviewed were not touched
	if !exists(filepath.Join(lib, "Shameless.S01E01.720p.mkv")) {
		t.Error("an unsure file must stay where it was")
	}
}

func TestOneAnswerSettlesEveryFileWithThatName(t *testing.T) {
	e, lib, _ := libRig(t)
	organize(t, e)
	path := "R/tgmoviebox/Shameless.S01E01.720p.mkv"
	err := e.ResolveMedia(context.Background(), path, Guess{Kind: "episode", TMDB: 34307, Title: "Shameless", Year: 2011})
	if err != nil {
		t.Fatal(err)
	}
	waitTasks(t, e)
	// the second episode now follows the same answer without asking
	organize(t, e)
	for _, p := range []string{
		"Shows/Shameless (2011)/Season 01/Shameless - S01E01.mkv",
		"Shows/Shameless (2011)/Season 01/Shameless - S01E02.mkv",
	} {
		if !exists(filepath.Join(lib, filepath.FromSlash(p))) {
			t.Errorf("missing: %s", p)
		}
	}
	rv, _ := e.Review()
	for _, r := range rv {
		if strings.HasPrefix(r.Name, "Shameless") {
			t.Errorf("Shameless should not be waiting any more: %+v", r)
		}
	}
}

func TestOrganizerLeavesFilesAloneWhenTMDBIsDown(t *testing.T) {
	e, lib, db := libRig(t)
	db.down = true
	organize(t, e)
	tv := lastTask(t, e)
	if len(tv.Errors) == 0 {
		t.Fatal("the outage should be reported")
	}
	if !exists(filepath.Join(lib, "Up.2009.1080p.BluRay.x264.mkv")) {
		t.Error("nothing should move while TMDB can't be asked")
	}
	rv, _ := e.Review()
	for _, r := range rv {
		if strings.HasPrefix(r.Name, "Up.") {
			t.Errorf("an outage must not put files in the review list: %+v", r)
		}
	}
}

func TestSkippedFilesAreNotAskedAgain(t *testing.T) {
	e, _, _ := libRig(t)
	organize(t, e)
	if err := e.SkipMedia("R/tgmoviebox/random holiday video.mp4"); err != nil {
		t.Fatal(err)
	}
	files, _ := e.inbox()
	for _, f := range files {
		if strings.Contains(f, "holiday") {
			t.Errorf("a skipped file must not come back: %s", f)
		}
	}
}

func TestOrganizeNeedsAKey(t *testing.T) {
	e, _, _, _, _ := moveRig(t)
	if _, err := e.OrganizeMedia(context.Background()); !errors.Is(err, tmdb.ErrNoKey) {
		t.Fatalf("want ErrNoKey, got %v", err)
	}
	if e.HasTMDBKey() {
		t.Fatal("no key yet")
	}
	e.SetTMDBKey(" abc ")
	if !e.HasTMDBKey() {
		t.Fatal("key should be saved")
	}
}

func TestLibraryFoldersAreMadeOnDisk(t *testing.T) {
	e, st, _, _, dir := moveRig(t)
	st.Set(SettingMediaRoot, "R/tgmoviebox")
	if err := e.EnsureMediaFolders(); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"tgmoviebox", "tgmoviebox/Movies", "tgmoviebox/Shows"} {
		if !exists(filepath.Join(dir, filepath.FromSlash(d))) {
			t.Errorf("%s should exist on disk", d)
		}
	}
	if err := e.EnsureMediaFolders(); err != nil {
		t.Fatalf("running it twice must be fine: %v", err)
	}
}

func TestLibraryListsTitlesAndDownloadsForOffline(t *testing.T) {
	e, _, _ := libRig(t)
	organize(t, e)
	waitTasks(t, e)
	lib, err := e.Library()
	if err != nil {
		t.Fatal(err)
	}
	byTitle := map[string]LibTitle{}
	for _, tl := range lib {
		byTitle[tl.Title] = tl
	}
	up, seed, jj := byTitle["Up"], byTitle["Seed of Chucky"], byTitle["Marvel's Jessica Jones"]
	if up.Kind != "movie" || up.Year != 2009 || len(up.Files) != 1 {
		t.Fatalf("Up: %+v", up)
	}
	if len(seed.Files) != 1 || len(seed.Files[0].Subs) != 1 || seed.Files[0].Subs[0].Lang != "en" {
		t.Fatalf("Seed of Chucky should carry its english subtitle: %+v", seed)
	}
	if jj.Kind != "show" || len(jj.Seasons) != 1 || jj.Seasons[0].Number != 3 || len(jj.Seasons[0].Episodes) != 2 ||
		jj.Seasons[0].Episodes[0].Episodes[0] != 3 || jj.Seasons[0].Episodes[1].Episodes[0] != 4 {
		t.Fatalf("Jessica Jones: %+v", jj)
	}
	if seed.Files[0].Local {
		t.Fatal("nothing is downloaded yet")
	}

	if _, err := e.DownloadMedia(context.Background(), []string{seed.Folder}); err != nil {
		t.Fatal(err)
	}
	waitTasks(t, e)
	video := e.MediaLocal(seed.Files[0].Path)
	if !exists(video) || !exists(e.MediaLocal(seed.Files[0].Subs[0].Path)) {
		t.Fatalf("the video and its subtitle should be on the phone: %s", video)
	}
	if !strings.Contains(filepath.ToSlash(video), "/mb/Movies/Seed of Chucky (2004)/") {
		t.Errorf("downloads keep the library's folders: %s", video)
	}
	lib, _ = e.Library()
	for _, tl := range lib {
		if tl.Title == "Seed of Chucky" && (!tl.Files[0].Local || !tl.Files[0].Subs[0].Local) {
			t.Errorf("downloaded files should be marked local: %+v", tl.Files[0])
		}
	}
	if _, err := e.DownloadMedia(context.Background(), []string{seed.Folder}); err == nil {
		t.Error("downloading what's already there should say so")
	}
	if n, err := e.RemoveMedia([]string{seed.Folder}); err != nil || n != 1 || exists(video) {
		t.Fatalf("remove: n=%d err=%v still=%v", n, err, exists(video))
	}
	if _, err := e.DownloadMedia(context.Background(), []string{"R/elsewhere/file.mkv"}); err == nil {
		t.Error("only the library can be downloaded this way")
	}
}

func TestPlaybackProgress(t *testing.T) {
	e, _, _ := libRig(t)
	p := "R/tgmoviebox/Movies/Up (2009)/Up (2009).mkv"
	if pos, _ := e.Progress(p); pos != 0 {
		t.Fatal("no progress yet")
	}
	e.SetProgress(p, 600000, 5400000)
	if pos, dur := e.Progress(p); pos != 600000 || dur != 5400000 {
		t.Fatalf("got %d/%d", pos, dur)
	}
	e.SetProgress(p, 5300000, 5400000) // nearly finished: watched, start over
	if pos, _ := e.Progress(p); pos != 0 {
		t.Errorf("a watched film restarts, got %d", pos)
	}
	if err := e.SetProgress("R/other/x.mkv", 1, 2); err == nil {
		t.Error("only library files have progress")
	}
}

func TestDiskUsageCountsDownloads(t *testing.T) {
	e, _, _ := libRig(t)
	if e.MediaDiskUsage() != 0 {
		t.Fatal("nothing downloaded yet")
	}
	organize(t, e)
	lib, _ := e.Library()
	for _, tl := range lib {
		if tl.Title == "Up" {
			e.DownloadMedia(context.Background(), []string{tl.Folder})
		}
	}
	waitTasks(t, e)
	if e.MediaDiskUsage() <= 0 {
		t.Error("a downloaded video should count")
	}
	// the "remove all" the settings page offers: the two library folders at once
	root := e.MediaRoot()
	if _, err := e.RemoveMedia([]string{root + "/Movies", root + "/Shows"}); err != nil {
		t.Fatal(err)
	}
	if e.MediaDiskUsage() != 0 {
		t.Errorf("everything should be gone: %d", e.MediaDiskUsage())
	}
}

func TestReviewForgetsFilesThatAreGone(t *testing.T) {
	e, lib, _ := libRig(t)
	organize(t, e)
	rv, _ := e.Review()
	if len(rv) != 3 {
		t.Fatalf("setup: %d waiting", len(rv))
	}
	path := "R/tgmoviebox/random holiday video.mp4"
	os.Remove(filepath.Join(lib, "random holiday video.mp4"))
	e.scanRoot(mustRoot(t, e))
	if _, err := e.DeleteFromCloudMany(context.Background(), []string{path}); err != nil {
		t.Fatal(err)
	}
	waitTasks(t, e)
	rv, _ = e.Review()
	for _, r := range rv {
		if r.Name == "random holiday video.mp4" {
			t.Errorf("a deleted file should leave the review list: %+v", r)
		}
	}
	if len(rv) != 2 {
		t.Errorf("the two Shameless files stay: %d", len(rv))
	}
}

func mustRoot(t *testing.T, e *Engine) store.Root {
	roots, err := e.st.Roots()
	if err != nil || len(roots) == 0 {
		t.Fatal("no root")
	}
	return roots[0]
}

func TestLibraryDropsFilesDeletedFromTelegram(t *testing.T) {
	e, st, tg, root, dir := moveRig(t)
	st.Set(SettingMediaRoot, "R/tgmoviebox")
	write(t, filepath.Join(dir, "tgmoviebox", "Movies", "Up (2009)", "Up (2009).mkv"), "up")
	write(t, filepath.Join(dir, "tgmoviebox", "Movies", "Heat (1995)", "Heat (1995).mkv"), "heat")
	syncAll(t, e, st, root)
	lib, _ := e.Library()
	if len(lib) != 2 {
		t.Fatalf("setup: %+v", lib)
	}
	// the owner deletes Heat in Telegram; this device hasn't noticed
	var heat int
	for _, tl := range lib {
		if tl.Title == "Heat" {
			heat = tl.Files[0].MsgID
		}
	}
	tg.DeleteMessages(context.Background(), []int{heat})
	if lib, _ = e.Library(); len(lib) != 2 {
		t.Fatal("nothing knows yet")
	}
	e.RefreshLibrary(context.Background())
	lib, _ = e.Library()
	if len(lib) != 1 || lib[0].Title != "Up" {
		t.Errorf("Heat should be gone from the list, Up stays: %+v", lib)
	}
	// an unreachable Telegram must not empty the list
	tg.blockReads = 0
	e.RefreshLibrary(canceled())
	if lib, _ = e.Library(); len(lib) != 1 {
		t.Errorf("no answer means no change: %+v", lib)
	}
}

func canceled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
