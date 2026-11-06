package web

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Sammy-77A/tgvault/internal/backup"
)

// The movie library: the TMDB key, sorting loose videos, and the list of files
// waiting for a decision.

func (s *Server) media(w http.ResponseWriter, r *http.Request) {
	if err := s.eng.EnsureMediaFolders(); err != nil {
		log.Printf("movie library folders: %v", err)
	}
	rv, err := s.eng.Review()
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	// pick up requests made since the last check (the background watcher does
	// this too; this is for when the page is opened first)
	if s.eng.Connected() {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		_, _ = s.eng.SyncRequests(ctx)
		cancel()
	}
	writeJSON(w, map[string]any{"hasKey": s.eng.HasTMDBKey(), "root": s.eng.MediaRoot(), "review": rv,
		"requests": s.eng.Requests(), "hasSubs": strings.TrimSpace(s.st.Get(backup.SettingSubtitleKey)) != ""})
}

func (s *Server) mediaKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Key string `json:"key"`
	}
	if err := readJSON(r, &body); err != nil {
		fail(w, 400, "bad request")
		return
	}
	if err := s.eng.SetTMDBKey(body.Key); err != nil {
		fail(w, 500, err.Error())
		return
	}
	go func() { // share it with the phone app
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = s.eng.PublishConfig(ctx)
	}()
	writeJSON(w, map[string]any{"hasKey": s.eng.HasTMDBKey()})
}

func (s *Server) mediaOrganize(w http.ResponseWriter, r *http.Request) {
	id, err := s.eng.OrganizeMedia(r.Context())
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"task": id})
}

func (s *Server) mediaResolve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path  string       `json:"path"`
		Guess backup.Guess `json:"guess"`
	}
	if err := readJSON(r, &body); err != nil {
		fail(w, 400, "bad request")
		return
	}
	p, ok := cleanCloudPath(body.Path)
	if !ok {
		fail(w, 400, "bad path")
		return
	}
	if err := s.eng.ResolveMedia(r.Context(), p, body.Guess); err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) mediaSkip(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := readJSON(r, &body); err != nil {
		fail(w, 400, "bad request")
		return
	}
	p, ok := cleanCloudPath(body.Path)
	if !ok {
		fail(w, 400, "bad path")
		return
	}
	if err := s.eng.SkipMedia(p); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) mediaSearch(w http.ResponseWriter, r *http.Request) {
	year, _ := strconv.Atoi(r.URL.Query().Get("year"))
	cs, err := s.eng.SearchMedia(r.Context(), r.URL.Query().Get("kind"), r.URL.Query().Get("q"), year)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"results": cs})
}

func (s *Server) mediaAnswer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID        string `json:"id"`
		Fulfilled bool   `json:"fulfilled"`
	}
	if err := readJSON(r, &body); err != nil || body.ID == "" {
		fail(w, 400, "bad request")
		return
	}
	if err := s.eng.Answer(r.Context(), body.ID, body.Fulfilled); err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) mediaLog(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"log": s.eng.SortLog(200)})
}
