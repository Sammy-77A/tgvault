// Package mobile is the small API the Android app calls, built with
// gomobile bind. Everything else is shared with the PC version.
package mobile

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Sammy-77A/tgvault/internal/app"
	"github.com/Sammy-77A/tgvault/internal/web"
)

// Version is set by the build.
var Version = "0.2.0-dev"

// Host is implemented by the Android app.
type Host interface {
	// ShowNotification shows a notification; tapping it opens tgvault.
	ShowNotification(title, body string)
	// FileChanged tells Android's media scanner that a file was created or deleted.
	FileChanged(path string)
	// ShareFiles opens Android's share sheet for files, one path per line.
	ShareFiles(paths string)
	// OpenFile lets the user pick another app to open a file with.
	OpenFile(path string)
}

var (
	mu      sync.Mutex
	running *app.App
)

// Start runs tgvault with its data in dataDir and returns the page's URL.
// storageRoot is the phone's shared storage. Calling Start again while
// running returns the same URL.
func Start(dataDir, storageRoot string, host Host) (string, error) {
	mu.Lock()
	defer mu.Unlock()
	if running != nil {
		select {
		case <-running.Done():
			running = nil
		default:
			return running.URL, nil
		}
	}
	if host == nil {
		return "", errors.New("no host")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", err
	}
	setupLog(dataDir)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		l.Close()
		return "", err
	}
	a, err := app.Start(context.Background(), app.Options{
		DataDir: dataDir, Listener: l, Token: hex.EncodeToString(buf), Version: Version,
		Phone: &app.PhoneOptions{StorageRoot: storageRoot, FileChanged: host.FileChanged, Notify: host.ShowNotification,
			Share: func(paths []string) { host.ShareFiles(strings.Join(paths, "\n")) }, Open: host.OpenFile},
	})
	if err != nil {
		l.Close()
		return "", err
	}
	running = a
	return a.URL, nil
}

func current() *app.App {
	mu.Lock()
	defer mu.Unlock()
	return running
}

// Stop shuts tgvault down and waits until it has saved its state.
func Stop() {
	mu.Lock()
	a := running
	running = nil
	mu.Unlock()
	if a != nil {
		a.Stop()
		<-a.Done()
	}
}

// RunBackup scans and uploads until nothing is left to do, for at most
// timeoutSeconds. It reports whether everything was done. Call Start first.
func RunBackup(timeoutSeconds int) bool {
	a := current()
	if a == nil {
		return false
	}
	return a.RunBackup(time.Duration(timeoutSeconds) * time.Second)
}

// SetConditions passes whether the phone is on an unmetered network (Wi-Fi)
// and whether it's charging.
func SetConditions(unmetered, charging bool) {
	if a := current(); a != nil {
		a.SetConditions(unmetered, charging)
	}
}

// SetForeground tells tgvault whether the app is on screen. In the
// background it disconnects from Telegram when there's nothing to do.
func SetForeground(on bool) {
	if a := current(); a != nil {
		a.SetForeground(on)
	}
}

// NetworkChanged tells tgvault the phone switched to another network (for
// example from Wi-Fi to mobile data), so it reconnects to Telegram over it.
func NetworkChanged() {
	if a := current(); a != nil {
		a.NetworkChanged()
	}
}

// SetPermissions passes which Android permissions are granted, so the page
// can ask for missing ones.
func SetPermissions(storage, notifications, battery bool) {
	if a := current(); a != nil {
		a.SetPermissions(web.PhonePermissions{Storage: storage, Notifications: notifications, Battery: battery})
	}
}

// WifiOnly reports whether backups should wait for Wi-Fi.
func WifiOnly() bool {
	if a := current(); a != nil {
		return a.WifiOnly()
	}
	return true
}

// ChargingOnly reports whether backups should wait for charging.
func ChargingOnly() bool {
	if a := current(); a != nil {
		return a.ChargingOnly()
	}
	return false
}

// LogPath is where tgvault writes its log, for the app's "Share log" button.
func LogPath(dataDir string) string { return filepath.Join(dataDir, "tgvault.log") }

func setupLog(dataDir string) {
	p := LogPath(dataDir)
	if fi, err := os.Stat(p); err == nil && fi.Size() > 5<<20 {
		_ = os.Rename(p, p+".old")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("log: %v", err)
		return
	}
	log.SetOutput(io.MultiWriter(f, os.Stderr))
}
