package web

import (
	"crypto/rand"
	"encoding/hex"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Sammy-77A/tgvault/internal/tgclient"
)

// Video players such as VLC (on the PC) and the phone's built-in player can't
// send tgvault's login cookie. They get a link with a random key instead. The
// key opens one file (or one file inside a ZIP) and nothing else, only from
// this device, and stops working after playKeyLife.
const playKeyLife = 6 * time.Hour

type playKey struct {
	msg     int
	entry   string
	expires time.Time
}

type playKeys struct {
	mu sync.Mutex
	m  map[string]playKey
}

// add makes a key for a file and returns it.
func (p *playKeys) add(msg int, entry string) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	key := hex.EncodeToString(b)
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.m == nil {
		p.m = map[string]playKey{}
	}
	for k, v := range p.m {
		if now.After(v.expires) {
			delete(p.m, k)
		}
	}
	p.m[key] = playKey{msg: msg, entry: entry, expires: now.Add(playKeyLife)}
	return key
}

func (p *playKeys) get(key string) (playKey, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k, ok := p.m[key]
	if !ok || time.Now().After(k.expires) {
		delete(p.m, key)
		return playKey{}, false
	}
	return k, true
}

// playURL is the link a player opens: the file's name ends it, so the player
// shows a title and knows the file type.
func (s *Server) playURL(msg int, entry, name string) string {
	return "http://" + s.addr + "/play/" + s.plays.add(msg, entry) + "/" + url.PathEscape(name)
}

// play serves a file to a player that has a key.
func (s *Server) play(w http.ResponseWriter, r *http.Request) {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err != nil || !net.ParseIP(host).IsLoopback() {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/play/"), "/", 2)
	k, ok := s.plays.get(parts[0])
	if !ok {
		http.Error(w, "this link has expired", http.StatusNotFound)
		return
	}
	v, err := s.eng.Version(k.msg)
	if err != nil {
		http.Error(w, "unknown version", http.StatusNotFound)
		return
	}
	s.servePreview(w, r, v, k.entry)
}

// playTarget reads which file the page wants to play and its name.
func (s *Server) playTarget(w http.ResponseWriter, r *http.Request) (msg int, entry, name string, ok bool) {
	var body struct {
		Msg   int    `json:"msg"`
		Entry string `json:"entry"`
	}
	if err := readJSON(r, &body); err != nil || body.Msg == 0 {
		fail(w, 400, "bad request")
		return 0, "", "", false
	}
	v, err := s.eng.Version(body.Msg)
	if err != nil {
		fail(w, 404, "unknown version")
		return 0, "", "", false
	}
	name = path.Base(v.CloudPath)
	if body.Entry != "" {
		name = path.Base(body.Entry)
	}
	return body.Msg, body.Entry, name, true
}

// playLink makes a player link (the phone's built-in player opens it).
func (s *Server) playLink(w http.ResponseWriter, r *http.Request) {
	msg, entry, name, ok := s.playTarget(w, r)
	if !ok {
		return
	}
	writeJSON(w, map[string]string{"url": s.playURL(msg, entry, name), "name": name})
}

// playVLC opens a file in VLC on this PC.
func (s *Server) playVLC(w http.ResponseWriter, r *http.Request) {
	vlc := vlcPath()
	if vlc == "" {
		fail(w, 400, "VLC isn't installed on this PC")
		return
	}
	msg, entry, name, ok := s.playTarget(w, r)
	if !ok {
		return
	}
	cmd := exec.Command(vlc, s.playURL(msg, entry, name))
	if err := cmd.Start(); err != nil {
		fail(w, 500, tgclient.Friendly(err))
		return
	}
	go cmd.Wait()
	writeJSON(w, map[string]string{"name": name, "pid": strconv.Itoa(cmd.Process.Pid)})
}
