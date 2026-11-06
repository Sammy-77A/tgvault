// Package app wires tgvault's parts together. It is shared by the PC command
// and the Android library.
package app

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/Sammy-77A/tgvault/internal/backup"
	"github.com/Sammy-77A/tgvault/internal/meta"
	"github.com/Sammy-77A/tgvault/internal/notify"
	"github.com/Sammy-77A/tgvault/internal/pair"
	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/tgclient"
	"github.com/Sammy-77A/tgvault/internal/tmdb"
	"github.com/Sammy-77A/tgvault/internal/web"
)

// Options configures a running tgvault.
type Options struct {
	DataDir     string
	Listener    net.Listener // the local web page listens here
	Token       string       // protects the web page
	Version     string
	Pairing     bool // let a logged-in PC log this device in over the local network
	Phone       *PhoneOptions
	TMDBKey     string // the library app's TMDB key
	SubtitleKey string // the library app's subtitle service key
	Library     string // the library app: only this folder of the backup can be browsed and downloaded
}

// PhoneOptions are set when tgvault runs inside the Android app.
type PhoneOptions struct {
	StorageRoot string                   // shared storage, e.g. /storage/emulated/0
	FileChanged func(path string)        // tell Android's media scanner
	Notify      func(title, body string) // show an Android notification
	Share       func(paths []string)     // open Android's share sheet for files
	Open        func(path string)        // open a file in another app ("Open with…")
}

// App is a running tgvault.
type App struct {
	URL    string // the web page, with its access token
	cancel context.CancelFunc
	done   chan struct{}
	eng    *backup.Engine
	st     *store.Store
	tg     *tgclient.Client
	phone  *web.Phone

	foreground atomic.Bool  // the phone app is on screen
	backups    atomic.Int32 // background backup jobs running
}

// Start opens the database and starts the Telegram client, backup engine,
// notifications and web page. It runs until ctx ends or Stop is called.
func Start(ctx context.Context, o Options) (*App, error) {
	st, err := store.Open(filepath.Join(o.DataDir, "tgvault.db"))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	addr := o.Listener.Addr().String()
	a := &App{URL: fmt.Sprintf("http://%s/?t=%s", addr, o.Token), cancel: cancel, done: make(chan struct{})}

	tmdb.SetCacheDir(filepath.Join(o.DataDir, "cache"))
	tg := tgclient.New(st, o.DataDir, o.Version)
	eng := backup.New(st, tg, o.DataDir)
	a.eng, a.st, a.tg = eng, st, tg
	tg.OnAccountChange(eng.ForgetAccount)
	nw := notify.New(st, tg, a.URL)
	srv := web.New(st, tg, eng, nw, o.Token, addr, o.Version, cancel)
	if p := o.Phone; p != nil {
		a.phone = &web.Phone{StorageRoot: p.StorageRoot, FileChanged: p.FileChanged,
			ShareDir: filepath.Join(o.DataDir, "share"), Share: p.Share, Open: p.Open}
		srv.SetPhone(a.phone)
		tg.SetDevice("Phone", "Android")
		// Uploads in the background run in the backup job, which Android
		// doesn't freeze; outside it, stay connected only for running tasks.
		// The grace period is short because the phone freezes idle apps soon.
		tg.DisconnectWhenIdle(func() bool {
			return a.foreground.Load() || a.backups.Load() > 0 || eng.TasksRunning() || eng.DownloadsActive()
		}, 15*time.Second)
		eng.Configure(backup.Config{Device: meta.DevicePhone, NoProjects: true,
			SkipDirs: []string{a.phone.DownloadDir()}, FileChanged: p.FileChanged})
		if p.Notify != nil {
			nw.SetSender(func(title, body, _ string) error { p.Notify(title, body); return nil })
		}
		o.Pairing = true
	}
	if o.Library != "" {
		srv.SetLibrary(o.Library)
		eng.ReadOnly()
		// The library app is the movie app: its folder is the movie library.
		_ = st.Set(backup.SettingMediaRoot, o.Library)
		// say when a download ends, and when a request is answered
		eng.SetDownloadHook(func(ev string, it backup.DLItem) {
			if ev == "done" {
				nw.Notify("Downloaded: "+it.Title, "Ready to watch offline. Tap to open it.", "")
			} else if ev == "failed" {
				nw.Notify("Download failed: "+it.Title, it.Error+" Tap to retry.", "")
			}
		})
		eng.SetRequestHook(func(state string, r backup.Request) {
			if state == "fulfilled" {
				nw.Notify("Ready to download: "+r.Title, "The owner added it. Open the Library to download it.", "")
			} else if state == "declined" {
				nw.Notify("Not available: "+r.Title, "The owner couldn't add it.", "")
			}
		})
		if o.TMDBKey != "" {
			_ = st.Set(backup.SettingTMDBKey, o.TMDBKey)
		}
		if o.SubtitleKey != "" {
			_ = st.Set(backup.SettingSubtitleKey, o.SubtitleKey)
		}
		o.Pairing = false // nothing in the library app opens to the local network
	}
	if o.Pairing {
		pl := pair.NewListener(tg)
		srv.SetPairing(pl)
		go pl.Run(ctx)
	}

	log.Printf("tgvault %s started; data in %s; page at http://%s", o.Version, o.DataDir, addr)
	go tg.Run(ctx)
	go nw.Run(ctx)
	if o.Library != "" {
		// requests made offline go out as soon as Telegram is back
		go func() {
			t := time.NewTicker(20 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
					eng.FlushQueued(fctx)
					cancel()
				}
			}
		}()
	}
	if o.Phone == nil && o.Library == "" {
		// New files are filed on their own; the owner hears only about problems.
		eng.SetSortHook(func(problems []string) {
			body := problems[0]
			if len(problems) > 1 {
				body += fmt.Sprintf(" (and %d more)", len(problems)-1)
			}
			nw.Notify(fmt.Sprintf("%d file(s) need your attention", len(problems)), body, a.URL+"#movies")
		})
		// On the PC, say when someone asks for a movie or show.
		go eng.WatchRequests(ctx, 20*time.Second, func(rs []backup.Request) {
			for _, r := range rs {
				nw.Notify("New request: "+r.Title, requestSummary(r), a.URL+"#movies")
			}
		})
	}
	go func() {
		if err := eng.Run(ctx); err != nil {
			log.Printf("engine: %v", err)
			cancel()
		}
	}()
	go func() {
		defer close(a.done)
		if err := srv.Serve(ctx, o.Listener); err != nil {
			log.Printf("web: %v", err)
		}
		cancel()
		time.Sleep(500 * time.Millisecond) // let workers record where they stopped
		st.Close()
		log.Printf("tgvault stopped")
	}()
	return a, nil
}

