package friends

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Sammy-77A/tgvault/internal/backup"
	"github.com/Sammy-77A/tgvault/internal/meta"
	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/tgclient"
	"github.com/Sammy-77A/tgvault/internal/web"
)

// Options configures the friends' file API.
type Options struct {
	DataDir    string
	SpoolMax   int64  // most bytes of uploads allowed to wait for Telegram at once (0 = no limit)
	PublicURL  string // address friends use, for the links the API hands out, e.g. https://files.example.com
	TrustProxy bool   // take a client's address from X-Forwarded-For (only behind a reverse proxy)
}

// Hub serves the friends' API.
type Hub struct {
	st    *store.Store
	eng   *backup.Engine
	opt   Options
	spool string // uploads wait here, one folder per friend
	tmp   string // uploads being received
	sem   chan struct{}

	mu    sync.Mutex
	fails map[string]*failure
}

type failure struct {
	n     int
	until time.Time
}

const (
	maxUploads   = 3                // uploads received at the same time
	authTries    = 10               // wrong keys allowed from one address...
	authWindow   = 10 * time.Minute // ...before it is blocked for this long
	releaseEvery = 30 * time.Second
)

// New creates the hub.
func New(st *store.Store, eng *backup.Engine, o Options) *Hub {
	return &Hub{st: st, eng: eng, opt: o,
		spool: filepath.Join(o.DataDir, "spool"), tmp: filepath.Join(o.DataDir, "spool-tmp"),
		sem: make(chan struct{}, maxUploads), fails: map[string]*failure{}}
}

// Run removes uploads that finished reaching Telegram until ctx ends.
func (h *Hub) Run(ctx context.Context) {
	_ = os.RemoveAll(h.tmp) // half-received uploads from a previous run
	_ = os.MkdirAll(h.tmp, 0o700)
	_ = os.MkdirAll(h.spool, 0o700)
	tick := time.NewTicker(releaseEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		list, err := h.st.Friends()
		if err != nil {
			continue
		}
		for _, f := range list {
			if _, err := os.Stat(filepath.Join(h.spool, f.Name)); err != nil {
				continue
			}
			if _, err := h.eng.ReleaseLocal(ctx, cloudRoot(f.Name)); err != nil && ctx.Err() == nil {
				log.Printf("friends: freeing %s's uploads: %v", f.Name, err)
			}
		}
	}
}

func cloudRoot(name string) string { return Folder + "/" + name }

// Handler is the API. Serve it over HTTPS (for example behind Caddy).
func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "tgvault file server\n")
	})
	mux.HandleFunc("GET /v1/me", h.auth(h.me))
	mux.HandleFunc("GET /v1/list", h.auth(h.list))
	mux.HandleFunc("GET /v1/files/{path...}", h.auth(h.get))
	mux.HandleFunc("PUT /v1/files/{path...}", h.auth(h.put))
	mux.HandleFunc("DELETE /v1/files/{path...}", h.auth(h.remove))
	mux.HandleFunc("GET /v1/status/{path...}", h.auth(h.status))
	mux.HandleFunc("GET /v1/zip", h.auth(h.zip))
	mux.HandleFunc("GET /v1/shares", h.auth(h.shares))
	mux.HandleFunc("POST /v1/shares", h.auth(h.share))
	mux.HandleFunc("DELETE /v1/shares/{id}", h.auth(h.unshare))
	mux.HandleFunc("GET /s/{token}", h.download)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		mux.ServeHTTP(w, r)
	})
}

