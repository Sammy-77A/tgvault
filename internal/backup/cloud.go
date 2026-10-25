package backup

import (
	"archive/zip"
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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tgerr"

	"github.com/Sammy-77A/tgvault/internal/meta"
	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/tgclient"
)

// ---- checking the local list against Telegram ----

func (e *Engine) reconcileLoop(ctx context.Context) {
	for ctx.Err() == nil {
		if err := e.tg.WaitReady(ctx); err != nil {
			return
		}
		e.Reconcile(ctx)
		// Run again after the interval, or when the connection is re-established.
		select {
		case <-ctx.Done():
			return
		case <-time.After(reconcileEvery):
		case <-e.notReady(ctx):
		}
	}
}

// notReady returns a channel closed when the Telegram connection drops.
func (e *Engine) notReady(ctx context.Context) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		for ctx.Err() == nil {
			if e.tg.Info().Status != tgclient.StatusReady {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
			}
		}
	}()
	return ch
}

// Reconcile reads every file in the Telegram channel and updates the local
// list: it adds versions uploaded elsewhere (e.g. before a reinstall) and
// forgets versions deleted inside Telegram, so those files get uploaded again.
func (e *Engine) Reconcile(ctx context.Context) {
	e.mu.Lock()
	if e.reconcile.Running {
		e.mu.Unlock()
		return
	}
	e.reconcile.Running = true
	e.mu.Unlock()

	n, err := e.reconcileOnce(ctx)

	e.mu.Lock()
	e.reconcile = Reconcile{Last: time.Now().Unix(), Files: n}
	if err != nil {
		e.reconcile.Error = tgclient.Friendly(err)
		log.Printf("reconcile: %v", err)
	}
	e.mu.Unlock()
	e.poke()
}

func (e *Engine) reconcileOnce(ctx context.Context) (int, error) {
	known, err := e.st.AllVersionIDs()
	if err != nil {
		return 0, err
	}
	seen := map[int]bool{}
	paths := map[string]bool{}
	newest := 0
	err = e.tg.ScanChannel(ctx, func(rf tgclient.RemoteFile) error {
		newest = max(newest, rf.MsgID)
		in, ok := meta.Parse(rf.Caption)
		if !ok {
			return nil // not a tgvault upload
		}
		seen[rf.MsgID] = true
		paths[in.Path] = true
		if known[rf.MsgID] {
			return nil
		}
		kind := store.KindFile
		if in.Kind == "p" {
			kind = store.KindProject
		}
		return e.st.AddVersion(store.Version{MsgID: rf.MsgID, CloudPath: in.Path, Kind: kind, Size: in.Size,
			Mtime: in.Mt, Hash: in.Hash, Fingerprint: in.FP, UploadedAt: int64(rf.Date)})
	})
	if err != nil {
		// An incomplete scan must never be read as "files were deleted".
		return 0, err
	}
	var missing []int
	for id := range known {
		if !seen[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		log.Printf("reconcile: %d version(s) were deleted inside Telegram", len(missing))
		if err := e.st.DeleteVersions(missing); err != nil {
			return 0, err
		}
	}
	if newest > 0 {
		_ = e.st.Set(settingSyncSeen, strconv.Itoa(newest))
	}
	if err := e.st.RequeueMissing(); err != nil {
		return 0, err
	}
	for p := range paths {
		e.prune(ctx, p)
	}
	return len(paths), nil
}

// ---- browsing ----

// Entry is one row in the cloud browser.
type Entry struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Kind     string `json:"kind"` // folder, file, project
	Size     int64  `json:"size"`
	Files    int    `json:"files,omitempty"`
	Mtime    int64  `json:"mtime,omitempty"`
	Versions int    `json:"versions,omitempty"`
	Local    string `json:"local"`          // "same", "changed", "cloud-only", "other-device", "mixed"
	Device   string `json:"device"`         // which device backed it up
	Type     string `json:"type,omitempty"` // audio, video, image, document, archive, other
	MsgID    int    `json:"msgId,omitempty"`
	Uploaded int64  `json:"uploaded,omitempty"`
}

