package backup

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sammy-77A/tgvault/internal/meta"
	"github.com/Sammy-77A/tgvault/internal/store"
)

// project is a confirmed code project in a backup folder, for chain tests.
type project struct {
	t     *testing.T
	e     *Engine
	st    *store.Store
	tg    *fakeTG
	dir   string
	clock time.Time // modified times move forward a second per edit
}

// logo is the size of an incompressible file in it, which sets the full ZIP's
// size (the test split limit is 10 KB).
func newProject(t *testing.T, tg *fakeTG, logo int) *project {
	t.Helper()
	e, st := newSplitEngine(t, tg)
	code := filepath.Join(t.TempDir(), "Code")
	p := &project{t: t, e: e, st: st, tg: tg, dir: filepath.Join(code, "shop"), clock: time.Now().Add(-time.Hour)}
	p.edit("go.mod", "module shop\n")
	// Random bytes don't compress, so the full ZIP is clearly bigger than a change.
	p.edit("assets/logo.bin", string(randomBytes(t, logo)))
	p.edit("main.go", "package main\n")
	p.edit("lib/a.go", "package lib // a\n")
	p.edit("lib/b.go", "package lib // b\n")
	p.edit("docs/readme.md", "# shop\n")
	root, err := st.AddRoot(code, "Code")
	if err != nil {
		t.Fatal(err)
	}
	e.scanRoot(root)
	ps, _ := st.Projects(store.ProjectSuggested)
	if len(ps) != 1 {
		t.Fatalf("projects: %+v", ps)
	}
	if err := st.SetProjectState(ps[0].ID, store.ProjectConfirmed); err != nil {
		t.Fatal(err)
	}
	e.scanRoot(root)
	return p
}

func (p *project) edit(rel, content string) {
	p.t.Helper()
	abs := filepath.Join(p.dir, filepath.FromSlash(rel))
	write(p.t, abs, content)
	p.clock = p.clock.Add(time.Second)
	if err := os.Chtimes(abs, p.clock, p.clock); err != nil {
		p.t.Fatal(err)
	}
}

func (p *project) remove(rel string) {
	p.t.Helper()
	if err := os.Remove(filepath.Join(p.dir, filepath.FromSlash(rel))); err != nil {
		p.t.Fatal(err)
	}
}

func (p *project) upload() outcome {
	p.t.Helper()
	it, err := p.st.ItemByPath("Code/shop")
	if err != nil {
		p.t.Fatal(err)
	}
	return p.e.uploadProject(context.Background(), it, p.dir)
}

func (p *project) mustUpload() store.Version {
	p.t.Helper()
	if out := p.upload(); out.state != store.StateSynced {
		p.t.Fatalf("upload: %+v", out)
	}
	return latest(p.t, p.st, "Code/shop")
}

// files returns the project's files on disk, path -> content.
func (p *project) files() map[string]string {
	p.t.Helper()
	out := map[string]string{}
	for _, f := range projectFiles(p.dir) {
		b, err := os.ReadFile(f.abs)
		if err != nil {
			p.t.Fatal(err)
		}
		out[f.rel] = string(b)
	}
	return out
}

// rebuilt returns the files of a stored version, path -> content.
func (p *project) rebuilt(v store.Version) map[string]string {
	p.t.Helper()
	tmp, err := p.e.projectZipFile(context.Background(), v)
	if err != nil {
		p.t.Fatalf("rebuild version %d: %v", v.MsgID, err)
	}
	defer os.Remove(tmp)
	return zipContents(p.t, tmp)
}

func zipContents(t *testing.T, name string) map[string]string {
	t.Helper()
	zr, err := zip.OpenReader(name)
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	out := map[string]string{}
	for _, zf := range zr.File {
		rc, err := zf.Open()
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		_, err = b.ReadFrom(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		out[zf.Name] = b.String()
	}
	return out
}

func sameFiles(t *testing.T, what string, got, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d files, want %d\ngot  %v\nwant %v", what, len(got), len(want), keys(got), keys(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s: %s differs (got %q)", what, k, got[k])
		}
	}
}

