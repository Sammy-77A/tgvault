package web

import (
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/tgclient"
)

// previewTypes are the only content types previews are served as. Anything
// else, HTML and scripts included, goes out as plain text or plain bytes, so
// a backed-up web page can never run inside tgvault's own page.
var previewTypes = map[string]string{
	".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".png": "image/png", ".gif": "image/gif", ".webp": "image/webp",
	".bmp": "image/bmp", ".ico": "image/x-icon", ".avif": "image/avif", ".svg": "image/svg+xml",
	".mp4": "video/mp4", ".m4v": "video/mp4", ".mov": "video/mp4", ".webm": "video/webm", ".mkv": "video/x-matroska",
	".3gp": "video/3gpp", ".ogv": "video/ogg",
	".mp3": "audio/mpeg", ".m4a": "audio/mp4", ".aac": "audio/aac", ".ogg": "audio/ogg", ".oga": "audio/ogg",
	".opus": "audio/ogg", ".wav": "audio/wav", ".flac": "audio/flac", ".weba": "audio/webm",
	".pdf": "application/pdf",
}

// textTypes are previewed as plain text (the first part of the file).
var textTypes = map[string]bool{
	".txt": true, ".md": true, ".log": true, ".csv": true, ".tsv": true, ".json": true, ".xml": true,
	".html": true, ".htm": true, ".css": true, ".js": true, ".mjs": true, ".ts": true, ".tsx": true, ".jsx": true,
	".go": true, ".py": true, ".java": true, ".kt": true, ".c": true, ".h": true, ".cpp": true, ".cs": true,
	".rb": true, ".php": true, ".rs": true, ".sh": true, ".bat": true, ".ps1": true, ".yml": true, ".yaml": true,
	".toml": true, ".ini": true, ".cfg": true, ".conf": true, ".sql": true, ".srt": true, ".vtt": true,
	".env": true, ".gitignore": true,
}

func previewType(name string) string {
	ext := strings.ToLower(path.Ext(name))
	if t, ok := previewTypes[ext]; ok {
		return t
	}
	if textTypes[ext] {
		return "text/plain; charset=utf-8"
	}
	return "application/octet-stream"
}

// preview serves a stored file for viewing in the page, with HTTP range
// requests so players can seek: from this device's disk when the file there
// is unchanged, otherwise only the requested bytes from Telegram.
func (s *Server) preview(w http.ResponseWriter, r *http.Request) {
	v, ok := s.queryVersion(w, r)
	if !ok {
		return
	}
	p, err := s.eng.OpenPreview(r.Context(), v)
	if err != nil {
		fail(w, 400, tgclient.Friendly(err))
		return
	}
	defer p.Close()
	name := path.Base(v.CloudPath)
	ct := previewType(name)
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Disposition", "inline; filename*=UTF-8''"+urlEscape(name))
	h.Set("Cache-Control", "private, max-age=3600")
	h.Set("X-Tgvault-Source", map[bool]string{true: "device", false: "telegram"}[p.Local])
	if ct != "application/pdf" {
		// Even opened on its own in a tab, a preview can't run script or
		// reach tgvault. Browsers refuse to show PDFs in a sandbox; their PDF
		// viewer keeps a PDF's scripts away from the page by itself.
		h.Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self' data:; media-src 'self'; style-src 'unsafe-inline'")
	}
	http.ServeContent(w, r, "", p.ModTime, p)
}

// queryVersion finds the version a request names with ?msg= (a version) or
// ?path= (the latest version of a file), or answers with an error.
func (s *Server) queryVersion(w http.ResponseWriter, r *http.Request) (store.Version, bool) {
	if m := r.URL.Query().Get("msg"); m != "" {
		id, _ := strconv.Atoi(m)
		v, err := s.eng.Version(id)
		if err != nil {
			fail(w, 404, "unknown version")
			return store.Version{}, false
		}
		return v, true
	}
	p, ok := cleanCloudPath(r.URL.Query().Get("path"))
	var found bool
	var v store.Version
	var err error
	if ok {
		v, found, err = s.eng.LatestVersion(p)
	}
	if !ok || err != nil || !found {
		fail(w, 404, "not found")
		return store.Version{}, false
	}
	return v, true
}

// thumbs fetches the thumbnails of the versions in ?ids= (comma-separated)
// and says which have one; the page then shows them with /api/thumb.
func (s *Server) thumbs(w http.ResponseWriter, r *http.Request) {
	var ids []int
	for _, f := range strings.Split(r.URL.Query().Get("ids"), ",") {
		if id, err := strconv.Atoi(f); err == nil {
			ids = append(ids, id)
		}
	}
	got, err := s.eng.Thumbs(r.Context(), ids)
	if err != nil {
		fail(w, 502, tgclient.Friendly(err))
		return
	}
	out := make(map[string]bool, len(got))
	for id, ok := range got {
		out[strconv.Itoa(id)] = ok
	}
	writeJSON(w, out)
}

// thumb serves a thumbnail kept on this device.
func (s *Server) thumb(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(r.URL.Query().Get("msg"))
	p, ok := s.eng.ThumbFile(id)
	if !ok {
		fail(w, 404, "no thumbnail")
		return
	}
	h := w.Header()
	h.Set("Content-Type", "image/jpeg")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	// A message's file never changes, so neither does its thumbnail.
	h.Set("Cache-Control", "private, max-age=604800")
	http.ServeFile(w, r, p)
}

func (s *Server) clearThumbs(w http.ResponseWriter, r *http.Request) {
	if err := s.eng.ClearThumbs(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