// List returns the children of a cloud folder ("" = top level).
func (e *Engine) List(prefix string) ([]Entry, error) {
	latest, err := e.st.LatestUnder(prefix)
	if err != nil {
		return nil, err
	}
	counts, err := e.st.VersionCounts()
	if err != nil {
		return nil, err
	}
	items, err := e.st.ItemsUnder(prefix)
	if err != nil {
		return nil, err
	}
	byPath := map[string]store.Item{}
	for _, it := range items {
		byPath[it.CloudPath] = it
	}
	folders := map[string]*Entry{}
	var out []Entry
	for _, v := range latest {
		rest := v.CloudPath
		if prefix != "" {
			if v.CloudPath == prefix {
				continue
			}
			rest = strings.TrimPrefix(v.CloudPath, prefix+"/")
		}
		local := localState(v, byPath, e.Device())
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			name := rest[:i]
			f := folders[name]
			if f == nil {
				f = &Entry{Name: name, Path: path.Join(prefix, name), Kind: "folder", Local: local,
					Device: meta.DeviceOf(v.CloudPath)}
				folders[name] = f
			}
			f.Files++
			f.Size += v.Size
			if f.Local != local {
				f.Local = "mixed"
			}
			continue
		}
		kind := "file"
		if v.Kind == store.KindProject {
			kind = "project"
		}
		out = append(out, Entry{Name: rest, Path: v.CloudPath, Kind: kind, Size: v.Size, Mtime: v.Mtime,
			Versions: counts[v.CloudPath], Local: local, MsgID: v.MsgID, Uploaded: v.UploadedAt,
			Device: meta.DeviceOf(v.CloudPath), Type: meta.FileType(rest)})
	}
	for _, f := range folders {
		out = append(out, *f)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Kind == "folder") != (out[j].Kind == "folder") {
			return out[i].Kind == "folder"
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

func localState(v store.Version, items map[string]store.Item, device string) string {
	if meta.DeviceOf(v.CloudPath) != device {
		return "other-device"
	}
	it, ok := items[v.CloudPath]
	if !ok || !it.LocalPresent {
		return "cloud-only"
	}
	if it.State == store.StateSynced {
		return "same"
	}
	return "changed"
}

// ---- background tasks (restore, free space) ----

// Task is a long-running user action.
type Task struct {
	mu       sync.Mutex
	ID       int64
	Kind     string
	Title    string
	Total    int
	Done     int
	Current  string
	Errors   []string
	Notes    []string
	Finished bool
	Started  time.Time
}

// TaskView is a snapshot of a task.
type TaskView struct {
	ID       int64    `json:"id"`
	Kind     string   `json:"kind"`
	Title    string   `json:"title"`
	Total    int      `json:"total"`
	Done     int      `json:"done"`
	Current  string   `json:"current"`
	Errors   []string `json:"errors"`
	Notes    []string `json:"notes"`
	Finished bool     `json:"finished"`
}

func (t *Task) view() TaskView {
	t.mu.Lock()
	defer t.mu.Unlock()
	return TaskView{ID: t.ID, Kind: t.Kind, Title: t.Title, Total: t.Total, Done: t.Done, Current: t.Current,
		Errors: append([]string(nil), t.Errors...), Notes: append([]string(nil), t.Notes...), Finished: t.Finished}
}

func (t *Task) update(f func(t *Task)) {
	t.mu.Lock()
	f(t)
	t.mu.Unlock()
}

func (e *Engine) newTask(kind, title string) *Task {
	e.mu.Lock()
	defer e.mu.Unlock()
	t := &Task{ID: time.Now().UnixNano(), Kind: kind, Title: title, Started: time.Now()}
	// Keep finished tasks for a while so the UI can show results.
	kept := e.tasks[:0]
	for _, old := range e.tasks {
		v := old.view()
		if !v.Finished || time.Since(old.Started) < time.Hour {
			kept = append(kept, old)
		}
	}
	e.tasks = append(kept, t)
	return t
}

// DismissTask removes a finished task from the list.
func (e *Engine) DismissTask(id int64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, t := range e.tasks {
		if t.ID == id && t.view().Finished {
			e.tasks = append(e.tasks[:i], e.tasks[i+1:]...)
			return
		}
	}
}

// rootFor finds the backup root whose label starts a cloud path.
func (e *Engine) rootFor(cloudPath string) (store.Root, string, bool) {
	roots, _ := e.st.Roots()
	for _, r := range roots {
		if cloudPath == r.Label {
			return r, "", true
		}
		if strings.HasPrefix(cloudPath, r.Label+"/") {
			return r, strings.TrimPrefix(cloudPath, r.Label+"/"), true
		}
	}
	return store.Root{}, "", false
}

// Restore downloads a cloud file or folder. With dest "" files go back to
// their original location; otherwise the selected item is placed inside dest.
// Existing files are never overwritten: if a different file is already there,
// the restored copy is saved next to it with " (restored)" in its name.
func (e *Engine) Restore(ctx context.Context, prefix, dest string) (int64, error) {
	return e.RestoreMany(ctx, []string{prefix}, dest, "Restoring", nil)
}

// RestoreMany restores several files or folders as one task. verb titles the
// task ("Restoring", "Downloading"). done, if set, gets the files written once
// the task ends.
func (e *Engine) RestoreMany(ctx context.Context, prefixes []string, dest, verb string, done func(written []string)) (int64, error) {
	var versions []store.Version
	var targets []string
	seen := map[int]bool{}
	for _, prefix := range prefixes {
		vs, err := e.st.LatestUnder(prefix)
		if err != nil {
			return 0, err
		}
		for _, v := range vs {
			if seen[v.MsgID] {
				continue // a file inside a selected folder that was also selected
			}
			seen[v.MsgID] = true
			if dest == "" {
				root, rel, ok := e.rootFor(v.CloudPath)
				if !ok {
					return 0, fmt.Errorf("%q isn't a backup folder on %s; choose a folder to restore into",
						topFolder(v.CloudPath), e.deviceWord())
				}
				targets = append(targets, filepath.Join(root.Path, filepath.FromSlash(rel)))
			} else {
				rel := v.CloudPath
				if parent := path.Dir(prefix); parent != "." {
					rel = strings.TrimPrefix(v.CloudPath, parent+"/")
				}
				targets = append(targets, filepath.Join(dest, filepath.FromSlash(rel)))
			}
			versions = append(versions, v)
		}
	}
	if len(versions) == 0 {
		return 0, errors.New("nothing in the backup there")
	}
	title := verb + " " + displayName(prefixes[0])
	if len(prefixes) > 1 {
		title = fmt.Sprintf("%s %d selected items", verb, len(prefixes))
	}
	t := e.newTask("restore", title)
	t.update(func(t *Task) { t.Total = len(versions) })
	go func() {
		ctx := context.WithoutCancel(ctx)
		var written []string
		for i, v := range versions {
			t.update(func(t *Task) { t.Current = v.CloudPath })
			var note, final string
			var err error
			if v.Kind == store.KindProject {
				note, err = e.restoreProject(ctx, v, targets[i])
				final = targets[i]
			} else {
				note, final, err = e.restoreFile(ctx, v, targets[i])
			}
			if err == nil && final != "" {
				written = append(written, final)
			}
			t.update(func(t *Task) {
				t.Done++
				if err != nil {
					t.Errors = append(t.Errors, v.CloudPath+": "+tgclient.Friendly(err))
				}
				if note != "" {
					t.Notes = append(t.Notes, note)
				}
			})
		}
		if done != nil {
			done(written)
		}
		if len(versions) > 0 && dest != "" {
			t.update(func(t *Task) { t.Notes = append(t.Notes, "saved in "+dest) })
		}
		t.update(func(t *Task) { t.Finished, t.Current = true, "" })
		e.RequestScan(0)
	}()
	return t.ID, nil
}

func topFolder(p string) string { return strings.SplitN(p, "/", 2)[0] }

func (e *Engine) deviceWord() string {
	if e.Device() == meta.DevicePhone {
		return "this phone"
	}
	return "this PC"
}

func displayName(p string) string {
	if p == "" {
		return "everything"
	}
	return p
}

// restoredName returns a free "name (restored).ext" path next to target.
func restoredName(target string) string {
	ext := filepath.Ext(target)
	base := strings.TrimSuffix(target, ext)
	for i := 1; ; i++ {
		suffix := " (restored)"
		if i > 1 {
			suffix = fmt.Sprintf(" (restored %d)", i)
		}
		p := base + suffix + ext
		if _, err := os.Stat(p); errors.Is(err, os.ErrNotExist) {
			return p
		}
	}
}

// downloadVerified downloads a version to a temp file and checks its hash.
func (e *Engine) downloadVerified(ctx context.Context, v store.Version, dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, ".restore-*.tgvault-tmp")
	if err != nil {
		return "", err
	}
	h := sha256.New()
	err = e.tg.Download(ctx, v.MsgID, io.MultiWriter(f, h))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && hex.EncodeToString(h.Sum(nil)) != v.Hash {
		err = errors.New("downloaded file failed its integrity check")
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func (e *Engine) restoreFile(ctx context.Context, v store.Version, target string) (note, final string, err error) {
	if _, err := os.Stat(target); err == nil {
		if h, err := hashFile(target); err == nil && h == v.Hash {
			return "", target, nil // already there
		}
		target = restoredName(target)
		note = "a different file already existed, so this was saved as " + filepath.Base(target)
	}
	tmp, err := e.downloadVerified(ctx, v, filepath.Dir(target))
	if err != nil {
		return "", "", err
	}
	mt := time.Unix(v.Mtime, 0)
	_ = os.Chtimes(tmp, mt, mt)
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		return "", "", err
	}
	e.fileChanged(target)
	return note, target, nil
}

func (e *Engine) restoreProject(ctx context.Context, v store.Version, targetDir string) (string, error) {
	tmp, err := e.downloadVerified(ctx, v, e.tmpDir)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp)
	zr, err := zip.OpenReader(tmp)
	if err != nil {
		return "", err
	}
	defer zr.Close()
	renamed := 0
	for _, zf := range zr.File {
		if zf.FileInfo().IsDir() {
			continue
		}
		rel := filepath.FromSlash(zf.Name)
		if !filepath.IsLocal(rel) {
			continue // never write outside the project folder
		}
		target := filepath.Join(targetDir, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		if _, err := os.Stat(target); err == nil {
			if same, _ := sameAsZipEntry(target, zf); same {
				continue
			}
			target = restoredName(target)
			renamed++
		}
		if err := extractEntry(zf, target); err != nil {
			return "", err
		}
		e.fileChanged(target)
	}
	note := ""
	if renamed > 0 {
		note = fmt.Sprintf("%s: %d file(s) already existed with different content and were saved with \"(restored)\" names", v.CloudPath, renamed)
	}
	return note, nil
}

func sameAsZipEntry(p string, zf *zip.File) (bool, error) {
	fi, err := os.Stat(p)
	if err != nil || fi.Size() != int64(zf.UncompressedSize64) {
		return false, err
	}
	a, err := hashFile(p)
	if err != nil {
		return false, err
	}
	rc, err := zf.Open()
	if err != nil {
		return false, err
	}
	defer rc.Close()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return false, err
	}
	return a == hex.EncodeToString(h.Sum(nil)), nil
}

