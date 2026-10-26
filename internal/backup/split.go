package backup

// Files over Telegram's 2 GB limit are stored as several messages ("parts"),
// each holding the next slice of the file's bytes. See docs/big-files-plan.md.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tgerr"

	"github.com/Sammy-77A/tgvault/internal/meta"
	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/tgclient"
)

// orphanAge is how long another device's unfinished split upload is left
// alone before its parts are removed.
const orphanAge = 7 * 24 * time.Hour

// For testing splitting against the real channel without uploading
// gigabytes: envTestPartSize makes parts smaller (in bytes, whole 512 KiB
// pieces) for files under the cloud folder envTestPath only. Both are unset
// in normal use.
const (
	envTestPartSize = "TGVAULT_TEST_PART_SIZE"
	envTestPath     = "TGVAULT_TEST_SPLIT_PATH"
)

// splitLimits returns the size above which a file is split, and the part size.
func (e *Engine) splitLimits(cloudPath string) (splitAt, partSize int64) {
	prefix := os.Getenv(envTestPath)
	if n, err := strconv.ParseInt(os.Getenv(envTestPartSize), 10, 64); err == nil && prefix != "" && underPrefix(cloudPath, prefix) && n >= 1<<20 && n%(512<<10) == 0 && n < e.partSize {
		return n, n
	}
	return e.splitAt, e.partSize
}

// layout splits size bytes into parts of partSize (the last one shorter).
func layout(size, partSize int64) []store.Part {
	var parts []store.Part
	for off, i := int64(0), 1; off < size; off, i = off+partSize, i+1 {
		parts = append(parts, store.Part{Index: i, Offset: off, Size: min(partSize, size-off)})
	}
	return parts
}

// hashParts reads a file once and returns the sha256 of the whole file and of
// each part. progress, if set, is called with the bytes read so far.
func hashParts(p string, parts []store.Part, progress func(done int64)) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	whole := sha256.New()
	buf := make([]byte, 1<<20)
	var done int64
	for i := range parts {
		ph := sha256.New()
		r := io.LimitReader(f, parts[i].Size)
		for {
			n, err := r.Read(buf)
			if n > 0 {
				whole.Write(buf[:n])
				ph.Write(buf[:n])
				done += int64(n)
				if progress != nil && done%(64<<20) < int64(n) {
					progress(done)
				}
			}
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return "", err
			}
		}
		parts[i].Hash = hex.EncodeToString(ph.Sum(nil))
	}
	// The file must end exactly where the last part does.
	if n, _ := f.Read(buf[:1]); n != 0 || done != parts[len(parts)-1].Offset+parts[len(parts)-1].Size {
		return "", errors.New("the file changed size while being read")
	}
	return hex.EncodeToString(whole.Sum(nil)), nil
}

func newGroupID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// splitCaption builds the caption of one part.
func splitCaption(kind, cloudPath string, size, mtime int64, wholeHash, fp, group string, p store.Part, n int, earlier []int) (string, error) {
	in := meta.Info{Kind: kind, Path: cloudPath, Size: size, Mt: mtime, Hash: wholeHash, FP: fp, Dev: meta.DeviceOf(cloudPath),
		Group: group, Part: p.Index, Parts: n, PartOff: p.Offset, PartSize: p.Size, PartHash: p.Hash}
	if p.Index == n {
		ids := make([]string, len(earlier))
		for i, id := range earlier {
			ids[i] = strconv.Itoa(id)
		}
		in.EarlierID = strings.Join(ids, ",")
	}
	return meta.Caption(in)
}

func partName(name string, p store.Part, n int) string {
	return fmt.Sprintf("%s.part%dof%d", name, p.Index, n)
}

