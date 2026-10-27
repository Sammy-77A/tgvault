package backup

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Sammy-77A/tgvault/internal/meta"
	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/tgclient"
)

// syncEvery is how often tgvault looks for files another device just backed up.
const syncEvery = time.Minute

// syncLoop picks up new uploads from other devices soon after they happen,
// between the full checks that also notice deletions.
func (e *Engine) syncLoop(ctx context.Context) {
	t := time.NewTicker(syncEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if e.tg.Info().Status == tgclient.StatusReady {
				if _, err := e.SyncNew(ctx); err != nil && ctx.Err() == nil {
					log.Printf("sync: %v", err)
				}
			}
		}
	}
}

// settingSyncSeen is the newest channel message tgvault has looked at. It's
// kept apart from this device's own uploads: another device can post a message
// with a lower ID while this one is uploading.
const settingSyncSeen = "sync_seen"

// SyncNew adds files uploaded to the channel since the last look, such as
// ones another device just backed up. It returns how many were new here.
func (e *Engine) SyncNew(ctx context.Context) (int, error) {
	from, _ := strconv.Atoi(e.st.Get(settingSyncSeen))
	if from == 0 {
		from = e.st.MaxVersionID()
	}
	added, newest := 0, from
	err := e.tg.ScanSince(ctx, from, func(rf tgclient.RemoteFile) error {
		newest = max(newest, rf.MsgID)
		in, ok := meta.Parse(rf.Caption)
		if !ok {
			return nil
		}
		if in.Split() {
			// Only the last part completes a split file; it lists the others.
			if ok, err := e.addSplitFromLastPart(ctx, rf, in); err != nil || !ok {
				return err
			}
			added++
			return nil
		}
		if _, err := e.st.Version(rf.MsgID); err == nil {
			return nil // one of this device's own uploads
		}
		added++
		return e.st.AddVersion(versionFromCaption(rf.MsgID, rf.Date, in))
	})
	if err != nil {
		return added, err
	}
	e.dropBrokenChanges()
	_ = e.st.Set(settingSyncSeen, strconv.Itoa(newest))
	if added > 0 {
		log.Printf("sync: %d new file(s) from another device", added)
	}
	return added, nil
}

// ---- search ----

// Query filters the file browser.
type Query struct {
	Prefix string // search inside this folder and its subfolders
	Text   string // part of the file name, any case
	Type   string // meta.Type*, or "" for any
	From   int64  // modified at or after (unix seconds), 0 for any
	To     int64  // modified before (unix seconds), 0 for any
	Limit  int
}

// Search finds backed-up files by name, type and date anywhere under a folder.
// It returns at most q.Limit entries and the total number of matches.
func (e *Engine) Search(q Query) ([]Entry, int, error) {
	latest, err := e.st.LatestUnder(q.Prefix)
	if err != nil {
		return nil, 0, err
	}
	counts, err := e.st.VersionCounts()
	if err != nil {
		return nil, 0, err
	}
	items, err := e.st.ItemsUnder(q.Prefix)
	if err != nil {
		return nil, 0, err
	}
	byPath := map[string]store.Item{}
	for _, it := range items {
		byPath[it.CloudPath] = it
	}
	text := strings.ToLower(strings.TrimSpace(q.Text))
	out := []Entry{}
	total := 0
	for _, v := range latest {
		name := path.Base(v.CloudPath)
		typ := meta.FileType(name)
		if v.Kind == store.KindProject {
			typ = meta.TypeArchive
		}
		switch {
		case text != "" && !strings.Contains(strings.ToLower(name), text):
			continue
		case q.Type != "" && typ != q.Type:
			continue
		case q.From > 0 && v.Mtime < q.From:
			continue
		case q.To > 0 && v.Mtime >= q.To:
			continue
		}
		total++
		if q.Limit > 0 && len(out) >= q.Limit {
			continue
		}
		kind := "file"
		if v.Kind == store.KindProject {
			kind = "project"
		}
		out = append(out, Entry{Name: name, Path: v.CloudPath, Kind: kind, Size: e.shownSize(v), Mtime: v.Mtime,
			Versions: counts[v.CloudPath], Local: localState(v, byPath, e.Device()), MsgID: v.MsgID,
			Uploaded: v.UploadedAt, Device: meta.DeviceOf(v.CloudPath), Type: typ, Parts: v.Parts, Changes: v.Seq})
	}
	return out, total, nil
}

// ---- moving inside the backup ----

