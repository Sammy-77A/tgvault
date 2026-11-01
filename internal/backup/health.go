package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/tgclient"
)

const (
	heartbeatEvery = time.Minute
	gapAfter       = 4 * time.Minute // a silence this long means tgvault wasn't running
	longGap        = 15 * time.Minute
	settingBeat    = "health.beat"
	settingGaps    = "health.gaps"
	maxGaps        = 10
	stuckAfter     = 30 * time.Minute // files waiting this long with no upload is worth a warning
)

// Gap is a stretch of time in which tgvault did nothing at all: it was closed,
// killed, or frozen by the phone's power manager.
type Gap struct {
	From int64  `json:"from"`
	To   int64  `json:"to"`
	Kind string `json:"kind"` // "stopped": the app was restarted after it; "frozen": it came back to life by itself
}

// heartbeat notes once a minute that tgvault is alive. Timers don't run while
// the phone freezes the app, so the first beat after a freeze comes late, and
// a restart leaves the last beat of the old run behind. Either way the silence
// is recorded, which answers "why didn't it back up overnight?".
func (e *Engine) heartbeat(ctx context.Context) {
	e.beat(time.Now())
	t := time.NewTicker(heartbeatEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.beat(time.Now())
		}
	}
}

func (e *Engine) beat(now time.Time) {
	last, _ := strconv.ParseInt(e.st.Get(settingBeat), 10, 64)
	if last > 0 && now.Unix()-last > int64(gapAfter/time.Second) {
		kind := "frozen"
		if !e.beatSeen {
			kind = "stopped"
		}
		e.addGap(Gap{From: last, To: now.Unix(), Kind: kind})
	}
	e.beatSeen = true
	_ = e.st.Set(settingBeat, strconv.FormatInt(now.Unix(), 10))
}

func (e *Engine) gaps() []Gap {
	var g []Gap
	_ = json.Unmarshal([]byte(e.st.Get(settingGaps)), &g)
	return g
}

func (e *Engine) addGap(g Gap) {
	all := append(e.gaps(), g)
	if len(all) > maxGaps {
		all = all[len(all)-maxGaps:]
	}
	if b, err := json.Marshal(all); err == nil {
		_ = e.st.Set(settingGaps, string(b))
	}
}

// Check is one line of the status screen.
type Check struct {
	Name   string `json:"name"`
	State  string `json:"state"` // ok, info, warn, bad
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"` // what to do about it
}

// Health explains, in plain words, whether backups are running and if not why.
type Health struct {
	Level      string  `json:"level"` // ok, working, attention, problem
	Headline   string  `json:"headline"`
	Checks     []Check `json:"checks"`
	Gaps       []Gap   `json:"gaps"` // silences in the last 7 days, oldest first
	LastUpload int64   `json:"lastUpload,omitempty"`
}

// Health looks at the connection, folders, queue and history.
func (e *Engine) Health() Health { return e.healthAt(time.Now()) }

