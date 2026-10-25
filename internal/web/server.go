// Package web serves tgvault's browser interface on this computer only.
package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Sammy-77A/tgvault/internal/backup"
	"github.com/Sammy-77A/tgvault/internal/notify"
	"github.com/Sammy-77A/tgvault/internal/pair"
	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/tgclient"
)

//go:embed ui
var uiFiles embed.FS

const cookieName = "tgvault"

// Server is the local web interface.
type Server struct {
	st      *store.Store
	tg      *tgclient.Client
	eng     *backup.Engine
	token   string
	addr    string
	version string
	quit    func()
	mux     *http.ServeMux
	notify  *notify.Watcher
	pairing *pair.Listener // set on devices that can be logged in from the PC
	phone   *Phone         // set when running on a phone
}

// SetPairing shows the "log in from your PC" details while logged out.
func (s *Server) SetPairing(p *pair.Listener) { s.pairing = p }

// New creates the server. token protects the API; quit stops the app.
func New(st *store.Store, tg *tgclient.Client, eng *backup.Engine, nw *notify.Watcher, token, addr, version string, quit func()) *Server {
	s := &Server{st: st, tg: tg, eng: eng, notify: nw, token: token, addr: addr, version: version, quit: quit, mux: http.NewServeMux()}
	s.routes()
	return s
}

// Serve runs the server on l until ctx ends.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	srv := &http.Server{Handler: s, ReadHeaderTimeout: 10 * time.Second}
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

