package backup

import (
	"archive/zip"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Sammy-77A/tgvault/internal/meta"
	"github.com/Sammy-77A/tgvault/internal/store"
)

// A project is backed up as a full ZIP now and then, and in between as change
// ZIPs holding only the files added or modified since the previous backup,
// plus a list of deleted files. A full ZIP and the changes after it form a
// chain; any version is rebuilt by applying its chain's changes in order.
const (
	// A new full ZIP is made once the changes since the last one add up to
	// more than 1/chainShare of its size, the chain is older than
	// chainMaxAge, or it has chainMaxChanges changes.
	chainShare      = 4
	chainMaxAge     = 7 * 24 * time.Hour
	chainMaxChanges = 200
	// keepChains is how many chains of a project are kept in Telegram: the
	// current one and the one before it.
	keepChains = 2

	// Files inside a change ZIP that aren't project files.
	changeDeleted = ".tgvault-deleted.txt"
	changeNote    = ".tgvault-change.txt"
)

// treeHash summarises a project's files (path -> sha256), so a rebuilt
// version can be checked against the moment it was backed up.
func treeHash(files map[string]string) string {
	rels := make([]string, 0, len(files))
	for rel := range files {
		rels = append(rels, rel)
	}
	sort.Strings(rels)
	h := sha256.New()
	for _, rel := range rels {
		fmt.Fprintf(h, "%s\x00%s\n", rel, files[rel])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (e *Engine) uploadProject(ctx context.Context, it store.Item, abs string) outcome {
	if fi, err := os.Stat(abs); errors.Is(err, os.ErrNotExist) || (err == nil && !fi.IsDir()) {
		_ = e.st.MarkGone([]int64{it.ID})
		return outcome{state: store.StateSynced}
	}
	listedAt := time.Now().Unix()
	files := projectFiles(abs)
	fp, _, mtime := fingerprintOf(files)
	// A file modified just as the project was listed could be written again
	// in the same second without its size or time changing; then the backup
	// isn't recorded as matching, so the project is checked again later.
	recent := 0
	for _, f := range files {
		if f.mtime >= listedAt-1 {
			recent++
		}
	}
	latest, haveLatest, _ := e.st.LatestVersion(it.CloudPath)
	if haveLatest && latest.Fingerprint == fp {
		return outcome{state: store.StateSynced}
	}
	ps, haveState := e.st.GetProjectState(it.CloudPath)
	chain := haveState && haveLatest && latest.MsgID == ps.Version && e.chainIntact(ps)
	if chain && ps.Fingerprint == fp {
		return outcome{state: store.StateSynced} // only touched, nothing new
	}
	if e.bigBusy(it.ID) {
		return outcome{wait: time.Now().Add(bigWait)}
	}
	if chain && !chainDue(ps) {
		if out, ok := e.uploadProjectChange(ctx, it, abs, files, fp, mtime, listedAt, recent, ps); ok {
			return out
		}
	}
	return e.uploadProjectFull(ctx, it, abs, files, fp, mtime, listedAt, recent)
}

// recordedFingerprint is the fingerprint stored with a backup. A backup that
// left files out (open in another program), or that has files modified as it
// listed them, gets none, so it never counts as matching the project: the
// project is looked at again, and Free up space won't delete files whose
// content isn't surely in Telegram.
func recordedFingerprint(fp string, unsure int) string {
	if unsure > 0 {
		return ""
	}
	return fp
}

// chainIntact reports whether a project's chain can take another change: its
// full ZIP and last piece are still recorded.
func (e *Engine) chainIntact(ps store.ProjectState) bool {
	last, err := e.st.Version(ps.Version)
	if err != nil {
		return false
	}
	_, err = e.st.ChainOf(last)
	return err == nil && last.ChainID() == ps.Base
}

// chainDue reports whether a project's next backup should be a full ZIP.
func chainDue(ps store.ProjectState) bool {
	return ps.Seq >= chainMaxChanges || ps.ChainBytes*chainShare > ps.BaseSize ||
		time.Since(time.Unix(ps.BaseAt, 0)) > chainMaxAge
}

func (e *Engine) uploadProjectFull(ctx context.Context, it store.Item, abs string, files []projectFile, fp string, mtime, listedAt int64, recent int) outcome {
	e.setProgress(Progress{ItemID: it.ID, CloudPath: it.CloudPath, Phase: "zipping"})
	tmp := filepath.Join(e.tmpDir, fmt.Sprintf("project-%d.zip", it.ID))
	defer os.Remove(tmp)
	zw, err := newProjectZip(tmp)
	if err != nil {
		return e.failure(ctx, err)
	}
	written, skipped, err := zw.add(ctx, files)
	if err == nil {
		err = zw.close()
	} else {
		zw.abort()
	}
	if err != nil {
		return e.failure(ctx, err)
	}
	if after, _, _ := projectFingerprint(abs); after != fp {
		return outcome{state: store.StateFailed, retry: quietProject, note: "the project changed while zipping; will try again"}
	}
	fi, err := os.Stat(tmp)
	if err != nil {
		return e.failure(ctx, err)
	}
	note := skippedNote(skipped)
	fp = recordedFingerprint(fp, skipped+recent)
	hash, err := hashFile(tmp)
	if err != nil {
		return e.failure(ctx, err)
	}
	var out outcome
	if splitAt, _ := e.splitLimits(it.CloudPath); fi.Size() > splitAt {
		out = e.uploadSplit(ctx, it, tmp, fi, store.Version{CloudPath: it.CloudPath, Kind: store.KindProject,
			Mtime: mtime, Fingerprint: fp}, path.Base(it.CloudPath)+".zip", note)
	} else {
		caption, err := meta.Caption(meta.Info{Kind: "p", Path: it.CloudPath, Size: fi.Size(), Mt: mtime, Hash: hash, FP: fp, Dev: e.Device()})
		if err != nil {
			return outcome{state: store.StateSkipped, note: err.Error()}
		}
		msgID, err := e.send(ctx, it, tmp, path.Base(it.CloudPath)+".zip", fi.Size(), hash, caption)
		if err != nil {
			return e.failure(ctx, err)
		}
		out = e.recordVersion(ctx, store.Version{MsgID: msgID, CloudPath: it.CloudPath, Kind: store.KindProject,
			Size: fi.Size(), Mtime: mtime, Hash: hash, Fingerprint: fp, UploadedAt: time.Now().Unix()}, note)
	}
	if out.state != store.StateSynced {
		return out
	}
	// Start a new chain from this full ZIP.
	v, ok, _ := e.st.LatestVersion(it.CloudPath)
	if !ok || v.Hash != hash {
		return out
	}
	ps := store.ProjectState{CloudPath: it.CloudPath, Version: v.MsgID, Base: v.MsgID, BaseSize: fi.Size(),
		BaseAt: time.Now().Unix(), Fingerprint: fp, ListedAt: listedAt}
	if err := e.st.SaveProjectState(ps, written, nil, true); err != nil {
		log.Printf("project %s: couldn't save its file list: %v (the next backup will be a full one)", it.CloudPath, err)
		_ = e.st.ForgetProjectState(it.CloudPath)
	}
	return out
}

// uploadProjectChange uploads only what changed since the project's last
// backup. ok is false when a full ZIP should be made instead.
func (e *Engine) uploadProjectChange(ctx context.Context, it store.Item, abs string, files []projectFile, fp string,
	mtime, listedAt int64, recent int, ps store.ProjectState) (out outcome, ok bool) {
	e.setProgress(Progress{ItemID: it.ID, CloudPath: it.CloudPath, Phase: "finding changes"})
	old, err := e.st.ProjectFiles(it.CloudPath)
	if err != nil {
		return outcome{}, false
	}
	var changed []projectFile
	var updated []store.ProjectFile // new rows: changed, or only touched
	want := map[string]string{}     // path -> hash for changed files
	present := map[string]bool{}
	unread := 0
	for _, f := range files {
		present[f.rel] = true
		o, known := old[f.rel]
		// Same size and time means unchanged, except for a file modified in
		// the second the last backup listed the project: it could have been
		// written again within that second, so it's read to make sure.
		if known && o.Size == f.size && o.Mtime == f.mtime && o.Mtime < ps.ListedAt-1 {
			continue
		}
		h, err := hashFile(f.abs)
		if err != nil {
			unread++ // open elsewhere: the backup keeps what it had, and it's tried again
			continue
		}
		if known && o.Hash == h {
			updated = append(updated, store.ProjectFile{Rel: f.rel, Size: f.size, Mtime: f.mtime, Hash: h})
			continue
		}
		changed = append(changed, f)
		want[f.rel] = h
	}
	var deleted []string
	for rel := range old {
		if !present[rel] {
			deleted = append(deleted, rel)
		}
	}
	sort.Strings(deleted)
	if len(changed) == 0 && len(deleted) == 0 {
		if unread > 0 {
			return outcome{state: store.StateFailed, note: skippedNote(unread) + "; will try again", count: true}, true
		}
		// Only modified times moved (e.g. a checkout that rewrote files as they were).
		fp = recordedFingerprint(fp, recent)
		ps.Fingerprint, ps.ListedAt = fp, listedAt
		if err := e.st.SaveProjectState(ps, updated, nil, false); err != nil {
			return outcome{}, false
		}
		_ = e.st.SetVersionFingerprint(ps.Version, fp)
		return outcome{state: store.StateSynced}, true
	}

	e.setProgress(Progress{ItemID: it.ID, CloudPath: it.CloudPath, Phase: "zipping changes"})
	tmp := filepath.Join(e.tmpDir, fmt.Sprintf("project-%d-change.zip", it.ID))
	defer os.Remove(tmp)
	zw, err := newProjectZip(tmp)
	if err != nil {
		return e.failure(ctx, err), true
	}
	written, skipped, err := zw.add(ctx, changed)
	if err == nil {
		err = zw.extra(changeDeleted, strings.Join(deleted, "\n"), len(deleted) > 0)
	}
	if err == nil {
		err = zw.extra(changeNote, fmt.Sprintf("tgvault change %d to the project %s.\n"+
			"Rebuild it by unzipping the full backup (message %d), then each change in order (1, 2, 3, ...) over it,\n"+
			"deleting after each one the files listed in its %s.\nThis change follows message %d.\n",
			ps.Seq+1, it.CloudPath, ps.Base, changeDeleted, ps.Version), true)
	}
	if err == nil {
		err = zw.close()
	} else {
		zw.abort()
	}
	if err != nil {
		return e.failure(ctx, err), true
	}
	for _, w := range written {
		if w.Hash != want[w.Rel] {
			return outcome{state: store.StateFailed, retry: time.Minute, note: "the project changed while zipping; will try again"}, true
		}
	}
	if after, _, _ := projectFingerprint(abs); after != fp {
		return outcome{state: store.StateFailed, retry: time.Minute, note: "the project changed while zipping; will try again"}, true
	}
	fi, err := os.Stat(tmp)
	if err != nil {
		return e.failure(ctx, err), true
	}
	if splitAt, _ := e.splitLimits(it.CloudPath); fi.Size() > splitAt {
		return outcome{}, false // that much change is better stored as a full ZIP
	}
	hash, err := hashFile(tmp)
	if err != nil {
		return e.failure(ctx, err), true
	}

	// The project's file list after this change, for checking a rebuild.
	tree := map[string]string{}
	for rel, f := range old {
		tree[rel] = f.Hash
	}
	for _, rel := range deleted {
		delete(tree, rel)
	}
	for _, f := range updated {
		tree[f.Rel] = f.Hash
	}
	for _, w := range written {
		tree[w.Rel] = w.Hash
	}
	fp = recordedFingerprint(fp, skipped+unread+recent)
	in := meta.Info{Kind: "d", Path: it.CloudPath, Size: fi.Size(), Mt: mtime, Hash: hash, FP: fp, Dev: e.Device(),
		Base: ps.Base, Prev: ps.Version, Seq: ps.Seq + 1, Tree: treeHash(tree), Changed: len(written), Deleted: len(deleted)}
	caption, err := meta.Caption(in)
	if err != nil {
		return outcome{}, false
	}
	msgID, err := e.send(ctx, it, tmp, fmt.Sprintf("%s.change%d.zip", path.Base(it.CloudPath), in.Seq), fi.Size(), hash, caption)
	if err != nil {
		return e.failure(ctx, err), true
	}
	out = e.recordVersion(ctx, store.Version{MsgID: msgID, CloudPath: it.CloudPath, Kind: store.KindProject,
		Size: fi.Size(), Mtime: mtime, Hash: hash, Fingerprint: fp, UploadedAt: time.Now().Unix(),
		Base: in.Base, Prev: in.Prev, Seq: in.Seq, Tree: in.Tree, Changed: in.Changed, Deleted: in.Deleted}, skippedNote(skipped+unread))
	if out.state == store.StateSynced {
		ps.Version, ps.Seq, ps.ChainBytes, ps.Fingerprint, ps.ListedAt = msgID, in.Seq, ps.ChainBytes+fi.Size(), fp, listedAt
		if err := e.st.SaveProjectState(ps, append(updated, written...), deleted, false); err != nil {
			log.Printf("project %s: couldn't save its file list: %v (the next backup will be a full one)", it.CloudPath, err)
			_ = e.st.ForgetProjectState(it.CloudPath)
		}
	}
	return out, true
}

func skippedNote(skipped int) string {
	if skipped == 0 {
		return ""
	}
	return fmt.Sprintf("%d file(s) were open in another program and left out of this backup", skipped)
}

// projectZip writes a project ZIP, hashing each file as it goes in.
type projectZip struct {
	f  *os.File
	zw *zip.Writer
}

func newProjectZip(dst string) (*projectZip, error) {
	f, err := os.Create(dst)
	if err != nil {
		return nil, err
	}
	return &projectZip{f: f, zw: zip.NewWriter(f)}, nil
}

// add packs files and returns what went in. Files that can't be opened are
// counted and left out rather than blocking the whole project.
func (z *projectZip) add(ctx context.Context, files []projectFile) (written []store.ProjectFile, skipped int, err error) {
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		src, err := os.Open(f.abs)
		if err != nil {
			skipped++
			continue
		}
		h := sha256.New()
		var n int64
		w, err := z.zw.CreateHeader(&zip.FileHeader{Name: f.rel, Method: zip.Deflate, Modified: time.Unix(f.mtime, 0)})
		if err == nil {
			n, err = io.Copy(io.MultiWriter(w, h), src)
		}
		src.Close()
		if err != nil {
			return nil, 0, err
		}
		written = append(written, store.ProjectFile{Rel: f.rel, Size: n, Mtime: f.mtime, Hash: hex.EncodeToString(h.Sum(nil))})
	}
	return written, skipped, nil
}

// extra adds a text file of tgvault's own (only when include is set).
func (z *projectZip) extra(name, text string, include bool) error {
	if !include {
		return nil
	}
	w, err := z.zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Now()})
	if err == nil {
		_, err = io.WriteString(w, text)
	}
	return err
}