func (e *Engine) healthAt(now time.Time) Health {
	h := Health{Gaps: []Gap{}}
	add := func(name, state, detail, hint string) {
		h.Checks = append(h.Checks, Check{Name: name, State: state, Detail: detail, Hint: hint})
	}
	counts, _ := e.st.Counts()
	waiting := counts[store.StatePending] + counts[store.StateUploading]
	failed := counts[store.StateFailed]
	e.mu.Lock()
	uploading := len(e.progress) > 0
	cool := e.cooldown
	conditions := e.conditions
	e.mu.Unlock()
	blocked := e.Blocked()

	// Telegram.
	tg := e.tg.Info()
	loggedOut := false
	switch tg.Status {
	case tgclient.StatusReady:
		add("Telegram", "ok", "Connected.", "")
	case tgclient.StatusIdle:
		add("Telegram", "ok", "Disconnected while there is nothing to send; it reconnects by itself.", "")
	case tgclient.StatusConnecting:
		detail := "Connecting to Telegram…"
		if tg.Error != "" {
			detail = "Can't reach Telegram: " + tg.Error
		}
		add("Telegram", "warn", detail, "Check the internet connection.")
	default:
		loggedOut = true
		add("Telegram", "bad", "Not logged in, so nothing can be backed up.", "Open tgvault and log in again.")
	}

	// Folders.
	roots, _ := e.st.Roots()
	switch {
	case len(roots) == 0:
		add("Folders", "warn", "No folders are chosen yet.", "Add a folder on the Overview page.")
	default:
		var problems []string
		var newest int64
		for _, r := range roots {
			switch {
			case r.ScanError != "":
				problems = append(problems, r.Label+": "+r.ScanError)
			case r.Paused:
				problems = append(problems, r.Label+" is paused")
			}
			newest = max(newest, r.LastScan)
		}
		if len(problems) > 0 {
			add("Folders", "bad", strings.Join(problems, "; "), "")
		} else {
			add("Folders", "ok", fmt.Sprintf("%s watched; last checked %s.", plural(len(roots), "folder"), ago(now, newest)), "")
		}
	}

	// Phone settings.
	if conditions.Known {
		if blocked != "" {
			add("Phone settings", "warn", blocked+".", "Change this in Settings if you want uploads to go on.")
		} else {
			add("Phone settings", "ok", "Wi-Fi and charging settings aren't holding anything back.", "")
		}
	}

	// Telegram's speed limit.
	if now.Before(cool) {
		add("Speed limit", "warn", fmt.Sprintf("Telegram asked tgvault to slow down; uploads resume at %s.", cool.Format("15:04")), "Nothing is wrong. It continues by itself.")
	}

	// The queue.
	switch {
	case failed > 0:
		var why []string
		if items, err := e.st.ItemsByState(store.StateFailed, 3); err == nil {
			for _, it := range items {
				why = append(why, it.CloudPath+": "+it.Note)
			}
		}
		add("Queue", "warn", fmt.Sprintf("%s failed and will be retried. %s", plural(failed, "file"), strings.Join(why, "; ")), "See Activity for each file.")
	case waiting > 0:
		add("Queue", "info", fmt.Sprintf("%s waiting to upload.", plural(waiting, "file")), "")
	default:
		add("Queue", "ok", "Everything that was found is backed up.", "")
	}

	// The last upload, and whether files are waiting with no sign of progress.
	h.LastUpload = e.st.LastUploadAt()
	switch {
	case h.LastUpload == 0:
		add("Last upload", "info", "Nothing has been backed up yet.", "")
	case waiting > 0 && !uploading && !loggedOut && blocked == "" && !now.Before(cool) &&
		now.Sub(time.Unix(h.LastUpload, 0)) > stuckAfter:
		add("Last upload", "warn", fmt.Sprintf("Files are waiting, but nothing has uploaded for %s (last: %s).",
			span(now.Sub(time.Unix(h.LastUpload, 0))), ago(now, h.LastUpload)),
			"Something is stopping tgvault from working. Check the pauses below.")
	default:
		add("Last upload", "ok", "The last file was backed up "+ago(now, h.LastUpload)+".", "")
	}

	// Silences.
	week := now.Add(-7 * 24 * time.Hour).Unix()
	var lastDay int
	var longest int64
	var lastEnd int64
	for _, g := range e.gaps() {
		if g.To < week {
			continue
		}
		h.Gaps = append(h.Gaps, g)
		if g.To >= now.Add(-24*time.Hour).Unix() && time.Duration(g.To-g.From)*time.Second >= longGap {
			lastDay++
			longest = max(longest, g.To-g.From)
			lastEnd = g.To
		}
	}
	switch {
	case lastDay > 0:
		add("Pauses", "warn", fmt.Sprintf("tgvault did nothing at all %s in the last day (longest %s, the latest ended %s). It was closed, or the device froze it.",
			times(lastDay), span(time.Duration(longest)*time.Second), ago(now, lastEnd)),
			"Backups can only run while tgvault is running.")
	case e.st.Get(settingBeat) != "":
		add("Pauses", "ok", "tgvault has run without long interruptions in the last day.", "")
	}

	// Verdict.
	h.Level, h.Headline = "ok", "Backups are working."
	if uploading || waiting > 0 {
		h.Level, h.Headline = "working", fmt.Sprintf("Backing up %s.", plural(max(waiting, 1), "file"))
	}
	for _, want := range []struct{ state, level string }{{"warn", "attention"}, {"bad", "problem"}} {
		for _, c := range h.Checks {
			if c.State == want.state {
				h.Level, h.Headline = want.level, c.Name+": "+c.Detail
				break
			}
		}
	}
	return h
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func times(n int) string {
	if n == 1 {
		return "once"
	}
	return fmt.Sprintf("%d times", n)
}

// span writes a length of time in the two units people use: "6 h 12 min".
func span(d time.Duration) string {
	d = d.Round(time.Minute)
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	}
	return "less than a minute"
}

// ago writes how long before now a Unix time was.
func ago(now time.Time, t int64) string {
	if t == 0 {
		return "never"
	}
	d := now.Sub(time.Unix(t, 0))
	if d < time.Minute {
		return "just now"
	}
	return span(d) + " ago"
}