// uploadSplit uploads src (size bytes) as several messages. It continues an
// earlier unfinished upload of the same content, and records the version once
// every part is in Telegram. For files, src is the file itself and fi its
// state before reading; the upload is dropped if it changes meanwhile.
func (e *Engine) uploadSplit(ctx context.Context, it store.Item, src string, fi os.FileInfo, v store.Version, name, note string) outcome {
	_, partSize := e.splitLimits(it.CloudPath)
	parts := layout(fi.Size(), partSize)
	n := len(parts)
	e.setProgress(Progress{ItemID: it.ID, CloudPath: it.CloudPath, Phase: "reading", Total: fi.Size(), Parts: n})
	wholeHash, err := hashParts(src, parts, func(done int64) {
		e.setProgress(Progress{ItemID: it.ID, CloudPath: it.CloudPath, Phase: "reading", Done: done, Total: fi.Size(), Parts: n})
	})
	if err != nil {
		return e.failure(ctx, err)
	}
	if latest, ok, _ := e.st.LatestVersion(it.CloudPath); ok && latest.Hash == wholeHash {
		e.dropPartUploads(ctx, it.CloudPath, "")
		return outcome{state: store.StateSynced} // content already in Telegram
	}
	kind := "f"
	if v.Kind == store.KindProject {
		kind = "p"
	}

	// Continue an unfinished upload of this exact content, if there is one.
	group := ""
	posted := map[int]int{} // part number -> message ID
	earlier, _ := e.st.PartUploads(it.CloudPath)
	for _, pu := range earlier {
		if pu.Hash == wholeHash && pu.Index <= n && parts[pu.Index-1].Offset == pu.Offset &&
			parts[pu.Index-1].Size == pu.Size && parts[pu.Index-1].Hash == pu.PartHash {
			group = pu.Group
		}
	}
	for _, pu := range earlier {
		if pu.Group == group {
			posted[pu.Index] = pu.MsgID
		}
	}
	e.dropPartUploads(ctx, it.CloudPath, group) // older content: those parts are useless now
	if group == "" {
		group = newGroupID()
	} else {
		log.Printf("upload %s: continuing after part %d of %d", it.CloudPath, len(posted), n)
	}

	for i, p := range parts {
		if id, ok := posted[p.Index]; ok {
			parts[i].MsgID = id
			continue
		}
		var before []int
		for _, q := range parts[:i] {
			before = append(before, q.MsgID)
		}
		caption, err := splitCaption(kind, it.CloudPath, fi.Size(), v.Mtime, wholeHash, v.Fingerprint, group, p, n, before)
		if err != nil {
			return outcome{state: store.StateSkipped, note: err.Error()}
		}
		key := fmt.Sprintf("%s\x00%s:%d", it.CloudPath, group, p.Index)
		msgID, err := e.sendRange(ctx, it, key, src, partName(name, p, n), p.Offset, p.Size, p.Hash, caption,
			func(done int64) {
				e.setProgress(Progress{ItemID: it.ID, CloudPath: it.CloudPath, Phase: "uploading",
					Done: p.Offset + done, Total: fi.Size(), Part: p.Index, Parts: n})
			})
		if err != nil {
			return e.failure(ctx, err) // posted parts are kept, so the next try continues here
		}
		parts[i].MsgID = msgID
		if err := e.st.AddPartUpload(store.PartUpload{CloudPath: it.CloudPath, Group: group, Hash: wholeHash, Index: p.Index,
			MsgID: msgID, Offset: p.Offset, Size: p.Size, PartHash: p.Hash, UploadedAt: time.Now().Unix()}); err != nil {
			_ = e.tg.DeleteMessages(ctx, []int{msgID})
			return outcome{state: store.StateFailed, note: "database error: " + err.Error(), count: true}
		}
		// A file that changes during a long upload would be stored inconsistently.
		if after, err := os.Stat(src); !uploadStillValid(fi, after, err) {
			e.dropPartUploads(ctx, it.CloudPath, "")
			return outcome{state: store.StateFailed, retry: quietFile, note: "the file changed while uploading; will upload the new version"}
		}
	}

	for i := range parts {
		parts[i].Group = group
	}
	v.MsgID, v.Size, v.Hash, v.UploadedAt = parts[0].MsgID, fi.Size(), wholeHash, time.Now().Unix()
	if err := e.st.AddSplitVersion(v, parts); err != nil {
		return outcome{state: store.StateFailed, note: "database error: " + err.Error(), count: true}
	}
	_ = e.st.DeletePartUploads(it.CloudPath, group)
	e.prune(ctx, v.CloudPath)
	return outcome{state: store.StateSynced, note: note}
}