// Serve runs the API on l until ctx ends.
func (h *Hub) Serve(ctx context.Context, l net.Listener) error {
	// No overall read or write timeout: uploads and downloads can be many GB.
	srv := &http.Server{Handler: h.Handler(), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	err := srv.Serve(l)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (h *Hub) clientIP(r *http.Request) string {
	if h.opt.TrustProxy {
		if x := r.Header.Get("X-Forwarded-For"); x != "" {
			parts := strings.Split(x, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (h *Hub) blocked(ip string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	f := h.fails[ip]
	return f != nil && f.n >= authTries && time.Now().Before(f.until)
}

func (h *Hub) failed(ip string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	f := h.fails[ip]
	if f == nil || time.Now().After(f.until) {
		f = &failure{}
		h.fails[ip] = f
	}
	f.n++
	f.until = time.Now().Add(authWindow)
	if len(h.fails) > 10000 { // forget old addresses
		for k, v := range h.fails {
			if time.Now().After(v.until) {
				delete(h.fails, k)
			}
		}
	}
}

// auth runs next for a request carrying a valid API key.
func (h *Hub) auth(next func(http.ResponseWriter, *http.Request, store.Friend)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ip := h.clientIP(r)
		if h.blocked(ip) {
			w.Header().Set("Retry-After", "600")
			fail(w, http.StatusTooManyRequests, "too many wrong keys; try again later")
			return
		}
		key := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if key == "" {
			key = r.Header.Get("X-Api-Key")
		}
		f, ok := h.st.FriendByKey(HashKey(key))
		if key == "" || !ok || f.Disabled {
			h.failed(ip)
			w.Header().Set("WWW-Authenticate", `Bearer realm="tgvault"`)
			fail(w, http.StatusUnauthorized, "missing or wrong API key")
			return
		}
		next(w, r, f)
	}
}

// target turns the path in a request into the friend's cloud path.
func target(r *http.Request, f store.Friend, raw string) (rel, cloud string, ok bool) {
	rel, ok = cleanRel(raw)
	if !ok {
		return "", "", false
	}
	return rel, path.Join(cloudRoot(f.Name), rel), true
}

// used is the space a friend's files take: the latest version of each file in
// Telegram plus uploads still waiting here.
func (h *Hub) used(name string) int64 {
	var n int64
	vs, _ := h.st.LatestUnder(cloudRoot(name))
	for _, v := range vs {
		n += v.Size
	}
	return n + dirSize(filepath.Join(h.spool, name))
}

func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// rejected says why tgvault would never back a path up (temporary and system
// files, folders such as node_modules), so an upload isn't accepted and then lost.
func rejected(rel string) string {
	segs := strings.Split(rel, "/")
	for _, d := range segs[:len(segs)-1] {
		if meta.SkipDir(d, nil, func(string) bool { return false }) {
			return "files inside a folder named " + d + " aren't stored"
		}
	}
	if meta.SkipFile(segs[len(segs)-1]) || meta.SkipPath(rel) {
		return "this kind of file (temporary or system) isn't stored"
	}
	return ""
}

// ---- handlers ----

func (h *Hub) me(w http.ResponseWriter, r *http.Request, f store.Friend) {
	used := h.used(f.Name)
	out := map[string]any{"name": f.Name, "used": used, "quota": f.Quota}
	if f.Quota > 0 {
		out["left"] = max(f.Quota-used, 0)
	}
	writeJSON(w, 200, out)
}

type entryOut struct {
	Name  string `json:"name"`
	Path  string `json:"path"`
	Kind  string `json:"kind"` // "file" or "folder"
	Size  int64  `json:"size"`
	Files int    `json:"files,omitempty"` // in a folder
	Mtime int64  `json:"mtime,omitempty"`
}

type pendingOut struct {
	Path  string `json:"path"`
	State string `json:"state"`
	Note  string `json:"note,omitempty"`
}

func (h *Hub) list(w http.ResponseWriter, r *http.Request, f store.Friend) {
	rel, cloud, ok := target(r, f, r.URL.Query().Get("path"))
	if !ok {
		fail(w, 400, "bad path")
		return
	}
	entries, err := h.eng.List(cloud)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	out := make([]entryOut, 0, len(entries))
	strip := cloudRoot(f.Name) + "/"
	for _, e := range entries {
		kind := "file"
		if e.Kind == "folder" {
			kind = "folder"
		}
		out = append(out, entryOut{Name: e.Name, Path: strings.TrimPrefix(e.Path, strip), Kind: kind,
			Size: e.Size, Files: e.Files, Mtime: e.Mtime})
	}
	pending := []pendingOut{}
	if items, err := h.st.ItemsUnder(cloud); err == nil {
		for _, it := range items {
			if it.LocalPresent && (it.State == store.StatePending || it.State == store.StateUploading || it.State == store.StateFailed) {
				pending = append(pending, pendingOut{Path: strings.TrimPrefix(it.CloudPath, strip), State: it.State, Note: it.Note})
			}
		}
	}
	writeJSON(w, 200, map[string]any{"path": rel, "entries": out, "pending": pending})
}

func (h *Hub) put(w http.ResponseWriter, r *http.Request, f store.Friend) {
	rel, cloud, ok := target(r, f, r.PathValue("path"))
	if !ok || rel == "" {
		fail(w, 400, "bad path")
		return
	}
	if why := rejected(rel); why != "" {
		fail(w, 422, why)
		return
	}
	if _, err := meta.Caption(meta.Info{Kind: "f", Path: cloud, Size: 1, Hash: strings.Repeat("0", 64)}); err != nil {
		fail(w, 400, err.Error())
		return
	}
	if r.ContentLength == 0 {
		fail(w, 400, "empty files aren't stored")
		return
	}

	limit := int64(math.MaxInt64)
	if f.Quota > 0 {
		used := h.used(f.Name)
		// The new file replaces the old one, so the old one stops counting.
		if old, ok, _ := h.st.LatestVersion(cloud); ok {
			used -= old.Size
		}
		if fi, err := os.Stat(filepath.Join(h.spool, f.Name, filepath.FromSlash(rel))); err == nil && fi.Mode().IsRegular() {
			used -= fi.Size()
		}
		left := f.Quota - used
		if left <= 0 || (r.ContentLength > left) {
			fail(w, http.StatusInsufficientStorage, "over your storage limit ("+FormatSize(f.Quota)+")")
			return
		}
		limit = left
	}
	if h.opt.SpoolMax > 0 {
		left := h.opt.SpoolMax - dirSize(h.spool)
		if left <= 0 || r.ContentLength > left {
			w.Header().Set("Retry-After", "300")
			fail(w, http.StatusServiceUnavailable, "the server is still sending earlier uploads to Telegram; try again later")
			return
		}
		limit = min(limit, left)
	}
	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	default:
		w.Header().Set("Retry-After", "30")
		fail(w, http.StatusTooManyRequests, "too many uploads at once; try again shortly")
		return
	}

	root, err := h.root(f.Name)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	dest := filepath.Join(root.Path, filepath.FromSlash(rel))
	if fi, err := os.Stat(dest); err == nil && fi.IsDir() {
		fail(w, 409, "a folder with that name exists")
		return
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		fail(w, 409, "a file is in the way of that folder path")
		return
	}
	tmp, err := os.CreateTemp(h.tmp, "up-*")
	if err != nil {
		fail(w, 500, "can't receive the file: "+err.Error())
		return
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, http.MaxBytesReader(w, r.Body, limit))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			fail(w, http.StatusInsufficientStorage, "that is more than your storage limit or the server's room allows")
			return
		}
		fail(w, 400, "upload interrupted: "+err.Error())
		return
	}
	if n == 0 {
		fail(w, 400, "empty files aren't stored")
		return
	}
	if r.ContentLength > 0 && n != r.ContentLength {
		fail(w, 400, "upload was cut short")
		return
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		fail(w, 500, "can't save the file: "+err.Error())
		return
	}
	h.eng.RequestScan(root.ID)
	log.Printf("friends: %s uploaded %s (%s)", f.Name, rel, FormatSize(n))
	writeJSON(w, http.StatusAccepted, map[string]any{"path": rel, "size": n, "state": "queued"})
}

// root is the folder a friend's uploads wait in, registered as a backup folder.
func (h *Hub) root(name string) (store.Root, error) {
	dir := filepath.Join(h.spool, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return store.Root{}, err
	}
	return h.st.FindOrAddRoot(dir, cloudRoot(name))
}

func (h *Hub) status(w http.ResponseWriter, r *http.Request, f store.Friend) {
	rel, cloud, ok := target(r, f, r.PathValue("path"))
	if !ok || rel == "" {
		fail(w, 400, "bad path")
		return
	}
	v, stored, _ := h.st.LatestVersion(cloud)
	it, err := h.st.ItemByPath(cloud)
	state, note := "unknown", ""
	switch {
	case err == nil && it.LocalPresent && it.State == store.StatePending:
		state = "queued"
	case err == nil && it.LocalPresent && it.State == store.StateUploading:
		state = "uploading"
	case err == nil && it.LocalPresent && it.State == store.StateFailed:
		state, note = "retrying", it.Note
	case err == nil && it.State == store.StateSkipped:
		state, note = "rejected", it.Note
	case stored:
		state = "stored"
	}
	out := map[string]any{"path": rel, "state": state}
	if note != "" {
		out["note"] = note
	}
	if stored {
		out["size"], out["mtime"] = v.Size, v.Mtime
	}
	if state == "unknown" {
		writeJSON(w, 404, out)
		return
	}
	writeJSON(w, 200, out)
}

// version finds the stored file a friend asked for.
func (h *Hub) version(cloud string) (store.Version, bool) {
	v, ok, err := h.st.LatestVersion(cloud)
	if err != nil || !ok || v.Kind != store.KindFile {
		return store.Version{}, false
	}
	return v, true
}

// serve sends a stored file, with range requests so downloads resume and
// videos seek. Only the bytes asked for are fetched from Telegram.
func (h *Hub) serve(w http.ResponseWriter, r *http.Request, v store.Version) {
	pv, err := h.eng.OpenPreview(r.Context(), v)
	if err != nil {
		fail(w, 502, tgclient.Friendly(err))
		return
	}
	defer pv.Close()
	name := path.Base(v.CloudPath)
	ct, disp := "application/octet-stream", "attachment"
	hd := w.Header()
	if r.URL.Query().Get("inline") == "1" {
		if t := web.PreviewType(name); t != "application/octet-stream" {
			ct, disp = t, "inline"
			if t != "application/pdf" {
				hd.Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src 'self' data:; media-src 'self'; style-src 'unsafe-inline'")
			}
		}
	}
	hd.Set("Content-Type", ct)
	hd.Set("Content-Disposition", disp+"; filename*=UTF-8''"+url.PathEscape(name))
	hd.Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, "", pv.ModTime, pv)
}

func (h *Hub) get(w http.ResponseWriter, r *http.Request, f store.Friend) {
	rel, cloud, ok := target(r, f, r.PathValue("path"))
	if !ok || rel == "" {
		fail(w, 400, "bad path")
		return
	}
	v, found := h.version(cloud)
	if !found {
		fail(w, 404, "not found")
		return
	}
	h.serve(w, r, v)
}

func (h *Hub) zip(w http.ResponseWriter, r *http.Request, f store.Friend) {
	rel, cloud, ok := target(r, f, r.URL.Query().Get("path"))
	if !ok {
		fail(w, 400, "bad path")
		return
	}
	name := path.Base(rel)
	if rel == "" {
		name = f.Name
	}
	if vs, _ := h.st.LatestUnder(cloud); len(vs) == 0 {
		fail(w, 404, "nothing there")
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name+".zip"))
	if err := h.eng.WriteZip(r.Context(), cloud, w); err != nil {
		log.Printf("friends: zip %s: %v", cloud, err)
	}
}

