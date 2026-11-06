package backup

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Sammy-77A/tgvault/internal/store"
)

// Sorting without being asked: new files in the library folder are filed as
// soon as they have finished arriving. Success is silent (it goes to the log);
// the owner hears about problems only.

var (
	autoSortEvery = 30 * time.Second
	autoSortFirst = 20 * time.Second
	stableAfter   = 45 * time.Second // a file untouched for this long has finished being written or synced
)

// SetSortHook is told about problems after an automatic sort: files that need
// a decision, duplicates, moves that failed.
func (e *Engine) SetSortHook(f func(problems []string)) { e.sortHook = f }

func (e *Engine) autoSortLoop(ctx context.Context) {
	if e.readOnly {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(autoSortFirst):
	}
	for ctx.Err() == nil {
		e.AutoSortOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(autoSortEvery):
		}
	}
}

// AutoSortOnce files what is ready. A watcher alone would miss files that a
// cloud-sync client drops in without events, so this runs on a timer, and
// each time looks at what has settled.
func (e *Engine) AutoSortOnce(ctx context.Context) {
	if e.readOnly || (!e.HasTMDBKey() && e.movieDB == nil) {
		return
	}
	files, err := e.inbox()
	if err != nil {
		return
	}
	var ready []string
	for _, p := range files {
		if _, waiting := e.st.MediaNote(p); waiting {
			continue // already asked about; the owner will answer or leave it
		}
		if !e.settled(p) {
			continue
		}
		ready = append(ready, p)
	}
	if len(ready) == 0 {
		e.FulfilRequests(ctx)
		return
	}
	if id, err := e.organize(ctx, ready, true); err == nil {
		e.awaitTask(id)
	}
}

// settled reports whether a file has finished arriving: a file on this PC must
// be backed up (so it is no longer changing) and untouched for a while.
func (e *Engine) settled(cloudPath string) bool {
	it, err := e.st.ItemByPath(cloudPath)
	if err != nil || !it.LocalPresent {
		return true // only in Telegram: nothing is being written
	}
	if it.State != store.StateSynced {
		return false
	}
	root, err := e.st.Root(it.RootID)
	if err != nil {
		return false
	}
	fi, err := os.Stat(filepath.Join(root.Path, filepath.FromSlash(it.Rel)))
	if err != nil {
		return false
	}
	return time.Since(fi.ModTime()) >= stableAfter
}

// FulfilRequests answers the requests whose files are all in the library: a
// movie once it is filed, a show once every requested season (or episode) is.
func (e *Engine) FulfilRequests(ctx context.Context) {
	if e.readOnly {
		return
	}
	open := []Request{}
	for _, r := range e.Requests() {
		if r.State == "requested" {
			open = append(open, r)
		}
	}
	if len(open) == 0 {
		return
	}
	filed, _ := e.st.MediaFiledAll()
	have := map[string]bool{}
	for _, f := range filed {
		if vs, _ := e.st.Versions(f.Path); len(vs) == 0 {
			if it, err := e.st.ItemByPath(f.Path); err != nil || !it.LocalPresent {
				_ = e.st.MediaFiledDrop(f.Path)
				continue
			}
		}
		if f.Kind == "movie" {
			have[strconv.Itoa(f.TMDB)+"|movie"] = true
			continue
		}
		for _, n := range strings.Split(f.Episodes, ",") {
			have[strconv.Itoa(f.TMDB)+"|"+strconv.Itoa(f.Season)+"|"+n] = true
		}
	}
	var lk *lookup
	for _, r := range open {
		done := false
		switch r.Kind {
		case "movie":
			done = have[strconv.Itoa(r.TMDB)+"|movie"]
		case "show":
			if len(r.Seasons) == 0 && len(r.Episodes) == 0 {
				continue // "every season" is the owner's call
			}
			done = true
			for _, sn := range r.Seasons {
				if lk == nil {
					db, err := e.tmdbClient()
					if err != nil {
						return
					}
					lk = newLookup(db)
				}
				eps, err := lk.season(ctx, r.TMDB, sn)
				if err != nil || len(eps) == 0 {
					done = false
					break
				}
				for _, ep := range eps {
					if !have[strconv.Itoa(r.TMDB)+"|"+strconv.Itoa(sn)+"|"+strconv.Itoa(ep.Number)] {
						done = false
					}
				}
			}
			for sn, list := range r.Episodes {
				for _, n := range list {
					if !have[strconv.Itoa(r.TMDB)+"|"+sn+"|"+strconv.Itoa(n)] {
						done = false
					}
				}
			}
		}
		if done {
			if err := e.Answer(ctx, r.ID, true); err == nil {
				e.st.MediaLogAdd("request", "", "answered: "+r.Title+" is in the library")
			}
		}
	}
}

// SortLog returns the organizer's log, newest first.
func (e *Engine) SortLog(limit int) []store.MediaLogEntry {
	out, _ := e.st.MediaLogList(limit)
	if out == nil {
		out = []store.MediaLogEntry{}
	}
	return out
}

var _ = path.Base