// dropPartUploads deletes this device's unfinished split uploads of a path
// from Telegram, except the one in keep.
func (e *Engine) dropPartUploads(ctx context.Context, cloudPath, keep string) {
	pus, err := e.st.PartUploads(cloudPath)
	if err != nil {
		return
	}
	byGroup := map[string][]int{}
	for _, pu := range pus {
		if pu.Group != keep {
			byGroup[pu.Group] = append(byGroup[pu.Group], pu.MsgID)
		}
	}
	for g, ids := range byGroup {
		e.dropGroup(ctx, cloudPath, g, ids)
	}
}

// dropGroup deletes the posted parts of one unfinished upload.
func (e *Engine) dropGroup(ctx context.Context, cloudPath, group string, ids []int) {
	// Never delete a message that belongs to a recorded version.
	var del []int
	for _, id := range ids {
		if _, recorded := e.st.PartOwner(id); !recorded {
			del = append(del, id)
		}
	}
	if err := e.tg.DeleteMessages(ctx, del); err != nil {
		log.Printf("remove unfinished upload of %s: %v (will retry later)", cloudPath, err)
		return
	}
	_ = e.st.DeletePartUploads(cloudPath, group)
}

// ---- messages of a version ----

// messageIDs returns every Telegram message that holds a version.
func (e *Engine) messageIDs(v store.Version) ([]int, error) {
	if v.Parts == 0 {
		return []int{v.MsgID}, nil
	}
	parts, err := e.st.Parts(v.MsgID)
	if err != nil {
		return nil, err
	}
	if len(parts) == 0 {
		return []int{v.MsgID}, nil
	}
	ids := make([]int, len(parts))
	for i, p := range parts {
		ids[i] = p.MsgID
	}
	return ids, nil
}

// messageIDsOf is messageIDs for several versions.
func (e *Engine) messageIDsOf(vs []store.Version) ([]int, error) {
	var out []int
	for _, v := range vs {
		ids, err := e.messageIDs(v)
		if err != nil {
			return nil, err
		}
		out = append(out, ids...)
	}
	return out, nil
}

// pieces returns what to download for a version: its parts, or the single
// message as one piece.
func (e *Engine) pieces(v store.Version) ([]store.Part, error) {
	if v.Parts > 0 {
		parts, err := e.st.Parts(v.MsgID)
		if err != nil {
			return nil, err
		}
		if len(parts) != v.Parts {
			return nil, errors.New("this backup's list of parts is incomplete; it will be checked against Telegram")
		}
		return parts, nil
	}
	return []store.Part{{Index: 1, MsgID: v.MsgID, Size: v.Size}}, nil
}

// checkInTelegram confirms every message of each version still exists in
// Telegram with the right size. It returns version ID -> problem for the ones
// that don't.
func (e *Engine) checkInTelegram(ctx context.Context, vs []store.Version) (map[int]string, error) {
	type want struct {
		version int
		size    int64
	}
	wants := map[int]want{}
	var ids []int
	for _, v := range vs {
		ps, err := e.pieces(v)
		if err != nil {
			return nil, err
		}
		for _, p := range ps {
			wants[p.MsgID] = want{v.MsgID, p.Size}
			ids = append(ids, p.MsgID)
		}
	}
	remote, err := e.tg.Exists(ctx, ids)
	if err != nil {
		return nil, err
	}
	problems := map[int]string{}
	for _, id := range ids {
		w := wants[id]
		rf, ok := remote[id]
		switch {
		case !ok:
			problems[w.version] = "its backup is missing in Telegram"
		case rf.Size != w.size:
			problems[w.version] = "its backup in Telegram has the wrong size"
		}
	}
	return problems, nil
}

// ---- downloading ----

