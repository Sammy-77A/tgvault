package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Sammy-77A/tgvault/internal/meta"
	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/tgclient"
)

// Organizing the backup: folders, moving and renaming. A file's folder is just
// its path, kept in the caption of its Telegram message, so moving it there is
// a caption edit. When the file is also on this device it is moved on disk in
// the same step; otherwise the next scan would see a "new" file at the old
// place and back it up again.

// A relocation is one folder or file taking a new path.
type relocation struct{ from, to string }

// badName reports whether name can't be used for a file or folder.
func badName(name string) bool {
	return name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\<>:\"|?*") ||
		strings.ContainsFunc(name, func(r rune) bool { return r < 32 })
}

const badNameMsg = `a name can't be empty or contain any of  / \ < > : " | ? *`

// MoveInCloud moves files or folders into another folder of the backup, and
// the real files with them when they are on this device. Other devices follow
// the next time they look at Telegram.
func (e *Engine) MoveInCloud(ctx context.Context, prefixes []string, toFolder string) (int64, error) {
	toFolder = strings.Trim(path.Clean("/"+toFolder), "/")
	if toFolder == "" {
		return 0, errors.New("choose a folder inside the backup to move to")
	}
	var rels []relocation
	for _, p := range prefixes {
		rels = append(rels, relocation{p, toFolder + "/" + path.Base(p)})
	}
	return e.relocate(ctx, "Moving "+describe(prefixes)+" to "+toFolder, rels)
}

// RenameInCloud gives a file or folder a new name.
func (e *Engine) RenameInCloud(ctx context.Context, prefix, name string) (int64, error) {
	name = strings.TrimSpace(name)
	if badName(name) {
		return 0, errors.New(badNameMsg)
	}
	to := path.Join(path.Dir(prefix), name)
	return e.relocate(ctx, "Renaming "+path.Base(prefix)+" to "+name, []relocation{{prefix, to}})
}

// NewFolder makes an empty folder inside parent, on this device too when the
// parent is one of its backed-up folders. It returns the new folder's path.
func (e *Engine) NewFolder(parent, name string) (string, error) {
	name = strings.TrimSpace(name)
	if badName(name) {
		return "", errors.New(badNameMsg)
	}
	if parent == "" {
		return "", errors.New("open one of your backed-up folders first; new folders go inside it")
	}
	if meta.DeviceOf(parent) != e.Device() {
		return "", errors.New("that part of the backup belongs to another device; make the folder there, or move files into a folder that already exists")
	}
	p := path.Join(parent, name)
	if root, rel, ok := e.rootFor(p); ok && rel != "" {
		if _, err := os.Stat(root.Path); err != nil {
			return "", errors.New("that folder isn't reachable right now; is the drive connected?")
		}
		if err := os.MkdirAll(filepath.Join(root.Path, filepath.FromSlash(rel)), 0o755); err != nil {
			return "", fmt.Errorf("couldn't make the folder: %w", err)
		}
	}
	return p, e.st.AddFolder(p)
}

type unit struct{ from, to string }

