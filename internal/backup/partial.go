package backup

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path"
	"strings"
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

// videoDurationAt reads the length of a video from its container: Matroska (mkv,
// webm) from the first bytes of the file, MP4 (mp4, m4v, mov) from its "moov" box,
// which may sit at the end of the file (then r reads it from Telegram). It
// returns false for anything else or when the file doesn't say.
func videoDurationAt(r io.ReaderAt, size int64, ext string) (time.Duration, bool) {
	switch strings.ToLower(ext) {
	case ".mkv", ".webm":
		buf := make([]byte, 128<<10)
		if int64(len(buf)) > size {
			buf = buf[:size]
		}
		n, _ := r.ReadAt(buf, 0)
		return mkvDuration(buf[:n])
	case ".mp4", ".m4v", ".mov":
		return mp4Duration(r, size)
	}
	return 0, false
}

// mp4Duration walks the top-level boxes (each hop is one small read, so a huge
// "mdat" is jumped over) to the "moov" box and reads the length from the
// movie header at its start.
func mp4Duration(r io.ReaderAt, size int64) (time.Duration, bool) {
	boxAt := func(off int64) (typ string, start, total int64, ok bool) {
		var h [16]byte
		n, _ := r.ReadAt(h[:], off)
		if n < 8 {
			return "", 0, 0, false
		}
		total = int64(binary.BigEndian.Uint32(h[0:4]))
		start = 8
		switch total {
		case 1: // a 64-bit size follows
			if n < 16 {
				return "", 0, 0, false
			}
			total = int64(binary.BigEndian.Uint64(h[8:16]))
			start = 16
		case 0: // to the end of the file
			total = size - off
		}
		if total < start {
			return "", 0, 0, false
		}
		return string(h[4:8]), start, total, true
	}
	off := int64(0)
	for hops := 0; hops < 64 && off < size; hops++ {
		typ, start, total, ok := boxAt(off)
		if !ok {
			return 0, false
		}
		if typ == "moov" {
			// the children of moov: the movie header is nearly always the first
			child := off + start
			end := off + total
			for k := 0; k < 16 && child < end; k++ {
				ct, cs, ctotal, ok := boxAt(child)
				if !ok {
					return 0, false
				}
				if ct == "mvhd" {
					var b [32]byte
					n, _ := r.ReadAt(b[:], child+cs)
					if n < 20 {
						return 0, false
					}
					var scale uint32
					var dur uint64
					if b[0] == 1 { // version 1: 64-bit times
						if n < 32 {
							return 0, false
						}
						scale = binary.BigEndian.Uint32(b[20:24])
						dur = binary.BigEndian.Uint64(b[24:32])
					} else {
						scale = binary.BigEndian.Uint32(b[12:16])
						dur = uint64(binary.BigEndian.Uint32(b[16:20]))
					}
					if scale == 0 || dur == 0 {
						return 0, false
					}
					return time.Duration(float64(dur) / float64(scale) * float64(time.Second)), true
				}
				child += ctotal
			}
			return 0, false
		}
		off += total
	}
	return 0, false
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

// durationOf is the length of the video in a download, once known. It never
// waits: the first call starts reading it in the background (an mp4 may have to
// fetch its end from Telegram), and a later call gets the answer. Until then,
// and for files that don't say, it is 0.
func (e *Engine) durationOf(cloudPath string) time.Duration {
	e.durMu.Lock()
	if e.durs == nil {
		e.durs = map[string]*durState{}
	}
	st := e.durs[cloudPath]
	if st == nil {
		st = &durState{}
		e.durs[cloudPath] = st
	}
	d := st.d
	start := d == 0 && !st.busy && st.fails < 3 && time.Since(st.tried) > 30*time.Second
	if start {
		st.busy = true
		st.tried = time.Now()
	}
	e.durMu.Unlock()
	if start {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			var dur time.Duration
			if pv, err := e.OpenGrowing(ctx, cloudPath); err == nil {
				dur, _ = videoDurationAt(pv.At, pv.Size, path.Ext(cloudPath))
				pv.Close()
			}
			e.durMu.Lock()
			st.d, st.busy = dur, false
			if dur == 0 {
				st.fails++
			}
			e.durMu.Unlock()
		}()
	}
	return d
}

type durState struct {
	d     time.Duration
	busy  bool
	fails int
	tried time.Time
}
