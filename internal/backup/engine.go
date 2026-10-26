// Package backup watches folders and uploads changes to Telegram.
package backup

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/gotd/td/tgerr"

	"github.com/Sammy-77A/tgvault/internal/meta"
	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/tgclient"
)

const (
	quietFile      = 20 * time.Second // a file must be unchanged this long before upload
	quietProject   = 5 * time.Minute  // a project must be unchanged this long before upload
	keepVersions   = 3
	workers        = 2
	rescanEvery    = 15 * time.Minute
	reconcileEvery = 6 * time.Hour
	maxWatches     = 20000
)

// Engine runs scanning, uploading and restoring.
type Engine struct {
	st     *store.Store
	tg     telegram
	tmpDir string

	// splitAt is the size above which files are split into several messages,
	// and partSize the size of each; tests make them small.
	splitAt, partSize int64

	wake   chan struct{}
	scanCh chan int64 // root ID, or 0 for all roots

	mu        sync.Mutex
	progress  map[int64]Progress
	scanning  bool
	watcher   *fsnotify.Watcher
	watched   map[string]int64
	pending   map[int64]*time.Timer
	tasks     []*Task
	reconcile Reconcile
	// Telegram rate limits apply to the whole account, so when one upload is
	// told to wait, every upload waits until cooldown.
	cooldown   time.Time
	cfg        Config
	conditions Conditions
}

// Progress of an item being uploaded.
type Progress struct {
	ItemID    int64  `json:"itemId"`
	CloudPath string `json:"cloudPath"`
	Phase     string `json:"phase"`
	Done      int64  `json:"done"`
	Total     int64  `json:"total"`
	Part      int    `json:"part,omitempty"`  // for split files: the part being uploaded
	Parts     int    `json:"parts,omitempty"` // and how many there are
	updated   time.Time
}

// Reconcile is the state of the last "check against Telegram".
type Reconcile struct {
	Running bool   `json:"running"`
	Last    int64  `json:"last"`
	Error   string `json:"error,omitempty"`
	Files   int    `json:"files"`
}

// New creates an engine.
func New(st *store.Store, tg *tgclient.Client, dataDir string) *Engine {
	e := newEngine(st, dataDir)
	if tg != nil {
		e.tg = tg
	}
	return e
}

func newEngine(st *store.Store, dataDir string) *Engine {
	return &Engine{
		st:       st,
		splitAt:  meta.MaxFileSize,
		partSize: meta.SplitPartSize,
		tmpDir:   filepath.Join(dataDir, "tmp"),
		wake:     make(chan struct{}, 1),
		scanCh:   make(chan int64, 64),
		progress: map[int64]Progress{},
		watched:  map[string]int64{},
		pending:  map[int64]*time.Timer{},
	}
}

// Run starts all background work and blocks until ctx ends.
func (e *Engine) Run(ctx context.Context) error {
	if err := os.MkdirAll(e.tmpDir, 0o700); err != nil {
		return err
	}
	cleanTmp(e.tmpDir)
	if err := e.st.ResetUploading(); err != nil {
		return err
	}
	w, err := fsnotify.NewWatcher()
	if err != nil {
		log.Printf("watcher unavailable, using periodic scans only: %v", err)
	} else {
		e.watcher = w
		defer w.Close()
		go e.watchLoop(ctx)
	}
	go e.scanLoop(ctx)
	go e.reconcileLoop(ctx)
	go e.syncLoop(ctx)
	for i := 0; i < workers; i++ {
		go e.workerLoop(ctx)
	}
	e.RequestScan(0)
	<-ctx.Done()
	return nil
}

func cleanTmp(dir string) {
	entries, _ := os.ReadDir(dir)
	for _, d := range entries {
		_ = os.RemoveAll(filepath.Join(dir, d.Name()))
	}
}

func (e *Engine) poke() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// RequestScan queues a scan of one root (or all with 0).
func (e *Engine) RequestScan(rootID int64) {
	select {
	case e.scanCh <- rootID:
	default:
	}
}

// ForgetAccount is called when a different Telegram account logs in.
func (e *Engine) ForgetAccount() {
	_ = e.st.Delete(settingSyncSeen)
	e.mu.Lock()
	e.reconcile = Reconcile{}
	e.mu.Unlock()
}

