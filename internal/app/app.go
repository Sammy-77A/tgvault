// Package app wires tgvault's parts together. It is shared by the PC command
// and the Android library.
package app

import (
	"context"
	"fmt"
	"log"
	"net"
	"path/filepath"
	"time"

	"github.com/Sammy-77A/tgvault/internal/backup"
	"github.com/Sammy-77A/tgvault/internal/meta"
	"github.com/Sammy-77A/tgvault/internal/notify"
	"github.com/Sammy-77A/tgvault/internal/pair"
	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/tgclient"
	"github.com/Sammy-77A/tgvault/internal/web"
)

// Options configures a running tgvault.
type Options struct {
	DataDir  string
	Listener net.Listener // the local web page listens here
	Token    string       // protects the web page
	Version  string
	Pairing  bool // let a logged-in PC log this device in over the local network
	Phone    *PhoneOptions
}

// PhoneOptions are set when tgvault runs inside the Android app.
type PhoneOptions struct {
	StorageRoot string                   // shared storage, e.g. /storage/emulated/0
	FileChanged func(path string)        // tell Android's media scanner
	Notify      func(title, body string) // show an Android notification
}

// App is a running tgvault.
type App struct {
	URL    string // the web page, with its access token
	cancel context.CancelFunc
	done   chan struct{}
	eng    *backup.Engine
	st     *store.Store
	phone  *web.Phone
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

	tg := tgclient.New(st, o.DataDir, o.Version)
	eng := backup.New(st, tg, o.DataDir)
	a.eng, a.st = eng, st
	tg.OnAccountChange(eng.ForgetAccount)
	nw := notify.New(st, tg, a.URL)
	srv := web.New(st, tg, eng, nw, o.Token, addr, o.Version, cancel)
	if p := o.Phone; p != nil {
		a.phone = &web.Phone{StorageRoot: p.StorageRoot, FileChanged: p.FileChanged}
		srv.SetPhone(a.phone)
		eng.Configure(backup.Config{Device: meta.DevicePhone, NoProjects: true,
			SkipDirs: []string{a.phone.DownloadDir()}, FileChanged: p.FileChanged})
		if p.Notify != nil {
			nw.SetSender(func(title, body, _ string) error { p.Notify(title, body); return nil })
		}
		o.Pairing = true
	}
	if o.Pairing {
		pl := pair.NewListener(tg)
		srv.SetPairing(pl)
		go pl.Run(ctx)
	}

	log.Printf("tgvault %s started; data in %s; page at http://%s", o.Version, o.DataDir, addr)
	go tg.Run(ctx)
	go nw.Run(ctx)
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
func (a *App) RunBackup(timeout time.Duration) bool { return a.eng.RunBackup(timeout, a.done) }

// SetConditions passes the phone's network and charging state to the engine.
func (a *App) SetConditions(unmetered, charging bool) {
	a.eng.SetConditions(backup.Conditions{Unmetered: unmetered, Charging: charging})
}

// SetPermissions tells the page which Android permissions are granted.
func (a *App) SetPermissions(p web.PhonePermissions) {
	if a.phone != nil {
		a.phone.SetPermissions(p)
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
