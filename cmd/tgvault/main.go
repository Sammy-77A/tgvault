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
	"time"

	"github.com/Sammy-77A/tgvault/internal/backup"
	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/tgclient"
	"github.com/Sammy-77A/tgvault/internal/web"
)

var version = "0.1.0-dev"

// uiInfo lets a second launch find the running instance.
type uiInfo struct {
	Port  int    `json:"port"`
	Token string `json:"token"`
}

func main() {
	background := flag.Bool("background", false, "start without opening the browser")
	dataFlag := flag.String("data", "", "folder for tgvault's settings and login (default: %APPDATA%\\tgvault)")
	port := flag.Int("port", 8790, "first local port to try for the web page")
	flag.Parse()

	dataDir := *dataFlag
	if dataDir == "" {
		cfg, err := os.UserConfigDir()
		if err != nil {
			fatal(err)
		}
		dataDir = filepath.Join(cfg, "tgvault")
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		fatal(err)
	}
	setupLog(dataDir)

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

	st, err := store.Open(filepath.Join(dataDir, "tgvault.db"))
	if err != nil {
		fatal(err)
	}
	defer st.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	tg := tgclient.New(st, dataDir, version)
	eng := backup.New(st, tg, dataDir)
	tg.OnAccountChange(eng.ForgetAccount)
	addr := fmt.Sprintf("127.0.0.1:%d", actualPort)
	srv := web.New(st, tg, eng, token, addr, version, cancel)

	log.Printf("tgvault %s started; data in %s; page at http://%s", version, dataDir, addr)
	go tg.Run(ctx)
	go func() {
		if err := eng.Run(ctx); err != nil {
			log.Printf("engine: %v", err)
			cancel()
		}
	}()
	if !*background {
		openBrowser(fmt.Sprintf("http://%s/?t=%s", addr, token))
	}
	if err := srv.Serve(ctx, listener); err != nil {
		log.Printf("web: %v", err)
	}
	cancel()
	time.Sleep(500 * time.Millisecond) // let workers record where they stopped
	log.Printf("tgvault stopped")
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