func (z *projectZip) close() error {
	if err := z.zw.Close(); err != nil {
		z.f.Close()
		return err
	}
	return z.f.Close()
}

func (z *projectZip) abort() { z.f.Close() }

// writeProjectZip packs files into a ZIP.
func writeProjectZip(ctx context.Context, dst string, files []projectFile) (skipped int, err error) {
	z, err := newProjectZip(dst)
	if err != nil {
		return 0, err
	}
	if _, skipped, err = z.add(ctx, files); err != nil {
		z.abort()
		return 0, err
	}
	return skipped, z.close()
}

// ---- rebuilding a version ----

var errChainBroken = errors.New("an earlier piece of this project's backup is missing in Telegram, so this version can't be rebuilt; an older version may still work")

// projectZipFile returns a temp file holding the complete ZIP of a project
// version: downloaded as is for a full ZIP, or rebuilt from its chain for a
// change. The caller removes the file.
func (e *Engine) projectZipFile(ctx context.Context, v store.Version) (string, error) {
	if !v.IsChange() {
		return e.downloadVerified(ctx, v, e.tmpDir)
	}
	chain, err := e.st.ChainOf(v)
	if err != nil {
		return "", errChainBroken
	}
	var need int64
	for _, c := range chain {
		need += c.Size
	}
	// The pieces, plus the rebuilt ZIP, which is about as big again.
	if err := checkFreeSpace(e.tmpDir, 2*need); err != nil {
		return "", err
	}
	var readers []*zip.ReadCloser
	var tmps []string
	defer func() {
		// Close first: Windows can't delete a file that's still open.
		for _, r := range readers {
			r.Close()
		}
		for _, t := range tmps {
			os.Remove(t)
		}
	}()
	for _, c := range chain {
		tmp, err := e.downloadVerified(ctx, c, e.tmpDir)
		if err != nil {
			return "", err
		}
		tmps = append(tmps, tmp)
		zr, err := zip.OpenReader(tmp)
		if err != nil {
			return "", err
		}
		readers = append(readers, zr)
	}
	final := map[string]*zip.File{}
	for i, zr := range readers {
		if i > 0 {
			gone, err := deletedList(zr)
			if err != nil {
				return "", err
			}
			for _, rel := range gone {
				delete(final, rel)
			}
		}
		for _, zf := range zr.File {
			if zf.FileInfo().IsDir() || (i > 0 && (zf.Name == changeDeleted || zf.Name == changeNote)) {
				continue
			}
			final[zf.Name] = zf
		}
	}
	return writeRebuilt(e.tmpDir, final, v.Tree)
}

