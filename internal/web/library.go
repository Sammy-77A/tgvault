package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// SetLibrary limits the page to one folder of the backup, for a copy of the
// app that only lets someone browse, play and download it. Everything else
// (backing up, deleting, moving, settings) is refused here, whatever the page
// asks for, so the limit doesn't rest on the page hiding its buttons.
func (s *Server) SetLibrary(folder string) { s.library = strings.Trim(folder, "/") }

// inLibrary reports whether a cloud path is the library folder or inside it.
func (s *Server) inLibrary(p string) bool {
	return p == s.library || strings.HasPrefix(p, s.library+"/")
}

// libraryRoutes are the only requests a library app answers (besides the
// page's own files): signing in, looking, playing and downloading.
var libraryRoutes = map[string]bool{
	"GET /api/state":            true,
	"POST /api/setup":           true,
	"POST /api/login/phone":     true,
	"POST /api/login/code":      true,
	"POST /api/login/password":  true,
	"POST /api/login/resend":    true,
	"POST /api/login/qr":        true,
	"POST /api/login/qr/cancel": true,
	"GET /api/login/qr.png":     true,
	"POST /api/logout":          true,
	"POST /api/sync":            true,
	"POST /api/reconcile":       true,
	"GET /api/cloud":            true,
	"GET /api/search":           true,
	"GET /api/versions":         true,
	"GET /api/download":         true,
	"GET /api/preview":          true,
	"GET /api/thumbs":           true,
	"GET /api/thumb":            true,
	"POST /api/play-link":       true,
	"POST /api/bulk":            true,
	"GET /api/download-zip":     true,
	"POST /api/tasks/dismiss":   true, // /api/tasks/{id}/dismiss, see libraryRoute
}

// libraryRoute is the route's key in libraryRoutes.
func libraryRoute(r *http.Request) string {
	p := r.URL.Path
	if strings.HasPrefix(p, "/api/tasks/") && strings.HasSuffix(p, "/dismiss") {
		p = "/api/tasks/dismiss"
	}
	return r.Method + " " + p
}

// libraryGate refuses what a library app may not do. It reports whether the
// request may go on.
func (s *Server) libraryGate(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/api/") {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			return true // the page's own files
		}
		fail(w, http.StatusForbidden, "this app only shows and downloads the shared folder")
		return false
	}
	if !libraryRoutes[libraryRoute(r)] {
		fail(w, http.StatusForbidden, "this app only shows and downloads the shared folder")
		return false
	}
	q := r.URL.Query()
	ok := true
	switch r.URL.Path {
	case "/api/cloud", "/api/search", "/api/versions", "/api/download-zip":
		p, clean := cleanCloudPath(q.Get("path"))
		ok = clean && s.inLibrary(p)
	case "/api/download", "/api/preview":
		ok = s.versionInLibrary(q.Get("msg"), q.Get("path"))
	case "/api/thumb":
		ok = s.versionInLibrary(q.Get("msg"), "")
	case "/api/thumbs":
		for _, id := range strings.Split(q.Get("ids"), ",") {
			if id != "" && !s.versionInLibrary(id, "") {
				ok = false
			}
		}
	case "/api/play-link":
		var body struct {
			Msg int `json:"msg"`
		}
		ok = s.peekJSON(r, &body) && s.versionInLibrary(strconv.Itoa(body.Msg), "")
	case "/api/bulk":
		var body struct {
			Action string   `json:"action"`
			Paths  []string `json:"paths"`
		}
		ok = s.peekJSON(r, &body) && len(body.Paths) > 0 &&
			(body.Action == "download" || body.Action == "open" || body.Action == "share")
		for _, p := range body.Paths {
			if cp, clean := cleanCloudPath(p); !clean || cp == "" || !s.inLibrary(cp) {
				ok = false
			}
		}
	}
	if !ok {
		fail(w, http.StatusForbidden, "that isn't in the shared folder")
	}
	return ok
}

// versionInLibrary reports whether the version with this message ID (or, if
// msg is empty, the file at this path) is in the library folder.
func (s *Server) versionInLibrary(msg, p string) bool {
	if msg != "" {
		id, err := strconv.Atoi(msg)
		if err != nil {
			return false
		}
		v, err := s.eng.Version(id)
		return err == nil && s.inLibrary(v.CloudPath)
	}
	cp, clean := cleanCloudPath(p)
	return clean && cp != "" && s.inLibrary(cp)
}

// peekJSON reads a request's JSON body without using it up.
func (s *Server) peekJSON(r *http.Request, v any) bool {
	b, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err != nil {
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	return json.Unmarshal(b, v) == nil
}
