package web

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/Sammy-77A/tgvault/internal/backup"
)

// dateRange reads the from and to days (YYYY-MM-DD, local time) of a request
// as Unix seconds; to is exclusive and includes the whole "to" day. 0 means
// no limit.
func dateRange(q url.Values) (from, to int64) {
	day := func(key string) int64 {
		t, err := time.ParseInLocation("2006-01-02", q.Get(key), time.Local)
		if err != nil {
			return 0
		}
		return t.Unix()
	}
	from, to = day("from"), day("to")
	if to > 0 {
		to += 24 * 3600
	}
	return from, to
}

// search finds files by name, type and date under the current folder.
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix, ok := cleanCloudPath(q.Get("path"))
	if !ok {
		fail(w, 400, "bad path")
		return
	}
	from, to := dateRange(q)
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 500
	}
	entries, total, err := s.eng.Search(backup.Query{Prefix: prefix, Text: q.Get("q"), Type: q.Get("type"),
		From: from, To: to, Limit: limit})
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"entries": entries, "total": total})
}

// syncNow looks for files other devices just backed up.
func (s *Server) syncNow(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	// The phone may be reconnecting after being in the background.
	if err := s.tg.WaitReady(ctx); err != nil {
		fail(w, 502, "couldn't reach Telegram; check the internet connection")
		return
	}
	n, err := s.eng.SyncNew(ctx)
	if err != nil {
		fail(w, 502, "couldn't check Telegram: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"added": n})
}

// bulk runs one action on many selected files and folders.
func (s *Server) bulk(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action string   `json:"action"`
		Paths  []string `json:"paths"`
		Dest   string   `json:"dest"` // restore/copy: folder on this device; "" = original place
		To     string   `json:"to"`   // move: folder inside the backup
	}
	if err := readJSON(r, &body); err != nil {
		fail(w, 400, "bad request")
		return
	}
	var paths []string
	for _, p := range body.Paths {
		cp, ok := cleanCloudPath(p)
		if !ok || cp == "" {
			fail(w, 400, "bad path: "+p)
			return
		}
		paths = append(paths, cp)
	}
	if len(paths) == 0 {
		fail(w, 400, "nothing selected")
		return
	}
	ctx := context.Background() // tasks outlive this request
	var id int64
	var err error
	switch body.Action {
	case "restore":
		dest := ""
		if body.Dest != "" {
			if dest, err = s.destFolder(body.Dest); err != nil {
				fail(w, 400, err.Error())
				return
			}
		}
		id, err = s.eng.RestoreMany(ctx, paths, dest, "Restoring", nil)
	case "download": // phone: save into Download/tgvault
		if s.phone == nil {
			fail(w, 400, "on a PC, downloads go through the browser")
			return
		}
		id, err = s.eng.RestoreMany(ctx, paths, s.phone.DownloadDir(), "Downloading", nil)
	case "share": // phone: copy into the app, then open Android's share sheet
		if s.phone == nil || s.phone.Share == nil {
			fail(w, 400, "sharing files is only on the phone; on a PC use Copy Telegram links")
			return
		}
		dir, release, derr := s.newShareDir() // only the latest share is kept
		if derr != nil {
			fail(w, 500, derr.Error())
			return
		}
		share := s.phone.Share
		id, err = s.eng.RestoreMany(ctx, paths, dir, "Preparing to share", func(written []string) {
			defer release()
			if len(written) > 0 {
				share(written)
			}
		})
		if err != nil {
			release()
		}
	case "open": // phone: copy one file into the app, then let another app open it
		if s.phone == nil || s.phone.Open == nil || len(paths) != 1 {
			fail(w, 400, "opening in another app is only on the phone, one file at a time")
			return
		}
		dir, release, derr := s.newShareDir() // shared with sharing; only the latest is kept
		if derr != nil {
			fail(w, 500, derr.Error())
			return
		}
		open := s.phone.Open
		id, err = s.eng.RestoreMany(ctx, paths, dir, "Preparing to open", func(written []string) {
			defer release()
			if len(written) == 1 {
				open(written[0])
			}
		})
		if err != nil {
			release()
		}
	case "free-plan":
		plan, err := s.eng.PlanFreeMany(paths)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		writeJSON(w, plan)
		return
	case "free":
		id, err = s.eng.FreeSpaceMany(ctx, paths)
	case "delete-cloud":
		id, err = s.eng.DeleteFromCloudMany(ctx, paths)
	case "move":
		id, err = s.eng.MoveInCloud(ctx, paths, body.To)
	case "links":
		links, err := s.eng.Links(paths)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		writeJSON(w, map[string]any{"links": links})
		return
	default:
		fail(w, 400, "unknown action")
		return
	}
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"task": id})
}

// destFolder checks a folder chosen to restore into, creating it if needed.
func (s *Server) destFolder(p string) (string, error) {
	dest := filepath.Clean(p)
	if !filepath.IsAbs(dest) {
		return "", errors.New("enter a full folder path to restore into")
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", errors.New("can't use that folder: " + err.Error())
	}
	return dest, nil
}

// bulkZip streams the selected items as one ZIP. It's a form post, so the
// browser treats the answer as a download.
func (s *Server) bulkZip(w http.ResponseWriter, r *http.Request) {
	var paths []string
	if err := json.Unmarshal([]byte(r.FormValue("paths")), &paths); err != nil || len(paths) == 0 {
		fail(w, 400, "nothing selected")
		return
	}
	for i, p := range paths {
		cp, ok := cleanCloudPath(p)
		if !ok || cp == "" {
			fail(w, 400, "bad path")
			return
		}
		paths[i] = cp
	}
	name := "tgvault-selection.zip"
	if len(paths) == 1 {
		name = filepath.Base(paths[0]) + ".zip"
	}
	attachment(w, name)
	if err := s.eng.WriteZipMany(r.Context(), paths, w); err != nil {
		log.Printf("zip: %v", err)
	}
}

// recommendations lists files that are safely backed up and could be removed
// from this device.
func (s *Server) recommendations(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	minSize, _ := strconv.ParseInt(q.Get("min"), 10, 64)
	from, to := dateRange(q)
	recs, err := s.eng.Recommendations(minSize, q.Get("type"), q.Get("sort") == "age", from, to)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	var total int64
	for _, rc := range recs {
		total += rc.Size
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	count := len(recs)
	if limit > 0 && len(recs) > limit {
		recs = recs[:limit]
	}
	writeJSON(w, map[string]any{"items": recs, "count": count, "bytes": total})
}