func extractEntry(zf *zip.File, target string) error {
	rc, err := zf.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	tmp := target + ".tgvault-tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, rc)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	_ = os.Chtimes(tmp, zf.Modified, zf.Modified)
	return os.Rename(tmp, target)
}

// ---- downloads to the browser ----

// LatestVersion returns the newest version of a cloud path.
func (e *Engine) LatestVersion(p string) (store.Version, bool, error) { return e.st.LatestVersion(p) }

// Versions lists stored versions of a cloud path.
func (e *Engine) Versions(p string) ([]store.Version, error) { return e.st.Versions(p) }

// Version returns one version.
func (e *Engine) Version(msgID int) (store.Version, error) { return e.st.Version(msgID) }

// DownloadTo streams one stored version to w.
func (e *Engine) DownloadTo(ctx context.Context, msgID int, w io.Writer) error {
	return e.tg.Download(ctx, msgID, w)
}

// WriteZip streams a cloud folder as a ZIP, with projects unpacked into their folders.
func (e *Engine) WriteZip(ctx context.Context, prefix string, w io.Writer) error {
	return e.WriteZipMany(ctx, []string{prefix}, w)
}

// WriteZipMany streams several files or folders as one ZIP. Each keeps its
// path from its own parent folder.
func (e *Engine) WriteZipMany(ctx context.Context, prefixes []string, w io.Writer) error {
	type entry struct {
		v   store.Version
		rel string
	}
	var entries []entry
	seen := map[int]bool{}
	for _, prefix := range prefixes {
		versions, err := e.st.LatestUnder(prefix)
		if err != nil {
			return err
		}
		base := path.Dir(prefix)
		for _, v := range versions {
			if seen[v.MsgID] {
				continue
			}
			seen[v.MsgID] = true
			rel := v.CloudPath
			if base != "." && base != "" {
				rel = strings.TrimPrefix(v.CloudPath, base+"/")
			}
			entries = append(entries, entry{v, rel})
		}
	}
	zw := zip.NewWriter(w)
	for _, en := range entries {
		v, rel := en.v, en.rel
		if v.Kind == store.KindProject {
			if err := e.zipProject(ctx, zw, v, rel); err != nil {
				return err
			}
			continue
		}
		fw, err := zw.CreateHeader(&zip.FileHeader{Name: rel, Method: zip.Deflate, Modified: time.Unix(v.Mtime, 0)})
		if err != nil {
			return err
		}
		if err := e.tg.Download(ctx, v.MsgID, fw); err != nil {
			return fmt.Errorf("%s: %w", v.CloudPath, err)
		}
	}
	return zw.Close()
}

