package backup

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/Sammy-77A/tgvault/internal/meta"
	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/thumb"
)

const (
	thumbCachePC    = 200 << 20 // bytes of thumbnails kept on the PC
	thumbCachePhone = 50 << 20  // and on the phone
	thumbLocalMax   = 50 << 20  // pictures over this get no thumbnail made here
	thumbBatch      = 100       // most thumbnails asked for at once
	thumbRetry      = 6 * time.Hour
)

// thumbCache keeps thumbnails on disk, named by the file's SHA-256 so the
// same picture in two places or versions is fetched once.
type thumbCache struct {
	dir string

	mu     sync.Mutex
	loaded bool
	bytes  int64
	none   map[int]time.Time // message ID -> when it was found to have no thumbnail
}

func (e *Engine) thumbMax() int64 {
	if e.Device() == meta.DevicePhone {
		return thumbCachePhone
	}
	return thumbCachePC
}

func thumbKey(v store.Version) string {
	if len(v.Hash) >= 8 {
		return v.Hash
	}
	return fmt.Sprintf("msg%08d", v.MsgID)
}

func (c *thumbCache) path(key string) string {
	return filepath.Join(c.dir, key[:2], key+".jpg")
}

// Thumbable says whether a version can have a thumbnail.
func Thumbable(v store.Version) bool {
	if v.Kind != store.KindFile {
		return false
	}
	switch meta.FileType(v.CloudPath) {
	case meta.TypeImage, meta.TypeVideo, meta.TypeAudio:
		return true
	}
	return false
}

// Thumbs makes sure the thumbnails of the given versions are on this device,
// and reports which have one. Telegram's own thumbnail is used when it keeps
// one; otherwise a picture still on this device is shrunk here.
func (e *Engine) Thumbs(ctx context.Context, ids []int) (map[int]bool, error) {
	if len(ids) > thumbBatch {
		ids = ids[:thumbBatch]
	}
	c := e.thumbs
	out := make(map[int]bool, len(ids))
	var want []store.Version
	byFirst := map[int]store.Version{} // the message holding the file (part 1 of a split one) -> version
	for _, id := range ids {
		v, err := e.st.Version(id)
		if err != nil || !Thumbable(v) {
			out[id] = false
			continue
		}
		if _, err := os.Stat(c.path(thumbKey(v))); err == nil {
			out[id] = true
			continue
		}
		c.mu.Lock()
		at, none := c.none[id]
		c.mu.Unlock()
		if none && time.Since(at) < thumbRetry {
			out[id] = false
			continue
		}
		pieces, err := e.pieces(v)
		if err != nil || len(pieces) == 0 {
			out[id] = false
			continue
		}
		byFirst[pieces[0].MsgID] = v
		want = append(want, v)
	}
	if len(want) == 0 {
		return out, nil
	}
	first := make([]int, 0, len(byFirst))
	for m := range byFirst {
		first = append(first, m)
	}
	got, err := e.tg.Thumbs(ctx, first)
	if err != nil {
		return nil, err
	}
	for m, v := range byFirst {
		data, ok := got[m]
		if !ok {
			continue // couldn't be fetched just now; asked again next time
		}
		if data == nil {
			data = e.localThumb(v)
		}
		if data == nil {
			c.mu.Lock()
			c.none[v.MsgID] = time.Now()
			c.mu.Unlock()
			out[v.MsgID] = false
			continue
		}
		if err := c.save(thumbKey(v), data, e.thumbMax()); err != nil {
			return nil, err
		}
		out[v.MsgID] = true
	}
	return out, nil
}

// localThumb shrinks a picture that is still on this device unchanged.
func (e *Engine) localThumb(v store.Version) []byte {
	if meta.FileType(v.CloudPath) != meta.TypeImage || v.Size > thumbLocalMax {
		return nil
	}
	f := e.localCopy(v)
	if f == nil {
		return nil
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil
	}
	t, err := thumb.Make(data)
	if err != nil {
		return nil
	}
	return t
}

// ThumbFile is where the thumbnail of a version is kept, if it is there.
func (e *Engine) ThumbFile(id int) (string, bool) {
	v, err := e.st.Version(id)
	if err != nil || !Thumbable(v) {
		return "", false
	}
	p := e.thumbs.path(thumbKey(v))
	fi, err := os.Stat(p)
	if err != nil {
		return "", false
	}
	// Recently shown thumbnails are the last to be dropped from the cache.
	if time.Since(fi.ModTime()) > 24*time.Hour {
		now := time.Now()
		_ = os.Chtimes(p, now, now)
	}
	return p, true
}

// ClearThumbs removes all thumbnails kept on this device.
func (e *Engine) ClearThumbs() error {
	c := e.thumbs
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bytes, c.loaded = 0, false
	c.none = map[int]time.Time{}
	return os.RemoveAll(c.dir)
}

// ThumbBytes is how much space the thumbnails take.
func (e *Engine) ThumbBytes() int64 {
	c := e.thumbs
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	return c.bytes
}

// load adds up the cache's size the first time it's needed. c.mu is held.
func (c *thumbCache) load() {
	if c.loaded {
		return
	}
	c.loaded = true
	c.bytes = 0
	_ = filepath.WalkDir(c.dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				c.bytes += fi.Size()
			}
		}
		return nil
	})
}

func (c *thumbCache) save(key string, data []byte, limit int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load() // before writing, so the new file isn't counted twice
	p := c.path(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	c.bytes += int64(len(data))
	if c.bytes > limit {
		c.trim(limit * 8 / 10)
	}
	return nil
}

// trim drops the least recently used thumbnails until the cache is under
// target bytes. c.mu is held.
func (c *thumbCache) trim(target int64) {
	type file struct {
		path string
		size int64
		mod  time.Time
	}
	var files []file
	var total int64
	_ = filepath.WalkDir(c.dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				files = append(files, file{p, fi.Size(), fi.ModTime()})
				total += fi.Size()
			}
		}
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].mod.Before(files[j].mod) })
	for _, f := range files {
		if total <= target {
			break
		}
		if os.Remove(f.path) == nil {
			total -= f.size
		}
	}
	c.bytes = total
}