// streamVersion writes a version to w, checking each part and the whole file
// as it goes. It can't retry once bytes are written, so a failure returns an
// error and the caller abandons w.
func (e *Engine) streamVersion(ctx context.Context, v store.Version, w io.Writer) error {
	parts, err := e.pieces(v)
	if err != nil {
		return err
	}
	whole := sha256.New()
	for _, p := range parts {
		ph := sha256.New()
		if err := e.tg.Download(ctx, p.MsgID, io.MultiWriter(w, whole, ph)); err != nil {
			return partErr(p, len(parts), err)
		}
		if p.Hash != "" && hex.EncodeToString(ph.Sum(nil)) != p.Hash {
			return partErr(p, len(parts), errors.New("failed its integrity check"))
		}
	}
	if hex.EncodeToString(whole.Sum(nil)) != v.Hash {
		return errors.New("the downloaded file failed its integrity check")
	}
	return nil
}

func partErr(p store.Part, n int, err error) error {
	if n == 1 {
		return err
	}
	return fmt.Errorf("part %d of %d: %w", p.Index, n, err)
}

// downloadParts writes a split version into f from its start. A part that
// fails is fetched again from its own start, without redoing earlier parts.
func (e *Engine) downloadParts(ctx context.Context, v store.Version, f *os.File) error {
	parts, err := e.pieces(v)
	if err != nil {
		return err
	}
	whole := sha256.New()
	for _, p := range parts {
		// Remember the whole-file hash before this part, to undo a failed try.
		saved, err := whole.(encoding.BinaryMarshaler).MarshalBinary()
		if err != nil {
			return err
		}
		var lastErr error
		for attempt := 0; attempt < 3; attempt++ {
			if attempt > 0 {
				log.Printf("download %s: retrying part %d of %d: %v", v.CloudPath, p.Index, len(parts), lastErr)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(time.Duration(attempt*2) * time.Second):
				}
			}
			if lastErr = rewind(f, p.Offset, whole, saved); lastErr != nil {
				return lastErr
			}
			ph := sha256.New()
			lastErr = e.tg.Download(ctx, p.MsgID, io.MultiWriter(f, whole, ph))
			if lastErr == nil && hex.EncodeToString(ph.Sum(nil)) != p.Hash {
				lastErr = errors.New("failed its integrity check")
			}
			if lastErr == nil || ctx.Err() != nil {
				break
			}
			if _, flood := tgerr.AsFloodWait(lastErr); flood {
				break
			}
		}
		if lastErr != nil {
			return partErr(p, len(parts), lastErr)
		}
	}
	if hex.EncodeToString(whole.Sum(nil)) != v.Hash {
		return errors.New("the downloaded file failed its integrity check")
	}
	return nil
}

func rewind(f *os.File, off int64, h hash.Hash, saved []byte) error {
	if err := f.Truncate(off); err != nil {
		return err
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		return err
	}
	return h.(encoding.BinaryUnmarshaler).UnmarshalBinary(saved)
}

// ---- moving and links ----

// editSplitCaptions rewrites the captions of every part of a split version
// for a new path. If one edit fails, the parts already edited are changed
// back, so the parts never disagree about where the file is.
func (e *Engine) editSplitCaptions(ctx context.Context, v store.Version, to string) error {
	parts, err := e.pieces(v)
	if err != nil {
		return err
	}
	kind := "f"
	if v.Kind == store.KindProject {
		kind = "p"
	}
	group := parts[0].Group
	captions := func(p string) ([]string, error) {
		out := make([]string, len(parts))
		var before []int
		for i, part := range parts {
			c, err := splitCaption(kind, p, v.Size, v.Mtime, v.Hash, v.Fingerprint, group, part, len(parts), before)
			if err != nil {
				return nil, err
			}
			out[i] = c
			before = append(before, part.MsgID)
		}
		return out, nil
	}
	newCaps, err := captions(to)
	if err != nil {
		return err
	}
	oldCaps, err := captions(v.CloudPath)
	if err != nil {
		return err
	}
	for i, part := range parts {
		if err := e.editCaption(ctx, part.MsgID, newCaps[i]); err != nil {
			for j := i - 1; j >= 0; j-- {
				if uerr := e.editCaption(ctx, parts[j].MsgID, oldCaps[j]); uerr != nil {
					log.Printf("move %s: couldn't change part %d back: %v (the next full check will sort it out)", v.CloudPath, parts[j].Index, uerr)
				}
			}
			return partErr(part, len(parts), err)
		}
	}
	return nil
}

