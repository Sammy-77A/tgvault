package tgclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/gotd/td/tgerr"
)

// Downloads on a poor connection (a phone moving between Wi-Fi and mobile
// data, a weak signal) fail for a moment now and then. A file is fetched in
// blocks and each block is tried again, for a few minutes if need be, so a big
// download carries on from where it was instead of failing as a whole.
const (
	downloadWindow = 3 // blocks fetched ahead at once
	blockTries     = 12
)

// retryDelay is the wait before try number n (counting from 1); tests shorten it.
var retryDelay = func(n int) time.Duration { return min(time.Duration(n)*time.Second*2, 20*time.Second) }

// transient reports whether a failed request is worth sending again: the
// connection or Telegram having a moment, not a definite answer.
func transient(err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, errGone), errors.Is(err, io.EOF), deadSession(err):
		return false
	}
	if _, ok := tgerr.AsFloodWait(err); ok {
		return true
	}
	if _, rpc := tgerr.As(err); rpc {
		return tgerr.IsCode(err, 500) // Telegram's own hiccups (RPC_CALL_FAIL, INTERDC_..._CALL_ERROR)
	}
	return true // the connection itself, or a request that timed out
}

// retry runs do until it works, waiting longer between tries, for as long as
// failures look temporary and ctx is alive.
func (c *Client) retry(ctx context.Context, what string, do func() error) error {
	var err error
	for try := 1; ; try++ {
		if err = do(); err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !transient(err) || try >= blockTries {
			return err
		}
		wait := retryDelay(try)
		if d, ok := tgerr.AsFloodWait(err); ok {
			wait = min(d+time.Second, 5*time.Minute)
		}
		if try > 2 {
			log.Printf("telegram: %s failed (%v); trying again in %s (%d of %d)", what, err, wait.Round(time.Second), try, blockTries)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		c.waitOnline(ctx, 10*time.Second)
	}
}

// Download writes a stored file to w.
func (c *Client) Download(ctx context.Context, msgID int, w io.Writer) error {
	var fl fileLoc
	if err := c.retry(ctx, "looking up the file", func() (err error) {
		fl, err = c.fileLoc(ctx, msgID, false)
		return err
	}); err != nil {
		return err
	}
	blocks := (fl.size + BlockSize - 1) / BlockSize

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		b   []byte
		err error
	}
	ahead := make(chan chan result, downloadWindow)
	go func() {
		defer close(ahead)
		for n := int64(0); n < blocks; n++ {
			ch := make(chan result, 1)
			select {
			case ahead <- ch:
			case <-ctx.Done():
				return
			}
			go func() {
				want := min(BlockSize, fl.size-n*BlockSize)
				var b []byte
				err := c.retry(ctx, fmt.Sprintf("downloading block %d of %d", n+1, blocks), func() (err error) {
					if b, err = c.ReadBlock(ctx, msgID, n); err == nil && int64(len(b)) != want {
						err = fmt.Errorf("Telegram sent %d bytes of block %d, expected %d", len(b), n+1, want)
					}
					return err
				})
				ch <- result{b, err}
			}()
		}
	}()
	for ch := range ahead {
		r := <-ch
		if r.err != nil {
			return r.err
		}
		if _, err := w.Write(r.b); err != nil {
			return err
		}
	}
	return ctx.Err()
}