// MoveInCloud moves files or folders to another folder of the backup by
// rewriting their captions in Telegram. Like removing, it's only for things
// no longer backed up from this device, or they would upload again.
func (e *Engine) MoveInCloud(ctx context.Context, prefixes []string, toFolder string) (int64, error) {
	toFolder = strings.Trim(path.Clean("/"+toFolder), "/")
	if toFolder == "" {
		return 0, errors.New("choose a folder inside the backup to move to")
	}
	if err := e.checkNotBackedUpHere(prefixes); err != nil {
		return 0, err
	}
	type move struct {
		v  store.Version
		to string
	}
	var moves []move
	moving := map[int]bool{}
	for _, prefix := range prefixes {
		if meta.DeviceOf(prefix) != meta.DeviceOf(toFolder) {
			return 0, errors.New("files can't move between the PC's and the phone's parts of the backup")
		}
		if underPrefix(toFolder, prefix) {
			return 0, fmt.Errorf("can't move %s into itself", prefix)
		}
		vs, err := e.st.VersionsUnder(prefix)
		if err != nil {
			return 0, err
		}
		parent := path.Dir(prefix)
		for _, v := range vs {
			rel := v.CloudPath
			if parent != "." {
				rel = strings.TrimPrefix(v.CloudPath, parent+"/")
			}
			moves = append(moves, move{v, toFolder + "/" + rel})
			moving[v.MsgID] = true
		}
	}
	if len(moves) == 0 {
		return 0, errors.New("nothing stored there")
	}
	t := e.newTask("move", "Moving "+describe(prefixes)+" to "+toFolder)
	t.update(func(t *Task) { t.Total = len(moves) })
	go func() {
		ctx := context.WithoutCancel(ctx)
		for _, m := range moves {
			t.update(func(t *Task) { t.Current = m.v.CloudPath })
			err := e.moveVersion(ctx, m.v, m.to, moving)
			t.update(func(t *Task) {
				t.Done++
				if err != nil {
					t.Errors = append(t.Errors, m.v.CloudPath+": "+tgclient.Friendly(err))
				}
			})
		}
		for _, prefix := range prefixes {
			_ = e.st.ForgetGoneUnder(prefix)
		}
		t.update(func(t *Task) { t.Finished, t.Current = true, "" })
	}()
	return t.ID, nil
}

// moveVersion moves one version. moving holds every version being moved with
// it (such as a file's older versions), which don't count as being in the way.
func (e *Engine) moveVersion(ctx context.Context, v store.Version, to string, moving map[int]bool) error {
	if existing, ok, _ := e.st.LatestVersion(to); ok && !moving[existing.MsgID] && existing.Hash != v.Hash {
		return errors.New("a different file already has that name there; rename or remove it first")
	}
	if v.Parts > 0 {
		if err := e.editSplitCaptions(ctx, v, to); err != nil {
			return err
		}
		return e.st.MoveVersion(v.MsgID, to)
	}
	caption, err := versionCaption(v, to)
	if err != nil {
		return err
	}
	if err := e.editCaption(ctx, v.MsgID, caption); err != nil {
		return err
	}
	return e.st.MoveVersion(v.MsgID, to)
}

// Links returns t.me links that open the latest version of each file under
// the given paths in the Telegram app, for sharing or forwarding.
func (e *Engine) Links(prefixes []string) ([]string, error) {
	var out []string
	seen := map[int]bool{}
	for _, prefix := range prefixes {
		vs, err := e.st.LatestUnder(prefix)
		if err != nil {
			return nil, err
		}
		for _, v := range vs {
			if seen[v.MsgID] {
				continue
			}
			seen[v.MsgID] = true
			if v.IsChange() {
				chain, err := e.chainPieces(v)
				if err != nil {
					return nil, err
				}
				for _, c := range chain {
					parts, err := e.pieces(c)
					if err != nil {
						return nil, err
					}
					what := "full backup"
					if c.IsChange() {
						what = fmt.Sprintf("change %d", c.Seq)
					}
					for _, p := range parts {
						if l := e.tg.MessageLink(p.MsgID); l != "" {
							label := what
							if len(parts) > 1 {
								label += fmt.Sprintf(", part %d of %d", p.Index, len(parts))
							}
							out = append(out, fmt.Sprintf("%s (%s): %s", path.Base(v.CloudPath), label, l))
						}
					}
				}
				continue
			}
			parts, err := e.pieces(v)
			if err != nil {
				return nil, err
			}
			for _, p := range parts {
				l := e.tg.MessageLink(p.MsgID)
				if l == "" {
					continue
				}
				if len(parts) > 1 {
					// The parts must be downloaded and joined to get the file back.
					l = fmt.Sprintf("%s (part %d of %d): %s", path.Base(v.CloudPath), p.Index, len(parts), l)
				}
				out = append(out, l)
			}
		}
	}
	return out, nil
}

// ---- recommendations ----

// Recommendation is a local file that's safely backed up and could be deleted
// from this device to free up space.
type Recommendation struct {
	CloudPath string `json:"cloudPath"`
	Size      int64  `json:"size"`
	Mtime     int64  `json:"mtime"`
	Uploaded  int64  `json:"uploaded"`
	Type      string `json:"type"`
	MsgID     int    `json:"msgId"`
}

// Recommendations lists files whose backup matches the file on this device,
// largest first (or oldest first with byAge). Only files are offered; code
// projects are freed from the Cloud screen.
func (e *Engine) Recommendations(minSize int64, typ string, byAge bool) ([]Recommendation, error) {
	items, err := e.st.LocalBackedUp()
	if err != nil {
		return nil, err
	}
	latest, err := e.st.LatestUnder("")
	if err != nil {
		return nil, err
	}
	byPath := make(map[string]store.Version, len(latest))
	for _, v := range latest {
		byPath[v.CloudPath] = v
	}
	out := []Recommendation{}
	for _, it := range items {
		v, ok := byPath[it.CloudPath]
		// Up to date: the backup is the same size and no older than the file's last change.
		if !ok || v.Size != it.Size || v.Mtime != it.Mtime || it.Size < minSize {
			continue
		}
		t := meta.FileType(it.CloudPath)
		if typ != "" && t != typ {
			continue
		}
		out = append(out, Recommendation{CloudPath: it.CloudPath, Size: it.Size, Mtime: it.Mtime, Uploaded: v.UploadedAt, Type: t, MsgID: v.MsgID})
	}
	sort.Slice(out, func(i, j int) bool {
		if byAge {
			return out[i].Mtime < out[j].Mtime
		}
		return out[i].Size > out[j].Size
	})
	return out, nil
}