// deletedList reads the paths a change ZIP deletes.
func deletedList(zr *zip.ReadCloser) ([]string, error) {
	for _, zf := range zr.File {
		if zf.Name != changeDeleted {
			continue
		}
		rc, err := zf.Open()
		if err != nil {
			return nil, err
		}
		defer rc.Close()
		var out []string
		sc := bufio.NewScanner(rc)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for sc.Scan() {
			if line := sc.Text(); line != "" {
				out = append(out, line)
			}
		}
		return out, sc.Err()
	}
	return nil, nil
}

// writeRebuilt writes the files of a rebuilt version into a new ZIP, copying
// each one's compressed bytes as they are, and checks the result against the
// version's file list.
func writeRebuilt(dir string, files map[string]*zip.File, wantTree string) (string, error) {
	out, err := os.CreateTemp(dir, ".project-*.tgvault-tmp")
	if err != nil {
		return "", err
	}
	fail := func(err error) (string, error) {
		out.Close()
		os.Remove(out.Name())
		return "", err
	}
	zw := zip.NewWriter(out)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	tree := map[string]string{}
	for _, name := range names {
		zf := files[name]
		// Reading it checks its CRC; the hash goes into the file list.
		rc, err := zf.Open()
		if err != nil {
			return fail(err)
		}
		h := sha256.New()
		_, err = io.Copy(h, rc)
		rc.Close()
		if err != nil {
			return fail(fmt.Errorf("%s: %w", name, err))
		}
		tree[name] = hex.EncodeToString(h.Sum(nil))
		hdr := zf.FileHeader
		raw, err := zf.OpenRaw()
		if err != nil {
			return fail(err)
		}
		fw, err := zw.CreateRaw(&hdr)
		if err == nil {
			_, err = io.Copy(fw, raw)
		}
		if err != nil {
			return fail(err)
		}
	}
	if err := zw.Close(); err != nil {
		return fail(err)
	}
	if treeHash(tree) != wantTree {
		return fail(errors.New("the rebuilt project failed its integrity check"))
	}
	if err := out.Close(); err != nil {
		os.Remove(out.Name())
		return "", err
	}
	return out.Name(), nil
}

