// Command tgvault backs up folders to your own Telegram account and serves a
// local web page to manage them.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/Sammy-77A/tgvault/internal/app"
	"github.com/Sammy-77A/tgvault/internal/friends"
)

var version = "0.1.0-dev"

// uiInfo lets a second launch find the running instance.
type uiInfo struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "friends" {
		friendsCommand(os.Args[2:])
		return
	}
	background := flag.Bool("background", false, "start without opening the browser")
	dataFlag := flag.String("data", "", "folder for tgvault's settings and login (default: %APPDATA%\\tgvault)")
	port := flag.Int("port", 8790, "first local port to try for the web page")
	server := flag.Bool("server", false, "run as a file server for friends (no browser; see 'tgvault friends')")
	api := flag.String("api", "127.0.0.1:8800", "with --server: address for the friends' file API (put HTTPS in front of it)")
	publicURL := flag.String("public-url", "", "with --server: the address friends use, e.g. https://files.example.com")
	proxy := flag.Bool("behind-proxy", false, "with --server: trust X-Forwarded-For from a reverse proxy on this machine")
	spoolMax := flag.String("spool-max", "20GB", "with --server: most uploads allowed to wait for Telegram at once (0 = no limit)")
	flag.Parse()

	dataDir := dataPath(*dataFlag)
	setupLog(dataDir)
	if *server {
		*background = true
	}

	infoPath := filepath.Join(dataDir, "ui.json")
	if openExisting(infoPath, *background) {
		return
	}

	listener, actualPort, err := listen(*port)
	if err != nil {
		fatal(fmt.Errorf("no free local port for the web page: %w", err))
	}
	token := loadToken(infoPath)
	if err := writeJSONFile(infoPath, uiInfo{Port: actualPort, Token: token}); err != nil {
		fatal(err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	opts := app.Options{DataDir: dataDir, Listener: listener, Token: token, Version: version}
	if *server {
		max, err := friends.ParseSize(*spoolMax)
		if err != nil {
			fatal(err)
		}
		apiListener, err := net.Listen("tcp", *api)
		if err != nil {
			fatal(fmt.Errorf("friends' API address %s: %w", *api, err))
		}
		opts.Server = &app.ServerOptions{Listener: apiListener, SpoolMax: max, PublicURL: *publicURL, TrustProxy: *proxy}
	}
	a, err := app.Start(ctx, opts)
	if err != nil {
		fatal(err)
	}
	if *server {
		// The management page only listens on this machine; reach it over an
		// SSH tunnel (ssh -L 8790:127.0.0.1:8790 you@server) to log in to Telegram.
		fmt.Println("tgvault server running. Management page:", a.URL)
	}
	if !*background {
		openBrowser(a.URL)
	}
	<-a.Done()
}

// dataPath is the folder for settings, login and logs, created if needed.
func dataPath(flagValue string) string {
	dir := flagValue
	if dir == "" {
		cfg, err := os.UserConfigDir()
		if err != nil {
			fatal(err)
		}
		dir = filepath.Join(cfg, "tgvault")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		fatal(err)
	}
	return dir
}

// openExisting opens the page of an already running tgvault, if any.
func openExisting(infoPath string, background bool) bool {
	var info uiInfo
	b, err := os.ReadFile(infoPath)
	if err != nil || json.Unmarshal(b, &info) != nil || info.Port == 0 {
		return false
	}
	client := http.Client{Timeout: 2 * time.Second}
	res, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/api/ping", info.Port))
	if err != nil {
		return false
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 64))
	if string(body) != "tgvault" {
		return false
	}
	if !background {
		openBrowser(fmt.Sprintf("http://127.0.0.1:%d/?t=%s", info.Port, info.Token))
	}
	return true
}

func listen(first int) (net.Listener, int, error) {
	var lastErr error
	for p := first; p < first+20; p++ {
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err == nil {
			return l, p, nil
		}
		lastErr = err
	}
	return nil, 0, lastErr
}

// loadToken reuses the saved page token so bookmarks keep working.
func loadToken(infoPath string) string {
	var info uiInfo
	if b, err := os.ReadFile(infoPath); err == nil && json.Unmarshal(b, &info) == nil && len(info.Token) == 64 {
		return info.Token
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		fatal(err)
	}
	return hex.EncodeToString(buf)
}

func writeJSONFile(p string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

func setupLog(dataDir string) {
	p := filepath.Join(dataDir, "tgvault.log")
	if fi, err := os.Stat(p); err == nil && fi.Size() > 5<<20 {
		_ = os.Rename(p, p+".old")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	log.SetOutput(io.MultiWriter(f, os.Stderr))
	log.SetFlags(log.LstdFlags)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("open browser: %v; visit %s", err, url)
	}
}

func fatal(err error) {
	log.Printf("fatal: %v", err)
	if !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "tgvault:", err)
	}
	os.Exit(1)
}
