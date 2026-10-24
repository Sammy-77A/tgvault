// Package mobile is the small API the Android app calls, built with
// gomobile bind. Everything else is shared with the PC version.
package mobile

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/Sammy-77A/tgvault/internal/app"
)

// Version is set by the build.
var Version = "0.2.0-dev"

var (
	mu      sync.Mutex
	running *app.App
)

// Start runs tgvault with its data in dataDir and returns the page's URL.
// Calling it again while running returns the same URL.
func Start(dataDir string) (string, error) {
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
	a, err := app.Start(context.Background(), app.Options{DataDir: dataDir, Listener: l, Token: hex.EncodeToString(buf), Version: Version})
	if err != nil {
		l.Close()
		return "", err
	}
	running = a
	return a.URL, nil
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
