package backup

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path"
	"sync"
	"time"

	"github.com/Sammy-77A/tgvault/internal/media"
	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/tgclient"
)

// Playing a video while it is still downloading. The download carries on exactly
// as it does when nobody is watching and is never touched from here; the
// player reads the half-written file (only ever the whole blocks of it) and, for
// anything further on, such as after seeking ahead, asks Telegram for just those
// blocks, the same way a preview does.

var (
	// A read just past the end of the downloaded part waits for the download to
	// get there, instead of fetching the same blocks a second time.
	growWaitAhead = int64(6 << 20)
	growWaitMax   = 25 * time.Second
	growPoll      = 150 * time.Millisecond
)

// minPlayable is how much has to be on disk before playing is offered: enough
// for the start of the file, where the video's details are.
const minPlayable = 2 * tgclient.BlockSize

type growing struct {
	e     *Engine
	ctx   context.Context
	v     store.Version
	cloud string
	final string
	tg    *blockReader

	mu   sync.Mutex
	size int64
	pos  int64
}

// OpenGrowing opens a library video that is downloading (or finished) for
// playing. It fails for files stored in several messages, which are downloaded
// whole.
func (e *Engine) OpenGrowing(ctx context.Context, cloudPath string) (*Preview, error) {
	if !e.inMediaRoot(cloudPath) {
		return nil, errors.New("that isn't in the library")
	}
	vs, err := e.st.LatestUnder(cloudPath)
	if err != nil {
		return nil, err
	}
	var v store.Version
	for _, x := range vs {
		if x.CloudPath == cloudPath {
			v = x
		}
	}
	if v.MsgID == 0 {
		return nil, errors.New("this file is no longer in the library")
	}
	if v.Parts > 0 {
		return nil, errors.New("a file over 2 GB can't be played while it downloads")
	}
	pieces, err := e.pieces(v)
	if err != nil {
		return nil, err
	}
	g := &growing{e: e, ctx: ctx, v: v, cloud: cloudPath, final: e.MediaLocal(cloudPath), size: v.Size,
		tg: &blockReader{ctx: ctx, e: e, pieces: pieces, size: v.Size, hashedTo: -1}}
	return &Preview{ReadSeeker: g, At: g, Size: v.Size, ModTime: time.Unix(v.Mtime, 0), close: g.Close}, nil
}

func (g *growing) Close() error { return nil }

// onDisk says which file holds the start of the video and how much of it can be
// trusted: a finished file in full, otherwise the whole blocks of the part file.
func (g *growing) onDisk() (file string, avail int64) {
	if fi, err := os.Stat(g.final); err == nil && !fi.IsDir() && fi.Size() == g.size {
		return g.final, g.size
	}
	part := g.final + ".part"
	if fi, err := os.Stat(part); err == nil && !fi.IsDir() {
		n := fi.Size()
		if n >= g.size {
			return part, g.size
		}
		return part, n / tgclient.BlockSize * tgclient.BlockSize
	}
	return "", 0
}

// fromDisk reads from the file and lets go of it again straight away. A file
// held open can't be renamed on some systems, and the download must be free
// to move its finished file into place at any moment.
func (g *growing) fromDisk(p []byte, off int64, file string) (int, error) {
	f, err := os.Open(file)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n, err := f.ReadAt(p, off)
	if err == io.EOF && n > 0 {
		err = nil
	}
	return n, err
}

func (g *growing) ReadAt(p []byte, off int64) (int, error) {
	n := 0
	for n < len(p) {
		if off+int64(n) >= g.size {
			return n, io.EOF
		}
		at := off + int64(n)
		want := int64(len(p) - n)
		if at+want > g.size {
			want = g.size - at
		}
		file, avail := g.onDisk()
		if at < avail {
			if at+want > avail {
				want = avail - at
			}
			m, err := g.fromDisk(p[n:n+int(want)], at, file)
			n += m
			if err != nil && m == 0 {
				// the part was renamed into place as we opened it; look again
				if _, again := g.onDisk(); again > at {
					continue
				}
				return n, err
			}
			continue
		}
		// Past what has arrived. If the download is close behind, let it catch up.
		if at-avail < growWaitAhead && g.e.dlRunning(g.cloud) {
			deadline := time.Now().Add(growWaitMax)
			for time.Now().Before(deadline) {
				if _, now := g.onDisk(); now > at {
					break
				}
				select {
				case <-g.ctx.Done():
					return n, g.ctx.Err()
				case <-time.After(growPoll):
				}
				if !g.e.dlRunning(g.cloud) {
					break
				}
			}
			if _, now := g.onDisk(); now > at {
				continue
			}
		}
		// Otherwise ask Telegram for just these bytes (one block at most).
		if lim := tgclient.BlockSize - at%tgclient.BlockSize; want > lim {
			want = lim
		}
		m, err := g.tg.ReadAt(p[n:n+int(want)], at)
		n += m
		if err != nil && m == 0 {
			return n, err
		}
	}
	return n, nil
}

func (g *growing) Read(p []byte) (int, error) {
	g.mu.Lock()
	pos := g.pos
	g.mu.Unlock()
	if pos >= g.size {
		return 0, io.EOF
	}
	n, err := g.ReadAt(p, pos)
	g.mu.Lock()
	g.pos = pos + int64(n)
	g.mu.Unlock()
	if n > 0 && err == io.EOF {
		err = nil
	}
	return n, err
}

