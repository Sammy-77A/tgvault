package backup

import (
	"context"
	"fmt"
	"log"

	"github.com/Sammy-77A/tgvault/internal/store"
)

// ReleaseLocal deletes the local copies under prefix that are safely in
// Telegram, re-checking each one first, and reports how many bytes it freed.
// It is "Free up space" without a task on the page, for folders that only hold
// files on their way to Telegram (the server's upload folder).
func (e *Engine) ReleaseLocal(ctx context.Context, prefix string) (int64, error) {
	plan, err := e.PlanFree(prefix)
	if err != nil || len(plan.Candidates) == 0 {
		return 0, err
	}
	latest := map[string]store.Version{}
	var vs []store.Version
	for _, c := range plan.Candidates {
		if v, ok, _ := e.st.LatestVersion(c.CloudPath); ok {
			latest[c.CloudPath] = v
			vs = append(vs, v)
		}
	}
	problems, err := e.checkInTelegram(ctx, vs)
	if err != nil {
		return 0, fmt.Errorf("could not check Telegram: %w", err)
	}
	var freed int64
	var dirs []freedDir
	for _, c := range plan.Candidates {
		v, ok := latest[c.CloudPath]
		if !ok || problems[v.MsgID] != "" {
			continue
		}
		if why := e.deleteLocal(c, v, &dirs); why != "" {
			log.Printf("release %s: kept, because %s", c.CloudPath, why)
			continue
		}
		freed += c.Size
	}
	removeEmptyDirs(dirs)
	return freed, nil
}
