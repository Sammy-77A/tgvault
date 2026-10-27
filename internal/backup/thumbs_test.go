package backup

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Sammy-77A/tgvault/internal/store"
)

// backedUp writes files into a backup folder "Pics", backs them up and
// returns the folder's path.
func backedUp(t *testing.T, e *Engine, st *store.Store, files map[string][]byte) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "Pics")
	for name, data := range files {
		write(t, filepath.Join(dir, name), string(data))
	}
	root, err := st.AddRoot(dir, "Pics")
	if err != nil {
		t.Fatal(err)
	}
	e.scanRoot(root)
	for name := range files {
		it, err := st.ItemByPath("Pics/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if out := e.uploadFile(context.Background(), it, filepath.Join(dir, name)); out.state != store.StateSynced {
			t.Fatalf("upload %s: %+v", name, out)
		}
	}
	return dir
}

func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, w, h))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func thumbs(t *testing.T, e *Engine, ids ...int) map[int]bool {
	t.Helper()
	got, err := e.Thumbs(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestThumbFromTelegram(t *testing.T) {
	tg := newFakeTG()
	e, st := newSplitEngine(t, tg)
	backedUp(t, e, st, map[string][]byte{"cat.png": pngOf(t, 800, 600), "notes.txt": []byte("hello")})
	cat, notes := latest(t, st, "Pics/cat.png"), latest(t, st, "Pics/notes.txt")
	tg.thumbs = map[int][]byte{cat.MsgID: []byte("telegram's thumbnail")}

	got := thumbs(t, e, cat.MsgID, notes.MsgID)
	if !got[cat.MsgID] || got[notes.MsgID] {
		t.Fatalf("got %v", got)
	}
	p, ok := e.ThumbFile(cat.MsgID)
	if data, _ := os.ReadFile(p); !ok || string(data) != "telegram's thumbnail" {
		t.Fatalf("kept %q", data)
	}
	if tg.thumbCalls != 1 {
		t.Fatalf("%d calls to Telegram", tg.thumbCalls)
	}
	// Kept on disk: not fetched again.
	thumbs(t, e, cat.MsgID)
	if tg.thumbCalls != 1 {
		t.Fatal("fetched again")
	}
	if _, ok := e.ThumbFile(notes.MsgID); ok {
		t.Fatal("a text file has a thumbnail")
	}
}

func TestThumbMadeFromLocalPicture(t *testing.T) {
	tg := newFakeTG()
	e, st := newSplitEngine(t, tg)
	dir := backedUp(t, e, st, map[string][]byte{"cat.png": pngOf(t, 800, 600), "dog.png": pngOf(t, 640, 480)})
	cat, dog := latest(t, st, "Pics/cat.png"), latest(t, st, "Pics/dog.png")
	if err := os.Remove(filepath.Join(dir, "dog.png")); err != nil { // only in Telegram now
		t.Fatal(err)
	}

	got := thumbs(t, e, cat.MsgID, dog.MsgID)
	if !got[cat.MsgID] || got[dog.MsgID] {
		t.Fatalf("got %v", got)
	}
	p, _ := e.ThumbFile(cat.MsgID)
	f, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if m, err := jpeg.Decode(f); err != nil || m.Bounds().Dx() != 320 {
		t.Fatalf("made %v, %v", m, err)
	}
	// A picture with no thumbnail anywhere isn't asked about again at once.
	thumbs(t, e, dog.MsgID)
	if tg.thumbCalls != 1 {
		t.Fatalf("%d calls to Telegram", tg.thumbCalls)
	}
}

func TestThumbOfSplitFileFromItsFirstPart(t *testing.T) {
	tg := newFakeTG()
	e, st := newSplitEngine(t, tg)
	data := randomBytes(t, testBigSize)
	it, abs := bigFile(t, e, st, data)
	if out := e.uploadFile(context.Background(), it, abs); out.state != store.StateSynced {
		t.Fatalf("upload: %+v", out)
	}
	v := latest(t, st, "Docs/movie.mkv")
	parts, err := st.Parts(v.MsgID)
	if err != nil || len(parts) < 2 {
		t.Fatalf("parts %v, %v", parts, err)
	}
	tg.thumbs = map[int][]byte{parts[0].MsgID: []byte("frame")}
	if got := thumbs(t, e, v.MsgID); !got[v.MsgID] {
		t.Fatalf("got %v", got)
	}
}

func TestThumbCacheStaysUnderItsLimit(t *testing.T) {
	c := &thumbCache{dir: t.TempDir(), none: map[int]time.Time{}}
	data := make([]byte, 1000)
	keys := []string{"aa01", "bb02", "cc03", "dd04", "ee05"}
	for i, k := range keys[:4] {
		if err := c.save(k, data, 4500); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-time.Duration(10-i) * time.Hour) // aa01 is the oldest
		os.Chtimes(c.path(k), old, old)
	}
	if err := c.save("ee05", data, 4500); err != nil { // 5000 bytes: over the limit
		t.Fatal(err)
	}
	if c.bytes > 3600 {
		t.Fatalf("cache holds %d bytes", c.bytes)
	}
	for i, k := range keys {
		_, err := os.Stat(c.path(k))
		if kept := err == nil; kept != (i >= 2) {
			t.Errorf("%s kept: %v", k, kept)
		}
	}
}
