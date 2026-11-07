package backup

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/tgerr"

	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/tgclient"
)

func init() {
	dlBackoffUnit, dlOfflineWait, dlPoll, dlStall = time.Millisecond, 30*time.Millisecond, 15*time.Millisecond, 300*time.Millisecond
}

// waitDownloads waits until nothing is queued or running.
func waitDownloads(t *testing.T, e *Engine) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if !e.DownloadsActive() {
			time.Sleep(60 * time.Millisecond)
			if !e.DownloadsActive() {
				return
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("downloads never settled: %+v", e.Downloads())
}

func waitState(t *testing.T, e *Engine, p, want string) store.MBDownload {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if d, ok := e.st.DLGet(p); ok && d.State == want {
			return d
		}
		time.Sleep(10 * time.Millisecond)
	}
	d, _ := e.st.DLGet(p)
	t.Fatalf("%s never reached %q: %+v", p, want, d)
	return d
}

// bigRig has one 3 MB movie in the library, uploaded.
func bigRig(t *testing.T) (*Engine, *fakeTG, string, []byte) {
	e, st, tg, root, dir := moveRig(t)
	st.Set(SettingMediaRoot, "R/tgmoviebox")
	e.splitAt = 2 << 30 // the test engine splits tiny files; a 3 MB movie is one message
	data := bytes.Repeat([]byte("0123456789abcdef"), 3*1024*1024/16)
	write(t, filepath.Join(dir, "tgmoviebox", "Movies", "Heat (1995)", "Heat (1995).mkv"), string(data))
	syncAll(t, e, st, root)
	return e, tg, "R/tgmoviebox/Movies/Heat (1995)/Heat (1995).mkv", data
}

func TestDownloadsCarryOnFromWhereTheyStoppedAfterA503(t *testing.T) {
	e, tg, p, data := bigRig(t)
	failed := false
	tg.dlFault = func(_ context.Context, _ int, off int64) error {
		if off == 2*tgclient.BlockSize && !failed {
			failed = true
			return tgerr.New(503, "Timeout")
		}
		return nil
	}
	if _, err := e.QueueDownloads(context.Background(), []string{p}); err != nil {
		t.Fatal(err)
	}
	waitDownloads(t, e)
	d, _ := e.st.DLGet(p)
	if d.State != "done" {
		t.Fatalf("should finish: %+v", d)
	}
	got, err := os.ReadFile(e.MediaLocal(p))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("the file should be whole and right: %v", err)
	}
	if len(tg.dlCalls) != 2 || tg.dlCalls[0] != 0 || tg.dlCalls[1] != 2*tgclient.BlockSize {
		t.Errorf("the second try should start where the first stopped (2 blocks in), not at zero: %v", tg.dlCalls)
	}
	if exists(e.MediaLocal(p) + ".part") {
		t.Error("no partial file left behind")
	}
}

func TestACorruptDownloadIsCaughtAndFetchedAgainFromZero(t *testing.T) {
	e, tg, p, data := bigRig(t)
	v, _ := e.st.LatestUnder(p)
	tg.badDownloads[v[0].MsgID] = 1
	e.QueueDownloads(context.Background(), []string{p})
	waitDownloads(t, e)
	got, _ := os.ReadFile(e.MediaLocal(p))
	if !bytes.Equal(got, data) {
		t.Fatal("the good copy should win")
	}
	if len(tg.dlCalls) != 2 || tg.dlCalls[1] != 0 {
		t.Errorf("after a failed check the pieces cannot be trusted: second try from zero, got %v", tg.dlCalls)
	}
}