func (e *Engine) scanLoop(ctx context.Context) {
	tick := time.NewTicker(rescanEvery)
	defer tick.Stop()
	for {
		var id int64
		select {
		case <-ctx.Done():
			return
		case id = <-e.scanCh:
		case <-tick.C:
		}
		// Collapse queued requests into one pass.
		all := id == 0
		ids := map[int64]bool{id: true}
	drain:
		for {
			select {
			case x := <-e.scanCh:
				if x == 0 {
					all = true
				}
				ids[x] = true
			default:
				break drain
			}
		}
		roots, err := e.st.Roots()
		if err != nil {
			log.Printf("scan: %v", err)
			continue
		}
		e.setScanning(true)
		for _, r := range roots {
			if ctx.Err() != nil {
				break
			}
			if r.Paused || (!all && !ids[r.ID]) {
				continue
			}
			e.scanRoot(r)
		}
		e.setScanning(false)
	}
}

func (e *Engine) setScanning(v bool) {
	e.mu.Lock()
	e.scanning = v
	e.mu.Unlock()
}

// ---- folder watching ----

func (e *Engine) watch(dir string, rootID int64) {
	if e.watcher == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.watched[dir]; ok || len(e.watched) >= maxWatches {
		return
	}
	if err := e.watcher.Add(dir); err == nil {
		e.watched[dir] = rootID
	}
}

// UnwatchRoot stops watching a removed root's folders.
func (e *Engine) UnwatchRoot(rootID int64) {
	if e.watcher == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for dir, id := range e.watched {
		if id == rootID {
			_ = e.watcher.Remove(dir)
			delete(e.watched, dir)
		}
	}
}

func (e *Engine) watchLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case err, ok := <-e.watcher.Errors:
			if !ok {
				return
			}
			log.Printf("watcher: %v (a full rescan will catch up)", err)
			e.RequestScan(0)
		case ev, ok := <-e.watcher.Events:
			if !ok {
				return
			}
			if meta.SkipFile(filepath.Base(ev.Name)) {
				continue
			}
			e.mu.Lock()
			rootID, ok := e.watched[filepath.Dir(ev.Name)]
			if ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
				delete(e.watched, ev.Name)
			}
			if ok {
				// Debounce: rescan the root a few seconds after the last change.
				if t := e.pending[rootID]; t != nil {
					t.Stop()
				}
				id := rootID
				e.pending[rootID] = time.AfterFunc(3*time.Second, func() { e.RequestScan(id) })
			}
			e.mu.Unlock()
		}
	}
}

// ---- uploading ----

func (e *Engine) workerLoop(ctx context.Context) {
	for ctx.Err() == nil {
		if err := e.tg.WaitReady(ctx); err != nil {
			return
		}
		if e.Blocked() != "" {
			select {
			case <-ctx.Done():
				return
			case <-e.wake: // conditions changed
			case <-time.After(30 * time.Second): // settings may have changed
			}
			continue
		}
		if d := time.Until(e.cooldownEnd()); d > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(d):
			}
			continue
		}
		it, ok, err := e.st.ClaimNext(quietFile, quietProject)
		if err != nil {
			log.Printf("queue: %v", err)
		}
		if !ok {
			select {
			case <-ctx.Done():
				return
			case <-e.wake:
			case <-time.After(5 * time.Second):
			}
			continue
		}
		e.process(ctx, it)
	}
}

func (e *Engine) setProgress(p Progress) {
	p.updated = time.Now()
	e.mu.Lock()
	e.progress[p.ItemID] = p
	e.mu.Unlock()
}

func (e *Engine) lastActivity(id int64) time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.progress[id].updated
}

func (e *Engine) cooldownEnd() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cooldown
}

// startCooldown pauses all uploads until Telegram's limit has passed.
func (e *Engine) startCooldown(d time.Duration) time.Time {
	end := time.Now().Add(d + 5*time.Second)
	e.mu.Lock()
	defer e.mu.Unlock()
	if end.After(e.cooldown) {
		e.cooldown = end
		log.Printf("Telegram rate limit: pausing uploads until %s", end.Format("15:04:05"))
	}
	return e.cooldown
}

func (e *Engine) clearProgress(id int64) {
	e.mu.Lock()
	delete(e.progress, id)
	e.mu.Unlock()
}

// outcome is how processing an item ended.
type outcome struct {
	state string
	note  string
	retry time.Duration // for failures; 0 = use backoff
	count bool          // whether this failure counts toward the backoff
	wait  time.Time     // rate-limited: stay pending until then (not an error)
}