// ServeHTTP checks that requests come from tgvault's own page before routing.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Only answer to our own address; blocks DNS-rebinding attacks from websites.
	if r.Host != s.addr && r.Host != strings.Replace(s.addr, "127.0.0.1", "localhost", 1) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'")
	w.Header().Set("Referrer-Policy", "no-referrer")

	if r.URL.Path == "/api/ping" {
		w.Write([]byte("tgvault"))
		return
	}
	// The browser is opened with ?t=<token>; swap it for a cookie.
	if t := r.URL.Query().Get("t"); t != "" {
		if subtle.ConstantTimeCompare([]byte(t), []byte(s.token)) == 1 {
			http.SetCookie(w, &http.Cookie{Name: cookieName, Value: s.token, Path: "/", HttpOnly: true,
				SameSite: http.SameSiteStrictMode, MaxAge: 400 * 24 * 3600})
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	c, err := r.Cookie(cookieName)
	if err != nil || subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.token)) != 1 {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>tgvault</title>
<body style="font-family:system-ui;padding:2rem">Open tgvault by running <b>tgvault.exe</b> again; it will open this page with access.</body>`))
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	sub, _ := fs.Sub(uiFiles, "ui")
	s.mux.Handle("GET /", http.FileServerFS(sub))

	s.mux.HandleFunc("GET /api/state", s.state)
	s.mux.HandleFunc("POST /api/setup", s.setup)
	s.mux.HandleFunc("POST /api/login/phone", s.loginPhone)
	s.mux.HandleFunc("POST /api/login/code", s.loginCode)
	s.mux.HandleFunc("POST /api/login/password", s.loginPassword)
	s.mux.HandleFunc("POST /api/logout", s.logout)
	s.mux.HandleFunc("POST /api/pair-phone", s.pairPhone)
	s.mux.HandleFunc("POST /api/phone-settings", s.phoneSettings)
	s.mux.HandleFunc("GET /api/suggest-folders", s.suggestFolders)
	s.mux.HandleFunc("GET /api/browse", s.browse)
	s.mux.HandleFunc("GET /api/search", s.search)
	s.mux.HandleFunc("POST /api/sync", s.syncNow)
	s.mux.HandleFunc("POST /api/bulk", s.bulk)
	s.mux.HandleFunc("POST /api/bulk-zip", s.bulkZip)
	s.mux.HandleFunc("GET /api/recommendations", s.recommendations)
	s.mux.HandleFunc("POST /api/login/resend", s.loginResend)
	s.mux.HandleFunc("POST /api/login/qr", s.loginQR)
	s.mux.HandleFunc("POST /api/login/qr/cancel", s.loginQRCancel)
	s.mux.HandleFunc("GET /api/login/qr.png", s.loginQRImage)

	s.mux.HandleFunc("POST /api/roots", s.addRoot)
	s.mux.HandleFunc("POST /api/roots/{id}/remove", s.removeRoot)
	s.mux.HandleFunc("POST /api/roots/{id}/pause", s.pauseRoot)
	s.mux.HandleFunc("POST /api/pick-folder", s.pickFolder)
	s.mux.HandleFunc("POST /api/scan", s.scan)

	s.mux.HandleFunc("POST /api/projects/{id}", s.setProject)

	s.mux.HandleFunc("GET /api/activity", s.activity)
	s.mux.HandleFunc("POST /api/items/{id}/retry", s.retryItem)
	s.mux.HandleFunc("POST /api/retry-all", s.retryAll)

	s.mux.HandleFunc("GET /api/cloud", s.cloud)
	s.mux.HandleFunc("GET /api/versions", s.versions)
	s.mux.HandleFunc("GET /api/download", s.download)
	s.mux.HandleFunc("GET /api/download-zip", s.downloadZip)
	s.mux.HandleFunc("POST /api/restore", s.restore)
	s.mux.HandleFunc("GET /api/free-plan", s.freePlan)
	s.mux.HandleFunc("POST /api/free", s.free)
	s.mux.HandleFunc("POST /api/delete-cloud", s.deleteCloud)
	s.mux.HandleFunc("POST /api/tasks/{id}/dismiss", s.dismissTask)
	s.mux.HandleFunc("POST /api/reconcile", s.reconcile)

	s.mux.HandleFunc("POST /api/autostart", s.setAutostart)
	s.mux.HandleFunc("POST /api/notify", s.setNotify)
	s.mux.HandleFunc("POST /api/notify/test", s.testNotify)
	s.mux.HandleFunc("POST /api/quit", s.quitApp)
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	return json.NewDecoder(r.Body).Decode(v)
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil
}

func reqCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 2*time.Minute)
}

// cleanCloudPath validates a cloud path from the browser.
func cleanCloudPath(p string) (string, bool) {
	p = strings.Trim(p, "/")
	if p == "" {
		return "", true
	}
	if path.Clean(p) != p || strings.HasPrefix(p, "..") {
		return "", false
	}
	return p, true
}

// ---- state ----

type rootView struct {
	store.Root
	Files int `json:"files"`
}

type projectView struct {
	store.Project
	Path string `json:"path"`
	Root string `json:"root"`
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	roots, _ := s.st.Roots()
	labels := map[int64]store.Root{}
	for _, rt := range roots {
		labels[rt.ID] = rt
	}
	var projects []projectView
	all, _ := s.st.Projects("")
	for _, p := range all {
		rt := labels[p.RootID]
		projects = append(projects, projectView{Project: p, Root: rt.Label,
			Path: filepath.Join(rt.Path, filepath.FromSlash(p.Rel))})
	}
	writeJSON(w, map[string]any{
		"version":    s.version,
		"telegram":   s.tg.Info(),
		"engine":     s.eng.Status(),
		"roots":      roots,
		"projects":   projects,
		"autostart":  autostartEnabled(),
		"notify":     notify.Enabled(s.st),
		"pairing":    s.pairingInfo(),
		"phone":      s.phoneState(),
		"cloudStamp": s.st.VersionStamp(),
	})
}

// ---- login ----

func (s *Server) pairingInfo() *pair.Info {
	if s.pairing == nil {
		return nil
	}
	return s.pairing.Info()
}

// pairPhone logs a phone in to this PC's Telegram account.
func (s *Server) pairPhone(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Address string `json:"address"`
		Code    string `json:"code"`
	}
	if err := readJSON(r, &body); err != nil {
		fail(w, 400, "bad request")
		return
	}
	if err := pair.Approve(r.Context(), s.tg, body.Address, body.Code); err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		APIID   string `json:"apiId"`
		APIHash string `json:"apiHash"`
	}
	if err := readJSON(r, &body); err != nil {
		fail(w, 400, "bad request")
		return
	}
	id, _ := strconv.Atoi(strings.TrimSpace(body.APIID))
	if err := s.tg.SetAPI(id, strings.TrimSpace(body.APIHash)); err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) loginStep(w http.ResponseWriter, r *http.Request, f func(context.Context, string) error) {
	var body struct {
		Value string `json:"value"`
	}
	if err := readJSON(r, &body); err != nil {
		fail(w, 400, "bad request")
		return
	}
	ctx, cancel := reqCtx(r)
	defer cancel()
	if err := f(ctx, strings.TrimSpace(body.Value)); err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) loginPhone(w http.ResponseWriter, r *http.Request) {
	s.loginStep(w, r, s.tg.SubmitPhone)
}

func (s *Server) loginCode(w http.ResponseWriter, r *http.Request) {
	s.loginStep(w, r, s.tg.SubmitCode)
}

func (s *Server) loginPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Value string `json:"value"`
	}
	if err := readJSON(r, &body); err != nil {
		fail(w, 400, "bad request")
		return
	}
	ctx, cancel := reqCtx(r)
	defer cancel()
	// Passwords are used exactly as typed.
	if err := s.tg.SubmitPassword(ctx, body.Value); err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) loginResend(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqCtx(r)
	defer cancel()
	if err := s.tg.ResendCode(ctx); err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) loginQR(w http.ResponseWriter, r *http.Request) {
	if err := s.tg.StartQR(); err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) loginQRCancel(w http.ResponseWriter, r *http.Request) {
	s.tg.CancelQR()
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) loginQRImage(w http.ResponseWriter, r *http.Request) {
	img, err := s.tg.QRImage()
	if err != nil {
		fail(w, 404, err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(img)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqCtx(r)
	defer cancel()
	if err := s.tg.Logout(ctx); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// ---- backup folders ----

func (s *Server) addRoot(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
		Name string `json:"name"` // optional; defaults to the folder's name
	}
	if err := readJSON(r, &body); err != nil {
		fail(w, 400, "bad request")
		return
	}
	rt, err := s.eng.AddRootAs(body.Path, body.Name)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, rt)
}

func (s *Server) removeRoot(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "bad id")
		return
	}
	s.eng.UnwatchRoot(id)
	if err := s.st.RemoveRoot(id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) pauseRoot(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	var body struct {
		Paused bool `json:"paused"`
	}
	if !ok || readJSON(r, &body) != nil {
		fail(w, 400, "bad request")
		return
	}
	if err := s.st.SetRootPaused(id, body.Paused); err != nil {
		fail(w, 500, err.Error())
		return
	}
	if !body.Paused {
		s.eng.RequestScan(id)
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) pickFolder(w http.ResponseWriter, r *http.Request) {
	p, err := pickFolder(r.Context())
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]string{"path": p})
}

func (s *Server) scan(w http.ResponseWriter, r *http.Request) {
	s.eng.RequestScan(0)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) setProject(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	var body struct {
		State string `json:"state"`
	}
	if !ok || readJSON(r, &body) != nil || (body.State != store.ProjectConfirmed && body.State != store.ProjectIgnored) {
		fail(w, 400, "bad request")
		return
	}
	p, err := s.st.Project(id)
	if err != nil {
		fail(w, 404, "unknown project")
		return
	}
	if err := s.st.SetProjectState(id, body.State); err != nil {
		fail(w, 500, err.Error())
		return
	}
	s.eng.RequestScan(p.RootID)
	writeJSON(w, map[string]bool{"ok": true})
}

// ---- activity ----

// activity returns items grouped by state, each list capped so the page
// stays fast with thousands of queued files.
func (s *Server) activity(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{}
	for _, st := range []string{store.StateUploading, store.StateFailed, store.StatePending, store.StateSkipped} {
		items, err := s.st.ItemsByState(st, 100)
		if err != nil {
			fail(w, 500, err.Error())
			return
		}
		out[st] = items
	}
	counts, _ := s.st.Counts()
	out["counts"] = counts
	writeJSON(w, out)
}

func (s *Server) retryItem(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "bad id")
		return
	}
	if err := s.st.RetryItem(id); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) retryAll(w http.ResponseWriter, r *http.Request) {
	if err := s.st.RetryAll(); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// ---- cloud ----

func (s *Server) cloud(w http.ResponseWriter, r *http.Request) {
	p, ok := cleanCloudPath(r.URL.Query().Get("path"))
	if !ok {
		fail(w, 400, "bad path")
		return
	}
	entries, err := s.eng.List(p)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"path": p, "entries": entries})
}

func (s *Server) versions(w http.ResponseWriter, r *http.Request) {
	p, ok := cleanCloudPath(r.URL.Query().Get("path"))
	if !ok || p == "" {
		fail(w, 400, "bad path")
		return
	}
	vs, err := s.eng.Versions(p)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, vs)
}

func attachment(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+urlEscape(name))
	w.Header().Set("Content-Type", "application/octet-stream")
}

func urlEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.IndexByte("-._~", c) >= 0 {
			b.WriteByte(c)
		} else {
			b.WriteString("%" + strings.ToUpper(strconv.FormatInt(int64(c)|0x100, 16)[1:]))
		}
	}
	return b.String()
}

// download sends one file version to the browser (latest unless ?msg= is given).
func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	var v store.Version
	if m := r.URL.Query().Get("msg"); m != "" {
		id, _ := strconv.Atoi(m)
		var err error
		if v, err = s.eng.Version(id); err != nil {
			fail(w, 404, "unknown version")
			return
		}
	} else {
		p, ok := cleanCloudPath(r.URL.Query().Get("path"))
		var found bool
		var err error
		if ok {
			v, found, err = s.eng.LatestVersion(p)
		}
		if !ok || err != nil || !found {
			fail(w, 404, "not found")
			return
		}
	}
	name := path.Base(v.CloudPath)
	if v.Kind == store.KindProject {
		name += ".zip"
	}
	attachment(w, name)
	w.Header().Set("Content-Length", strconv.FormatInt(v.Size, 10))
	if err := s.eng.DownloadTo(r.Context(), v.MsgID, w); err != nil {
		log.Printf("download %s: %v", v.CloudPath, err)
	}
}

func (s *Server) downloadZip(w http.ResponseWriter, r *http.Request) {
	p, ok := cleanCloudPath(r.URL.Query().Get("path"))
	if !ok {
		fail(w, 400, "bad path")
		return
	}
	name := path.Base(p)
	if p == "" {
		name = "tgvault"
	}
	attachment(w, name+".zip")
	if err := s.eng.WriteZip(r.Context(), p, w); err != nil {
		log.Printf("zip %s: %v", p, err)
	}
}

func (s *Server) restore(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
		Dest string `json:"dest"`
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
	dest := strings.Trim(strings.TrimSpace(body.Dest), `"`)
	if dest != "" {
		dest = filepath.Clean(dest)
		if !filepath.IsAbs(dest) {
			fail(w, 400, "enter a full folder path to restore into")
			return
		}
		if err := os.MkdirAll(dest, 0o755); err != nil {
			fail(w, 400, "can't use that folder: "+err.Error())
			return
		}
	}
	id, err := s.eng.Restore(context.Background(), p, dest)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]int64{"task": id})
}