func (h *Hub) remove(w http.ResponseWriter, r *http.Request, f store.Friend) {
	rel, cloud, ok := target(r, f, r.PathValue("path"))
	if !ok || rel == "" {
		fail(w, 400, "bad path")
		return
	}
	if _, err := h.eng.DeleteFromCloud(r.Context(), cloud); err != nil {
		switch {
		case strings.Contains(err.Error(), "still on"):
			fail(w, 409, "that is still being uploaded to Telegram; try again in a few minutes")
		case strings.Contains(err.Error(), "nothing stored"):
			fail(w, 404, "not found")
		default:
			fail(w, 500, err.Error())
		}
		return
	}
	log.Printf("friends: %s removed %s", f.Name, rel)
	writeJSON(w, http.StatusAccepted, map[string]any{"path": rel, "state": "removing"})
}

// ---- share links ----

type shareOut struct {
	ID        string `json:"id"`
	Path      string `json:"path"`
	Expires   int64  `json:"expires,omitempty"`
	Downloads int    `json:"downloads"`
	URL       string `json:"url,omitempty"` // only when the link is created
}

func (h *Hub) shares(w http.ResponseWriter, r *http.Request, f store.Friend) {
	list, err := h.st.SharesOf(f.Name)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	out := make([]shareOut, 0, len(list))
	for _, s := range list {
		out = append(out, shareOut{ID: s.TokenHash, Path: strings.TrimPrefix(s.CloudPath, cloudRoot(f.Name)+"/"),
			Expires: s.Expires, Downloads: s.Downloads})
	}
	writeJSON(w, 200, out)
}