func (e *Engine) process(ctx context.Context, it store.Item) {
	started := time.Now().Unix()
	defer e.clearProgress(it.ID)
	e.setProgress(Progress{ItemID: it.ID, CloudPath: it.CloudPath, Phase: "checking"})

	root, err := e.st.Root(it.RootID)
	var out outcome
	if err != nil {
		out = outcome{state: store.StateFailed, note: "its backup folder was removed", count: true}
	} else {
		abs := filepath.Join(root.Path, filepath.FromSlash(it.Rel))
		ictx, cancel := context.WithCancelCause(ctx)
		stop := e.watchStall(ictx, it.ID, cancel)
		if it.Kind == store.KindProject {
			out = e.uploadProject(ictx, it, abs)
		} else {
			out = e.uploadFile(ictx, it, abs)
		}
		stop()
		cancel(nil)
		if ctx.Err() != nil {
			// Shutting down: leave it for next start.
			_ = e.st.FinishItem(it.ID, store.StatePending, "", it.Attempts, 0, started)
			return
		}
	}

	attempts, next := it.Attempts, int64(0)
	if !out.wait.IsZero() {
		out.state, next = store.StatePending, out.wait.Unix()
	} else if out.state == store.StateFailed {
		if out.count {
			attempts++
		}
		d := out.retry
		if d == 0 {
			d = backoff(attempts)
		}
		next = time.Now().Add(d).Unix()
		log.Printf("upload %s: %s (retry in %s)", it.CloudPath, out.note, d.Round(time.Second))
	} else {
		attempts = 0
	}
	if err := e.st.FinishItem(it.ID, out.state, out.note, attempts, next, started); err != nil {
		log.Printf("queue: %v", err)
	}
}

// errStalled cancels an upload that has made no progress for a long time.
var errStalled = errors.New("no progress for 10 minutes")

// watchStall cancels the item's work if it stops making progress. Slow but
// moving uploads are never cut off.
func (e *Engine) watchStall(ctx context.Context, id int64, cancel context.CancelCauseFunc) (stop func()) {
	done, exited := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(exited)
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				// Time spent waiting out a rate limit is not a stall.
				if time.Now().Before(e.cooldownEnd()) {
					e.setProgress(e.progressOf(id))
					continue
				}
				if time.Since(e.lastActivity(id)) > 10*time.Minute {
					cancel(errStalled)
					return
				}
			}
		}
	}()
	return func() { close(done); <-exited }
}

func (e *Engine) progressOf(id int64) Progress {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.progress[id]
}

func backoff(attempts int) time.Duration {
	d := 30 * time.Second
	for i := 1; i < attempts && d < time.Hour; i++ {
		d *= 2
	}
	return min(d, time.Hour)
}

// failure converts an error into an outcome with a plain-language note.
func (e *Engine) failure(ctx context.Context, err error) outcome {
	if d, ok := tgerr.AsFloodWait(err); ok {
		return outcome{wait: e.startCooldown(d)}
	}
	if errors.Is(context.Cause(ctx), errStalled) {
		return outcome{state: store.StateFailed, note: "the upload stopped making progress (connection problem?); will try again", count: true}
	}
	if isConnErr(err) {
		return outcome{state: store.StateFailed, retry: 30 * time.Second, note: "the connection to Telegram dropped; will try again"}
	}
	if isInUse(err) {
		return outcome{state: store.StateFailed, note: "the file is open in another program; will try again", count: true}
	}
	if strings.Contains(err.Error(), "not connected to Telegram") {
		return outcome{state: store.StateFailed, retry: 30 * time.Second, note: "waiting for the Telegram connection"}
	}
	return outcome{state: store.StateFailed, note: tgclient.Friendly(err), count: true}
}