func TestPauseKeepsTheProgressAndResumeFinishes(t *testing.T) {
	e, tg, p, data := bigRig(t)
	gate := make(chan struct{})
	reached := make(chan struct{}, 4)
	tg.dlFault = func(ctx context.Context, _ int, off int64) error {
		if off == tgclient.BlockSize {
			reached <- struct{}{}
			select {
			case <-gate:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	e.QueueDownloads(context.Background(), []string{p})
	<-reached
	if err := e.DownloadControl("pause", p); err != nil {
		t.Fatal(err)
	}
	waitState(t, e, p, "paused")
	time.Sleep(150 * time.Millisecond)
	if d2, _ := e.st.DLGet(p); d2.State != "paused" {
		t.Fatalf("a paused download stays paused: %+v", d2)
	}
	part, err := os.Stat(e.MediaLocal(p) + ".part")
	if err != nil || part.Size() < tgclient.BlockSize {
		t.Fatalf("the part must stay: %v", err)
	}
	for _, it := range e.Downloads() {
		if it.Path == p && it.Done < tgclient.BlockSize {
			t.Errorf("a paused download keeps showing its progress, not zero: %+v", it)
		}
	}
	close(gate)
	tg.mu.Lock()
	tg.dlFault = nil
	tg.mu.Unlock()
	e.DownloadControl("resume", p)
	waitDownloads(t, e)
	got, _ := os.ReadFile(e.MediaLocal(p))
	if !bytes.Equal(got, data) {
		t.Fatal("resumed download should be right")
	}
	if tg.dlCalls[len(tg.dlCalls)-1] < tgclient.BlockSize {
		t.Errorf("resuming starts after what was already fetched: %v", tg.dlCalls)
	}
}

func TestCancelRemovesEverything(t *testing.T) {
	e, tg, p, _ := bigRig(t)
	reached := make(chan struct{}, 4)
	tg.dlFault = func(ctx context.Context, _ int, off int64) error {
		if off == tgclient.BlockSize {
			reached <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	e.QueueDownloads(context.Background(), []string{p})
	<-reached
	e.DownloadControl("cancel", p)
	time.Sleep(200 * time.Millisecond)
	if _, ok := e.st.DLGet(p); ok {
		t.Error("a cancelled download leaves the queue")
	}
	if exists(e.MediaLocal(p)+".part") || exists(e.MediaLocal(p)) {
		t.Error("and its files")
	}
}

func TestNoConnectionIsWaitedOutWithoutUsingUpTries(t *testing.T) {
	e, st, tg, root, dir := moveRig(t)
	st.Set(SettingMediaRoot, "R/tgmoviebox")
	write(t, filepath.Join(dir, "tgmoviebox", "Movies", "Heat (1995)", "Heat (1995).mkv"), "heat")
	syncAll(t, e, st, root)
	link := &flaky{fakeTG: tg, down: true}
	e.tg = link
	p := "R/tgmoviebox/Movies/Heat (1995)/Heat (1995).mkv"
	e.QueueDownloads(context.Background(), []string{p})
	time.Sleep(400 * time.Millisecond)
	d, _ := e.st.DLGet(p)
	if d.State == "failed" || d.Attempts != 0 {
		t.Fatalf("offline is not a failure: %+v", d)
	}
	link.down = false
	waitDownloads(t, e)
	if d, _ = e.st.DLGet(p); d.State != "done" {
		t.Fatalf("it finishes once the connection is back: %+v", d)
	}
}

func TestItGivesUpAfterTheRetryLimitAndRetryStartsAgain(t *testing.T) {
	e, tg, p, data := bigRig(t)
	tg.dlFault = func(context.Context, int, int64) error { return tgerr.New(503, "Timeout") }
	e.QueueDownloads(context.Background(), []string{p})
	d := waitState(t, e, p, "failed")
	if d.Attempts != dlMaxAttempts || d.Err == "" {
		t.Fatalf("a clear failure with a reason: %+v", d)
	}
	tg.mu.Lock()
	tg.dlFault = nil
	tg.mu.Unlock()
	e.DownloadControl("retry", p)
	waitDownloads(t, e)
	got, _ := os.ReadFile(e.MediaLocal(p))
	if !bytes.Equal(got, data) {
		t.Fatal("Retry should work")
	}
}

func TestAFileDeletedFromTelegramFailsStraightAway(t *testing.T) {
	e, tg, p, _ := bigRig(t)
	v, _ := e.st.LatestUnder(p)
	tg.DeleteMessages(context.Background(), []int{v[0].MsgID})
	e.QueueDownloads(context.Background(), []string{p})
	d := waitState(t, e, p, "failed")
	if len(tg.dlCalls) != 1 || d.Err == "" {
		t.Errorf("no point retrying a deleted file: %+v, tried %d times", d, len(tg.dlCalls))
	}
}

func TestNoRoomOnThePhoneIsReportedNotRetried(t *testing.T) {
	e, _, _, _ := bigRig(t)
	huge := "R/tgmoviebox/Movies/Huge (2020)/Huge (2020).mkv"
	if err := e.st.AddVersion(store.Version{MsgID: 9999, CloudPath: huge, Kind: store.KindFile, Size: 1 << 60, Hash: "h", UploadedAt: 5}); err != nil {
		t.Fatal(err)
	}
	e.QueueDownloads(context.Background(), []string{huge})
	d := waitState(t, e, huge, "failed")
	if d.Attempts > 1 || d.Err == "" {
		t.Errorf("not enough space is a clear failure straight away: %+v", d)
	}
}

func TestTheQueueSurvivesARestart(t *testing.T) {
	e, tg, p, data := bigRig(t)
	tg.dlFault = func(ctx context.Context, _ int, off int64) error {
		if off == tgclient.BlockSize {
			<-ctx.Done() // stuck, as if the process were about to be killed
			return ctx.Err()
		}
		return nil
	}
	e.QueueDownloads(context.Background(), []string{p})
	time.Sleep(200 * time.Millisecond)
	e.dlStop(p, "pause") // stops the transfer; the row is left saying "running", as a killed process leaves it
	time.Sleep(150 * time.Millisecond)
	tg.mu.Lock()
	tg.dlFault = nil
	tg.mu.Unlock()
	e.st.DLSet(p, "running", 0, "", 0, 0) // what the table says after a kill
	e.ResumeDownloads()
	waitDownloads(t, e)
	got, _ := os.ReadFile(e.MediaLocal(p))
	if !bytes.Equal(got, data) {
		t.Fatal("after a restart the download carries on and finishes")
	}
}

func TestADownloadThatGoesQuietIsAbandonedAndResumed(t *testing.T) {
	e, tg, p, data := bigRig(t)
	var once sync.Once
	tg.dlFault = func(ctx context.Context, _ int, off int64) error {
		if off == tgclient.BlockSize {
			hang := false
			once.Do(func() { hang = true })
			if hang { // the connection died without a word
				<-ctx.Done()
				return ctx.Err()
			}
		}
		return nil
	}
	e.QueueDownloads(context.Background(), []string{p})
	waitDownloads(t, e)
	d, _ := e.st.DLGet(p)
	if d.State != "done" || d.Attempts != 0 {
		t.Fatalf("a stall is waited out without using up tries: %+v", d)
	}
	got, _ := os.ReadFile(e.MediaLocal(p))
	if !bytes.Equal(got, data) {
		t.Fatal("the file should be whole")
	}
}

func TestDownloadsGoWhereTheOwnerChoseAndOldOnesAreStillFound(t *testing.T) {
	e, _, p, data := bigRig(t)
	sd := filepath.Join(t.TempDir(), "sdcard", "tgmoviebox")
	e.SetMediaStores([]MediaStore{{Label: "SD card", Path: sd}})
	if err := e.SetMediaDir("/nowhere/else"); err == nil {
		t.Fatal("only the places the phone offered can be chosen")
	}
	// first download goes to the phone's own folder
	e.QueueDownloads(context.Background(), []string{p})
	waitDownloads(t, e)
	first := e.MediaLocal(p)
	if !strings.HasPrefix(first, e.defaultMediaDir()) {
		t.Fatalf("default is the phone's own folder: %s", first)
	}
	// now the SD card is chosen: the file already downloaded stays found there
	if err := e.SetMediaDir(sd); err != nil {
		t.Fatal(err)
	}
	if e.MediaLocal(p) != first {
		t.Fatalf("an existing download is found where it is: %s", e.MediaLocal(p))
	}
	st := e.MediaStorage()
	if st.Current != sd || len(st.Stores) != 2 || st.Used < int64(len(data)) {
		t.Fatalf("storage summary: %+v", st)
	}
	// removing it frees the space wherever it is, and the next download uses the SD card
	if n, err := e.RemoveMedia([]string{p}); err != nil || n != 1 {
		t.Fatalf("remove: %d %v", n, err)
	}
	e.QueueDownloads(context.Background(), []string{p})
	waitDownloads(t, e)
	if got := e.MediaLocal(p); !strings.HasPrefix(got, sd) {
		t.Fatalf("new downloads go to the SD card: %s", got)
	}
	b, _ := os.ReadFile(e.MediaLocal(p))
	if !bytes.Equal(b, data) {
		t.Fatal("the file should be whole")
	}
	// the card is taken out: back to the phone's folder
	os.RemoveAll(filepath.Dir(sd))
	if e.mediaBases()[0] != e.defaultMediaDir() {
		t.Fatalf("a missing card falls back to the phone: %v", e.mediaBases())
	}
}

// The library app wraps its Telegram connection so it can't change anything;
// the wrapper must still let downloads resume (it once hid that, and every
// try began again from zero on the phone).
func TestAReadOnlyEngineStillResumesDownloads(t *testing.T) {
	e, tg, p, data := bigRig(t)
	e.ReadOnly()
	failed := false
	tg.dlFault = func(_ context.Context, _ int, off int64) error {
		if off == 2*tgclient.BlockSize && !failed {
			failed = true
			return tgerr.New(503, "Timeout")
		}
		return nil
	}
	e.QueueDownloads(context.Background(), []string{p})
	waitDownloads(t, e)
	got, _ := os.ReadFile(e.MediaLocal(p))
	if !bytes.Equal(got, data) {
		t.Fatal("the file should be whole")
	}
	for _, off := range tg.dlCalls[1:] {
		if off == 0 {
			t.Fatalf("a retry started again from zero: %v", tg.dlCalls)
		}
	}
}

func TestTheTimeLeftComesFromWhatHasReallyArrived(t *testing.T) {
	e, tg, p, _ := bigRig(t)
	gate := make(chan struct{})
	reached := make(chan struct{}, 4)
	tg.dlFault = func(ctx context.Context, _ int, off int64) error {
		if off == 2*tgclient.BlockSize {
			reached <- struct{}{}
			select {
			case <-gate:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	e.QueueDownloads(context.Background(), []string{p})
	<-reached
	waitPart(t, e, p, 2*tgclient.BlockSize)
	// pretend a steady 1 MB/s
	e.dl.mu.Lock()
	l := e.dl.live[p]
	l.speed = 1 << 20
	l.lastData = time.Now()
	e.dl.mu.Unlock()
	var it DLItem
	for _, x := range e.Downloads() {
		if x.Path == p {
			it = x
		}
	}
	// 3 MB file, 2 MB on disk, 1 MB/s: about a second left (the saved figure is still 0)
	if it.ETA < 1 || it.ETA > 2 {
		t.Fatalf("time left should follow the file on disk, not the saved figure: %+v", it)
	}
	close(gate)
}
