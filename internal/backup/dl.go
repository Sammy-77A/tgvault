package backup

import (
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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Sammy-77A/tgvault/internal/media"
	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/tgclient"
)

// The movie app's download manager. What is wanted is a row in a table, so it
// survives the app being killed or the phone restarting; a download writes to
// a ".part" file and carries on from the last whole block after any
// interruption; transient trouble (Telegram timeouts, rate limits, a dropped
// connection, switching networks) is waited out with growing pauses, and only
// then does a download show as failed, with a Retry. A finished file is
// checked against its hash before it counts.

const (
	dlParallel    = 2
	dlMaxAttempts = 8
	dlBackoffMax  = 5 * time.Minute
)

// Timings tests shorten.
var (
	dlBackoffUnit = 2 * time.Second // the first wait; it doubles each try
	dlOfflineWait = 5 * time.Second // how long to wait for a connection to come back
	dlPoll        = 2 * time.Second // how often the queue is looked at
)

// rangeDownloader is a Telegram connection that can start a download part-way.
type rangeDownloader interface {
	DownloadFrom(ctx context.Context, msgID int, offset int64, w io.Writer) error
}

// DLItem is what the page and the notification show about one download.
type DLItem struct {
	Path     string  `json:"path"`
	Name     string  `json:"name"`
	Title    string  `json:"title"`
	State    string  `json:"state"` // queued, running, paused, failed, done
	Done     int64   `json:"done"`
	Size     int64   `json:"size"`
	Speed    float64 `json:"speed"` // bytes per second
	ETA      int     `json:"eta"`   // seconds, 0 when unknown
	Error    string  `json:"error,omitempty"`
	Attempts int     `json:"attempts,omitempty"`
	Waiting  string  `json:"waiting,omitempty"` // why a queued download isn't moving
}

type dlLive struct {
	cancel   context.CancelFunc
	reason   string // "pause" or "cancel" when stopped on purpose
	lastAt   time.Time
	lastDone int64
	speed    float64
}

type dlState struct {
	mu      sync.Mutex
	running bool
	live    map[string]*dlLive
	hook    func(event string, it DLItem)
	wake    chan struct{}
	waiting map[string]string
}

// SetDownloadHook is told when a download finishes or fails ("done",
// "failed"), so the phone can notify.
func (e *Engine) SetDownloadHook(f func(event string, it DLItem)) {
	e.dl.mu.Lock()
	e.dl.hook = f
	e.dl.mu.Unlock()
}

func (e *Engine) dlTitle(p string) string {
	info := media.Parse(path.Base(p))
	name := info.Title
	if name == "" {
		name = path.Base(p)
	}
	rel := strings.TrimPrefix(p, e.MediaRoot()+"/")
	parts := strings.Split(rel, "/")
	if len(parts) >= 2 && parts[0] == "Shows" {
		show := media.Parse(parts[1]).Title
		if show == "" {
			show = parts[1]
		}
		if info.HasSeason && len(info.Episodes) > 0 {
			return fmt.Sprintf("%s S%02dE%02d", show, info.Season, info.Episodes[0])
		}
		return show
	}
	if info.Year > 0 {
		return fmt.Sprintf("%s (%d)", name, info.Year)
	}
	return name
}

func (e *Engine) dlItem(d store.MBDownload) DLItem {
	it := DLItem{Path: d.Path, Name: path.Base(d.Path), Title: e.dlTitle(d.Path), State: d.State, Done: d.Done, Size: d.Size,
		Error: d.Err, Attempts: d.Attempts}
	e.dl.mu.Lock()
	if l := e.dl.live[d.Path]; l != nil && d.State == "running" {
		it.Speed = l.speed
		if l.speed > 1 && d.Size > d.Done {
			it.ETA = int(float64(d.Size-d.Done) / l.speed)
		}
	}
	it.Waiting = e.dl.waiting[d.Path]
	e.dl.mu.Unlock()
	return it
}

// Downloads lists the queue, with live progress.
func (e *Engine) Downloads() []DLItem {
	rows, _ := e.st.DLList()
	out := make([]DLItem, 0, len(rows))
	for _, d := range rows {
		it := e.dlItem(d)
		// the byte count of a running download is the file as it stands
		if d.State == "running" {
			if fi, err := os.Stat(e.MediaLocal(d.Path) + ".part"); err == nil && fi.Size() > it.Done {
				it.Done = fi.Size()
			}
		}
		out = append(out, it)
	}
	return out
}