func (e *Engine) zipProject(ctx context.Context, zw *zip.Writer, v store.Version, rel string) error {
	tmp, err := e.downloadVerified(ctx, v, e.tmpDir)
	if err != nil {
		return fmt.Errorf("%s: %w", v.CloudPath, err)
	}
	defer os.Remove(tmp)
	zr, err := zip.OpenReader(tmp)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, zf := range zr.File {
		hdr := zf.FileHeader
		hdr.Name = rel + "/" + zf.Name
		raw, err := zf.OpenRaw()
		if err != nil {
			return err
		}
		fw, err := zw.CreateRaw(&hdr)
		if err != nil {
			return err
		}
		if _, err := io.Copy(fw, raw); err != nil {
			return err
		}
	}
	return nil
}

// ---- removing things from the backup ----

// DeleteFromCloud permanently deletes every stored version at or inside
// prefix from Telegram. It refuses while any of those files are still being
// backed up from this PC, because they would simply be uploaded again.
func (e *Engine) DeleteFromCloud(ctx context.Context, prefix string) (int64, error) {
	return e.DeleteFromCloudMany(ctx, []string{prefix})
}

// checkNotBackedUpHere refuses paths still being backed up from this device:
// removing or moving them in Telegram would just make them upload again.
func (e *Engine) checkNotBackedUpHere(prefixes []string) error {
	for _, prefix := range prefixes {
		if prefix == "" {
			return errors.New("choose a folder or file")
		}
		items, err := e.st.ItemsUnder(prefix)
		if err != nil {
			return err
		}
		for _, it := range items {
			if it.LocalPresent && it.State != store.StateSkipped {
				return fmt.Errorf("%s is still on %s and being backed up; stop backing up its folder (or delete it locally) first",
					it.CloudPath, e.deviceWord())
			}
		}
	}
	return nil
}