func (s *Server) freePlan(w http.ResponseWriter, r *http.Request) {
	p, ok := cleanCloudPath(r.URL.Query().Get("path"))
	if !ok {
		fail(w, 400, "bad path")
		return
	}
	plan, err := s.eng.PlanFree(p)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, plan)
}

func (s *Server) free(w http.ResponseWriter, r *http.Request) {
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
	id, err := s.eng.FreeSpace(context.Background(), p)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]int64{"task": id})
}

func (s *Server) deleteCloud(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := readJSON(r, &body); err != nil {
		fail(w, 400, "bad request")
		return
	}
	p, ok := cleanCloudPath(body.Path)
	if !ok || p == "" {
		fail(w, 400, "bad path")
		return
	}
	id, err := s.eng.DeleteFromCloud(context.Background(), p)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]int64{"task": id})
}

func (s *Server) dismissTask(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		fail(w, 400, "bad id")
		return
	}
	s.eng.DismissTask(id)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) reconcile(w http.ResponseWriter, r *http.Request) {
	go s.eng.Reconcile(context.Background())
	writeJSON(w, map[string]bool{"ok": true})
}

// ---- app ----

func (s *Server) setAutostart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		On bool `json:"on"`
	}
	if err := readJSON(r, &body); err != nil {
		fail(w, 400, "bad request")
		return
	}
	if err := setAutostart(body.On); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) setNotify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		On bool `json:"on"`
	}
	if err := readJSON(r, &body); err != nil {
		fail(w, 400, "bad request")
		return
	}
	v := "on"
	if !body.On {
		v = "off"
	}
	if err := s.st.Set(notify.SettingKey, v); err != nil {
		fail(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) testNotify(w http.ResponseWriter, r *http.Request) {
	if err := s.notify.Test(); err != nil {
		fail(w, 500, "couldn't show a notification: "+err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) quitApp(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]bool{"ok": true})
	go func() {
		time.Sleep(300 * time.Millisecond)
		s.quit()
	}()
}
