package backup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Sammy-77A/tgvault/internal/meta"
	"github.com/Sammy-77A/tgvault/internal/store"
)

// Settings that only matter on a phone.
const (
	SettingWifiOnly     = "wifi_only"     // "off" to allow mobile data; on by default
	SettingChargingOnly = "charging_only" // "on" to back up only while charging
)

// Config describes the device tgvault runs on.
type Config struct {
	Device     string   // meta.DevicePC or meta.DevicePhone
	NoProjects bool     // back up code projects as plain folders, never ask
	SkipDirs   []string // absolute folders never backed up (e.g. where restores are saved)
	// FileChanged is called after tgvault creates or deletes a local file, so a
	// phone's gallery can update.
	FileChanged func(path string)
}

// Configure sets up the engine for this device. Call it before Run.
func (e *Engine) Configure(c Config) {
	if c.Device == "" {
		c.Device = meta.DevicePC
	}
	e.cfg = c
}

// Device is this device's name.
func (e *Engine) Device() string {
	if e.cfg.Device == "" {
		return meta.DevicePC
	}
	return e.cfg.Device
}

func (e *Engine) skipAbs(p string) bool {
	for _, d := range e.cfg.SkipDirs {
		if filepath.Clean(d) == p {
			return true
		}
	}
	return false
}

func (e *Engine) fileChanged(p string) {
	if e.cfg.FileChanged != nil {
		e.cfg.FileChanged(p)
	}
}

// Conditions is the phone's network and power state.
type Conditions struct {
	Known     bool // false on a PC: no limits apply
	Unmetered bool // on Wi-Fi (or another network that isn't billed by use)
	Charging  bool
}

// SetConditions records the phone's network and power state.
func (e *Engine) SetConditions(c Conditions) {
	c.Known = true
	e.mu.Lock()
	e.conditions = c
	e.mu.Unlock()
	e.poke()
}

// Blocked returns why uploads are paused by the phone's settings, or "".
func (e *Engine) Blocked() string {
	e.mu.Lock()
	c := e.conditions
	e.mu.Unlock()
	if !c.Known {
		return ""
	}
	if !c.Unmetered && e.st.Get(SettingWifiOnly) != "off" {
		return "Waiting for Wi-Fi (you chose Wi-Fi only)"
	}
	if !c.Charging && e.st.Get(SettingChargingOnly) == "on" {
		return "Waiting for the phone to charge (you chose only while charging)"
	}
	return ""
}

// Idle reports whether there is nothing left to do right now: no scan, no
// upload, and nothing queued except files waiting for a later retry.
func (e *Engine) Idle() bool {
	e.mu.Lock()
	busy := e.scanning || len(e.progress) > 0 || len(e.scanCh) > 0
	e.mu.Unlock()
	if busy {
		return false
	}
	n, err := e.st.CountWaiting()
	return err == nil && n == 0
}

// TasksRunning reports whether a restore, download or other task is running.
func (e *Engine) TasksRunning() bool {
	e.mu.Lock()
	tasks := append([]*Task(nil), e.tasks...)
	e.mu.Unlock()
	for _, t := range tasks {
		if !t.view().Finished {
			return true
		}
	}
	return false
}

// AddRoot starts backing up a folder. Its cloud path is the folder's name,
// under the device's folder on devices other than the PC ("Phone/Camera").
func (e *Engine) AddRoot(p string) (store.Root, error) { return e.AddRootAs(p, "") }

// AddRootAs is AddRoot with a chosen name instead of the folder's own, for
// folders whose name says little (WhatsApp's is "Media").
func (e *Engine) AddRootAs(p, name string) (store.Root, error) {
	p = filepath.Clean(strings.Trim(strings.TrimSpace(p), `"`))
	if !filepath.IsAbs(p) {
		return store.Root{}, errors.New("enter a full folder path, like C:\\Users\\you\\Documents")
	}
	fi, err := os.Stat(p)
	if err != nil || !fi.IsDir() {
		return store.Root{}, errors.New("that folder doesn't exist")
	}
	if filepath.Dir(p) == p {
		return store.Root{}, errors.New("back up folders, not a whole drive")
	}
	roots, err := e.st.Roots()
	if err != nil {
		return store.Root{}, err
	}
	taken := map[string]bool{}
	for _, rt := range roots {
		if rel, err := filepath.Rel(rt.Path, p); err == nil && !strings.HasPrefix(rel, "..") {
			return store.Root{}, errors.New("that folder is already backed up as part of " + rt.Path)
		}
		if rel, err := filepath.Rel(p, rt.Path); err == nil && !strings.HasPrefix(rel, "..") {
			return store.Root{}, errors.New("that folder contains " + rt.Path + ", which is already backed up; remove that one first")
		}
		taken[rt.Label] = true
	}
	name = strings.TrimSpace(strings.ReplaceAll(name, "/", " "))
	if name == "" {
		name = filepath.Base(p)
	}
	prefix := meta.DevicePrefix(e.Device())
	if prefix == "" && meta.DeviceOf(name) != meta.DevicePC {
		return store.Root{}, fmt.Errorf("a folder named %q would mix with your phone's backups; rename it first", name)
	}
	label := name
	for i := 2; taken[joinLabel(prefix, label)]; i++ {
		label = fmt.Sprintf("%s (%d)", name, i) // two folders with the same name
	}
	rt, err := e.st.AddRoot(p, joinLabel(prefix, label))
	if err != nil {
		return store.Root{}, err
	}
	e.RequestScan(rt.ID)
	return rt, nil
}

func joinLabel(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "/" + name
}

// waitIdle blocks until the engine is idle, or the timeout passes. It returns
// whether it became idle.
func (e *Engine) waitIdle(timeout time.Duration, stop <-chan struct{}) bool {
	deadline := time.Now().Add(timeout)
	// Give a requested scan a moment to start before checking.
	time.Sleep(2 * time.Second)
	for time.Now().Before(deadline) {
		if e.Idle() {
			return true
		}
		if e.Blocked() != "" && !e.Status().Scanning {
			return true // nothing can upload until the phone's conditions change
		}
		select {
		case <-stop:
			return false
		case <-time.After(2 * time.Second):
		}
	}
	return false
}

// RunBackup scans every folder and waits until everything queued is uploaded,
// or until timeout. It's used by the phone's background job.
func (e *Engine) RunBackup(timeout time.Duration, stop <-chan struct{}) bool {
	e.RequestScan(0)
	return e.waitIdle(timeout, stop)
}