// versionsUnderAll returns every version under any of the prefixes, once each.
func (e *Engine) versionsUnderAll(prefixes []string) ([]store.Version, error) {
	var out []store.Version
	seen := map[int]bool{}
	for _, prefix := range prefixes {
		vs, err := e.st.VersionsUnder(prefix)
		if err != nil {
			return nil, err
		}
		for _, v := range vs {
			if !seen[v.MsgID] {
				seen[v.MsgID] = true
				out = append(out, v)
			}
		}
	}
	return out, nil
}

func describe(prefixes []string) string {
	if len(prefixes) == 1 {
		return prefixes[0]
	}
	return fmt.Sprintf("%d selected items", len(prefixes))
}

// DeleteFromCloudMany is DeleteFromCloud for several paths at once.
func (e *Engine) DeleteFromCloudMany(ctx context.Context, prefixes []string) (int64, error) {
	if len(prefixes) == 0 {
		return 0, errors.New("choose a folder or file to remove")
	}
	if err := e.checkNotBackedUpHere(prefixes); err != nil {
		return 0, err
	}
	versions, err := e.versionsUnderAll(prefixes)
	if err != nil {
		return 0, err
	}
	if len(versions) == 0 {
		return 0, errors.New("nothing stored there")
	}
	t := e.newTask("delete", "Removing "+describe(prefixes)+" from the backup")
	t.update(func(t *Task) { t.Total = len(versions) })
	go func() {
		ctx := context.WithoutCancel(ctx)
		for i := 0; i < len(versions); i += 100 {
			batch := versions[i:min(i+100, len(versions))]
			ids := make([]int, len(batch))
			for j, v := range batch {
				ids[j] = v.MsgID
			}
			err := e.tg.DeleteMessages(ctx, ids)
			if d, ok := tgerr.AsFloodWait(err); ok {
				time.Sleep(d + time.Second)
				err = e.tg.DeleteMessages(ctx, ids)
			}
			if err != nil {
				t.update(func(t *Task) {
					t.Errors = append(t.Errors, "stopped: "+tgclient.Friendly(err))
					t.Finished = true
				})
				return
			}
			_ = e.st.DeleteVersions(ids)
			t.update(func(t *Task) { t.Done += len(ids) })
		}
		for _, prefix := range prefixes {
			_ = e.st.ForgetGoneUnder(prefix)
		}
		t.update(func(t *Task) { t.Finished = true })
	}()
	return t.ID, nil
}

