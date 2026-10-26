package backup

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Sammy-77A/tgvault/internal/meta"
	"github.com/Sammy-77A/tgvault/internal/store"
)

// cloudPath joins a root label and a slash-separated relative path.
func cloudPath(label, rel string) string {
	if rel == "" {
		return label
	}
	return label + "/" + rel
}

func underPrefix(p, prefix string) bool {
	return prefix == "" || p == prefix || strings.HasPrefix(p, prefix+"/")
}

func isLink(d fs.DirEntry) bool {
	t := d.Type()
	return t&fs.ModeSymlink != 0 || t&fs.ModeIrregular != 0
}

func dirHasFile(dir string) func(string) bool {
	return func(name string) bool {
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	}
}

func markerSet(markers []string) map[string]bool {
	m := make(map[string]bool, len(markers))
	for _, k := range markers {
		m[k] = true
	}
	return m
}

// scanRoot compares a backup folder with the database and queues changes.
func (e *Engine) scanRoot(root store.Root) {
	info, err := os.Stat(root.Path)
	if err != nil || !info.IsDir() {
		// The folder may be on a drive that is unplugged; never treat that as deletions.
		_ = e.st.SetRootScan(root.ID, "folder not found; is the drive connected?")
		return
	}
	projects, err := e.st.ProjectStates(root.ID)
	if err != nil {
		log.Printf("scan %s: %v", root.Path, err)
		return
	}
	existing, err := e.st.ItemsByRoot(root.ID)
	if err != nil {
		log.Printf("scan %s: %v", root.Path, err)
		return
	}
	seen := map[string]bool{}
	var held []string   // cloud-path prefixes waiting for a project decision
	var failed []string // cloud-path prefixes that could not be read
	var dbErr error

	upsert := func(it store.Item) {
		seen[it.CloudPath] = true
		if err := e.st.UpsertSeen(it); err != nil && dbErr == nil {
			dbErr = err
		}
	}

	var walk func(abs, rel string)
	walk = func(abs, rel string) {
		cp := cloudPath(root.Label, rel)
		entries, err := os.ReadDir(abs)
		if err != nil {
			failed = append(failed, cp)
			return
		}
		e.watch(abs, root.ID)
		names := make([]string, len(entries))
		for i, d := range entries {
			names[i] = d.Name()
		}
		var markers []string
		if !e.cfg.NoProjects {
			markers = meta.ProjectMarkers(names)
		}
		if len(markers) > 0 {
			switch projects[rel] {
			case store.ProjectConfirmed:
				fp, size, mtime := projectFingerprint(abs)
				upsert(store.Item{Kind: store.KindProject, CloudPath: cp, RootID: root.ID, Rel: rel,
					Size: size, Mtime: mtime, Fingerprint: fp})
				return
			case store.ProjectIgnored:
				// Back it up as a normal folder.
			default:
				if _, known := projects[rel]; !known {
					if err := e.st.SuggestProject(root.ID, rel, strings.Join(markers, ", ")); err != nil && dbErr == nil {
						dbErr = err
					}
				}
				held = append(held, cp)
				return
			}
		}
		ms := markerSet(markers)
		for _, d := range entries {
			name := d.Name()
			childAbs := filepath.Join(abs, name)
			childRel := path.Join(rel, name)
			if isLink(d) {
				continue
			}
			if d.IsDir() {
				if meta.SkipDir(name, ms, dirHasFile(childAbs)) || meta.SkipPath(childRel) || e.skipAbs(childAbs) {
					continue
				}
				walk(childAbs, childRel)
				continue
			}
			if !d.Type().IsRegular() || meta.SkipFile(name) {
				continue
			}
			fi, err := d.Info()
			if err != nil || fi.Size() == 0 {
				continue // vanished while scanning, or empty: nothing to back up
			}
			it := store.Item{Kind: store.KindFile, CloudPath: cloudPath(root.Label, childRel), RootID: root.ID,
				Rel: childRel, Size: fi.Size(), Mtime: fi.ModTime().Unix()}
			upsert(it)
		}
	}
	walk(root.Path, "")

	var gone []int64
	for p, it := range existing {
		if seen[p] || !it.LocalPresent {
			continue
		}
		keep := false
		for _, pre := range append(held, failed...) {
			if underPrefix(p, pre) {
				keep = true
				break
			}
		}
		if !keep {
			gone = append(gone, it.ID)
		}
	}
	if err := e.st.MarkGone(gone); err != nil && dbErr == nil {
		dbErr = err
	}
	msg := ""
	if len(failed) > 0 {
		msg = fmt.Sprintf("%d folder(s) could not be read, e.g. %s", len(failed), failed[0])
	}
	if dbErr != nil {
		msg = "database error: " + dbErr.Error()
		log.Printf("scan %s: %v", root.Path, dbErr)
	}
	_ = e.st.SetRootScan(root.ID, msg)
	e.poke()
}

// projectFile is one file included in a project ZIP.
type projectFile struct {
	abs, rel string
	size     int64
	mtime    int64
}

// projectFiles lists what goes into a project ZIP, skipping rebuildable folders.
func projectFiles(dir string) []projectFile {
	var out []projectFile
	var walk func(abs, rel string)
	walk = func(abs, rel string) {
		entries, err := os.ReadDir(abs)
		if err != nil {
			return
		}
		names := make([]string, len(entries))
		for i, d := range entries {
			names[i] = d.Name()
		}
		ms := markerSet(meta.ProjectMarkers(names))
		for _, d := range entries {
			name := d.Name()
			childAbs := filepath.Join(abs, name)
			childRel := path.Join(rel, name)
			if isLink(d) {
				continue
			}
			if d.IsDir() {
				if !meta.SkipDir(name, ms, dirHasFile(childAbs)) {
					walk(childAbs, childRel)
				}
				continue
			}
			if !d.Type().IsRegular() || meta.SkipFile(name) {
				continue
			}
			fi, err := d.Info()
			if err != nil {
				continue
			}
			out = append(out, projectFile{abs: childAbs, rel: childRel, size: fi.Size(), mtime: fi.ModTime().Unix()})
		}
	}
	walk(dir, "")
	sort.Slice(out, func(i, j int) bool { return out[i].rel < out[j].rel })
	return out
}

// projectFingerprint summarises a project's files so changes can be detected
// without reading them.
func projectFingerprint(dir string) (fp string, size, mtime int64) {
	return fingerprintOf(projectFiles(dir))
}

func fingerprintOf(files []projectFile) (fp string, size, mtime int64) {
	h := sha256.New()
	for _, f := range files {
		fmt.Fprintf(h, "%s\x00%d\x00%d\n", f.rel, f.size, f.mtime)
		size += f.size
		if f.mtime > mtime {
			mtime = f.mtime
		}
	}
	return hex.EncodeToString(h.Sum(nil)), size, mtime
}
