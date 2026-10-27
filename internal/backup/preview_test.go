package backup

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/Sammy-77A/tgvault/internal/meta"
	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/tgclient"
)

// readAt reads n bytes at off through the preview, as a range request would.
func readAt(t *testing.T, p *Preview, off, n int64) []byte {
	t.Helper()
	if _, err := p.Seek(off, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(p, buf); err != nil {
		t.Fatalf("read %d bytes at %d: %v", n, off, err)
	}
	return buf
}

func TestPreviewReadsOnlyWhatIsAsked(t *testing.T) {
	tg := newFakeTG()
	e, st := newSplitEngine(t, tg)
	e.splitAt, e.partSize = meta.MaxFileSize, meta.SplitPartSize // one message
	data := randomBytes(t, 2*tgclient.BlockSize+12345)
	it, abs := bigFile(t, e, st, data)
	if out := e.uploadFile(context.Background(), it, abs); out.state != store.StateSynced {
		t.Fatalf("upload: %+v", out)
	}
	v := latest(t, st, "Docs/movie.mkv")
	if err := os.Remove(abs); err != nil { // only in Telegram now
		t.Fatal(err)
	}

	p, err := e.OpenPreview(context.Background(), v)
	if err != nil || p.Local {
		t.Fatalf("open: %v, local %v", err, p != nil && p.Local)
	}
	defer p.Close()
	if end, _ := p.Seek(0, io.SeekEnd); end != int64(len(data)) {
		t.Fatalf("size %d, want %d", end, len(data))
	}
	// Across the edge of the first and second block.
	off := int64(tgclient.BlockSize - 100)
	if got := readAt(t, p, off, 300); !bytes.Equal(got, data[off:off+300]) {
		t.Fatal("bytes across a block edge differ")
	}
	if tg.blockReads != 2 {
		t.Fatalf("%d blocks fetched, want 2", tg.blockReads)
	}
	// Read again: from the cache.
	readAt(t, p, off, 300)
	if tg.blockReads != 2 {
		t.Fatalf("cached blocks fetched again (%d reads)", tg.blockReads)
	}
	// The tail.
	if got := readAt(t, p, int64(len(data))-10, 10); !bytes.Equal(got, data[len(data)-10:]) {
		t.Fatal("tail differs")
	}
	if n, err := p.Read(make([]byte, 1)); n != 0 || err != io.EOF {
		t.Fatalf("read past the end: %d, %v", n, err)
	}
}

func TestPreviewWholeFileIsChecked(t *testing.T) {
	tg := newFakeTG()
	e, st := newSplitEngine(t, tg)
	e.splitAt, e.partSize = meta.MaxFileSize, meta.SplitPartSize
	data := randomBytes(t, 5000)
	it, abs := bigFile(t, e, st, data)
	e.uploadFile(context.Background(), it, abs)
	v := latest(t, st, "Docs/movie.mkv")
	os.Remove(abs)

	p, _ := e.OpenPreview(context.Background(), v)
	got, err := io.ReadAll(p)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("whole read: %v", err)
	}

	tg.msgs[v.MsgID].data[2500] ^= 0xff // damaged in Telegram
	e2, _ := newSplitEngine(t, tg)      // a fresh cache
	e2.st = st
	p2, _ := e2.OpenPreview(context.Background(), v)
	if _, err := io.ReadAll(p2); err == nil {
		t.Fatal("a damaged file read whole must fail")
	}
}

func TestPreviewSplitFileAndLocalCopy(t *testing.T) {
	tg := newFakeTG()
	e, st := newSplitEngine(t, tg) // parts of 4096 bytes
	data := randomBytes(t, testBigSize)
	it, abs := bigFile(t, e, st, data)
	e.uploadFile(context.Background(), it, abs)
	v := latest(t, st, "Docs/movie.mkv")

	// The unchanged copy on this device is used, without Telegram.
	p, err := e.OpenPreview(context.Background(), v)
	if err != nil || !p.Local {
		t.Fatalf("expected the local copy: %v", err)
	}
	p.Close()

	// Changed locally: read from Telegram, across the parts.
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(abs, later, later); err != nil {
		t.Fatal(err)
	}
	p, err = e.OpenPreview(context.Background(), v)
	if err != nil || p.Local {
		t.Fatalf("expected Telegram: %v", err)
	}
	defer p.Close()
	if got := readAt(t, p, 4000, 5000); !bytes.Equal(got, data[4000:9000]) {
		t.Fatal("bytes across parts differ")
	}
	all, err := io.ReadAll(io.NewSectionReader(readerAt{p}, 0, int64(len(data))))
	if err != nil || !bytes.Equal(all, data) {
		t.Fatalf("whole split file: %v", err)
	}
}

// readerAt adapts a Preview for io.SectionReader.
type readerAt struct{ p *Preview }

func (r readerAt) ReadAt(b []byte, off int64) (int, error) {
	if _, err := r.p.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	return io.ReadFull(r.p, b)
}

func TestPreviewRefusesProjects(t *testing.T) {
	tg := newFakeTG()
	p := newProject(t, tg, 1000)
	v := p.mustUpload()
	if _, err := p.e.OpenPreview(context.Background(), v); err == nil {
		t.Fatal("projects must not be previewed")
	}
}