// QueueDownloads asks for library videos (with the subtitles beside them) to
// be downloaded for offline watching. It returns how many files were queued.
func (e *Engine) QueueDownloads(ctx context.Context, paths []string) (int, error) {
	root := e.MediaRoot()
	n := 0
	seen := map[string]bool{}
	add := func(v store.Version) {
		if seen[v.CloudPath] || e.haveLocal(v) {
			return
		}
		seen[v.CloudPath] = true
		if d, ok := e.st.DLGet(v.CloudPath); ok && (d.State == "queued" || d.State == "running") {
			return // already on its way
		}
		_ = e.st.DLUpsert(store.MBDownload{Path: v.CloudPath, MsgID: v.MsgID, Size: v.Size, State: "queued"})
		n++
	}
	for _, p := range paths {
		if !e.inMediaRoot(p) || p == root {
			return 0, errors.New("that isn't in the library")
		}
		vs, err := e.st.LatestUnder(p)
		if err != nil {
			return 0, err
		}
		for _, v := range vs {
			add(v)
			if media.IsVideo(path.Ext(v.CloudPath)) {
				stem := strings.TrimSuffix(path.Base(v.CloudPath), path.Ext(v.CloudPath))
				sibs, _ := e.st.LatestUnder(path.Dir(v.CloudPath))
				for _, s := range sibs {
					if path.Dir(s.CloudPath) == path.Dir(v.CloudPath) && media.IsSubtitle(path.Ext(s.CloudPath)) &&
						strings.HasPrefix(strings.TrimSuffix(path.Base(s.CloudPath), path.Ext(s.CloudPath)), stem) {
						add(s)
					}
				}
			}
		}
	}
	if n == 0 {
		return 0, errors.New("already on this phone")
	}
	e.dlKick()
	return n, nil
}

// DownloadControl pauses, resumes, cancels or retries one download ("" for all
// of them, for pause and resume).
func (e *Engine) DownloadControl(action, p string) error {
	rows, _ := e.st.DLList()
	pick := func(f func(d store.MBDownload)) {
		for _, d := range rows {
			if p == "" || d.Path == p {
				f(d)
			}
		}
	}
	switch action {
	case "pause":
		pick(func(d store.MBDownload) {
			if d.State == "queued" || d.State == "running" {
				e.dlStop(d.Path, "pause")
				_ = e.st.DLSet(d.Path, "paused", d.Done, "", d.Attempts, 0)
			}
		})
	case "resume", "retry":
		pick(func(d store.MBDownload) {
			if d.State == "paused" || d.State == "failed" {
				attempts := d.Attempts
				if action == "retry" {
					attempts = 0
				}
				_ = e.st.DLSet(d.Path, "queued", d.Done, "", attempts, 0)
			}
		})
		e.dlKick()
	case "cancel":
		if p == "" {
			return errors.New("say which download")
		}
		e.dlStop(p, "cancel")
		_ = os.Remove(e.MediaLocal(p) + ".part")
		_ = e.st.DLDelete(p)
	case "clear":
		pick(func(d store.MBDownload) {
			if d.State == "done" || d.State == "failed" {
				_ = e.st.DLDelete(d.Path)
			}
		})
	default:
		return errors.New("unknown action")
	}
	return nil
}

func (e *Engine) dlStop(p, reason string) {
	e.dl.mu.Lock()
	l := e.dl.live[p]
	if l != nil {
		l.reason = reason
	}
	e.dl.mu.Unlock()
	if l != nil {
		l.cancel()
	}
}

// dlKick makes sure the manager is working through the queue.
func (e *Engine) dlKick() {
	e.dl.mu.Lock()
	if e.dl.live == nil {
		e.dl.live = map[string]*dlLive{}
		e.dl.wake = make(chan struct{}, 1)
		e.dl.waiting = map[string]string{}
	}
	start := !e.dl.running
	e.dl.running = true
	e.dl.mu.Unlock()
	select {
	case e.dl.wake <- struct{}{}:
	default:
	}
	if start {
		go e.dlManager()
	}
}

// ResumeDownloads picks the queue up again after a restart.
func (e *Engine) ResumeDownloads() {
	_ = e.st.DLResetRunning()
	rows, _ := e.st.DLList()
	for _, d := range rows {
		if d.State == "queued" {
			e.dlKick()
			return
		}
	}
}

