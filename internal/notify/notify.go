// Package notify tells the user, through Windows notifications, about the few
// things that need them. It stays quiet otherwise.
package notify

import (
	"context"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/tgclient"
)

// SettingKey turns notifications off when set to "off".
const SettingKey = "notify"

const (
	checkEvery     = 5 * time.Minute
	loggedOutAfter = 5 * time.Minute  // ignore brief reconnects
	failingFor     = 24 * time.Hour   // a file must fail this long before we bother you
	missingFor     = 30 * time.Minute // a backup folder must be missing this long
	repeatEvery    = 24 * time.Hour   // remind at most daily about the same thing
)

// Sender shows a notification. It is replaceable in tests.
type Sender func(title, body, url string) error

// Watcher checks for problems and notifies about each one once.
type Watcher struct {
	st   *store.Store
	tg   *tgclient.Client
	url  string
	send Sender
	now  func() time.Time

	notReadySince time.Time
	missingSince  map[int64]time.Time
}

// New creates a watcher. url is opened when a notification is clicked.
func New(st *store.Store, tg *tgclient.Client, url string) *Watcher {
	return &Watcher{st: st, tg: tg, url: url, send: send, now: time.Now, missingSince: map[int64]time.Time{}}
}

// Enabled reports whether notifications are on (the default).
func Enabled(st *store.Store) bool { return st.Get(SettingKey) != "off" }

// Run checks periodically until ctx ends.
func (w *Watcher) Run(ctx context.Context) {
	t := time.NewTicker(checkEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Check()
		}
	}
}

// Test sends a sample notification.
func (w *Watcher) Test() error {
	return w.send("tgvault notifications work", "You'll only hear from tgvault when something needs you.", w.url)
}

// Check looks for problems once.
func (w *Watcher) Check() {
	if !Enabled(w.st) {
		return
	}
	w.checkLogin()
	w.checkFailures()
	w.checkFolders()
	w.checkProjects()
}

// notifyOnce sends a notification unless the same key was sent within repeatEvery.
func (w *Watcher) notifyOnce(key, title, body string) {
	k := "notified:" + key
	if last, _ := strconv.ParseInt(w.st.Get(k), 10, 64); last > 0 && w.now().Sub(time.Unix(last, 0)) < repeatEvery {
		return
	}
	if err := w.send(title, body, w.url); err != nil {
		log.Printf("notify: %v", err)
		return
	}
	log.Printf("notify: %s", title)
	_ = w.st.Set(k, strconv.FormatInt(w.now().Unix(), 10))
}

func (w *Watcher) resolved(key string) { _ = w.st.Delete("notified:" + key) }

func (w *Watcher) checkLogin() {
	st := w.tg.Info().Status
	loggedOut := st == tgclient.StatusNeedPhone || st == tgclient.StatusNeedCode ||
		st == tgclient.StatusNeedPassword || st == tgclient.StatusQR || st == tgclient.StatusNeedAPI
	if !loggedOut || w.st.Get("account_id") == "" { // never logged in yet: nothing to warn about
		w.notReadySince = time.Time{}
		if st == tgclient.StatusReady {
			w.resolved("logout")
		}
		return
	}
	if w.notReadySince.IsZero() {
		w.notReadySince = w.now()
	}
	if w.now().Sub(w.notReadySince) >= loggedOutAfter {
		w.notifyOnce("logout", "tgvault is logged out of Telegram",
			"Backups are paused. Open tgvault and log in again (the QR code is quickest).")
	}
}

func (w *Watcher) checkFailures() {
	n, sample, err := w.st.StaleFailures(w.now().Add(-failingFor).Unix())
	if err != nil || n == 0 {
		if err == nil {
			w.resolved("failing")
		}
		return
	}
	body := fmt.Sprintf("%s keeps failing to back up", sample)
	if n > 1 {
		body = fmt.Sprintf("%d files keep failing to back up, e.g. %s", n, sample)
	}
	w.notifyOnce("failing", "Some files aren't backed up", body+". Open Activity to see why.")
}

func (w *Watcher) checkFolders() {
	roots, err := w.st.Roots()
	if err != nil {
		return
	}
	for _, r := range roots {
		key := "missing:" + strconv.FormatInt(r.ID, 10)
		if r.Paused || !strings.HasPrefix(r.ScanError, "folder not found") {
			delete(w.missingSince, r.ID)
			w.resolved(key)
			continue
		}
		if _, ok := w.missingSince[r.ID]; !ok {
			w.missingSince[r.ID] = w.now()
		}
		if w.now().Sub(w.missingSince[r.ID]) >= missingFor {
			w.notifyOnce(key, "A backup folder is missing",
				r.Path+" can't be found, so it isn't being backed up. Reconnect the drive, or stop backing it up in tgvault.")
		}
	}
}

func (w *Watcher) checkProjects() {
	ps, err := w.st.Projects(store.ProjectSuggested)
	if err != nil || len(ps) == 0 {
		return
	}
	// One notification per new batch: key on the newest suggestion.
	newest := ps[0]
	for _, p := range ps {
		if p.ID > newest.ID {
			newest = p
		}
	}
	body := "A folder looks like a code project. It won't be backed up until you choose how."
	if len(ps) > 1 {
		body = fmt.Sprintf("%d folders look like code projects. They won't be backed up until you choose how.", len(ps))
	}
	w.notifyOnce("project:"+strconv.FormatInt(newest.ID, 10), "tgvault needs a quick decision", body)
}
