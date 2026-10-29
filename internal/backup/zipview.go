package backup

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/tgclient"
)

// Looking inside ZIPs and code projects without downloading them: a ZIP keeps
// its table of contents at its end, so listing reads only that, and opening a
// file inside reads only that file's bytes. A project is a full ZIP plus
// change ZIPs; its listing merges them the way a rebuild would, and each file
// is read from whichever ZIP holds its latest copy.

// zipListCap is how many listings are kept in memory.
const zipListCap = 8

// maxZipEntries stops a ZIP that claims an absurd number of files.
const maxZipEntries = 2_000_000

// ZipItem is one thing inside a ZIP or project, as shown when browsing it.
type ZipItem struct {
	Name   string `json:"name"`            // inside the folder being shown
	Path   string `json:"path"`            // inside the ZIP
	Kind   string `json:"kind"`            // "dir" or "file"
	Size   int64  `json:"size"`            // a file's unpacked size; a folder's total
	Files  int    `json:"files,omitempty"` // folders: files inside, all levels
	Mtime  int64  `json:"mtime"`
	Locked bool   `json:"locked,omitempty"` // password-protected
	Stored bool   `json:"stored,omitempty"` // not compressed, so it can be jumped around in
}

type zipEntry struct {
	path   string // cleaned
	orig   string // as written in the ZIP
	size   int64
	mtime  int64
	locked bool
	stored bool
	piece  int // which ZIP of the chain holds it
}

type zipListing struct {
	chain   []store.Version
	entries []zipEntry // sorted by path
	byPath  map[string]int
}

type zipLists struct {
	mu    sync.Mutex
	order []int
	data  map[int]*zipListing
}

func (c *zipLists) get(id int) *zipListing {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.data[id]
}

func (c *zipLists) put(id int, l *zipListing) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data == nil {
		c.data = map[int]*zipListing{}
	}
	if _, ok := c.data[id]; !ok {
		c.order = append(c.order, id)
	}
	c.data[id] = l
	for len(c.order) > zipListCap {
		delete(c.data, c.order[0])
		c.order = c.order[1:]
	}
}