func (e *Engine) relocate(ctx context.Context, title string, rels []relocation) (int64, error) {
	if len(rels) == 0 {
		return 0, errors.New("nothing selected")
	}
	roots, _ := e.st.Roots()
	var plan []relocation
	for _, r := range rels {
		if r.from == "" || r.to == "" || r.from == r.to {
			continue
		}
		if meta.DeviceOf(r.from) != meta.DeviceOf(r.to) {
			return 0, errors.New("files can't move between the PC's and the phone's parts of the backup")
		}
		if underPrefix(r.to, r.from) {
			return 0, fmt.Errorf("can't move %s into itself", r.from)
		}
		for _, root := range roots {
			if underPrefix(root.Label, r.from) {
				return 0, fmt.Errorf("%s is a folder backed up from %s as a whole; move or rename what's inside it instead", r.from, e.deviceWord())
			}
		}
		plan = append(plan, r)
	}
	if len(plan) == 0 {
		return 0, errors.New("they're already there")
	}
	// A folder and something inside it both selected: the folder carries it.
	var top []relocation
	for _, r := range plan {
		nested := false
		for _, o := range plan {
			if o.from != r.from && underPrefix(r.from, o.from) {
				nested = true
			}
		}
		if !nested {
			top = append(top, r)
		}
	}
	plan = top

	var units []unit
	moving := map[int]bool{}
	seen := map[string]bool{}
	folders := map[string][]string{} // relocation.from -> recorded empty folders
	recorded := 0
	for _, r := range plan {
		paths, err := e.pathsUnder(r.from, moving)
		if err != nil {
			return 0, err
		}
		for _, p := range paths {
			if !seen[p] {
				seen[p] = true
				units = append(units, unit{p, r.to + strings.TrimPrefix(p, r.from)})
			}
		}
		folders[r.from], _ = e.st.FoldersAt(r.from)
		recorded += len(folders[r.from])
	}
	if len(units) == 0 && recorded == 0 {
		return 0, errors.New("nothing stored there")
	}

	t := e.newTask("move", title)
	t.update(func(t *Task) { t.Total = len(units) })
	go func() {
		ctx := context.WithoutCancel(ctx)
		emptied := map[string]bool{} // cloud folders a moved file left
		for _, u := range units {
			t.update(func(t *Task) { t.Current = u.from })
			err := e.relocateOne(ctx, u.from, u.to, moving)
			if err == nil {
				emptied[path.Dir(u.from)] = true
			}
			t.update(func(t *Task) {
				t.Done++
				if err != nil {
					t.Errors = append(t.Errors, u.from+": "+tgclient.Friendly(err))
				}
			})
		}
		for _, r := range plan {
			e.moveFolders(r, folders[r.from], emptied)
			_ = e.st.ForgetGoneUnder(r.from)
		}
		t.update(func(t *Task) { t.Finished, t.Current = true, "" })
	}()
	return t.ID, nil
}