// ---- keeping and cleaning up chains ----

// pruneChains keeps a project's newest keepChains chains, and more if needed
// to keep at least keepVersions versions, and deletes older chains from
// Telegram, every piece of them. vs is newest first.
func (e *Engine) pruneChains(ctx context.Context, cloudPath string, vs []store.Version) {
	size := map[int]int{}
	var order []int
	for _, v := range vs {
		id := v.ChainID()
		if size[id] == 0 {
			order = append(order, id)
		}
		size[id]++
	}
	keep := map[int]bool{}
	kept := 0
	for i, id := range order {
		if i < keepChains || kept < keepVersions {
			keep[id] = true
			kept += size[id]
		}
	}
	var old []store.Version
	for _, v := range vs {
		if !keep[v.ChainID()] {
			old = append(old, v)
		}
	}
	e.deleteVersions(ctx, cloudPath, old)
}

// deleteVersions removes versions from Telegram and then from the list.
func (e *Engine) deleteVersions(ctx context.Context, cloudPath string, vs []store.Version) {
	if len(vs) == 0 {
		return
	}
	msgs, err := e.messageIDsOf(vs)
	if err != nil {
		return
	}
	if err := e.tg.DeleteMessages(ctx, msgs); err != nil {
		log.Printf("prune %s: %v (will retry later)", cloudPath, err)
		return
	}
	ids := make([]int, len(vs))
	for i, v := range vs {
		ids[i] = v.MsgID
	}
	_ = e.st.DeleteVersions(ids)
}