// cleanZipName makes a name inside a ZIP safe to use as a path: forward
// slashes, nothing absolute or with a drive letter, no "." or ".." parts.
func cleanZipName(name string) (string, bool) {
	name = strings.ReplaceAll(name, `\`, "/")
	if name == "" || strings.HasPrefix(name, "/") || strings.ContainsRune(name, 0) {
		return "", false
	}
	if len(name) >= 2 && name[1] == ':' {
		return "", false
	}
	parts := strings.Split(strings.TrimSuffix(name, "/"), "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return "", false
		}
	}
	return strings.Join(parts, "/"), true
}

// IsZipName says whether a file looks like a ZIP that can be browsed.
func IsZipName(name string) bool {
	return strings.EqualFold(path.Ext(name), ".zip")
}

// zipSource opens one stored ZIP (a plain version, or one piece of a project).
type zipSource struct {
	pv *Preview
	zr *zip.Reader
}

func (z *zipSource) Close() error { return z.pv.Close() }

func (e *Engine) openZip(ctx context.Context, v store.Version) (*zipSource, error) {
	pv, err := e.openStored(ctx, v)
	if err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(pv.At, pv.Size)
	if err != nil {
		pv.Close()
		return nil, fmt.Errorf("can't read this as a ZIP: %w", err)
	}
	return &zipSource{pv: pv, zr: zr}, nil
}

// zipListing returns what's in a version's ZIP (merged over its chain for a
// project), from memory when it's been read lately.
func (e *Engine) zipListing(ctx context.Context, v store.Version) (*zipListing, error) {
	if l := e.zipLists.get(v.MsgID); l != nil {
		return l, nil
	}
	chain, err := e.chainPieces(v)
	if err != nil {
		return nil, err
	}
	project := v.Kind == store.KindProject
	final := map[string]zipEntry{}
	for i, c := range chain {
		src, err := e.openZip(ctx, c)
		if err != nil {
			return nil, err
		}
		if project && i > 0 {
			gone, err := deletedNames(src.zr.File)
			if err != nil {
				src.Close()
				return nil, err
			}
			for _, rel := range gone {
				delete(final, rel)
			}
		}
		if len(src.zr.File) > maxZipEntries {
			src.Close()
			return nil, errors.New("this ZIP has too many files to browse")
		}
		for _, zf := range src.zr.File {
			if project && (zf.Name == changeDeleted || zf.Name == changeNote) {
				continue
			}
			if strings.HasSuffix(zf.Name, "/") {
				continue // folders are drawn from their files' paths
			}
			p, ok := cleanZipName(zf.Name)
			if !ok {
				continue
			}
			final[p] = zipEntry{path: p, orig: zf.Name, size: int64(zf.UncompressedSize64), mtime: zf.Modified.Unix(),
				locked: zf.Flags&0x1 != 0, stored: zf.Method == zip.Store, piece: i}
		}
		src.Close()
	}
	l := &zipListing{chain: chain, entries: make([]zipEntry, 0, len(final)), byPath: make(map[string]int, len(final))}
	for _, en := range final {
		l.entries = append(l.entries, en)
	}
	sort.Slice(l.entries, func(i, j int) bool { return l.entries[i].path < l.entries[j].path })
	for i, en := range l.entries {
		l.byPath[en.path] = i
	}
	e.zipLists.put(v.MsgID, l)
	return l, nil
}

// deletedNames reads the paths a change ZIP deletes.
func deletedNames(files []*zip.File) ([]string, error) {
	for _, zf := range files {
		if zf.Name != changeDeleted {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		b, err := io.ReadAll(io.LimitReader(rc, 64<<20))
		if err != nil {
			return nil, err
		}
		var out []string
		for _, line := range strings.Split(string(b), "\n") {
			if line = strings.TrimSuffix(line, "\r"); line != "" {
				out = append(out, line)
			}
		}
		return out, nil
	}
	return nil, nil
}

// ZipDir lists the files and folders directly inside dir ("" for the top) of a
// ZIP or project version, and the number of files in the whole ZIP.
func (e *Engine) ZipDir(ctx context.Context, v store.Version, dir string) ([]ZipItem, int, error) {
	l, err := e.zipListing(ctx, v)
	if err != nil {
		return nil, 0, err
	}
	dir = strings.Trim(dir, "/")
	prefix := ""
	if dir != "" {
		if _, ok := cleanZipName(dir); !ok {
			return nil, 0, errors.New("bad folder")
		}
		prefix = dir + "/"
	}
	var items []ZipItem
	dirs := map[string]int{} // folder name -> its index in items
	for _, en := range l.entries {
		if !strings.HasPrefix(en.path, prefix) {
			continue
		}
		rest := en.path[len(prefix):]
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			idx, ok := dirs[rest[:i]]
			if !ok {
				items = append(items, ZipItem{Name: rest[:i], Path: prefix + rest[:i], Kind: "dir"})
				idx = len(items) - 1
				dirs[rest[:i]] = idx
			}
			d := &items[idx]
			d.Size += en.size
			d.Files++
			if en.mtime > d.Mtime {
				d.Mtime = en.mtime
			}
			continue
		}
		items = append(items, ZipItem{Name: rest, Path: en.path, Kind: "file", Size: en.size, Mtime: en.mtime,
			Locked: en.locked, Stored: en.stored})
	}
	// Folders first, then names.
	sort.SliceStable(items, func(i, j int) bool {
		if (items[i].Kind == "dir") != (items[j].Kind == "dir") {
			return items[i].Kind == "dir"
		}
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
	return items, len(l.entries), nil
}

// ZipFile is one file inside a ZIP, open for reading and seeking.
type ZipFile struct {
	io.ReadSeeker
	Size    int64
	ModTime time.Time
	Stored  bool
	close   func() error
}

// Close releases the file.
func (z *ZipFile) Close() error {
	if z.close == nil {
		return nil
	}
	return z.close()
}

// OpenZipFile opens a file inside a ZIP or project version. Files stored
// without compression can be jumped around in freely; a compressed one is
// unpacked from its start, so a jump far ahead takes a while.
func (e *Engine) OpenZipFile(ctx context.Context, v store.Version, name string) (*ZipFile, error) {
	l, err := e.zipListing(ctx, v)
	if err != nil {
		return nil, err
	}
	i, ok := l.byPath[strings.Trim(name, "/")]
	if !ok {
		return nil, errors.New("that file isn't in the ZIP")
	}
	en := l.entries[i]
	if en.locked {
		return nil, errors.New("this file is password-protected, so it can't be opened here")
	}
	src, err := e.openZip(ctx, l.chain[en.piece])
	if err != nil {
		return nil, err
	}
	zf := findZipFile(src.zr, en.orig)
	if zf == nil {
		src.Close()
		return nil, errors.New("that file isn't in the ZIP any more")
	}
	mod := zf.Modified
	if zf.Method == zip.Store {
		off, err := zf.DataOffset()
		if err != nil {
			src.Close()
			return nil, err
		}
		sr := io.NewSectionReader(src.pv.At, off, int64(zf.UncompressedSize64))
		return &ZipFile{ReadSeeker: sr, Size: sr.Size(), ModTime: mod, Stored: true, close: src.Close}, nil
	}
	r := &unpackReader{zf: zf, size: int64(zf.UncompressedSize64)}
	return &ZipFile{ReadSeeker: r, Size: r.size, ModTime: mod, close: func() error { r.closeFlate(); return src.Close() }}, nil
}

func findZipFile(zr *zip.Reader, orig string) *zip.File {
	for _, zf := range zr.File {
		if zf.Name == orig {
			return zf
		}
	}
	return nil
}

// unpackReader reads a compressed ZIP entry as a seekable file: going forward
// unpacks and discards what's skipped, going back starts again. It never
// yields more than the entry's stated size, so a "ZIP bomb" can't run on.
type unpackReader struct {
	zf   *zip.File
	size int64
	rc   io.ReadCloser
	at   int64 // where rc is
	pos  int64
}

func (u *unpackReader) closeFlate() {
	if u.rc != nil {
		u.rc.Close()
		u.rc = nil
	}
}

func (u *unpackReader) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += u.pos
	case io.SeekEnd:
		offset += u.size
	default:
		return 0, errors.New("bad whence")
	}
	if offset < 0 {
		return 0, errors.New("negative position")
	}
	u.pos = offset
	return offset, nil
}

func (u *unpackReader) Read(p []byte) (int, error) {
	if u.pos >= u.size {
		return 0, io.EOF
	}
	if u.rc == nil || u.pos < u.at {
		u.closeFlate()
		rc, err := u.zf.Open()
		if err != nil {
			return 0, err
		}
		u.rc, u.at = rc, 0
	}
	if u.pos > u.at {
		n, err := io.CopyN(io.Discard, u.rc, u.pos-u.at)
		u.at += n
		if err != nil {
			return 0, err
		}
	}
	if int64(len(p)) > u.size-u.pos {
		p = p[:u.size-u.pos]
	}
	n, err := u.rc.Read(p)
	u.at += int64(n)
	u.pos += int64(n)
	if err == io.EOF && u.pos < u.size {
		err = io.ErrUnexpectedEOF
	}
	if err == io.EOF && n > 0 {
		err = nil
	}
	return n, err
}

// zipSelection resolves chosen files and folders (paths inside the ZIP) to the
// files they hold, each with the name it gets relative to its chosen item's
// parent folder.
type zipPick struct {
	en  zipEntry
	rel string
}

func (l *zipListing) pick(names []string) ([]zipPick, error) {
	seen := map[string]bool{}
	var out []zipPick
	for _, name := range names {
		name = strings.Trim(name, "/")
		if _, ok := cleanZipName(name); !ok {
			return nil, errors.New("bad name: " + name)
		}
		parent := path.Dir(name)
		if parent == "." {
			parent = ""
		}
		start := sort.Search(len(l.entries), func(i int) bool { return l.entries[i].path >= name })
		for i := start; i < len(l.entries) && strings.HasPrefix(l.entries[i].path, name); i++ {
			en := l.entries[i]
			if en.path != name && !strings.HasPrefix(en.path, name+"/") {
				continue // a sibling that only starts with the same letters
			}
			if seen[en.path] {
				continue
			}
			seen[en.path] = true
			rel := en.path
			if parent != "" {
				rel = strings.TrimPrefix(en.path, parent+"/")
			}
			out = append(out, zipPick{en: en, rel: rel})
		}
	}
	if len(out) == 0 {
		return nil, errors.New("nothing there")
	}
	return out, nil
}

// WriteZipEntries sends the chosen files and folders of a ZIP or project as a
// new ZIP, copying their compressed bytes as they are.
func (e *Engine) WriteZipEntries(ctx context.Context, v store.Version, names []string, w io.Writer) error {
	l, err := e.zipListing(ctx, v)
	if err != nil {
		return err
	}
	picks, err := l.pick(names)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(w)
	byPiece := map[int][]zipPick{}
	for _, p := range picks {
		if !p.en.locked {
			byPiece[p.en.piece] = append(byPiece[p.en.piece], p)
		}
	}
	for piece := range l.chain {
		group := byPiece[piece]
		if len(group) == 0 {
			continue
		}
		src, err := e.openZip(ctx, l.chain[piece])
		if err != nil {
			return err
		}
		files := map[string]*zip.File{}
		for _, zf := range src.zr.File {
			files[zf.Name] = zf
		}
		for _, p := range group {
			zf := files[p.en.orig]
			if zf == nil {
				continue
			}
			hdr := zf.FileHeader
			hdr.Name = p.rel
			raw, err := zf.OpenRaw()
			if err != nil {
				src.Close()
				return err
			}
			fw, err := zw.CreateRaw(&hdr)
			if err == nil {
				_, err = io.Copy(fw, raw)
			}
			if err != nil {
				src.Close()
				return err
			}
		}
		src.Close()
	}
	return zw.Close()
}

// ExtractZipEntries puts the chosen files and folders of a ZIP or project
// version on this device, as one task. With dest "" a project's files go back
// into the project's own folder (leaving everything else in it alone);
// otherwise they go into dest. A file that already exists with different
// content is never overwritten: the copy gets a " (restored)" name.
func (e *Engine) ExtractZipEntries(ctx context.Context, v store.Version, names []string, dest, verb string, done func(written []string)) (int64, error) {
	l, err := e.zipListing(ctx, v)
	if err != nil {
		return 0, err
	}
	picks, err := l.pick(names)
	if err != nil {
		return 0, err
	}
	base := dest
	inPlace := dest == ""
	if inPlace {
		if v.Kind != store.KindProject {
			return 0, errors.New("choose a folder to put the files in")
		}
		root, rel, ok := e.rootFor(v.CloudPath)
		if !ok {
			return 0, fmt.Errorf("%q isn't a backup folder on %s; choose a folder to restore into", topFolder(v.CloudPath), e.deviceWord())
		}
		base = filepath.Join(root.Path, filepath.FromSlash(rel))
	}
	title := verb + " " + path.Base(picks[0].en.path)
	if len(picks) > 1 {
		title = fmt.Sprintf("%s %d files from %s", verb, len(picks), path.Base(v.CloudPath))
	}
	t := e.newTask("restore", title)
	t.update(func(t *Task) { t.Total = len(picks) })
	go func() {
		ctx := context.WithoutCancel(ctx)
		var written []string
		renamed := 0
		byPiece := map[int][]zipPick{}
		for _, p := range picks {
			byPiece[p.en.piece] = append(byPiece[p.en.piece], p)
		}
		fail := func(p zipPick, err error) {
			t.update(func(t *Task) {
				t.Done++
				t.Errors = append(t.Errors, p.en.path+": "+tgclient.Friendly(err))
			})
		}
		for piece := range l.chain {
			group := byPiece[piece]
			if len(group) == 0 {
				continue
			}
			src, err := e.openZip(ctx, l.chain[piece])
			if err != nil {
				for _, p := range group {
					fail(p, err)
				}
				continue
			}
			files := map[string]*zip.File{}
			for _, zf := range src.zr.File {
				files[zf.Name] = zf
			}
			for _, p := range group {
				t.update(func(t *Task) { t.Current = p.en.path })
				rel := p.rel
				if inPlace {
					rel = p.en.path // the project's own layout, whatever was chosen
				}
				zf := files[p.en.orig]
				switch {
				case zf == nil:
					fail(p, errors.New("not in the ZIP any more"))
					continue
				case p.en.locked:
					fail(p, errors.New("password-protected"))
					continue
				}
				target := filepath.Join(base, filepath.FromSlash(rel))
				if !filepath.IsLocal(filepath.FromSlash(rel)) {
					fail(p, errors.New("unsafe name"))
					continue
				}
				final, was, err := extractInto(zf, target)
				if err == nil {
					written = append(written, final)
					if was {
						renamed++
					}
					e.fileChanged(final)
				}
				if err != nil {
					fail(p, err)
					continue
				}
				t.update(func(t *Task) { t.Done++ })
			}
			src.Close()
		}
		if done != nil {
			done(written)
		}
		t.update(func(t *Task) {
			if renamed > 0 {
				t.Notes = append(t.Notes, fmt.Sprintf("%d file(s) already existed with different content and were saved with \"(restored)\" names", renamed))
			}
			if dest != "" {
				t.Notes = append(t.Notes, "saved in "+dest)
			}
			t.Finished, t.Current = true, ""
		})
		e.RequestScan(0)
	}()
	return t.ID, nil
}

// extractInto writes a ZIP entry to target, unless the same content is there;
// different content already there means the copy is saved next to it.
func extractInto(zf *zip.File, target string) (final string, renamed bool, err error) {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", false, err
	}
	if _, err := os.Stat(target); err == nil {
		if same, _ := sameAsZipEntry(target, zf); same {
			return target, false, nil
		}
		target = restoredName(target)
		renamed = true
	}
	if err := extractEntry(zf, target); err != nil {
		return "", false, err
	}
	return target, renamed, nil
}
