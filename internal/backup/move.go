package backup

import (
	"context"
	"log"
	"time"

	"github.com/Sammy-77A/tgvault/internal/meta"
	"github.com/Sammy-77A/tgvault/internal/store"
)

// movedWithin is how long after a file went missing a file with the same
// content can still be taken for it having moved. Files freed on purpose are
// never taken, and older ones are left alone too.
const movedWithin = 3 * 24 * time.Hour

// adoptMoved handles a file at a new path whose content is already in
// Telegram under a path that has just disappeared from this device: it was
// moved or renamed. Rather than uploading it again, the stored copy (with its
// older versions) is relabelled to the new path. It reports whether it did.
func (e *Engine) adoptMoved(ctx context.Context, it store.Item, size, mtime int64, hash string) bool {
	if it.Kind != store.KindFile {
		return false
	}
	// A path that already has backups is a changed file, not a moved one.
	if vs, err := e.st.Versions(it.CloudPath); err != nil || len(vs) > 0 {
		return false
	}
	// Two moved copies of the same file must not both claim one stored copy.
	e.moveMu.Lock()
	defer e.moveMu.Unlock()
	since := time.Now().Add(-movedWithin).Unix()
	cands, err := e.st.MovedFrom(hash, size, it.CloudPath, since)
	if err != nil {
		return false
	}
	for _, c := range cands {
		if meta.DeviceOf(c.CloudPath) != meta.DeviceOf(it.CloudPath) {
			continue
		}
		vs, err := e.st.Versions(c.CloudPath)
		if err != nil || len(vs) == 0 {
			continue
		}
		moving := make(map[int]bool, len(vs))
		for _, v := range vs {
			moving[v.MsgID] = true
		}
		first := vs[0]
		first.Mtime = mtime
		if err := e.moveVersion(ctx, first, it.CloudPath, moving); err != nil {
			log.Printf("move %s -> %s: %v (uploading it instead)", c.CloudPath, it.CloudPath, err)
			continue
		}
		_ = e.st.SetVersionMtime(first.MsgID, mtime)
		for _, v := range vs[1:] {
			if err := e.moveVersion(ctx, v, it.CloudPath, moving); err != nil {
				log.Printf("move %s: an older version stayed at %s: %v", it.CloudPath, c.CloudPath, err)
				break
			}
		}
		_ = e.st.ForgetGoneUnder(c.CloudPath)
		log.Printf("moved: %s is now %s (not uploaded again)", c.CloudPath, it.CloudPath)
		return true
	}
	return false
}