func isConnErr(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"connection was aborted", "connection reset", "broken pipe", "i/o timeout", "eof", "connection refused", "no such host"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

func isInUse(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "being used by another process") || strings.Contains(msg, "sharing violation") ||
		strings.Contains(msg, "locked a portion of the file")
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (e *Engine) uploadFile(ctx context.Context, it store.Item, abs string) outcome {
	fi, err := os.Stat(abs)
	if errors.Is(err, os.ErrNotExist) {
		_ = e.st.MarkGone([]int64{it.ID})
		return outcome{state: store.StateSynced}
	}
	if err != nil {
		return e.failure(ctx, err)
	}
	if fi.Size() == 0 {
		return outcome{state: store.StateSkipped, note: "empty file; nothing to back up"}
	}
	if splitAt, _ := e.splitLimits(); fi.Size() > splitAt {
		return e.uploadSplit(ctx, it, abs, fi, store.Version{CloudPath: it.CloudPath, Kind: store.KindFile,
			Mtime: fi.ModTime().Unix()}, path.Base(it.CloudPath), "")
	}
	e.setProgress(Progress{ItemID: it.ID, CloudPath: it.CloudPath, Phase: "reading", Total: fi.Size()})
	hash, err := hashFile(abs)
	if err != nil {
		return e.failure(ctx, err)
	}
	if latest, ok, _ := e.st.LatestVersion(it.CloudPath); ok && latest.Hash == hash {
		return outcome{state: store.StateSynced} // content already in Telegram
	}
	caption, err := meta.Caption(meta.Info{Kind: "f", Path: it.CloudPath, Size: fi.Size(), Mt: fi.ModTime().Unix(), Hash: hash, Dev: e.Device()})
	if err != nil {
		return outcome{state: store.StateSkipped, note: err.Error()}
	}
	msgID, err := e.send(ctx, it, abs, path.Base(it.CloudPath), fi.Size(), hash, caption)
	if err != nil {
		return e.failure(ctx, err)
	}
	// If the file changed while it was uploading, the copy may be inconsistent.
	if after, err := os.Stat(abs); !uploadStillValid(fi, after, err) {
		_ = e.tg.DeleteMessages(ctx, []int{msgID})
		return outcome{state: store.StateFailed, retry: quietFile, note: "the file changed while uploading; will upload the new version"}
	}
	return e.recordVersion(ctx, store.Version{MsgID: msgID, CloudPath: it.CloudPath, Kind: store.KindFile,
		Size: fi.Size(), Mtime: fi.ModTime().Unix(), Hash: hash, UploadedAt: time.Now().Unix()}, "")
}

// uploadStillValid reports whether a finished upload matches the file. A file
// deleted right after its upload is kept: Windows can't delete it while
// tgvault has it open, so the upload was complete, and it may be the only copy.
func uploadStillValid(before, after os.FileInfo, statErr error) bool {
	if errors.Is(statErr, os.ErrNotExist) {
		return true
	}
	return statErr == nil && after.Size() == before.Size() && after.ModTime().Equal(before.ModTime())
}

func (e *Engine) send(ctx context.Context, it store.Item, abs, name string, size int64, hash, caption string) (int, error) {
	return e.sendRange(ctx, it, it.CloudPath, abs, name, 0, size, hash, caption, func(done int64) {
		e.setProgress(Progress{ItemID: it.ID, CloudPath: it.CloudPath, Phase: "uploading", Done: done, Total: size})
	})
}

// sendRange posts size bytes of abs, starting at offset, as one message.
// Progress within a big upload is saved under key, so an interrupted upload
// continues where it stopped.
func (e *Engine) sendRange(ctx context.Context, it store.Item, key, abs, name string, offset, size int64, hash, caption string,
	progress func(done int64)) (int, error) {
	var resume *store.Resume
	if r, ok := e.st.GetResume(key); ok {
		resume = &r
	}
	req := tgclient.UploadRequest{
		Path: abs, Name: name, Size: size, Offset: offset, Caption: caption, Resume: resume, Hash: hash,
		OnResume: func(r store.Resume) { _ = e.st.SaveResume(key, r) },
		Progress: func(done, _ int64) { progress(done) },
	}
	msgID, err := e.tg.Upload(ctx, req)
	if errors.Is(err, tgclient.ErrPartsExpired) && resume != nil {
		// Telegram dropped the earlier pieces; start this message over.
		_ = e.st.ClearResume(key)
		req.Resume = nil
		msgID, err = e.tg.Upload(ctx, req)
	}
	if err != nil {
		return 0, err
	}
	_ = e.st.ClearResume(key)
	return msgID, nil
}

// recordVersion saves a new version and deletes versions beyond the newest three.
func (e *Engine) recordVersion(ctx context.Context, v store.Version, note string) outcome {
	if err := e.st.AddVersion(v); err != nil {
		return outcome{state: store.StateFailed, note: "database error: " + err.Error(), count: true}
	}
	e.prune(ctx, v.CloudPath)
	return outcome{state: store.StateSynced, note: note}
}

func (e *Engine) prune(ctx context.Context, cloudPath string) {
	vs, err := e.st.Versions(cloudPath)
	if err != nil || len(vs) <= keepVersions {
		return
	}
	var old []int
	for _, v := range vs[keepVersions:] {
		old = append(old, v.MsgID)
	}
	msgs, err := e.messageIDsOf(vs[keepVersions:])
	if err != nil {
		return
	}
	if err := e.tg.DeleteMessages(ctx, msgs); err != nil {
		log.Printf("prune %s: %v (will retry later)", cloudPath, err)
		return
	}
	_ = e.st.DeleteVersions(old)
}

func (e *Engine) uploadProject(ctx context.Context, it store.Item, abs string) outcome {
	if fi, err := os.Stat(abs); errors.Is(err, os.ErrNotExist) || (err == nil && !fi.IsDir()) {
		_ = e.st.MarkGone([]int64{it.ID})
		return outcome{state: store.StateSynced}
	}
	files := projectFiles(abs)
	fp, _, mtime := fingerprintOf(files)
	if latest, ok, _ := e.st.LatestVersion(it.CloudPath); ok && latest.Fingerprint == fp {
		return outcome{state: store.StateSynced}
	}
	e.setProgress(Progress{ItemID: it.ID, CloudPath: it.CloudPath, Phase: "zipping"})
	tmp := filepath.Join(e.tmpDir, fmt.Sprintf("project-%d.zip", it.ID))
	defer os.Remove(tmp)
	skipped, err := writeProjectZip(ctx, tmp, files)
	if err != nil {
		return e.failure(ctx, err)
	}
	if after, _, _ := projectFingerprint(abs); after != fp {
		return outcome{state: store.StateFailed, retry: quietProject, note: "the project changed while zipping; will try again"}
	}
	fi, err := os.Stat(tmp)
	if err != nil {
		return e.failure(ctx, err)
	}
	note := ""
	if skipped > 0 {
		note = fmt.Sprintf("%d file(s) were open in another program and left out of this backup", skipped)
	}
	if splitAt, _ := e.splitLimits(); fi.Size() > splitAt {
		return e.uploadSplit(ctx, it, tmp, fi, store.Version{CloudPath: it.CloudPath, Kind: store.KindProject,
			Mtime: mtime, Fingerprint: fp}, path.Base(it.CloudPath)+".zip", note)
	}
	hash, err := hashFile(tmp)
	if err != nil {
		return e.failure(ctx, err)
	}
	caption, err := meta.Caption(meta.Info{Kind: "p", Path: it.CloudPath, Size: fi.Size(), Mt: mtime, Hash: hash, FP: fp, Dev: e.Device()})
	if err != nil {
		return outcome{state: store.StateSkipped, note: err.Error()}
	}
	msgID, err := e.send(ctx, it, tmp, path.Base(it.CloudPath)+".zip", fi.Size(), hash, caption)
	if err != nil {
		return e.failure(ctx, err)
	}
	return e.recordVersion(ctx, store.Version{MsgID: msgID, CloudPath: it.CloudPath, Kind: store.KindProject,
		Size: fi.Size(), Mtime: mtime, Hash: hash, Fingerprint: fp, UploadedAt: time.Now().Unix()}, note)
}

// writeProjectZip packs files into a ZIP. Files that can't be opened are
// counted and left out rather than blocking the whole project.
func writeProjectZip(ctx context.Context, dst string, files []projectFile) (skipped int, err error) {
	out, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		src, err := os.Open(f.abs)
		if err != nil {
			skipped++
			continue
		}
		hdr := &zip.FileHeader{Name: f.rel, Method: zip.Deflate, Modified: time.Unix(f.mtime, 0)}
		w, err := zw.CreateHeader(hdr)
		if err == nil {
			_, err = io.Copy(w, src)
		}
		src.Close()
		if err != nil {
			return 0, err
		}
	}
	if err := zw.Close(); err != nil {
		return 0, err
	}
	return skipped, out.Close()
}