func (e *Engine) dlManager() {
	var wg sync.WaitGroup
	for {
		rows, _ := e.st.DLList()
		now := time.Now().Unix()
		running := 0
		var next *store.MBDownload
		soonest := int64(0)
		pending := false
		for i := range rows {
			d := rows[i]
			switch d.State {
			case "running":
				running++
			case "queued":
				pending = true
				if d.Next > now {
					if soonest == 0 || d.Next < soonest {
						soonest = d.Next
					}
					continue
				}
				e.dl.mu.Lock()
				busy := e.dl.live[d.Path] != nil
				e.dl.mu.Unlock()
				if next == nil && !busy {
					next = &rows[i]
				}
			}
		}
		if running < dlParallel && next != nil {
			d := *next
			_ = e.st.DLSet(d.Path, "running", d.Done, "", d.Attempts, 0)
			wg.Add(1)
			go func() {
				defer wg.Done()
				e.dlRun(d)
				select {
				case e.dl.wake <- struct{}{}:
				default:
				}
			}()
			continue
		}
		if running == 0 && !pending {
			e.dl.mu.Lock()
			e.dl.running = false
			e.dl.mu.Unlock()
			// something may have been queued while we decided to stop
			again := false
			rows, _ = e.st.DLList()
			for _, d := range rows {
				if d.State == "queued" {
					again = true
				}
			}
			if again {
				e.dl.mu.Lock()
				if !e.dl.running {
					e.dl.running = true
					e.dl.mu.Unlock()
					continue
				}
				e.dl.mu.Unlock()
			}
			return
		}
		wait := dlPoll
		if soonest > 0 {
			if d := time.Duration(soonest-now) * time.Second; d > 0 && d < wait {
				wait = d
			}
		}
		select {
		case <-e.dl.wake:
		case <-time.After(wait):
		}
	}
}

func dlBackoff(attempts int) time.Duration {
	d := time.Duration(1<<min(attempts, 8)) * dlBackoffUnit
	return min(d, dlBackoffMax)
}

func (e *Engine) dlFail(d store.MBDownload, done int64, err error, countAttempt bool) {
	attempts := d.Attempts
	if countAttempt {
		attempts++
	}
	msg := tgclient.Friendly(err)
	if attempts >= dlMaxAttempts {
		_ = e.st.DLSet(d.Path, "failed", done, msg, attempts, 0)
		log.Printf("download %s: giving up after %d tries: %v", d.Path, attempts, err)
		e.dlEvent("failed", d.Path)
		return
	}
	wait := dlBackoff(attempts)
	if !countAttempt {
		wait = dlOfflineWait
	}
	log.Printf("download %s: %v; trying again in %s (try %d of %d)", d.Path, err, wait.Round(time.Second), attempts, dlMaxAttempts)
	_ = e.st.DLSet(d.Path, "queued", done, msg, attempts, time.Now().Add(wait).Unix())
}

func (e *Engine) dlEvent(ev, p string) {
	e.dl.mu.Lock()
	hook := e.dl.hook
	e.dl.mu.Unlock()
	if hook == nil {
		return
	}
	if d, ok := e.st.DLGet(p); ok {
		hook(ev, e.dlItem(d))
	}
}

// dlRun does one attempt at one download.
func (e *Engine) dlRun(d store.MBDownload) {
	ctx, cancel := context.WithCancel(context.Background())
	live := &dlLive{cancel: cancel, lastAt: time.Now()}
	e.dl.mu.Lock()
	e.dl.live[d.Path] = live
	delete(e.dl.waiting, d.Path)
	e.dl.mu.Unlock()
	defer func() {
		cancel()
		e.dl.mu.Lock()
		delete(e.dl.live, d.Path)
		e.dl.mu.Unlock()
	}()

	vs, _ := e.st.LatestUnder(d.Path)
	var v store.Version
	for _, x := range vs {
		if x.CloudPath == d.Path {
			v = x
		}
	}
	if v.MsgID == 0 {
		_ = e.st.DLSet(d.Path, "failed", 0, "this file is no longer in the library", d.Attempts, 0)
		e.dlEvent("failed", d.Path)
		return
	}
	final := e.MediaLocal(d.Path)
	if e.haveLocal(v) {
		_ = e.st.DLSet(d.Path, "done", v.Size, "", 0, 0)
		return
	}
	// Telegram has to be reachable; waiting for it isn't a failed try.
	wctx, wcancel := context.WithTimeout(ctx, 20*time.Second)
	werr := e.tg.WaitReady(wctx)
	wcancel()
	if werr != nil {
		if ctx.Err() == nil {
			e.dl.mu.Lock()
			e.dl.waiting[d.Path] = "Waiting for a connection"
			e.dl.mu.Unlock()
			e.dlFail(d, d.Done, errors.New("no connection yet"), false)
		}
		return
	}
	err := e.dlTransfer(ctx, d, v, final, live)
	if err == nil {
		_ = e.st.DLSet(d.Path, "done", v.Size, "", 0, 0)
		e.dlEvent("done", d.Path)
		e.fileChanged(final)
		return
	}
	live2 := func() string {
		e.dl.mu.Lock()
		defer e.dl.mu.Unlock()
		return live.reason
	}()
	switch {
	case live2 == "pause":
		return // the row was set to paused by the caller; the part file stays
	case live2 == "cancel":
		return
	case errors.Is(err, errNoSpace):
		_ = e.st.DLSet(d.Path, "failed", d.Done, err.Error(), d.Attempts, 0)
		e.dlEvent("failed", d.Path)
	case errors.Is(err, tgclient.ErrGone):
		_ = e.st.DLSet(d.Path, "failed", d.Done, "this file is no longer in the Telegram channel", dlMaxAttempts, 0)
		e.dlEvent("failed", d.Path)
	default:
		var done int64
		if fi, serr := os.Stat(final + ".part"); serr == nil {
			done = fi.Size() / tgclient.BlockSize * tgclient.BlockSize
		}
		e.dlFail(d, done, err, true)
	}
}

