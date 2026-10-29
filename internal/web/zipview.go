package web

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"path"
	"strconv"

	"github.com/Sammy-77A/tgvault/internal/backup"
	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/tgclient"
)

// zipList lists a folder inside a ZIP or code project version.
func (s *Server) zipList(w http.ResponseWriter, r *http.Request) {
	v, ok := s.queryVersion(w, r)
	if !ok {
		return
	}
	if v.Kind != store.KindProject && !backup.IsZipName(v.CloudPath) {
		fail(w, 400, "only ZIP files and code projects can be opened")
		return
	}
	dir := r.URL.Query().Get("dir")
	items, total, err := s.eng.ZipDir(r.Context(), v, dir)
	if err != nil {
		fail(w, 400, tgclient.Friendly(err))
		return
	}
	writeJSON(w, map[string]any{
		"name": path.Base(v.CloudPath), "msgId": v.MsgID, "project": v.Kind == store.KindProject,
		"dir": dir, "files": total, "items": items,
	})
}

// zipEntryDownload sends one file from inside a ZIP to the browser.
func (s *Server) zipEntryDownload(w http.ResponseWriter, r *http.Request, v store.Version, entry string) {
	zf, err := s.eng.OpenZipFile(r.Context(), v, entry)
	if err != nil {
		fail(w, 400, tgclient.Friendly(err))
		return
	}
	defer zf.Close()
	attachment(w, path.Base(entry))
	w.Header().Set("Content-Length", strconv.FormatInt(zf.Size, 10))
	if _, err := io.Copy(w, zf); err != nil {
		log.Printf("download %s in %s: %v", entry, v.CloudPath, err)
	}
}

// zipDownload streams the chosen files and folders of a ZIP as a new ZIP. It's
// a form post, so the browser treats the answer as a download.
func (s *Server) zipDownload(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.FormValue("msg"))
	v, err := s.eng.Version(id)
	if err != nil {
		fail(w, 404, "unknown version")
		return
	}
	var names []string
	if err := json.Unmarshal([]byte(r.FormValue("names")), &names); err != nil || len(names) == 0 {
		fail(w, 400, "nothing selected")
		return
	}
	name := path.Base(v.CloudPath)
	if backup.IsZipName(name) {
		name = name[:len(name)-len(".zip")]
	}
	if len(names) == 1 {
		name = path.Base(names[0])
	}
	if name == "" || name == "." {
		name = "tgvault"
	}
	attachment(w, name+".zip")
	if err := s.eng.WriteZipEntries(r.Context(), v, names, w); err != nil {
		log.Printf("zip %s: %v", v.CloudPath, err)
	}
}

// zipExtract puts files from inside a ZIP or project on this device: restore
// (into the project's folder, or a chosen one), download (phone), or hand to
// another app (phone: open, share).
func (s *Server) zipExtract(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Msg    int      `json:"msg"`
		Names  []string `json:"names"`
		Action string   `json:"action"`
		Dest   string   `json:"dest"`
	}
	if err := readJSON(r, &body); err != nil || len(body.Names) == 0 {
		fail(w, 400, "bad request")
		return
	}
	v, err := s.eng.Version(body.Msg)
	if err != nil {
		fail(w, 404, "unknown version")
		return
	}
	ctx := r.Context()
	var id int64
	switch body.Action {
	case "restore":
		dest := ""
		if body.Dest != "" {
			if dest, err = s.destFolder(body.Dest); err != nil {
				fail(w, 400, err.Error())
				return
			}
		}
		id, err = s.eng.ExtractZipEntries(ctx, v, body.Names, dest, "Restoring", nil)
	case "download":
		if s.phone == nil {
			fail(w, 400, "on a PC, downloads go through the browser")
			return
		}
		id, err = s.eng.ExtractZipEntries(ctx, v, body.Names, s.phone.DownloadDir(), "Downloading", nil)
	case "share", "open":
		if s.phone == nil || (body.Action == "share" && s.phone.Share == nil) || (body.Action == "open" && (s.phone.Open == nil || len(body.Names) != 1)) {
			fail(w, 400, "this is only on the phone")
			return
		}
		dir, release, derr := s.newShareDir()
		if derr != nil {
			fail(w, 500, derr.Error())
			return
		}
		hand := s.phone.Share
		if body.Action == "open" {
			open := s.phone.Open
			hand = func(files []string) { open(files[0]) }
		}
		id, err = s.eng.ExtractZipEntries(ctx, v, body.Names, dir, "Preparing", func(written []string) {
			defer release()
			if len(written) > 0 {
				hand(written)
			}
		})
		if err != nil {
			release()
		}
	default:
		fail(w, 400, "unknown action")
		return
	}
	if err != nil {
		fail(w, 400, tgclient.Friendly(err))
		return
	}
	writeJSON(w, map[string]any{"task": id})
}
