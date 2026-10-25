package web

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Sammy-77A/tgvault/internal/backup"
	"github.com/Sammy-77A/tgvault/internal/store"
)

// Phone is what the Android app tells the page about the phone.
type Phone struct {
	StorageRoot string // the phone's shared storage, e.g. /storage/emulated/0
	FileChanged func(path string)

	mu    sync.Mutex
	perms PhonePermissions
}

// PhonePermissions are the Android permissions tgvault asks for.
type PhonePermissions struct {
	Storage       bool `json:"storage"`       // "All files access"
	Notifications bool `json:"notifications"` // allowed to show notifications
	Battery       bool `json:"battery"`       // exempt from battery optimisation
}

// SetPermissions records the current permission state.
func (p *Phone) SetPermissions(pp PhonePermissions) {
	p.mu.Lock()
	p.perms = pp
	p.mu.Unlock()
}

func (p *Phone) permissions() PhonePermissions {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.perms
}

// DownloadDir is where the phone saves files from the Cloud screen.
func (p *Phone) DownloadDir() string { return filepath.Join(p.StorageRoot, "Download", "tgvault") }

// SetPhone enables the phone-only parts of the page.
func (s *Server) SetPhone(p *Phone) { s.phone = p }

func (s *Server) phoneState() any {
	if s.phone == nil {
		return nil
	}
	return map[string]any{
		"permissions":  s.phone.permissions(),
		"wifiOnly":     s.st.Get(backup.SettingWifiOnly) != "off",
		"chargingOnly": s.st.Get(backup.SettingChargingOnly) == "on",
		"downloadDir":  s.phone.DownloadDir(),
	}
}

func (s *Server) phoneSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		WifiOnly     *bool `json:"wifiOnly"`
		ChargingOnly *bool `json:"chargingOnly"`
	}
	if err := readJSON(r, &body); err != nil {
		fail(w, 400, "bad request")
		return
	}
	onOff := func(b bool) string {
		if b {
			return "on"
		}
		return "off"
	}
	if body.WifiOnly != nil {
		_ = s.st.Set(backup.SettingWifiOnly, onOff(*body.WifiOnly))
	}
	if body.ChargingOnly != nil {
		_ = s.st.Set(backup.SettingChargingOnly, onOff(*body.ChargingOnly))
	}
	writeJSON(w, map[string]any{"ok": true})
}

// suggestion is a common phone folder offered on first use.
type suggestion struct {
	Label   string `json:"label"`
	Path    string `json:"path"`
	Hint    string `json:"hint,omitempty"`
	Checked bool   `json:"checked"`
}

// suggestFolders lists the usual phone folders that exist. Telegram's own
// download folders are left out: those files are already in Telegram.
func (s *Server) suggestFolders(w http.ResponseWriter, r *http.Request) {
	if s.phone == nil {
		writeJSON(w, map[string]any{"folders": []suggestion{}})
		return
	}
	root := s.phone.StorageRoot
	candidates := []suggestion{
		{Label: "Camera", Path: "DCIM/Camera", Hint: "photos and videos you take", Checked: true},
		{Label: "Screenshots", Path: "Pictures/Screenshots", Checked: true},
		{Label: "Screenshots", Path: "DCIM/Screenshots", Checked: true},
		{Label: "Pictures", Path: "Pictures", Hint: "includes Screenshots", Checked: false},
		{Label: "Movies", Path: "Movies", Checked: false},
		{Label: "Documents", Path: "Documents", Checked: true},
		{Label: "Download", Path: "Download", Checked: true},
		{Label: "Recordings", Path: "Recordings", Hint: "voice recordings", Checked: true},
		{Label: "Music", Path: "Music", Checked: false},
		{Label: "WhatsApp media", Path: "Android/media/com.whatsapp/WhatsApp/Media", Hint: "photos, videos, voice notes and documents from chats", Checked: true},
		{Label: "WhatsApp Business media", Path: "Android/media/com.whatsapp.w4b/WhatsApp Business/Media", Checked: true},
	}
	roots, _ := s.st.Roots()
	var out []suggestion
	for _, c := range candidates {
		abs := filepath.Join(root, filepath.FromSlash(c.Path))
		if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
			continue
		}
		covered := false
		for _, rt := range roots {
			if rel, err := filepath.Rel(rt.Path, abs); err == nil && !strings.HasPrefix(rel, "..") {
				covered = true
			}
		}
		if covered {
			continue
		}
		c.Path = abs
		out = append(out, c)
	}
	writeJSON(w, map[string]any{"folders": out})
}

