package web

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Sammy-77A/tgvault/internal/backup"
)

// Phone is what the Android app tells the page about the phone.
type Phone struct {
	StorageRoot string // the phone's shared storage, e.g. /storage/emulated/0
	FileChanged func(path string)
	ShareDir    string               // files are copied here before sharing
	Share       func(paths []string) // opens Android's share sheet
	Open        func(path string)    // opens a file in another app ("Open with…")

	mu      sync.Mutex
	perms   PhonePermissions
	volumes []Volume
}

// Volume is one storage of the phone: its internal storage or a memory card.
type Volume struct {
	Label string `json:"label"`
	Path  string `json:"path"`
	Free  int64  `json:"free"`
	Total int64  `json:"total"`
}

// SetVolumes records the phone's storages, as Android reports them.
func (p *Phone) SetVolumes(v []Volume) {
	p.mu.Lock()
	p.volumes = v
	p.mu.Unlock()
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
		{Label: "WhatsApp", Path: "Android/media/com.whatsapp/WhatsApp/Media", Hint: "photos, videos, voice notes and documents from chats", Checked: true},
		{Label: "WhatsApp Business", Path: "Android/media/com.whatsapp.w4b/WhatsApp Business/Media", Checked: true},
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
// With no path it lists the phone's storages (internal storage, memory cards).
// Folders whose names start with a dot are listed only with hidden=1.
func (s *Server) browse(w http.ResponseWriter, r *http.Request) {
	vols := s.volumes()
	p := r.URL.Query().Get("path")
	if p == "" {
		writeJSON(w, map[string]any{"path": "", "top": true, "dirs": []string{}, "volumes": vols})
		return
	}
	p = filepath.Clean(p)
	entries, err := os.ReadDir(p)
	if err != nil {
		fail(w, 400, "can't open that folder: "+err.Error())
		return
	}
	hidden := r.URL.Query().Get("hidden") == "1"
	dirs := []string{}
	for _, d := range entries {
		if d.IsDir() && (hidden || !strings.HasPrefix(d.Name(), ".")) {
			dirs = append(dirs, d.Name())
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.ToLower(dirs[i]) < strings.ToLower(dirs[j]) })
	parent := filepath.Dir(p)
	var volume *Volume
	for i, v := range vols {
		vp := filepath.Clean(v.Path)
		if p == vp {
			parent = "" // the top of this storage: up goes to the list of storages
		}
		if volume == nil || len(vp) > len(volume.Path) {
			if rel, err := filepath.Rel(vp, p); err == nil && !strings.HasPrefix(rel, "..") {
				volume = &vols[i]
			}
		}
	}
	if parent == p {
		parent = ""
	}
	writeJSON(w, map[string]any{"path": p, "parent": parent, "dirs": dirs, "volumes": vols, "volume": volume})
}

// volumes lists the phone's storages: those Android reported, or else the
// shared storage plus whatever is mounted under /storage.
func (s *Server) volumes() []Volume {
	if s.phone == nil {
		return nil
	}
	s.phone.mu.Lock()
	reported := append([]Volume(nil), s.phone.volumes...)
	s.phone.mu.Unlock()
	if len(reported) > 0 {
		return reported
	}
	out := []Volume{{Label: "Phone storage", Path: s.phone.StorageRoot}}
	entries, _ := os.ReadDir("/storage")
	for _, d := range entries {
		n := d.Name()
		if n == "emulated" || n == "self" {
			continue
		}
		p := "/storage/" + n
		if fi, err := os.Stat(p); err == nil && fi.IsDir() && filepath.Clean(p) != filepath.Clean(s.phone.StorageRoot) {
			if _, err := os.ReadDir(p); err == nil {
				out = append(out, Volume{Label: "Memory card (" + n + ")", Path: p})
			}
		}
	}
	return out
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