func keys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestProjectUploadsOnlyChanges(t *testing.T) {
	tg := newFakeTG()
	p := newProject(t, tg, 6_000)
	full := p.mustUpload()
	snap0 := p.files()
	if full.IsChange() || tg.count() != 1 {
		t.Fatalf("first backup = %+v, %d messages; want one full ZIP", full, tg.count())
	}

	p.edit("lib/a.go", "package lib // a, edited\n")
	p.edit("lib/c.go", "package lib // new\n")
	p.remove("docs/readme.md")
	c1 := p.mustUpload()
	snap1 := p.files()
	if !c1.IsChange() || c1.Seq != 1 || c1.Base != full.MsgID || c1.Prev != full.MsgID || c1.Changed != 2 || c1.Deleted != 1 {
		t.Fatalf("change = %+v", c1)
	}
	// The change ZIP holds just the two files and the notes.
	inZip := zipContents(t, writeTemp(t, tg.msgs[c1.MsgID].data))
	if len(inZip) != 4 || inZip["lib/a.go"] == "" || inZip["lib/c.go"] == "" || inZip[changeDeleted] != "docs/readme.md" || inZip[changeNote] == "" {
		t.Fatalf("change ZIP holds %v", keys(inZip))
	}
	in, ok := meta.Parse(tg.msgs[c1.MsgID].caption)
	if !ok || !in.Change() || in.Base != full.MsgID || in.Seq != 1 || in.Tree != c1.Tree {
		t.Fatalf("change caption = %+v (%v)", in, ok)
	}
	if c1.Size >= full.Size/2 {
		t.Fatalf("change ZIP is %d bytes, full ZIP %d: not much smaller", c1.Size, full.Size)
	}

	p.edit("main.go", "package main // rewritten\n")
	c2 := p.mustUpload()
	snap2 := p.files()
	if c2.Seq != 2 || c2.Prev != c1.MsgID || c2.Base != full.MsgID {
		t.Fatalf("second change = %+v", c2)
	}

	// Every version rebuilds to exactly the files of its moment.
	sameFiles(t, "full", p.rebuilt(full), snap0)
	sameFiles(t, "change 1", p.rebuilt(c1), snap1)
	sameFiles(t, "change 2", p.rebuilt(c2), snap2)

	// Nothing changed: nothing uploaded.
	before := tg.count()
	p.mustUpload()
	// Touched but identical (e.g. a checkout): nothing uploaded either.
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(filepath.Join(p.dir, "lib", "b.go"), later, later); err != nil {
		t.Fatal(err)
	}
	if v := p.mustUpload(); v.MsgID != c2.MsgID || tg.count() != before {
		t.Fatalf("a touched file was uploaded again: latest %d, %d messages (was %d)", v.MsgID, tg.count(), before)
	}
	// And it isn't hashed again next time: the new modified time was saved.
	if v := p.mustUpload(); v.MsgID != c2.MsgID {
		t.Fatal("unexpected upload")
	}
}

