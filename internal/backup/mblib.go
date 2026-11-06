package backup

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Sammy-77A/tgvault/internal/media"
	"github.com/Sammy-77A/tgvault/internal/meta"
	"github.com/Sammy-77A/tgvault/internal/store"
)

// The movie app's view of the library folder: Movies and Shows as the
// organizer files them, with what's on this phone (downloaded for offline
// watching) next to what's in Telegram.

// DataDir is where the engine keeps its own files.
func (e *Engine) DataDir() string { return e.dataDir }

// LibSub is a subtitle file beside a video.
type LibSub struct {
	Path   string `json:"path"`
	MsgID  int    `json:"msgId"`
	Lang   string `json:"lang,omitempty"`
	Local  bool   `json:"local"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Source string `json:"source,omitempty"` // "library", or "downloaded" when it came from a subtitle search
}

// LibFile is one video in the library.
type LibFile struct {
	Path     string   `json:"path"`
	Name     string   `json:"name"`
	MsgID    int      `json:"msgId"`
	Size     int64    `json:"size"`
	Season   int      `json:"season,omitempty"`
	Episodes []int    `json:"episodes,omitempty"`
	Local    bool     `json:"local"` // downloaded to this phone
	Subs     []LibSub `json:"subs,omitempty"`
	Pos      int64    `json:"pos,omitempty"` // where playback left off, milliseconds
	Dur      int64    `json:"dur,omitempty"`
	Added    int64    `json:"added,omitempty"` // when it was uploaded
}

// LibSeason is the episodes of a show's season that are in the library.
type LibSeason struct {
	Number   int       `json:"number"`
	Episodes []LibFile `json:"episodes"`
}

// LibTitle is a movie or a show.
type LibTitle struct {
	Kind    string      `json:"kind"` // "movie" or "show"
	Title   string      `json:"title"`
	Year    int         `json:"year,omitempty"`
	Norm    string      `json:"norm"`
	Folder  string      `json:"folder"`
	Files   []LibFile   `json:"files,omitempty"`   // a movie's videos
	Seasons []LibSeason `json:"seasons,omitempty"` // a show's seasons
	Size    int64       `json:"size"`
	Newest  int64       `json:"newest"`
}

// MediaLocal is where a library file lives on this device once downloaded.
func (e *Engine) MediaLocal(cloudPath string) string {
	rel := strings.TrimPrefix(cloudPath, e.MediaRoot()+"/")
	return filepath.Join(e.dataDir, "mb", filepath.FromSlash(rel))
}

func (e *Engine) haveLocal(v store.Version) bool {
	fi, err := os.Stat(e.MediaLocal(v.CloudPath))
	return err == nil && !fi.IsDir() && fi.Size() == v.Size
}

func langOf(name string) string {
	stem := strings.TrimSuffix(name, path.Ext(name))
	if i := strings.LastIndex(stem, "."); i >= 0 && langTags[strings.ToLower(stem[i+1:])] {
		return strings.ToLower(stem[i+1:])
	}
	return ""
}

// Library lists the movies and shows in the library folder.
func (e *Engine) Library() ([]LibTitle, error) {
	root := e.MediaRoot()
	vs, err := e.st.LatestUnder(root)
	if err != nil {
		return nil, err
	}
	type key struct{ kind, folder string }
	titles := map[key]*LibTitle{}
	subsOf := map[string][]LibSub{} // folder -> subtitle files
	var vids []store.Version
	for _, v := range vs {
		rel := strings.TrimPrefix(v.CloudPath, root+"/")
		if !strings.HasPrefix(rel, "Movies/") && !strings.HasPrefix(rel, "Shows/") {
			continue
		}
		ext := path.Ext(v.CloudPath)
		switch {
		case media.IsSubtitle(ext):
			dir := path.Dir(v.CloudPath)
			subsOf[dir] = append(subsOf[dir], LibSub{Path: v.CloudPath, MsgID: v.MsgID, Lang: langOf(v.CloudPath),
				Local: e.haveLocal(v), Name: path.Base(v.CloudPath), Size: v.Size, Source: "library"})
		case media.IsVideo(ext):
			vids = append(vids, v)
		}
	}
	for _, v := range vids {
		rel := strings.TrimPrefix(v.CloudPath, root+"/")
		parts := strings.Split(rel, "/")
		info := media.Parse(parts[len(parts)-1])
		f := LibFile{Path: v.CloudPath, Name: path.Base(v.CloudPath), MsgID: v.MsgID, Size: v.Size, Local: e.haveLocal(v), Added: v.UploadedAt}
		f.Pos, f.Dur = e.Progress(v.CloudPath)
		stem := strings.TrimSuffix(f.Name, path.Ext(f.Name))
		for _, s := range subsOf[path.Dir(v.CloudPath)] {
			if strings.HasPrefix(strings.TrimSuffix(s.Name, path.Ext(s.Name)), stem) {
				f.Subs = append(f.Subs, s)
			}
		}
		switch parts[0] {
		case "Movies":
			// Movies/Title (Year)/file, or just Movies/file when filed by hand
			if len(parts) < 2 {
				continue
			}
			folder := root + "/Movies/" + parts[1]
			t := titles[key{"movie", folder}]
			if t == nil {
				mi := media.Parse(parts[1])
				if len(parts) == 2 {
					mi.Title = info.Title
					mi.Year = info.Year
				}
				t = &LibTitle{Kind: "movie", Title: mi.Title, Year: mi.Year, Folder: folder}
				if t.Title == "" {
					t.Title = parts[1]
				}
				t.Norm = media.Norm(t.Title)
				titles[key{"movie", folder}] = t
			}
			t.Files = append(t.Files, f)
			t.Size += f.Size
			t.Newest = max(t.Newest, v.UploadedAt)
		case "Shows":
			// Shows/Title/Season 01/file, or Shows/Title/file with the season in the name
			if len(parts) < 3 {
				continue
			}
			folder := root + "/Shows/" + parts[1]
			t := titles[key{"show", folder}]
			if t == nil {
				mi := media.Parse(parts[1])
				t = &LibTitle{Kind: "show", Title: mi.Title, Year: mi.Year, Folder: folder}
				if t.Title == "" {
					t.Title = parts[1]
				}
				t.Norm = media.Norm(t.Title)
				titles[key{"show", folder}] = t
			}
			f.Season, f.Episodes = info.Season, info.Episodes
			if !info.HasSeason && len(parts) >= 4 {
				if m := parseSeasonDir(parts[2]); m >= 0 {
					f.Season = m
				}
			}
			if len(f.Episodes) == 0 {
				continue
			}
			var sn *LibSeason
			for i := range t.Seasons {
				if t.Seasons[i].Number == f.Season {
					sn = &t.Seasons[i]
				}
			}
			if sn == nil {
				t.Seasons = append(t.Seasons, LibSeason{Number: f.Season})
				sn = &t.Seasons[len(t.Seasons)-1]
			}
			sn.Episodes = append(sn.Episodes, f)
			t.Size += f.Size
			t.Newest = max(t.Newest, v.UploadedAt)
		}
	}
	out := make([]LibTitle, 0, len(titles))
	for _, t := range titles {
		sort.Slice(t.Files, func(i, j int) bool { return t.Files[i].Name < t.Files[j].Name })
		sort.Slice(t.Seasons, func(i, j int) bool { return t.Seasons[i].Number < t.Seasons[j].Number })
		for i := range t.Seasons {
			eps := t.Seasons[i].Episodes
			sort.Slice(eps, func(a, b int) bool { return eps[a].Episodes[0] < eps[b].Episodes[0] })
		}
		out = append(out, *t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return strings.ToLower(out[i].Title) < strings.ToLower(out[j].Title)
	})
	return out, nil
}

func parseSeasonDir(name string) int {
	f := strings.Fields(name)
	if len(f) == 2 && strings.EqualFold(f[0], "season") {
		if n, err := strconv.Atoi(f[1]); err == nil {
			return n
		}
	}
	return -1
}

// inLibrary reports whether a cloud path is inside the movie library.
func (e *Engine) inMediaRoot(p string) bool { return underPrefix(p, e.MediaRoot()) }

// RemoveMedia deletes downloaded copies from this device (the library in
// Telegram is untouched).
func (e *Engine) RemoveMedia(paths []string) (int, error) {
	root := e.MediaRoot()
	n := 0
	base := filepath.Join(e.dataDir, "mb")
	for _, p := range paths {
		if !e.inMediaRoot(p) || p == root {
			return n, errors.New("that isn't in the library")
		}
		local := e.MediaLocal(p)
		if !strings.HasPrefix(local, base+string(filepath.Separator)) {
			continue
		}
		fi, err := os.Stat(local)
		if err != nil {
			continue
		}
		if fi.IsDir() {
			if err := os.RemoveAll(local); err == nil {
				n++
			}
		} else if err := os.Remove(local); err == nil {
			n++
		}
		// tidy emptied folders up to the library's top
		for d := filepath.Dir(local); strings.HasPrefix(d, base+string(filepath.Separator)); d = filepath.Dir(d) {
			if os.Remove(d) != nil {
				break
			}
		}
	}
	return n, nil
}

// ---- where I left off ----

func progressKey(cloudPath string) string { return "mb.pos:" + cloudPath }

// SetProgress remembers how far into a file playback got (milliseconds).
func (e *Engine) SetProgress(cloudPath string, pos, dur int64) error {
	if !e.inMediaRoot(cloudPath) {
		return errors.New("that isn't in the library")
	}
	if pos < 0 || dur <= 0 {
		return errors.New("bad position")
	}
	// within the last few percent counts as watched: start over next time
	if pos > dur*95/100 || pos < 5000 {
		return e.st.Set(progressKey(cloudPath), fmt.Sprintf("0/%d", dur))
	}
	return e.st.Set(progressKey(cloudPath), fmt.Sprintf("%d/%d", pos, dur))
}

// Progress returns the saved position and length of a file, 0 when unknown.
func (e *Engine) Progress(cloudPath string) (pos, dur int64) {
	f := strings.SplitN(e.st.Get(progressKey(cloudPath)), "/", 2)
	if len(f) != 2 {
		return 0, 0
	}
	pos, _ = strconv.ParseInt(f[0], 10, 64)
	dur, _ = strconv.ParseInt(f[1], 10, 64)
	return pos, dur
}

// MediaDiskUsage is how much the downloaded videos take on this device.
func (e *Engine) MediaDiskUsage() int64 {
	var n int64
	_ = filepath.WalkDir(filepath.Join(e.dataDir, "mb"), func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if fi, err := d.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	return n
}

// RefreshLibrary forgets library files that were deleted from Telegram (by the
// owner, on the PC), so the phone doesn't offer them. It asks Telegram about
// just the library's files, which is cheap, and does nothing when it can't
// get an answer.
func (e *Engine) RefreshLibrary(ctx context.Context) {
	if !e.Connected() {
		return
	}
	// Files the owner's PC added since this phone last looked.
	if _, err := e.SyncNew(ctx); err != nil && ctx.Err() == nil {
		log.Printf("library: looking for new files: %v", err)
	}
	vs, err := e.st.VersionsUnder(e.MediaRoot())
	if err != nil || len(vs) == 0 {
		return
	}
	ids := make([]int, 0, len(vs))
	for _, v := range vs {
		ids = append(ids, v.MsgID)
	}
	remote, err := e.tg.Exists(ctx, ids)
	if err != nil || len(remote) == 0 {
		// no answer, or an answer that says nothing exists: more likely a
		// hiccup than the whole library having been deleted
		return
	}
	var gone []int
	have := map[int]string{}
	for _, v := range vs {
		have[v.MsgID] = v.CloudPath
	}
	for _, id := range ids {
		rf, ok := remote[id]
		if !ok {
			gone = append(gone, id)
			continue
		}
		// The owner's PC files loose videos into Movies/ and Shows/ after
		// they've arrived, which changes the path in the message's caption.
		if in, ok := meta.Parse(rf.Caption); ok && !in.Split() && !in.Change() && in.Path != "" && in.Path != have[id] {
			e.followLibraryMove(id, have[id], in.Path)
		}
	}
	if len(gone) > 0 {
		_ = e.st.DeleteVersions(gone)
	}
}

// followLibraryMove records that the owner moved a library file, and moves the
// copy already on this phone along with it.
func (e *Engine) followLibraryMove(id int, from, to string) {
	if err := e.st.MoveVersion(id, to); err != nil {
		return
	}
	log.Printf("library: %s is now %s", from, to)
	if d, ok := e.st.DLGet(from); ok {
		_ = e.st.DLDelete(from)
		d.Path = to
		_ = e.st.DLUpsert(d)
	}
	for _, suffix := range []string{"", ".part"} {
		src, dst := e.MediaLocal(from)+suffix, e.MediaLocal(to)+suffix
		if _, err := os.Stat(src); err == nil {
			if _, err := os.Stat(dst); err != nil {
				_ = moveOnDisk(src, dst, false)
			}
		}
	}
}