func (h *Hub) share(w http.ResponseWriter, r *http.Request, f store.Friend) {
	var body struct {
		Path  string `json:"path"`
		Hours int    `json:"hours"` // 0 means the default, 7 days; -1 means never expires
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		fail(w, 400, "bad request")
		return
	}
	rel, cloud, ok := target(r, f, body.Path)
	if !ok || rel == "" {
		fail(w, 400, "bad path")
		return
	}
	if _, found := h.version(cloud); !found {
		fail(w, 404, "not found")
		return
	}
	var expires int64
	switch {
	case body.Hours == 0:
		expires = time.Now().Add(7 * 24 * time.Hour).Unix()
	case body.Hours > 0:
		expires = time.Now().Add(time.Duration(min(body.Hours, 24*365)) * time.Hour).Unix()
	}
	token := randomToken(24)
	hash := HashKey(token)
	if err := h.st.AddShare(store.Share{TokenHash: hash, Friend: f.Name, CloudPath: cloud, Expires: expires}); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, 201, shareOut{ID: hash, Path: rel, Expires: expires, URL: strings.TrimRight(h.opt.PublicURL, "/") + "/s/" + token})
}

func (h *Hub) unshare(w http.ResponseWriter, r *http.Request, f store.Friend) {
	ok, err := h.st.DeleteShare(f.Name, r.PathValue("id"))
	switch {
	case err != nil:
		fail(w, 500, err.Error())
	case !ok:
		fail(w, 404, "no such link")
	default:
		writeJSON(w, 200, map[string]bool{"ok": true})
	}
}

// download is the public side of a share link: no key, one file.
func (h *Hub) download(w http.ResponseWriter, r *http.Request) {
	sh, ok := h.st.ShareByToken(HashKey(r.PathValue("token")))
	if !ok || (sh.Expires > 0 && time.Now().Unix() > sh.Expires) {
		fail(w, 404, "this link doesn't exist or has expired")
		return
	}
	if f, ok := h.st.FriendByName(sh.Friend); !ok || f.Disabled {
		fail(w, 404, "this link doesn't exist or has expired")
		return
	}
	v, found := h.version(sh.CloudPath)
	if !found {
		fail(w, 404, "the file is gone")
		return
	}
	if r.Header.Get("Range") == "" {
		h.st.CountShareDownload(sh.TokenHash)
	}
	h.serve(w, r, v)
}