// dropBrokenChanges takes change ZIPs that can't be rebuilt (a piece before
// them is gone from Telegram) off the list; the project's next backup is then
// a full ZIP. Their messages stay in Telegram: tgvault never deletes data on
// the strength of a missing link.
func (e *Engine) dropBrokenChanges() {
	broken, err := e.st.BrokenChanges()
	if err != nil || len(broken) == 0 {
		return
	}
	log.Printf("%d project change(s) can't be rebuilt any more (an earlier piece is gone)", len(broken))
	ids := make([]int, len(broken))
	for i, v := range broken {
		ids[i] = v.MsgID
	}
	_ = e.st.DeleteVersions(ids)
}

// ProjectZip returns a temp file with the complete ZIP of a project version
// (rebuilt from its chain for a change), for downloading. The caller removes it.
func (e *Engine) ProjectZip(ctx context.Context, v store.Version) (string, error) {
	return e.projectZipFile(ctx, v)
}

// chainPieces returns every version needed to rebuild v (just v unless it's a
// change).
func (e *Engine) chainPieces(v store.Version) ([]store.Version, error) {
	if !v.IsChange() {
		return []store.Version{v}, nil
	}
	chain, err := e.st.ChainOf(v)
	if err != nil {
		return nil, errChainBroken
	}
	return chain, nil
}