var errNoSpace = errors.New("not enough free space")

type progressWriter struct {
	w    io.Writer
	e    *Engine
	live *dlLive
	done int64
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.done += int64(n)
	now := time.Now()
	p.e.dl.mu.Lock()
	if dt := now.Sub(p.live.lastAt); dt >= time.Second {
		inst := float64(p.done-p.live.lastDone) / dt.Seconds()
		if p.live.speed == 0 {
			p.live.speed = inst
		} else {
			p.live.speed = p.live.speed*0.6 + inst*0.4
		}
		p.live.lastAt, p.live.lastDone = now, p.done
	}
	p.e.dl.mu.Unlock()
	return n, err
}

// dlTransfer fetches the file into final+".part", carrying on from a partial
// one, checks it, and moves it into place.
func (e *Engine) dlTransfer(ctx context.Context, d store.MBDownload, v store.Version, final string, live *dlLive) error {
	dir := filepath.Dir(final)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	part := final + ".part"
	if v.Parts > 0 {
		// A file over 2 GB is stored in several messages and is fetched whole.
		if err := checkFreeSpace(dir, v.Size); err != nil {
			return fmt.Errorf("%w: %v", errNoSpace, err)
		}
		tmp, err := e.downloadVerified(ctx, v, dir)
		if err != nil {
			return err
		}
		return os.Rename(tmp, final)
	}
	rd, canResume := e.tg.(rangeDownloader)
	var off int64
	if fi, err := os.Stat(part); err == nil && canResume && fi.Size() <= v.Size {
		off = fi.Size() / tgclient.BlockSize * tgclient.BlockSize
	}
	if err := checkFreeSpace(dir, v.Size-off); err != nil {
		return fmt.Errorf("%w: %v", errNoSpace, err)
	}
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if err := f.Truncate(off); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		f.Close()
		return err
	}
	live.lastDone = off
	pw := &progressWriter{w: f, e: e, live: live, done: off}
	if canResume {
		err = rd.DownloadFrom(ctx, v.MsgID, off, pw)
	} else {
		err = e.tg.Download(ctx, v.MsgID, pw)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err // the part stays, so the next try starts from here
	}
	// Whole and unchanged, or it doesn't count.
	if fi, serr := os.Stat(part); serr != nil || fi.Size() != v.Size {
		os.Remove(part)
		return errors.New("the download came out the wrong size")
	}
	sum, err := hashFileCtx(ctx, part)
	if err != nil {
		return err
	}
	if sum != v.Hash {
		os.Remove(part) // start from zero next time: the pieces can't be trusted
		return errors.New("downloaded file failed its integrity check")
	}
	mt := time.Unix(v.Mtime, 0)
	_ = os.Chtimes(part, mt, mt)
	return os.Rename(part, final)
}

func hashFileCtx(ctx context.Context, p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 1<<20)
	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		n, err := f.Read(buf)
		h.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// DownloadsActive reports whether anything is queued or running (the phone
// keeps its download service up while so).
func (e *Engine) DownloadsActive() bool {
	rows, _ := e.st.DLList()
	for _, d := range rows {
		if d.State == "queued" || d.State == "running" {
			return true
		}
	}
	return false
}

// DownloadSummary is the one line the notification shows, and what it counts.
func (e *Engine) DownloadSummary() (title string, done, size int64, speed float64, count int) {
	items := e.Downloads()
	sort.SliceStable(items, func(i, j int) bool { return items[i].State == "running" && items[j].State != "running" })
	for _, it := range items {
		if it.State != "running" && it.State != "queued" {
			continue
		}
		count++
		done += it.Done
		size += it.Size
		speed += it.Speed
		if title == "" {
			title = it.Title
		}
	}
	return
}