func (e *Engine) editCaption(ctx context.Context, msgID int, caption string) error {
	err := e.tg.EditCaption(ctx, msgID, caption)
	if d, ok := tgerr.AsFloodWait(err); ok {
		time.Sleep(d + time.Second)
		err = e.tg.EditCaption(ctx, msgID, caption)
	}
	return err
}

// ---- reading split files back from Telegram ----

// partMsg is one part message seen in the channel.
type partMsg struct {
	rf tgclient.RemoteFile
	in meta.Info
}

// assemble checks that the part messages of one group form a whole file. When
// a part was posted twice (an upload interrupted at the wrong moment), prefer
// says which message to use; the others are returned as extra.
func assemble(msgs []partMsg, prefer func(msgID int) bool) (v store.Version, parts []store.Part, extra []int, ok bool) {
	if len(msgs) == 0 {
		return v, nil, nil, false
	}
	first := msgs[0].in
	n := first.Parts
	byIndex := map[int]partMsg{}
	for _, m := range msgs {
		in := m.in
		if in.Group != first.Group || in.Path != first.Path || in.Size != first.Size || in.Hash != first.Hash ||
			in.Parts != n || in.Kind != first.Kind || in.Mt != first.Mt || m.rf.Size != in.PartSize {
			return v, nil, nil, false
		}
		if cur, dup := byIndex[in.Part]; dup {
			keep, drop := cur, m
			if prefer != nil && prefer(m.rf.MsgID) && !prefer(cur.rf.MsgID) {
				keep, drop = m, cur
			}
			if keep.in.PartHash != drop.in.PartHash {
				return v, nil, nil, false
			}
			byIndex[in.Part] = keep
			extra = append(extra, drop.rf.MsgID)
			continue
		}
		byIndex[in.Part] = m
	}
	if len(byIndex) != n {
		return v, nil, nil, false
	}
	var off int64
	date := 0
	for i := 1; i <= n; i++ {
		m, found := byIndex[i]
		if !found || m.in.PartOff != off {
			return v, nil, nil, false
		}
		off += m.in.PartSize
		date = max(date, m.rf.Date)
		parts = append(parts, store.Part{Index: i, MsgID: m.rf.MsgID, Offset: m.in.PartOff, Size: m.in.PartSize,
			Hash: m.in.PartHash, Group: m.in.Group})
	}
	if off != first.Size {
		return v, nil, nil, false
	}
	kind := store.KindFile
	if first.Kind == "p" {
		kind = store.KindProject
	}
	v = store.Version{MsgID: parts[0].MsgID, CloudPath: first.Path, Kind: kind, Size: first.Size, Mtime: first.Mt,
		Hash: first.Hash, Fingerprint: first.FP, UploadedAt: int64(date), Parts: n}
	return v, parts, extra, true
}

// ownsMessage reports whether a part message is known here, as part of a
// recorded version or of this device's unfinished upload.
func (e *Engine) ownsMessage(pending map[int]bool) func(int) bool {
	return func(id int) bool {
		if pending[id] {
			return true
		}
		_, ok := e.st.PartOwner(id)
		return ok
	}
}