// browse lists the subfolders of a folder, for picking folders on the phone.
func (s *Server) browse(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if p == "" {
		p = s.browseStart()
	}
	p = filepath.Clean(p)
	entries, err := os.ReadDir(p)
	if err != nil {
		fail(w, 400, "can't open that folder: "+err.Error())
		return
	}
	var dirs []string
	for _, d := range entries {
		if d.IsDir() && !strings.HasPrefix(d.Name(), ".") {
			dirs = append(dirs, d.Name())
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.ToLower(dirs[i]) < strings.ToLower(dirs[j]) })
	parent := filepath.Dir(p)
	if parent == p {
		parent = ""
	}
	for _, v := range s.volumes() {
		if p == filepath.Clean(v["path"]) {
			parent = "" // don't wander above the phone's storage
		}
	}
	writeJSON(w, map[string]any{"path": p, "parent": parent, "dirs": dirs, "volumes": s.volumes()})
}

func (s *Server) browseStart() string {
	if s.phone != nil {
		return s.phone.StorageRoot
	}
	home, _ := os.UserHomeDir()
	return home
}

// volumes lists the phone's storage: internal storage and any memory card.
func (s *Server) volumes() []map[string]string {
	if s.phone == nil {
		return nil
	}
	out := []map[string]string{{"label": "Phone storage", "path": s.phone.StorageRoot}}
	entries, _ := os.ReadDir("/storage")
	for _, d := range entries {
		n := d.Name()
		if n == "emulated" || n == "self" {
			continue
		}
		p := "/storage/" + n
		if _, err := os.ReadDir(p); err == nil {
			out = append(out, map[string]string{"label": "Memory card (" + n + ")", "path": p})
		}
	}
	return out
}

// save writes a cloud file (or a folder as ZIP) into the phone's Download folder.
func (s *Server) save(w http.ResponseWriter, r *http.Request) {
	if s.phone == nil {
		fail(w, 400, "only on the phone")
		return
	}
	var body struct {
		Path string `json:"path"`
		Msg  int    `json:"msg"`
		Zip  bool   `json:"zip"`
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
	dir := s.phone.DownloadDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fail(w, 500, err.Error())
		return
	}
	var name string
	var write func(io.Writer) error
	if body.Zip {
		name = path.Base(p) + ".zip"
		if p == "" {
			name = "tgvault.zip"
		}
		write = func(out io.Writer) error { return s.eng.WriteZip(r.Context(), p, out) }
	} else {
		var v store.Version
		var err error
		if body.Msg != 0 {
			v, err = s.eng.Version(body.Msg)
		} else {
			var found bool
			v, found, err = s.eng.LatestVersion(p)
			if err == nil && !found {
				err = errors.New("not found")
			}
		}
		if err != nil {
			fail(w, 404, "not in the backup")
			return
		}
		name = path.Base(v.CloudPath)
		if v.Kind == store.KindProject {
			name += ".zip"
		}
		write = func(out io.Writer) error { return s.eng.DownloadTo(r.Context(), v.MsgID, out) }
	}
	target := freeName(filepath.Join(dir, name))
	tmp := target + ".tgvault-tmp"
	f, err := os.Create(tmp)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	err = write(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, target)
	}
	if err != nil {
		os.Remove(tmp)
		log.Printf("save %s: %v", p, err)
		fail(w, 500, "saving failed: "+err.Error())
		return
	}
	if s.phone.FileChanged != nil {
		s.phone.FileChanged(target)
	}
	writeJSON(w, map[string]any{"ok": true, "saved": target})
}

// freeName returns p, or "name (2).ext" etc. if p exists.
func freeName(p string) string {
	if _, err := os.Stat(p); err != nil {
		return p
	}
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	for i := 2; ; i++ {
		c := fmt.Sprintf("%s (%s)%s", base, strconv.Itoa(i), ext)
		if _, err := os.Stat(c); err != nil {
			return c
		}
	}
}