func writeTemp(t *testing.T, data []byte) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "x.zip")
	if err := os.WriteFile(name, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestProjectRestoreAndDownloadRebuildChanges(t *testing.T) {
	tg := newFakeTG()
	p := newProject(t, tg, 6_000)
	p.mustUpload()
	p.edit("lib/a.go", "package lib // changed\n")
	p.remove("lib/b.go")
	c := p.mustUpload()
	want := p.files()

	// Restore into an empty folder.
	target := filepath.Join(t.TempDir(), "shop")
	if _, err := p.e.restoreProject(context.Background(), c, target); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, f := range projectFiles(target) {
		b, _ := os.ReadFile(f.abs)
		got[f.rel] = string(b)
	}
	sameFiles(t, "restored", got, want)

	// Download (the browser's ZIP) is the rebuilt project.
	zipped, err := p.e.ProjectZip(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(zipped)
	sameFiles(t, "downloaded", zipContents(t, zipped), want)
	// Streaming a change as it is would hand out only the changed files.
	if err := p.e.DownloadTo(context.Background(), c, &bytes.Buffer{}); err == nil {
		t.Fatal("DownloadTo must refuse a change that hasn't been rebuilt")
	}

	// A wrong file list is caught instead of handing out a bad project.
	bad := c
	bad.Tree = strings.Repeat("0", 64)
	if _, err := p.e.projectZipFile(context.Background(), bad); err == nil {
		t.Fatal("a rebuild that doesn't match its file list must fail")
	}
}

func TestProjectNewFullZipWhenChangesPileUp(t *testing.T) {
	tg := newFakeTG()
	p := newProject(t, tg, 6_000)
	full := p.mustUpload()

	// A change bigger than a quarter of the full ZIP: the one after it is full.
	p.edit("big.txt", string(randomBytes(t, int(full.Size))))
	c := p.mustUpload()
	if !c.IsChange() {
		t.Fatalf("expected a change first, got %+v", c)
	}
	if c.Size*chainShare <= full.Size {
		t.Fatalf("the change (%d bytes) should pass a quarter of the full ZIP (%d)", c.Size, full.Size)
	}
	p.edit("lib/a.go", "package lib // later\n")
	full2 := p.mustUpload()
	if full2.IsChange() {
		t.Fatalf("expected a new full ZIP once the changes passed a quarter, got %+v", full2)
	}

	// An old chain is restarted too.
	weekOld := func(what string) store.Version {
		t.Helper()
		ps, _ := p.st.GetProjectState("Code/shop")
		ps.BaseAt = time.Now().Add(-8 * 24 * time.Hour).Unix()
		if err := p.st.SaveProjectState(ps, nil, nil, false); err != nil {
			t.Fatal(err)
		}
		p.edit("lib/a.go", "package lib // "+what+"\n")
		v := p.mustUpload()
		if v.IsChange() {
			t.Fatalf("expected a full ZIP for a week-old chain, got %+v", v)
		}
		return v
	}
	full3 := weekOld("a week later")

	chainsKept := func() map[int]bool {
		vs, _ := p.st.Versions("Code/shop")
		chains := map[int]bool{}
		for _, v := range vs {
			chains[v.ChainID()] = true
		}
		return chains
	}
	// Three chains of 2, 1 and 1 versions: the newest two hold only 2
	// versions, so the oldest stays too (at least 3 versions are kept).
	if got := chainsKept(); len(got) != 3 {
		t.Fatalf("kept chains %v, want all three", got)
	}
	full4 := weekOld("two weeks later")
	// Four chains: the newest three hold 3 versions, so the oldest goes,
	// every piece of it, from Telegram too.
	if got := chainsKept(); len(got) != 3 || got[full.MsgID] || !got[full2.MsgID] || !got[full3.MsgID] || !got[full4.MsgID] {
		t.Fatalf("kept chains %v, want the newest three", got)
	}
	for _, id := range []int{full.MsgID, c.MsgID} {
		if _, ok := tg.msgs[id]; ok {
			t.Fatalf("message %d of the oldest chain is still in Telegram", id)
		}
	}
}

func TestProjectChainRebuiltFromTelegram(t *testing.T) {
	tg := newFakeTG()
	p := newProject(t, tg, 25_000) // the full ZIP is split into parts
	if full := p.mustUpload(); full.Parts < 2 {
		t.Fatalf("full ZIP = %+v, want it split", full)
	}
	p.mustUpload()
	p.edit("lib/a.go", "package lib // from the PC\n")
	p.mustUpload()
	p.edit("lib/c.go", "package lib // also new\n")
	last := p.mustUpload()
	want := p.files()

	// Another device (or a reinstall) knows only what's in Telegram.
	other, _ := newSplitEngine(t, tg)
	other.Reconcile(context.Background())
	v, ok, _ := other.st.LatestVersion("Code/shop")
	if !ok || v.MsgID != last.MsgID || v.Seq != 2 || v.Tree != last.Tree {
		t.Fatalf("reconciled latest = %+v", v)
	}
	tmp, err := other.projectZipFile(context.Background(), v)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmp)
	sameFiles(t, "rebuilt elsewhere", zipContents(t, tmp), want)

	// The minute sync picks up a new change too.
	p.edit("lib/a.go", "package lib // newer\n")
	newest := p.mustUpload()
	if _, err := other.SyncNew(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v, _, _ := other.st.LatestVersion("Code/shop"); v.MsgID != newest.MsgID || v.Seq != 3 {
		t.Fatalf("synced latest = %+v", v)
	}
}

func TestProjectBrokenChain(t *testing.T) {
	tg := newFakeTG()
	p := newProject(t, tg, 6_000)
	full := p.mustUpload()
	p.edit("lib/a.go", "package lib // 1\n")
	c1 := p.mustUpload()
	p.edit("lib/a.go", "package lib // 2\n")
	c2 := p.mustUpload()

	// Someone deletes the first change inside Telegram.
	_ = tg.DeleteMessages(context.Background(), []int{c1.MsgID})
	if problems, err := p.e.checkInTelegram(context.Background(), []store.Version{c2}); err != nil || problems[c2.MsgID] == "" {
		t.Fatalf("check of a change whose chain lost a piece: %v %v", problems, err)
	}
	p.e.Reconcile(context.Background())
	if v := latest(t, p.st, "Code/shop"); v.MsgID != full.MsgID {
		t.Fatalf("after the gap the latest usable version should be the full ZIP, got %+v", v)
	}
	if _, err := p.e.projectZipFile(context.Background(), c2); !errors.Is(err, errChainBroken) {
		t.Fatalf("rebuilding past the gap: %v", err)
	}
	// The next backup starts a new chain.
	p.edit("lib/a.go", "package lib // 3\n")
	if v := p.mustUpload(); v.IsChange() {
		t.Fatalf("expected a full ZIP after a broken chain, got %+v", v)
	}
}

func TestProjectChainLinksAndMove(t *testing.T) {
	tg := newFakeTG()
	p := newProject(t, tg, 6_000)
	full := p.mustUpload()
	p.edit("lib/a.go", "package lib // changed\n")
	c := p.mustUpload()

	links, err := p.e.Links([]string{"Code/shop"})
	if err != nil || len(links) != 2 || !strings.Contains(links[0], "(full backup)") || !strings.Contains(links[1], "(change 1)") {
		t.Fatalf("links = %v, %v", links, err)
	}

	// Moving every version of the project keeps the chain working.
	moving := map[int]bool{full.MsgID: true, c.MsgID: true}
	for _, v := range []store.Version{full, c} {
		if err := p.e.moveVersion(context.Background(), v, "Code/Archive/shop", moving); err != nil {
			t.Fatal(err)
		}
	}
	in, ok := meta.Parse(tg.msgs[c.MsgID].caption)
	if !ok || in.Path != "Code/Archive/shop" || in.Seq != 1 || in.Base != full.MsgID || in.Tree != c.Tree {
		t.Fatalf("moved change caption = %+v (%v)", in, ok)
	}
	moved := latest(t, p.st, "Code/Archive/shop")
	sameFiles(t, "moved", p.rebuilt(moved), p.files())
}

// A file written again in the same second the last backup listed the project,
// with the same size, still gets its new content backed up.
func TestProjectSameSecondRewrite(t *testing.T) {
	tg := newFakeTG()
	p := newProject(t, tg, 6_000)
	p.mustUpload()
	abs := filepath.Join(p.dir, "lib", "a.go")
	now := time.Now().Truncate(time.Second)
	write(t, abs, "package lib // version 1\n")
	if err := os.Chtimes(abs, now, now); err != nil {
		t.Fatal(err)
	}
	p.mustUpload()
	write(t, abs, "package lib // version 2\n") // same length, same second
	if err := os.Chtimes(abs, now, now); err != nil {
		t.Fatal(err)
	}
	// Size and time can't tell, so the last backup wasn't recorded as matching.
	if v := latest(t, p.st, "Code/shop"); v.Fingerprint != "" {
		t.Fatal("a backup with a file modified as it was listed must not count as matching")
	}
	v := p.mustUpload()
	if got := p.rebuilt(v)["lib/a.go"]; got != "package lib // version 2\n" {
		t.Fatalf("backup holds %q", got)
	}
}