// versionFromCaption turns a channel message into a version record.
func versionFromCaption(msgID, date int, in meta.Info) store.Version {
	kind := store.KindFile
	if in.Kind != "f" {
		kind = store.KindProject
	}
	return store.Version{MsgID: msgID, CloudPath: in.Path, Kind: kind, Size: in.Size, Mtime: in.Mt, Hash: in.Hash,
		Fingerprint: in.FP, UploadedAt: int64(date), Base: in.Base, Prev: in.Prev, Seq: in.Seq, Tree: in.Tree,
		Changed: in.Changed, Deleted: in.Deleted}
}

// versionCaption rebuilds the caption of a single-message version, at a
// (possibly new) cloud path.
func versionCaption(v store.Version, cloudPath string) (string, error) {
	kind := "f"
	switch {
	case v.IsChange():
		kind = "d"
	case v.Kind == store.KindProject:
		kind = "p"
	}
	return meta.Caption(meta.Info{Kind: kind, Path: cloudPath, Size: v.Size, Mt: v.Mtime, Hash: v.Hash,
		FP: v.Fingerprint, Dev: meta.DeviceOf(cloudPath), Base: v.Base, Prev: v.Prev, Seq: v.Seq, Tree: v.Tree,
		Changed: v.Changed, Deleted: v.Deleted})
}
