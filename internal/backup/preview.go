package backup

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/Sammy-77A/tgvault/internal/meta"
	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/tgclient"
)

// previewCacheBlocks is how many recent 1 MB blocks are kept in memory, so a
// player's many small range requests don't fetch the same block twice.
const previewCacheBlocks = 32

// Preview is an open file for viewing: it reads and seeks like a file, so it
// can serve HTTP range requests.
type Preview struct {
	io.ReadSeeker
	Size    int64
	ModTime time.Time
	Local   bool // read from this device's disk, not Telegram
	close   func() error
}

// Close releases the preview.
func (p *Preview) Close() error {
	if p.close == nil {
		return nil
	}
	return p.close()
}

// OpenPreview opens a stored file version for viewing. It reads the copy on
// this device when it's unchanged, and otherwise only the parts of the file
// that are read, from Telegram. Project ZIPs aren't previewed.
func (e *Engine) OpenPreview(ctx context.Context, v store.Version) (*Preview, error) {
	if v.Kind == store.KindProject {
		return nil, errors.New("code projects can't be previewed; download or restore them instead")
	}
	mod := time.Unix(v.Mtime, 0)
	if f := e.localCopy(v); f != nil {
		return &Preview{ReadSeeker: f, Size: v.Size, ModTime: mod, Local: true, close: f.Close}, nil
	}
	pieces, err := e.pieces(v)
	if err != nil {
		return nil, err
	}
	r := &blockReader{ctx: ctx, e: e, pieces: pieces, size: v.Size, want: v.Hash, h: sha256.New()}
	return &Preview{ReadSeeker: r, Size: v.Size, ModTime: mod}, nil
}

// localCopy opens the file on this device if it still matches the version.
func (e *Engine) localCopy(v store.Version) *os.File {
	if meta.DeviceOf(v.CloudPath) != e.Device() {
		return nil
	}
	root, rel, ok := e.rootFor(v.CloudPath)
	if !ok {
		return nil
	}
	abs := filepath.Join(root.Path, filepath.FromSlash(rel))
	fi, err := os.Stat(abs)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != v.Size || fi.ModTime().Unix() != v.Mtime {
		return nil
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil
	}
	return f
}

// blockReader reads a stored file from Telegram block by block, at any
// position. When the file is read from start to end in order, it's checked
// against its SHA-256 and the last read fails if it doesn't match, so a
// corrupt picture shows as broken rather than wrong.
type blockReader struct {
	ctx    context.Context
	e      *Engine
	pieces []store.Part // one, or the parts of a split file, in order
	size   int64
	pos    int64

	want     string    // the file's SHA-256
	h        hash.Hash // over bytes read in order from the start
	hashedTo int64     // how far h has got; -1 once reading jumped around
}

func (r *blockReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += r.pos
	case io.SeekEnd:
		offset += r.size
	default:
		return 0, errors.New("bad whence")
	}
	if offset < 0 {
		return 0, errors.New("negative position")
	}
	r.pos = offset
	return offset, nil
}

func (r *blockReader) Read(p []byte) (int, error) {
	if r.pos >= r.size {
		return 0, io.EOF
	}
	// Find the piece holding pos, and the block inside it.
	var start int64
	var piece store.Part
	for _, pc := range r.pieces {
		if r.pos < start+pc.Size {
			piece = pc
			break
		}
		start += pc.Size
	}
	if piece.MsgID == 0 {
		return 0, io.ErrUnexpectedEOF
	}
	inPiece := r.pos - start
	block := inPiece / tgclient.BlockSize
	data, err := r.e.previewBlock(r.ctx, piece.MsgID, block)
	if err != nil {
		return 0, err
	}
	off := inPiece - block*tgclient.BlockSize
	if off >= int64(len(data)) {
		return 0, io.ErrUnexpectedEOF
	}
	n := copy(p, data[off:])
	if r.hashedTo >= 0 && r.want != "" {
		if r.pos == r.hashedTo {
			r.h.Write(p[:n])
			r.hashedTo += int64(n)
			if r.hashedTo == r.size && hex.EncodeToString(r.h.Sum(nil)) != r.want {
				return 0, errors.New("the file failed its integrity check")
			}
		} else {
			r.hashedTo = -1
		}
	}
	r.pos += int64(n)
	return n, nil
}

// blockCache keeps recently read blocks.
type blockCache struct {
	mu    sync.Mutex
	order *list.List // of blockKey, most recent first
	data  map[blockKey]*list.Element
}

type blockKey struct {
	msgID int
	block int64
}

type cachedBlock struct {
	key  blockKey
	data []byte
}

func newBlockCache() *blockCache {
	return &blockCache{order: list.New(), data: map[blockKey]*list.Element{}}
}

func (c *blockCache) get(k blockKey) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.data[k]; ok {
		c.order.MoveToFront(el)
		return el.Value.(cachedBlock).data, true
	}
	return nil, false
}

func (c *blockCache) put(k blockKey, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.data[k]; ok {
		c.order.MoveToFront(el)
		return
	}
	c.data[k] = c.order.PushFront(cachedBlock{k, data})
	for c.order.Len() > previewCacheBlocks {
		old := c.order.Back()
		c.order.Remove(old)
		delete(c.data, old.Value.(cachedBlock).key)
	}
}

func (e *Engine) previewBlock(ctx context.Context, msgID int, block int64) ([]byte, error) {
	k := blockKey{msgID, block}
	if data, ok := e.previews.get(k); ok {
		return data, nil
	}
	data, err := e.tg.ReadBlock(ctx, msgID, block)
	if err != nil {
		return nil, err
	}
	e.previews.put(k, data)
	return data, nil
}
