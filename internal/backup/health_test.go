package backup

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sammy-77A/tgvault/internal/store"
	"github.com/Sammy-77A/tgvault/internal/tgclient"
)

// statusTG is a fake Telegram that reports a chosen connection status.
type statusTG struct {
	*fakeTG
	status, err string
}

func (s statusTG) Info() tgclient.Info { return tgclient.Info{Status: s.status, Error: s.err} }

func healthEngine(t *testing.T, status string) (*Engine, *store.Store) {
	t.Helper()
	e, st := newTestEngine(t)
	e.tg = statusTG{fakeTG: newFakeTG(), status: status}
	return e, st
}

func check(h Health, name string) (Check, bool) {
	for _, c := range h.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return Check{}, false
}

func addRoot(t *testing.T, st *store.Store) store.Root {
	t.Helper()
	r, err := st.AddRoot(filepath.Join(t.TempDir(), "Docs"), "Docs")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestHeartbeatRecordsSilences(t *testing.T) {
	e, st := healthEngine(t, tgclient.StatusReady)
	t0 := time.Now().Add(-10 * time.Hour)
	e.beat(t0)
	e.beat(t0.Add(time.Minute)) // an ordinary beat
	if len(e.gaps()) != 0 {
		t.Fatalf("a normal minute is not a silence: %+v", e.gaps())
	}
	// The same process wakes up hours later: it was frozen.
	e.beat(t0.Add(6 * time.Hour))
	g := e.gaps()
	if len(g) != 1 || g[0].Kind != "frozen" || g[0].To-g[0].From < 5*3600 {
		t.Fatalf("want one long frozen gap, got %+v", g)
	}
	// A new process finds the old one's last beat: it was stopped.
	e2 := newEngine(st, t.TempDir())
	e2.tg = e.tg
	e2.beat(t0.Add(7 * time.Hour))
	if g := e2.gaps(); len(g) != 2 || g[1].Kind != "stopped" {
		t.Fatalf("want a stopped gap after a restart, got %+v", g)
	}
}

func TestGapsKeepOnlyTheLatest(t *testing.T) {
	e, _ := healthEngine(t, tgclient.StatusReady)
	for i := 0; i < maxGaps+5; i++ {
		e.addGap(Gap{From: int64(i), To: int64(i + 1), Kind: "frozen"})
	}
	if g := e.gaps(); len(g) != maxGaps || g[len(g)-1].From != int64(maxGaps+4) {
		t.Fatalf("kept %d gaps, last %+v", len(g), g[len(g)-1])
	}
}

func TestHealthAllGood(t *testing.T) {
	e, st := healthEngine(t, tgclient.StatusReady)
	addRoot(t, st)
	e.beat(time.Now())
	h := e.Health()
	if h.Level != "ok" || h.Headline != "Backups are working." {
		t.Fatalf("level %q headline %q checks %+v", h.Level, h.Headline, h.Checks)
	}
	if _, ok := check(h, "Telegram"); !ok {
		t.Fatal("no Telegram check")
	}
	if _, ok := check(h, "Phone settings"); ok {
		t.Error("a PC has no phone settings check")
	}
}

func TestHealthLoggedOutIsAProblem(t *testing.T) {
	e, st := healthEngine(t, tgclient.StatusNeedPhone)
	addRoot(t, st)
	h := e.Health()
	c, _ := check(h, "Telegram")
	if h.Level != "problem" || c.State != "bad" || !strings.Contains(h.Headline, "Not logged in") {
		t.Fatalf("level %q headline %q", h.Level, h.Headline)
	}
}

func TestHealthNoFoldersAndFolderErrors(t *testing.T) {
	e, st := healthEngine(t, tgclient.StatusReady)
	if c, _ := check(e.Health(), "Folders"); c.State != "warn" {
		t.Errorf("no folders: %+v", c)
	}
	r := addRoot(t, st)
	_ = st.SetRootScan(r.ID, "folder not found; is the drive connected?")
	h := e.Health()
	c, _ := check(h, "Folders")
	if c.State != "bad" || h.Level != "problem" || !strings.Contains(c.Detail, "folder not found") {
		t.Errorf("folder error: %+v level %q", c, h.Level)
	}
}

func TestHealthPhoneSettingsHoldingBack(t *testing.T) {
	e, st := healthEngine(t, tgclient.StatusReady)
	r := addRoot(t, st)
	_ = st.UpsertSeen(store.Item{Kind: store.KindFile, CloudPath: "Docs/a.txt", RootID: r.ID, Rel: "a.txt", Size: 1})
	e.SetConditions(Conditions{Unmetered: false, Charging: true})
	h := e.Health()
	c, _ := check(h, "Phone settings")
	if c.State != "warn" || h.Level != "attention" || !strings.Contains(c.Detail, "Wi-Fi") {
		t.Fatalf("level %q, %+v", h.Level, c)
	}
	e.SetConditions(Conditions{Unmetered: true, Charging: false})
	if c, _ := check(e.Health(), "Phone settings"); c.State != "ok" {
		t.Errorf("on Wi-Fi: %+v", c)
	}
}

func TestHealthFilesWaitingButNothingUploads(t *testing.T) {
	e, st := healthEngine(t, tgclient.StatusReady)
	r := addRoot(t, st)
	_ = st.UpsertSeen(store.Item{Kind: store.KindFile, CloudPath: "Docs/a.txt", RootID: r.ID, Rel: "a.txt", Size: 1})
	_ = st.AddVersion(store.Version{MsgID: 5, CloudPath: "Docs/old.txt", Kind: store.KindFile, Size: 1, Hash: "h",
		UploadedAt: time.Now().Add(-3 * time.Hour).Unix()})
	h := e.Health()
	c, _ := check(h, "Last upload")
	if c.State != "warn" || !strings.Contains(c.Detail, "nothing has uploaded for 3 h") || h.Level != "attention" {
		t.Fatalf("level %q, %+v", h.Level, c)
	}
	// A recent upload, or being logged out (which explains it), is not "stuck".
	_ = st.AddVersion(store.Version{MsgID: 6, CloudPath: "Docs/new.txt", Kind: store.KindFile, Size: 1, Hash: "h2",
		UploadedAt: time.Now().Unix()})
	if c, _ := check(e.Health(), "Last upload"); c.State != "ok" {
		t.Errorf("recent upload: %+v", c)
	}
}

func TestHealthLongPausesInTheLastDay(t *testing.T) {
	e, st := healthEngine(t, tgclient.StatusReady)
	addRoot(t, st)
	now := time.Now()
	e.beat(now)
	e.addGap(Gap{From: now.Add(-9 * time.Hour).Unix(), To: now.Add(-3 * time.Hour).Unix(), Kind: "frozen"})
	e.addGap(Gap{From: now.Add(-30 * 24 * time.Hour).Unix(), To: now.Add(-29 * 24 * time.Hour).Unix(), Kind: "stopped"})
	h := e.Health()
	c, _ := check(h, "Pauses")
	if c.State != "warn" || !strings.Contains(c.Detail, "once") || !strings.Contains(c.Detail, "6 h 0 min") {
		t.Fatalf("pauses: %+v", c)
	}
	if len(h.Gaps) != 1 {
		t.Errorf("only the last 7 days are listed, got %d", len(h.Gaps))
	}
	if b, err := json.Marshal(h); err != nil || !strings.Contains(string(b), `"gaps"`) {
		t.Errorf("health must encode for the page: %v", err)
	}
}

func TestSpanAndAgo(t *testing.T) {
	now := time.Now()
	for d, want := range map[time.Duration]string{
		20 * time.Second: "less than a minute", 5 * time.Minute: "5 min", 6*time.Hour + 12*time.Minute: "6 h 12 min", 72 * time.Hour: "3 days",
	} {
		if got := span(d); got != want {
			t.Errorf("span(%s) = %q, want %q", d, got, want)
		}
	}
	if ago(now, 0) != "never" || ago(now, now.Unix()) != "just now" || ago(now, now.Add(-2*time.Hour).Unix()) != "2 h 0 min ago" {
		t.Error("ago wording changed")
	}
}