// ---- status ----

// Status summarises the engine for the UI.
type Status struct {
	Counts    map[string]int `json:"counts"`
	Uploading []Progress     `json:"uploading"`
	Scanning  bool           `json:"scanning"`
	Tasks     []TaskView     `json:"tasks"`
	Reconcile Reconcile      `json:"reconcile"`
	// CooldownUntil is set while Telegram's rate limit has paused uploads.
	CooldownUntil int64 `json:"cooldownUntil,omitempty"`
	// Blocked explains why the phone's settings are holding uploads back.
	Blocked string `json:"blocked,omitempty"`
	Device  string `json:"device"`
}

// Status returns current activity.
func (e *Engine) Status() Status {
	counts, _ := e.st.Counts()
	blocked := e.Blocked()
	e.mu.Lock()
	defer e.mu.Unlock()
	s := Status{Counts: counts, Scanning: e.scanning, Reconcile: e.reconcile, Blocked: blocked, Device: e.Device()}
	if time.Now().Before(e.cooldown) {
		s.CooldownUntil = e.cooldown.Unix()
	}
	for _, p := range e.progress {
		s.Uploading = append(s.Uploading, p)
	}
	for _, t := range e.tasks {
		s.Tasks = append(s.Tasks, t.view())
	}
	return s
}