// addSplitFromLastPart records a split file another device just finished,
// using the earlier parts' IDs listed on its last part. It reports whether a
// version was added.
func (e *Engine) addSplitFromLastPart(ctx context.Context, last tgclient.RemoteFile, in meta.Info) (bool, error) {
	if in.Part != in.Parts || in.EarlierID == "" {
		return false, nil
	}
	var ids []int
	for _, s := range strings.Split(in.EarlierID, ",") {
		id, err := strconv.Atoi(s)
		if err != nil {
			return false, nil
		}
		ids = append(ids, id)
	}
	remote, err := e.tg.Exists(ctx, ids)
	if err != nil {
		return false, err
	}
	msgs := []partMsg{{last, in}}
	for _, id := range ids {
		rf, ok := remote[id]
		if !ok {
			return false, nil // a part is gone; the full check will deal with it
		}
		pin, ok := meta.Parse(rf.Caption)
		if !ok || !pin.Split() {
			return false, nil
		}
		msgs = append(msgs, partMsg{rf, pin})
	}
	v, parts, _, ok := assemble(msgs, nil)
	if !ok {
		return false, nil
	}
	if _, err := e.st.Version(v.MsgID); err == nil {
		return false, nil // already known, e.g. this device's own upload
	}
	return true, e.st.AddSplitVersion(v, parts)
}

// reconcileGroups records the complete split files found by a full check and
// cleans up parts that can never be used. It returns the version IDs found.
func (e *Engine) reconcileGroups(ctx context.Context, groups map[string][]partMsg, paths map[string]bool, known map[int]bool) (map[int]bool, error) {
	pending, err := e.st.PartUploadIDs()
	if err != nil {
		return nil, err
	}
	prefer := e.ownsMessage(pending)
	seen := map[int]bool{}
	var junk []int
	cutoff := time.Now().Add(-orphanAge).Unix()
	for g, msgs := range groups {
		sort.Slice(msgs, func(i, j int) bool { return msgs[i].rf.MsgID < msgs[j].rf.MsgID })
		v, parts, extra, ok := assemble(msgs, prefer)
		if ok {
			seen[v.MsgID] = true
			paths[v.CloudPath] = true
			// Leftover duplicates of a part, unless this device is still using them.
			for _, id := range extra {
				if !pending[id] {
					junk = append(junk, id)
				}
			}
			if !known[v.MsgID] {
				if err := e.st.AddSplitVersion(v, parts); err != nil {
					return nil, err
				}
			}
			continue
		}
		// Incomplete: still uploading here or elsewhere, or abandoned.
		newest, mine := 0, false
		for _, m := range msgs {
			newest = max(newest, m.rf.Date)
			mine = mine || pending[m.rf.MsgID]
		}
		if !mine && int64(newest) < cutoff {
			log.Printf("reconcile: removing %d part(s) of an upload of %s abandoned since %s (group %s)",
				len(msgs), msgs[0].in.Path, time.Unix(int64(newest), 0).Format("2006-01-02"), g)
			for _, m := range msgs {
				junk = append(junk, m.rf.MsgID)
			}
		}
	}
	if len(junk) > 0 {
		if err := e.tg.DeleteMessages(ctx, junk); err != nil {
			log.Printf("reconcile: couldn't remove unused parts: %v (will retry later)", err)
		}
	}
	return seen, nil
}

// cleanPartUploads removes this device's unfinished uploads that will never
// finish: the file is gone, or newer content has been backed up since.
func (e *Engine) cleanPartUploads(ctx context.Context) {
	pus, err := e.st.PartUploads("")
	if err != nil {
		return
	}
	type key struct{ path, group string }
	ids := map[key][]int{}
	hashes := map[key]string{}
	for _, pu := range pus {
		k := key{pu.CloudPath, pu.Group}
		ids[k] = append(ids[k], pu.MsgID)
		hashes[k] = pu.Hash
	}
	for k, msgIDs := range ids {
		recorded := false
		for _, id := range msgIDs {
			if _, ok := e.st.PartOwner(id); ok {
				recorded = true
			}
		}
		it, err := e.st.ItemByPath(k.path)
		switch {
		case recorded:
			// Finished after all (the version was recorded); only the bookkeeping is left.
			_ = e.st.DeletePartUploads(k.path, k.group)
		case err != nil || !it.LocalPresent:
			e.dropGroup(ctx, k.path, k.group, msgIDs) // the file is gone
		case it.State == store.StateSynced:
			if latest, ok, _ := e.st.LatestVersion(k.path); ok && latest.Hash != hashes[k] {
				e.dropGroup(ctx, k.path, k.group, msgIDs) // newer content was backed up instead
			}
		}
	}
}