// RunBackup scans and uploads until nothing is left or timeout passes; it
// reports whether everything was done. Used by the phone's background job.
func (a *App) RunBackup(timeout time.Duration) bool {
	a.backups.Add(1)
	defer a.backups.Add(-1)
	return a.eng.RunBackup(timeout, a.done)
}

// SetForeground tells tgvault whether the phone app is on screen, so it stays
// connected to Telegram while it is.
// DownloadStatus is the download queue as JSON, for the phone's notification.
func (a *App) DownloadStatus() string {
	title, done, size, speed, count := a.eng.DownloadSummary()
	b, _ := json.Marshal(map[string]any{"active": a.eng.DownloadsActive(), "title": title, "done": done, "size": size, "speed": speed, "count": count})
	return string(b)
}

// DownloadAction pauses, resumes or cancels downloads from the notification.
func (a *App) DownloadAction(action string) error {
	if action == "cancel-all" {
		for _, it := range a.eng.Downloads() {
			if it.State == "queued" || it.State == "running" || it.State == "paused" {
				_ = a.eng.DownloadControl("cancel", it.Path)
			}
		}
		return nil
	}
	return a.eng.DownloadControl(action, "")
}

func (a *App) SetForeground(on bool) { a.foreground.Store(on) }

// SetConditions passes the phone's network and charging state to the engine.
func (a *App) SetConditions(unmetered, charging bool) {
	a.eng.SetConditions(backup.Conditions{Unmetered: unmetered, Charging: charging})
}

// NetworkChanged reconnects to Telegram over the phone's new network.
func (a *App) NetworkChanged() { a.tg.Reconnect() }

// SetPermissions tells the page which Android permissions are granted.
func (a *App) SetPermissions(p web.PhonePermissions) {
	if a.phone != nil {
		a.phone.SetPermissions(p)
	}
}

// SetVolumes tells the page which storages the phone has (internal storage,
// memory cards), for choosing folders.
// SetMediaStores passes the places the phone offers for downloads.
func (a *App) SetMediaStores(m []backup.MediaStore) { a.eng.SetMediaStores(m) }

func (a *App) SetVolumes(v []web.Volume) {
	if a.phone != nil {
		a.phone.SetVolumes(v)
	}
}

// WifiOnly reports whether the phone should back up only on Wi-Fi (the default).
func (a *App) WifiOnly() bool { return a.st.Get(backup.SettingWifiOnly) != "off" }

// ChargingOnly reports whether the phone should back up only while charging.
func (a *App) ChargingOnly() bool { return a.st.Get(backup.SettingChargingOnly) == "on" }

// Stop shuts tgvault down.
func (a *App) Stop() { a.cancel() }

// Done is closed once tgvault has fully stopped.
func (a *App) Done() <-chan struct{} { return a.done }

// requestSummary says what was asked for, in a notification's one line.
func requestSummary(r backup.Request) string {
	what := "a movie"
	if r.Kind == "show" {
		what = "a show"
		switch len(r.Seasons) {
		case 0:
			what += ", all seasons"
		case 1:
			what += fmt.Sprintf(", season %d", r.Seasons[0])
		default:
			what += fmt.Sprintf(", %d seasons", len(r.Seasons))
		}
	}
	if r.Year > 0 {
		return fmt.Sprintf("%s (%d): %s. Open tgvault to file it and mark it done.", r.Title, r.Year, what)
	}
	return fmt.Sprintf("%s: %s. Open tgvault to file it and mark it done.", r.Title, what)
}
