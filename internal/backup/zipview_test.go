package backup

import (
	"archive/zip"
	"bytes"
	"compress/flate"
	"context"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Sammy-77A/tgvault/internal/meta"
	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/tgclient"
)

// makeZip builds a ZIP with its files in the given order: name -> content,
// deflated unless the name starts with "stored:" (then it's kept as is, and
// the content is looked up under the full name).
func makeZip(t *testing.T, files map[string][]byte, order ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range order {
		method, name := zip.Deflate, n
		if strings.HasPrefix(n, "stored:") {
			method, name = zip.Store, strings.TrimPrefix(n, "stored:")
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: method, Modified: time.Unix(1_700_000_000, 0)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(files[n]); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// uploadZip backs up data as Docs/pack.zip and returns its version, with the
// local copy removed so everything is read from Telegram.
func uploadZip(t *testing.T, tg *fakeTG, data []byte) (*Engine, store.Version) {
	t.Helper()
	e, st := newSplitEngine(t, tg)
	e.splitAt, e.partSize = meta.MaxFileSize, meta.SplitPartSize // one message
	docs := filepath.Join(t.TempDir(), "Docs")
	abs := filepath.Join(docs, "pack.zip")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, data, 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := st.AddRoot(docs, "Docs")
	if err != nil {
		t.Fatal(err)
	}
	e.scanRoot(root)
	it, err := st.ItemByPath("Docs/pack.zip")
	if err != nil {
		t.Fatal(err)
	}
	if out := e.uploadFile(context.Background(), it, abs); out.state != store.StateSynced {
		t.Fatalf("upload: %+v", out)
	}
	v := latest(t, st, "Docs/pack.zip")
	if err := os.Remove(abs); err != nil {
		t.Fatal(err)
	}
	return e, v
}

func readAllZip(t *testing.T, z *ZipFile) []byte {
	t.Helper()
	if _, err := z.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(z)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func itemNames(items []ZipItem) []string {
	var out []string
	for _, it := range items {
		n := it.Name
		if it.Kind == "dir" {
			n += "/"
		}
		out = append(out, n)
	}
	return out
}

func TestZipBrowseAndOpen(t *testing.T) {
	tg := newFakeTG()
	text := bytes.Repeat([]byte("hello zip "), 500)
	bin := randomBytes(t, 3000)
	order := []string{"a.txt", "stored:photos/x.bin", "photos/deep/y.txt", "../evil.txt", "/abs.txt", "C:/win.txt", "a/../b.txt"}
	data := makeZip(t, map[string][]byte{
		"a.txt":               text,
		"stored:photos/x.bin": bin,
		"photos/deep/y.txt":   []byte("why"),
		"../evil.txt":         []byte("no"),
		"/abs.txt":            []byte("no"),
		"C:/win.txt":          []byte("no"),
		"a/../b.txt":          []byte("no"),
	}, order...)
	e, v := uploadZip(t, tg, data)
	ctx := context.Background()

	items, total, err := e.ZipDir(ctx, v, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(itemNames(items), ","); got != "photos/,a.txt" || total != 3 {
		t.Fatalf("top = %s (%d files); unsafe names must not show", got, total)
	}
	if items[0].Files != 2 || items[0].Size != 3003 {
		t.Fatalf("folder totals: %+v", items[0])
	}
	items, _, _ = e.ZipDir(ctx, v, "photos")
	if got := strings.Join(itemNames(items), ","); got != "deep/,x.bin" {
		t.Fatalf("photos = %s", got)
	}
	if _, _, err := e.ZipDir(ctx, v, "../x"); err == nil {
		t.Fatal("a folder with .. must be refused")
	}

	z, err := e.OpenZipFile(ctx, v, "photos/x.bin")
	if err != nil || !z.Stored {
		t.Fatalf("stored entry: %v", err)
	}
	if got := readAllZip(t, z); !bytes.Equal(got, bin) {
		t.Fatal("stored entry differs")
	}
	z.Seek(2000, io.SeekStart)
	part := make([]byte, 500)
	if _, err := io.ReadFull(z, part); err != nil || !bytes.Equal(part, bin[2000:2500]) {
		t.Fatalf("jump inside a stored entry: %v", err)
	}
	z.Close()

	z, err = e.OpenZipFile(ctx, v, "a.txt")
	if err != nil || z.Stored || z.Size != int64(len(text)) {
		t.Fatalf("compressed entry: %v", err)
	}
	if got := readAllZip(t, z); !bytes.Equal(got, text) {
		t.Fatal("compressed entry differs")
	}
	// Forward, then back: unpacked again from the start.
	z.Seek(3000, io.SeekStart)
	if _, err := io.ReadFull(z, part); err != nil || !bytes.Equal(part, text[3000:3500]) {
		t.Fatalf("jump forward: %v", err)
	}
	z.Seek(10, io.SeekStart)
	if _, err := io.ReadFull(z, part); err != nil || !bytes.Equal(part, text[10:510]) {
		t.Fatalf("jump back: %v", err)
	}
	z.Close()

	if _, err := e.OpenZipFile(ctx, v, "../evil.txt"); err == nil {
		t.Fatal("an unsafe name must not open")
	}
	if _, err := e.OpenZipFile(ctx, v, "nope.txt"); err == nil {
		t.Fatal("a missing file must not open")
	}
}

func TestZipListingReadsOnlyTheEnd(t *testing.T) {
	tg := newFakeTG()
	big := randomBytes(t, 2*tgclient.BlockSize+5000)
	data := makeZip(t, map[string][]byte{"stored:big.bin": big, "stored:z.txt": []byte("tail")}, "stored:big.bin", "stored:z.txt")
	e, v := uploadZip(t, tg, data)
	before := tg.blockReads
	items, _, err := e.ZipDir(context.Background(), v, "")
	if err != nil || len(items) != 2 {
		t.Fatalf("list: %v %v", items, err)
	}
	if n := tg.blockReads - before; n != 1 {
		t.Fatalf("listing read %d blocks, want 1 (the end of the ZIP)", n)
	}
	// Listing again comes from memory.
	e.ZipDir(context.Background(), v, "")
	if n := tg.blockReads - before; n != 1 {
		t.Fatalf("a second listing fetched again (%d)", n)
	}
	// A file near the start reads only its own block.
	z, err := e.OpenZipFile(context.Background(), v, "big.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	z.Seek(0, io.SeekStart)
	got := make([]byte, 100)
	if _, err := io.ReadFull(z, got); err != nil || !bytes.Equal(got, big[:100]) {
		t.Fatalf("read: %v", err)
	}
	if n := tg.blockReads - before; n > 3 {
		t.Fatalf("%d blocks read for 100 bytes", n)
	}
}

func TestZipBombIsStopped(t *testing.T) {
	// An entry that says it is 10 bytes but unpacks to a megabyte.
	var comp bytes.Buffer
	fw, _ := flate.NewWriter(&comp, flate.BestCompression)
	fw.Write(make([]byte, 1<<20))
	fw.Close()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	hdr := &zip.FileHeader{Name: "bomb.txt", Method: zip.Deflate, CRC32: crc32.ChecksumIEEE(make([]byte, 10)),
		CompressedSize64: uint64(comp.Len()), UncompressedSize64: 10}
	w, err := zw.CreateRaw(hdr)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(comp.Bytes())
	zw.Close()
	e, v := uploadZip(t, newFakeTG(), buf.Bytes())
	z, err := e.OpenZipFile(context.Background(), v, "bomb.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	got, err := io.ReadAll(z)
	if len(got) > 10 {
		t.Fatalf("read %d bytes of a 10-byte entry (err %v)", len(got), err)
	}
}

func TestZipWriteAndExtractSelection(t *testing.T) {
	text := bytes.Repeat([]byte("line\n"), 300)
	bin := randomBytes(t, 2000)
	order := []string{"top.txt", "stored:d/one.bin", "d/sub/two.txt", "d2.txt"}
	data := makeZip(t, map[string][]byte{"top.txt": text, "stored:d/one.bin": bin, "d/sub/two.txt": []byte("two"), "d2.txt": []byte("sibling")}, order...)
	e, v := uploadZip(t, newFakeTG(), data)
	ctx := context.Background()

	// A folder and a file as a new ZIP: "d" must not pull in "d2.txt".
	var out bytes.Buffer
	if err := e.WriteZipEntries(ctx, v, []string{"d", "top.txt"}, &out); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(out.Bytes()), int64(out.Len()))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]byte{}
	for _, f := range zr.File {
		rc, _ := f.Open()
		b, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatalf("%s: %v", f.Name, err)
		}
		got[f.Name] = b
	}
	if len(got) != 3 || !bytes.Equal(got["d/one.bin"], bin) || string(got["d/sub/two.txt"]) != "two" || !bytes.Equal(got["top.txt"], text) {
		var names []string
		for n := range got {
			names = append(names, n)
		}
		t.Fatalf("zip holds %v", names)
	}

	// Extract into a folder; an existing different file is kept.
	dest := t.TempDir()
	if err := os.WriteFile(filepath.Join(dest, "top.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan []string, 1)
	if _, err := e.ExtractZipEntries(ctx, v, []string{"d", "top.txt"}, dest, "Restoring", func(w []string) { done <- w }); err != nil {
		t.Fatal(err)
	}
	select {
	case w := <-done:
		if len(w) != 3 {
			t.Fatalf("wrote %v", w)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("extract didn't finish")
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "top.txt")); string(b) != "mine" {
		t.Fatal("overwrote a different file")
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "top (restored).txt")); !bytes.Equal(b, text) {
		t.Fatal("the copy wasn't saved next to it")
	}
	if b, _ := os.ReadFile(filepath.Join(dest, "d", "one.bin")); !bytes.Equal(b, bin) {
		t.Fatal("d/one.bin differs")
	}
	if _, err := e.ExtractZipEntries(ctx, v, []string{"top.txt"}, "", "Restoring", nil); err == nil {
		t.Fatal("a plain ZIP has no original place to restore to")
	}
}

func TestProjectBrowseAcrossChain(t *testing.T) {
	tg := newFakeTG()
	p := newProject(t, tg, 6_000)
	full := p.mustUpload()
	p.edit("lib/a.go", "package lib // a, edited\n")
	p.edit("lib/c.go", "package lib // new\n")
	p.remove("docs/readme.md")
	c1 := p.mustUpload()
	p.edit("main.go", "package main // rewritten\n")
	c2 := p.mustUpload()
	ctx := context.Background()

	// Every version lists exactly the files it rebuilds to.
	for name, v := range map[string]store.Version{"full": full, "c1": c1, "c2": c2} {
		want := p.rebuilt(v)
		var got []string
		var walk func(dir string)
		walk = func(dir string) {
			items, _, err := p.e.ZipDir(ctx, v, dir)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			for _, it := range items {
				if it.Kind == "dir" {
					walk(it.Path)
				} else {
					got = append(got, it.Path)
				}
			}
		}
		walk("")
		sort.Strings(got)
		if w := keys(want); len(w) != len(got) {
			sort.Strings(w)
			t.Fatalf("%s lists %v, want %v", name, got, w)
		}
		for _, f := range got {
			z, err := p.e.OpenZipFile(ctx, v, f)
			if err != nil {
				t.Fatalf("%s: open %s: %v", name, f, err)
			}
			if b := readAllZip(t, z); string(b) != want[f] {
				t.Fatalf("%s: %s differs", name, f)
			}
			z.Close()
		}
	}
	if items, _, _ := p.e.ZipDir(ctx, c2, ""); strings.Contains(strings.Join(itemNames(items), ","), ".tgvault") {
		t.Fatalf("the notes files must not show: %v", itemNames(items))
	}

	// One file back into the project, leaving the rest alone.
	abs := filepath.Join(p.dir, "lib", "a.go")
	os.Remove(filepath.Join(p.dir, "main.go"))
	if err := os.WriteFile(abs, []byte("my newer work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan []string, 1)
	if _, err := p.e.ExtractZipEntries(ctx, c2, []string{"main.go", "lib"}, "", "Restoring", func(w []string) { done <- w }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("restore didn't finish")
	}
	if b, _ := os.ReadFile(filepath.Join(p.dir, "main.go")); string(b) != "package main // rewritten\n" {
		t.Fatalf("main.go = %q", b)
	}
	if b, _ := os.ReadFile(abs); string(b) != "my newer work\n" {
		t.Fatal("a different local file was overwritten")
	}
	if b, _ := os.ReadFile(filepath.Join(p.dir, "lib", "a (restored).go")); string(b) != "package lib // a, edited\n" {
		t.Fatalf("restored copy = %q", b)
	}
}