// pathsUnder lists every path under from that has stored versions or a file on
// this device, and notes the stored versions in moving.
func (e *Engine) pathsUnder(from string, moving map[int]bool) ([]string, error) {
	vs, err := e.st.VersionsUnder(from)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, v := range vs {
		set[v.CloudPath] = true
		moving[v.MsgID] = true
	}
	items, err := e.st.ItemsUnder(from)
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		if it.LocalPresent {
			set[it.CloudPath] = true
		}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// relocateOne moves one path: its stored versions in Telegram and its file on
// this device. Scans wait, so none sees the file half-moved.
func (e *Engine) relocateOne(ctx context.Context, from, to string, moving map[int]bool) error {
	e.orgMu.Lock()
	defer e.orgMu.Unlock()
	var lm *localMove
	if it, err := e.st.ItemByPath(from); err == nil && it.LocalPresent {
		if lm, err = e.planLocal(it, to); err != nil {
			return err
		}
	}
	vs, err := e.st.Versions(from)
	if err != nil {
		return err
	}
	var done []store.Version
	undo := func() {
		for _, v := range done {
			if err := e.moveVersion(ctx, v, from, moving); err != nil {
				log.Printf("move %s: couldn't put a stored copy back: %v", from, err)
			}
		}
	}
	for _, v := range vs {
		if err := e.moveVersion(ctx, v, to, moving); err != nil {
			undo()
			return err
		}
		done = append(done, v)
	}
	if lm != nil {
		if err := e.doLocal(lm); err != nil {
			undo()
			return err
		}
	}
	return nil
}

// moveFolders carries recorded empty folders to the new place and tidies the
// folders that moving left empty on this device. Only folders inside what was
// moved are removed, and only when they are empty.
func (e *Engine) moveFolders(r relocation, recorded []string, emptied map[string]bool) {
	dirs := map[string]bool{}
	for d := range emptied {
		if underPrefix(d, r.from) {
			dirs[d] = true
		}
	}
	for _, f := range recorded {
		to := r.to + strings.TrimPrefix(f, r.from)
		if root, rel, ok := e.rootFor(to); ok && rel != "" {
			_ = os.MkdirAll(filepath.Join(root.Path, filepath.FromSlash(rel)), 0o755)
		}
		dirs[f] = true
	}
	_ = e.st.MoveFolders(r.from, r.to)
	list := make([]string, 0, len(dirs))
	for d := range dirs {
		list = append(list, d)
	}
	sort.Slice(list, func(i, j int) bool { return len(list[i]) > len(list[j]) })
	for _, d := range list {
		for cp := d; underPrefix(cp, r.from); cp = path.Dir(cp) {
			root, rel, ok := e.rootFor(cp)
			if !ok || rel == "" || os.Remove(filepath.Join(root.Path, filepath.FromSlash(rel))) != nil {
				break
			}
		}
	}
}

// localMove is a file or project folder about to move on this device.
type localMove struct {
	it             store.Item
	to             string
	oldAbs, newAbs string
	newRoot        store.Root
	newRel         string
}

// planLocal checks that a tracked item can move to the cloud path to on this
// device. It returns nil when the item is no longer on disk (a scan will see
// that), and an error when it can't or shouldn't be moved.
func (e *Engine) planLocal(it store.Item, to string) (*localMove, error) {
	if it.State == store.StateUploading {
		return nil, errors.New("it is being backed up right now; try again in a moment")
	}
	root, err := e.st.Root(it.RootID)
	if err != nil {
		return nil, errors.New("its backup folder was removed")
	}
	if _, err := os.Stat(root.Path); err != nil {
		return nil, errors.New("its folder isn't reachable right now; is the drive connected?")
	}
	oldAbs := filepath.Join(root.Path, filepath.FromSlash(it.Rel))
	old, err := os.Lstat(oldAbs)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	newRoot, newRel, ok := e.rootFor(to)
	if !ok || newRel == "" {
		return nil, fmt.Errorf("%s isn't inside a folder backed up from %s, so the file on %s has nowhere to go; "+
			"move it into one of your backed-up folders", to, e.deviceWord(), e.deviceWord())
	}
	if _, err := os.Stat(newRoot.Path); err != nil {
		return nil, errors.New("the destination folder isn't reachable right now; is the drive connected?")
	}
	newAbs := filepath.Join(newRoot.Path, filepath.FromSlash(newRel))
	if cur, err := os.Lstat(newAbs); err == nil {
		if !os.SameFile(old, cur) {
			return nil, fmt.Errorf("something is already at %s on %s", newAbs, e.deviceWord())
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return &localMove{it: it, to: to, oldAbs: oldAbs, newAbs: newAbs, newRoot: newRoot, newRel: newRel}, nil
}

// doLocal moves the file or folder and updates what's tracked about it.
func (e *Engine) doLocal(m *localMove) error {
	if err := moveOnDisk(m.oldAbs, m.newAbs, m.it.Kind == store.KindProject); err != nil {
		return fmt.Errorf("couldn't move it on %s: %w", e.deviceWord(), err)
	}
	if err := e.st.MoveItem(m.it.ID, m.to, m.newRoot.ID, m.newRel); err != nil {
		return err
	}
	if m.it.Kind == store.KindProject {
		return e.st.MoveProject(m.it.CloudPath, m.to, m.it.RootID, m.it.Rel, m.newRoot.ID, m.newRel)
	}
	return nil
}

// followLocal moves the file on this device after its stored copy was moved
// from another device: organizing on the PC organizes the phone too. When it
// can't, the file stays where it is.
func (e *Engine) followLocal(have, now string) {
	if meta.DeviceOf(have) != e.Device() {
		return
	}
	e.orgMu.Lock()
	defer e.orgMu.Unlock()
	it, err := e.st.ItemByPath(have)
	if err != nil || !it.LocalPresent {
		return
	}
	lm, err := e.planLocal(it, now)
	if err == nil && lm != nil {
		err = e.doLocal(lm)
	}
	if err != nil {
		log.Printf("moved elsewhere, left here: %s -> %s: %v", have, now, err)
		return
	}
	if lm != nil {
		log.Printf("followed a move: %s is now %s on %s", have, now, e.deviceWord())
	}
}

// moveOnDisk renames src to dst, creating dst's folder. Across drives a file
// is copied (keeping its time) and then removed.
func moveOnDisk(src, dst string, dir bool) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	renameErr := os.Rename(src, dst)
	if renameErr == nil {
		return nil
	}
	if dir {
		return renameErr
	}
	fi, err := os.Stat(src)
	if err != nil {
		return renameErr
	}
	if _, err := os.Lstat(dst); err == nil {
		return renameErr
	}
	in, err := os.Open(src)
	if err != nil {
		return renameErr
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".tgvault-move-*")
	if err != nil {
		return err
	}
	_, err = io.Copy(tmp, in)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chtimes(tmp.Name(), fi.ModTime(), fi.ModTime())
	}
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return err
	}
	in.Close()
	if err := os.Remove(src); err != nil {
		os.Remove(dst) // keep the original as it was
		return err
	}
	return nil
}