// ---- freeing up space ----

// FreeCandidate is a local file or project that can be deleted safely.
type FreeCandidate struct {
	ItemID    int64  `json:"itemId"`
	CloudPath string `json:"cloudPath"`
	Kind      string `json:"kind"`
	Size      int64  `json:"size"`
}

// FreePlan says what "free up space" would do for a cloud path.
type FreePlan struct {
	Candidates []FreeCandidate `json:"candidates"`
	Bytes      int64           `json:"bytes"`
	Kept       []string        `json:"kept"` // not safe to delete, with reasons
}

// PlanFree lists local copies under prefix that are safely in Telegram.
func (e *Engine) PlanFree(prefix string) (FreePlan, error) { return e.PlanFreeMany([]string{prefix}) }

// PlanFreeMany is PlanFree for several paths at once.
func (e *Engine) PlanFreeMany(prefixes []string) (FreePlan, error) {
	var items []store.Item
	seen := map[int64]bool{}
	for _, prefix := range prefixes {
		its, err := e.st.ItemsUnder(prefix)
		if err != nil {
			return FreePlan{}, err
		}
		for _, it := range its {
			if !seen[it.ID] {
				seen[it.ID] = true
				items = append(items, it)
			}
		}
	}
	plan := FreePlan{Candidates: []FreeCandidate{}}
	for _, it := range items {
		if !it.LocalPresent {
			continue
		}
		latest, ok, _ := e.st.LatestVersion(it.CloudPath)
		switch {
		case !ok || it.State != store.StateSynced:
			plan.Kept = append(plan.Kept, it.CloudPath+": not backed up yet")
		case it.Kind == store.KindProject && it.Note != "":
			plan.Kept = append(plan.Kept, it.CloudPath+": last backup left out files that were in use")
		case it.Kind == store.KindFile && it.Size != latest.Size:
			plan.Kept = append(plan.Kept, it.CloudPath+": changed since its backup")
		default:
			size := it.Size
			plan.Candidates = append(plan.Candidates, FreeCandidate{ItemID: it.ID, CloudPath: it.CloudPath, Kind: it.Kind, Size: size})
			plan.Bytes += size
		}
	}
	return plan, nil
}

// FreeSpace deletes local copies under prefix after re-checking each one
// against Telegram. It runs as a task.
func (e *Engine) FreeSpace(ctx context.Context, prefix string) (int64, error) {
	return e.FreeSpaceMany(ctx, []string{prefix})
}

