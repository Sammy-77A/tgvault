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
	"github.com/Sammy-77A/tgvault/internal/notify"
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
}

// App is a running tgvault.
type App struct {
	URL    string // the web page, with its access token
	cancel context.CancelFunc
	done   chan struct{}
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
	tg.OnAccountChange(eng.ForgetAccount)
	nw := notify.New(st, tg, a.URL)
	srv := web.New(st, tg, eng, nw, o.Token, addr, o.Version, cancel)

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

// Stop shuts tgvault down.
func (a *App) Stop() { a.cancel() }

// Done is closed once tgvault has fully stopped.
func (a *App) Done() <-chan struct{} { return a.done }
