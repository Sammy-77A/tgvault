package backup

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math"
	"testing"
	"time"

	"github.com/Sammy-77A/tgvault/internal/tgclient"
)

// ebml builds one EBML element with a size that fits in 4 bytes.
func ebml(id uint32, body []byte) []byte {
	var idb []byte
	for _, shift := range []uint{24, 16, 8, 0} {
		if c := byte(id >> shift); len(idb) > 0 || c != 0 {
			idb = append(idb, c)
		}
	}
	size := []byte{0x10 | byte(len(body)>>24), byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}
	return append(append(idb, size...), body...)
}

func TestMkvDurationIsReadFromTheStartOfTheFile(t *testing.T) {
	f32 := make([]byte, 4)
	binary.BigEndian.PutUint32(f32, math.Float32bits(2_700_000)) // ms at the default scale: 45 minutes
	info := append(ebml(0x2AD7B1, []byte{0x0F, 0x42, 0x40}), ebml(0x4489, f32)...)
	seekhead := ebml(0x114D9B74, bytes.Repeat([]byte{0}, 30))
	segBody := append(seekhead, ebml(0x1549A966, info)...)
	// the Segment's length is "unknown" (all ones), as when a file is being written
	segment := append([]byte{0x18, 0x53, 0x80, 0x67, 0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, segBody...)
	file := append(ebml(0x1A45DFA3, []byte("webm")), segment...)
	got, ok := mkvDuration(file)
	if !ok || got != 45*time.Minute {
		t.Fatalf("duration: %v %v", got, ok)
	}
	// the same with the duration before the scale, as 8-byte floats
	f64 := make([]byte, 8)
	binary.BigEndian.PutUint64(f64, math.Float64bits(1500)) // 1500 ticks of 2 ms
	info2 := append(ebml(0x4489, f64), ebml(0x2AD7B1, []byte{0x1E, 0x84, 0x80})...) // 2,000,000 ns
	file2 := append(ebml(0x1A45DFA3, []byte("webm")), append([]byte{0x18, 0x53, 0x80, 0x67, 0x01, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, ebml(0x1549A966, info2)...)...)
	if got, ok := mkvDuration(file2); !ok || got != 3*time.Second {
		t.Fatalf("duration with the scale after it: %v %v", got, ok)
	}
	// a file cut short before the information, or not a Matroska file at all, says nothing
	if _, ok := mkvDuration(file[:20]); ok {
		t.Error("too little of the file")
	}
	if _, ok := mkvDuration([]byte("not a video at all, just text")); ok {
		t.Error("not Matroska")
	}
}

func TestReadyInWaitsUntilTheRestCanArriveWithinTheVideo(t *testing.T) {
	const mb = 1 << 20
	dur := 45 * time.Minute // 2700 s; 90% of it is 2430 s
	cases := []struct {
		name        string
		size, done  int64
		speed       float64
		dur         time.Duration
		want        int
		wantNegOnly bool
	}{
		{"fast connection: now", 800 * mb, 10 * mb, 2 * mb, dur, 0, false},
		{"slow connection: wait for most of it", 800 * mb, 100 * mb, 0.4 * mb, dur, 0, false}, // 700/0.4 = 1750 s < 2430
		{"slower: still waiting", 800 * mb, 10 * mb, 0.25 * mb, dur, 0, false},                // 790/0.25 = 3160 s: 730 s
		{"unknown length", 800 * mb, 100 * mb, mb, 0, -1, false},
		{"no speed yet", 800 * mb, 100 * mb, 0, dur, -1, false},
		{"finished", 800 * mb, 800 * mb, 0, dur, 0, false},
	}
	cases[2].want = int(math.Ceil(float64(790*mb)/(0.25*mb) - 2430))
	for _, c := range cases {
		if got := readyIn(c.size, c.done, c.speed, c.dur); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

// waitPart waits until at least n bytes of the download are on disk.
func waitPart(t *testing.T, e *Engine, p string, n int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if e.dlPartSize(p) >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the download never reached %d bytes (at %d)", n, e.dlPartSize(p))
}

func TestAVideoCanBePlayedWhileItDownloads(t *testing.T) {
	e, tg, p, data := bigRig(t)
	gate := make(chan struct{})
	reached := make(chan struct{}, 8)
	tg.dlFault = func(ctx context.Context, _ int, off int64) error {
		if off == 2*tgclient.BlockSize {
			reached <- struct{}{}
			select {
			case <-gate:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	if _, err := e.QueueDownloads(context.Background(), []string{p}); err != nil {
		t.Fatal(err)
	}
	<-reached
	waitPart(t, e, p, 2*tgclient.BlockSize)
	if !e.PlayableWhileDownloading(p) {
		t.Fatal("two blocks are enough to start playing")
	}
	pv, err := e.OpenGrowing(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer pv.Close()
	if pv.Size != int64(len(data)) {
		t.Fatalf("the player is told the whole length from the start: %d", pv.Size)
	}
	// what has arrived is read from the file
	buf := make([]byte, 1000)
	if _, err := pv.At.ReadAt(buf, 100); err != nil || !bytes.Equal(buf, data[100:1100]) {
		t.Fatalf("the start: %v", err)
	}
	// a read just past it waits for the download to get there
	done := make(chan error, 1)
	var got []byte
	go func() {
		b := make([]byte, 5000)
		_, err := pv.At.ReadAt(b, 2*tgclient.BlockSize+10)
		got = b
		done <- err
	}()
	select {
	case <-done:
		t.Fatal("that part hasn't arrived: the read should wait")
	case <-time.After(150 * time.Millisecond):
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data[2*tgclient.BlockSize+10:2*tgclient.BlockSize+5010]) {
		t.Fatal("the waited-for bytes are wrong")
	}
	waitDownloads(t, e)
	// the part file has become the finished file; reading carries on seamlessly
	all, err := io.ReadAll(io.NewSectionReader(pv.At, 0, pv.Size))
	if err != nil || !bytes.Equal(all, data) {
		t.Fatalf("the whole file after it finished: %v", err)
	}
	// and the download itself was not disturbed
	if d, _ := e.st.DLGet(p); d.State != "done" {
		t.Fatalf("the download finishes as usual: %+v", d)
	}
}

func TestSeekingFarAheadFetchesJustThoseBlocksFromTelegram(t *testing.T) {
	e, tg, p, data := bigRig(t)
	gate := make(chan struct{})
	reached := make(chan struct{}, 8)
	tg.dlFault = func(ctx context.Context, _ int, off int64) error {
		if off == tgclient.BlockSize {
			reached <- struct{}{}
			select {
			case <-gate:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	e.QueueDownloads(context.Background(), []string{p})
	<-reached
	waitPart(t, e, p, tgclient.BlockSize)
	if e.PlayableWhileDownloading(p) {
		t.Fatal("one block isn't enough to offer playing")
	}
	waitPart(t, e, p, tgclient.BlockSize)
	// pause so nothing is downloading; the reader has to ask Telegram
	if err := e.DownloadControl("pause", p); err != nil {
		t.Fatal(err)
	}
	waitState(t, e, p, "paused")
	pv, err := e.OpenGrowing(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer pv.Close()
	off := int64(2*tgclient.BlockSize + 123)
	buf := make([]byte, 4000)
	if _, err := pv.At.ReadAt(buf, off); err != nil || !bytes.Equal(buf, data[off:off+4000]) {
		t.Fatalf("a read past the part comes from Telegram: %v", err)
	}
	// the end of the file too (where a player looks for its index)
	tail := make([]byte, 500)
	if _, err := pv.At.ReadAt(tail, pv.Size-500); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	if !bytes.Equal(tail, data[pv.Size-500:]) {
		t.Fatal("the tail")
	}
	// the part on disk was not touched by any of it
	if got := e.dlPartSize(p); got != tgclient.BlockSize {
		t.Fatalf("reading never changes the download's file: %d", got)
	}
	close(gate)
}

func TestFilesInSeveralMessagesAreNotOfferedWhileDownloading(t *testing.T) {
	e, _, p, _ := bigRig(t)
	e.QueueDownloads(context.Background(), []string{p})
	waitDownloads(t, e)
	// a finished file opens fine; the check for splits is on the version
	if _, err := e.OpenGrowing(context.Background(), p); err != nil {
		t.Fatal(err)
	}
}

// sparse is a file made of a few pieces; everywhere else reads as zeros.
type sparse struct {
	size   int64
	pieces map[int64][]byte
	reads  int
}

func (s *sparse) ReadAt(p []byte, off int64) (int, error) {
	s.reads++
	if off >= s.size {
		return 0, io.EOF
	}
	n := int64(len(p))
	if off+n > s.size {
		n = s.size - off
	}
	for i := int64(0); i < n; i++ {
		p[i] = 0
	}
	for at, b := range s.pieces {
		for i := range b {
			if pos := at + int64(i) - off; pos >= 0 && pos < n {
				p[pos] = b[i]
			}
		}
	}
	if n < int64(len(p)) {
		return int(n), io.EOF
	}
	return int(n), nil
}

func box(typ string, body []byte) []byte {
	b := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(b, uint32(8+len(body)))
	copy(b[4:], typ)
	return append(b, body...)
}

func mvhd(version byte, scale uint32, dur uint64) []byte {
	var b []byte
	if version == 1 {
		b = make([]byte, 4+8+8+4+8)
		b[0] = 1
		binary.BigEndian.PutUint32(b[20:], scale)
		binary.BigEndian.PutUint64(b[24:], dur)
	} else {
		b = make([]byte, 4+4+4+4+4)
		binary.BigEndian.PutUint32(b[12:], scale)
		binary.BigEndian.PutUint32(b[16:], uint32(dur))
	}
	return box("mvhd", b)
}

func TestMp4DurationFromTheMovieHeader(t *testing.T) {
	ftyp := box("ftyp", []byte("isom\x00\x00\x00\x00isom"))
	// moov up front (a file made for streaming)
	moov := box("moov", append(mvhd(0, 1000, 5_400_000), box("trak", make([]byte, 40))...))
	front := append(append(append([]byte{}, ftyp...), moov...), box("mdat", make([]byte, 100))...)
	r := &sparse{size: int64(len(front)), pieces: map[int64][]byte{0: front}}
	if d, ok := mp4Duration(r, r.size); !ok || d != 90*time.Minute {
		t.Fatalf("moov first: %v %v", d, ok)
	}
	// moov at the end after a 700 MB mdat: one hop over the mdat, nothing read inside it
	const mdat = int64(700 << 20)
	hdr := make([]byte, 8)
	binary.BigEndian.PutUint32(hdr, uint32(mdat))
	copy(hdr[4:], "mdat")
	moov2 := box("moov", mvhd(1, 600, 600*3000)) // 3000 s at 600/s
	at := int64(len(ftyp)) + mdat
	r2 := &sparse{size: at + int64(len(moov2)), pieces: map[int64][]byte{0: ftyp, int64(len(ftyp)): hdr, at: moov2}}
	if d, ok := mp4Duration(r2, r2.size); !ok || d != 50*time.Minute {
		t.Fatalf("moov last, version 1: %v %v", d, ok)
	}
	if r2.reads > 12 {
		t.Errorf("the walk should read only box headers: %d reads", r2.reads)
	}
	// not a video, or a moov with no header
	if _, ok := mp4Duration(&sparse{size: 40, pieces: map[int64][]byte{0: []byte("this is certainly not an mp4 file at all!!")[:40]}}, 40); ok {
		t.Error("not an mp4")
	}
	empty := box("moov", box("trak", make([]byte, 20)))
	if _, ok := mp4Duration(&sparse{size: int64(len(ftyp) + len(empty)), pieces: map[int64][]byte{0: append(append([]byte{}, ftyp...), empty...)}}, int64(len(ftyp)+len(empty))); ok {
		t.Error("no movie header")
	}
	// the extension picks the reader
	if d, ok := videoDurationAt(r, r.size, ".MP4"); !ok || d != 90*time.Minute {
		t.Fatalf("by extension: %v %v", d, ok)
	}
	if _, ok := videoDurationAt(r, r.size, ".avi"); ok {
		t.Error("avi isn't read")
	}
}