// FreeSpaceMany is FreeSpace for several paths at once.
func (e *Engine) FreeSpaceMany(ctx context.Context, prefixes []string) (int64, error) {
	plan, err := e.PlanFreeMany(prefixes)
	if err != nil {
		return 0, err
	}
	if len(plan.Candidates) == 0 {
		return 0, errors.New("nothing here is safely backed up yet, so nothing can be removed")
	}
	title := "Freeing space in " + displayName(prefixes[0])
	if len(prefixes) > 1 {
		title = fmt.Sprintf("Freeing space: %d selected items", len(prefixes))
	}
	t := e.newTask("free", title)
	t.update(func(t *Task) { t.Total = len(plan.Candidates); t.Notes = plan.Kept })
	go func() {
		ctx := context.WithoutCancel(ctx)
		var freed int64
		// Confirm every candidate's latest version still exists in Telegram.
		var ids []int
		latest := map[string]store.Version{}
		for _, c := range plan.Candidates {
			if v, ok, _ := e.st.LatestVersion(c.CloudPath); ok {
				latest[c.CloudPath] = v
				ids = append(ids, v.MsgID)
			}
		}
		remote, err := e.tg.Exists(ctx, ids)
		if err != nil {
			t.update(func(t *Task) {
				t.Errors = append(t.Errors, "could not check Telegram, so nothing was deleted: "+tgclient.Friendly(err))
				t.Finished = true
			})
			return
		}
		var dirs []freedDir
		for _, c := range plan.Candidates {
			t.update(func(t *Task) { t.Current = c.CloudPath })
			v := latest[c.CloudPath]
			rf, ok := remote[v.MsgID]
			var problem string
			switch {
			case !ok:
				problem = "its backup is missing in Telegram"
			case rf.Size != v.Size:
				problem = "its backup in Telegram has the wrong size"
			default:
				problem = e.deleteLocal(c, v, &dirs)
			}
			t.update(func(t *Task) {
				t.Done++
				if problem != "" {
					t.Errors = append(t.Errors, c.CloudPath+": kept, because "+problem)
				}
			})
			if problem == "" {
				freed += c.Size
			}
		}
		removeEmptyDirs(dirs)
		t.update(func(t *Task) {
			t.Finished, t.Current = true, ""
			t.Notes = append(t.Notes, fmt.Sprintf("freed %s", humanBytes(freed)))
		})
		e.RequestScan(0)
	}()
	return t.ID, nil
}

// deleteLocal re-verifies one candidate and deletes it. It returns a reason
// when the file was kept.
type freedDir struct{ dir, root string }

func (e *Engine) deleteLocal(c FreeCandidate, v store.Version, dirs *[]freedDir) string {
	it, err := e.st.Item(c.ItemID)
	if err != nil {
		return "it is no longer tracked"
	}
	root, err := e.st.Root(it.RootID)
	if err != nil {
		return "its backup folder was removed"
	}
	abs := filepath.Join(root.Path, filepath.FromSlash(it.Rel))
	if it.Kind == store.KindProject {
		fp, _, _ := projectFingerprint(abs)
		if fp != v.Fingerprint {
			return "it changed since its last backup"
		}
		if err := os.RemoveAll(abs); err != nil {
			return "deleting failed: " + err.Error()
		}
	} else {
		h, err := hashFile(abs)
		if err != nil {
			return "it could not be read: " + err.Error()
		}
		if h != v.Hash {
			return "it changed since its last backup"
		}
		if err := os.Remove(abs); err != nil {
			return "deleting failed: " + err.Error()
		}
	}
	e.fileChanged(abs)
	*dirs = append(*dirs, freedDir{dir: filepath.Dir(abs), root: filepath.Clean(root.Path)})
	_ = e.st.MarkGone([]int64{it.ID})
	return ""
}

// removeEmptyDirs removes folders left empty, walking upwards but stopping
// at (and never removing) the backup folder itself.
func removeEmptyDirs(dirs []freedDir) {
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i].dir) > len(dirs[j].dir) })
	for _, fd := range dirs {
		d := filepath.Clean(fd.dir)
		for d != fd.root && strings.HasPrefix(d, fd.root+string(filepath.Separator)) {
			entries, err := os.ReadDir(d)
			if err != nil || len(entries) > 0 || os.Remove(d) != nil {
				break
			}
			d = filepath.Dir(d)
		}
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