func (g *growing) Seek(offset int64, whence int) (int64, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += g.pos
	case io.SeekEnd:
		offset += g.size
	default:
		return 0, errors.New("bad whence")
	}
	if offset < 0 {
		return 0, errors.New("negative position")
	}
	g.pos = offset
	return offset, nil
}

// dlRunning reports whether a download of this file is being fetched now.
func (e *Engine) dlRunning(cloudPath string) bool {
	e.dl.mu.Lock()
	defer e.dl.mu.Unlock()
	return e.dl.live[cloudPath] != nil
}

// PlayableWhileDownloading reports whether a library video that is downloading
// has enough of its start on disk to be played now.
func (e *Engine) PlayableWhileDownloading(cloudPath string) bool {
	d, ok := e.st.DLGet(cloudPath)
	return ok && e.canPlayPart(d)
}

// canPlayPart: a single-message video with enough of its start on disk.
func (e *Engine) canPlayPart(d store.MBDownload) bool {
	if d.State == "done" || !media.IsVideo(path.Ext(d.Path)) {
		return false
	}
	if e.dlPartSize(d.Path) < minPlayable {
		return false
	}
	v, err := e.st.Version(d.MsgID)
	return err == nil && v.Parts == 0
}

// ---- how long the video is, from the start of the file ----

// readyIn estimates how many seconds to wait before playing from the start can
// carry on to the end without stopping: the rest of the download has to arrive
// within the length of the video (with a tenth to spare). 0 means it can start
// now; -1 means not known.
func readyIn(size, done int64, speed float64, dur time.Duration) int {
	if dur <= 0 || size <= 0 {
		return -1
	}
	if done >= size {
		return 0
	}
	if speed < 1 {
		return -1
	}
	need := float64(size-done)/speed - dur.Seconds()*0.9
	if need <= 0 {
		return 0
	}
	return int(math.Ceil(need))
}

// videoDuration reads the length of a Matroska (mkv, webm) video from its first
// bytes, where the file's information lives. It returns false for anything else
// or when the part read so far doesn't say.
func videoDuration(file string) (time.Duration, bool) {
	f, err := os.Open(file)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	buf := make([]byte, 128<<10)
	n, _ := io.ReadFull(f, buf)
	return mkvDuration(buf[:n])
}

func ebmlVint(b []byte, keepMarker bool) (val uint64, n int, unknown bool) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, false
	}
	n = 1
	for mask := byte(0x80); b[0]&mask == 0; mask >>= 1 {
		n++
	}
	if n > 8 || len(b) < n {
		return 0, 0, false
	}
	v := uint64(b[0])
	if !keepMarker {
		v &= uint64(0xff>>uint(n)) // drop the length marker
	}
	allOnes := !keepMarker && v == uint64(0xff>>uint(n))
	for i := 1; i < n; i++ {
		v = v<<8 | uint64(b[i])
		if b[i] != 0xff {
			allOnes = false
		}
	}
	return v, n, allOnes
}

func mkvDuration(b []byte) (time.Duration, bool) {
	const (
		idSegment   = 0x18538067
		idInfo      = 0x1549A966
		idTimeScale = 0x2AD7B1
		idDuration  = 0x4489
	)
	scale := uint64(1_000_000)
	var dur float64
	var haveDur bool
	var walk func(b []byte) bool
	walk = func(b []byte) bool {
		for len(b) > 0 {
			id, n, _ := ebmlVint(b, true)
			if n == 0 {
				return haveDur
			}
			size, m, unknown := ebmlVint(b[n:], false)
			if m == 0 {
				return haveDur
			}
			body := b[n+m:]
			if unknown || size > uint64(len(body)) {
				size = uint64(len(body)) // an unknown, or not yet downloaded, length: what there is
			}
			switch id {
			case idSegment, idInfo:
				if walk(body[:size]) {
					return true
				}
			case idTimeScale:
				var v uint64
				for _, c := range body[:size] {
					v = v<<8 | uint64(c)
				}
				if v > 0 {
					scale = v
				}
			case idDuration:
				switch size {
				case 4:
					dur, haveDur = float64(math.Float32frombits(binary.BigEndian.Uint32(body[:4]))), true
				case 8:
					dur, haveDur = math.Float64frombits(binary.BigEndian.Uint64(body[:8])), true
				}
			}
			b = body[size:]
		}
		return haveDur
	}
	if !walk(b) || dur <= 0 {
		return 0, false
	}
	return time.Duration(dur * float64(scale)), true
}

// durationOf is the length of the video in a download, remembered once known.
func (e *Engine) durationOf(cloudPath string) time.Duration {
	e.durMu.Lock()
	d, ok := e.durs[cloudPath]
	e.durMu.Unlock()
	if ok {
		return d
	}
	file := e.MediaLocal(cloudPath)
	if _, err := os.Stat(file); err != nil {
		file += ".part"
	}
	if dur, ok := videoDuration(file); ok {
		e.durMu.Lock()
		if e.durs == nil {
			e.durs = map[string]time.Duration{}
		}
		e.durs[cloudPath] = dur
		e.durMu.Unlock()
		return dur
	}
	return 0
}
