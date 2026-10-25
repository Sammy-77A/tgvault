package backup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sammy-77A/tgvault/internal/meta"
	"github.com/Sammy-77A/tgvault/internal/store"
)

func TestPhoneFoldersGoUnderPhone(t *testing.T) {
	e, _ := newTestEngine(t)
	e.Configure(Config{Device: meta.DevicePhone})
	base := t.TempDir()
	cam := filepath.Join(base, "DCIM", "Camera")
	os.MkdirAll(cam, 0o755)
	rt, err := e.AddRoot(cam)
	if err != nil {
		t.Fatal(err)
	}
	if rt.Label != "Phone/Camera" {
		t.Fatalf("label = %q, want Phone/Camera", rt.Label)
	}
	// A second folder with the same name gets a distinct label.
	other := filepath.Join(base, "Other", "Camera")
	os.MkdirAll(other, 0o755)
	rt2, err := e.AddRoot(other)
	if err != nil || rt2.Label != "Phone/Camera (2)" {
		t.Fatalf("second Camera: %+v %v", rt2, err)
	}
}

func TestPCKeepsPlainPathsAndRefusesPhoneName(t *testing.T) {
	e, _ := newTestEngine(t)
	base := t.TempDir()
	docs := filepath.Join(base, "Documents")
	os.MkdirAll(docs, 0o755)
	rt, err := e.AddRoot(docs)
	if err != nil || rt.Label != "Documents" {
		t.Fatalf("PC folder: %+v %v", rt, err)
	}
	ph := filepath.Join(base, "Phone")
	os.MkdirAll(ph, 0o755)
	if _, err := e.AddRoot(ph); err == nil || !strings.Contains(err.Error(), "phone") {
		t.Fatalf("a PC folder named Phone must be refused, got %v", err)
	}
}

func TestOtherDeviceFilesAreMarked(t *testing.T) {
	e, st := newTestEngine(t)
	st.AddVersion(store.Version{MsgID: 1, CloudPath: "Phone/Camera/a.jpg", Kind: store.KindFile, Size: 5, Hash: "h", UploadedAt: 1})
	st.AddVersion(store.Version{MsgID: 2, CloudPath: "Documents/b.txt", Kind: store.KindFile, Size: 5, Hash: "h", UploadedAt: 1})
	entries, err := e.List("")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, en := range entries {
		got[en.Name] = en.Local + "/" + en.Device
	}
	if got["Phone"] != "other-device/Phone" || got["Documents"] != "cloud-only/PC" {
		t.Fatalf("on the PC: %v", got)
	}
	e.Configure(Config{Device: meta.DevicePhone})
	entries, _ = e.List("")
	for _, en := range entries {
		got[en.Name] = en.Local + "/" + en.Device
	}
	if got["Documents"] != "other-device/PC" {
		t.Fatalf("on the phone: %v", got)
	}
}

func TestWifiOnlyBlocksUploads(t *testing.T) {
	e, st := newTestEngine(t)
	if e.Blocked() != "" {
		t.Fatal("a PC is never blocked")
	}
	e.SetConditions(Conditions{Unmetered: false, Charging: true})
	if !strings.Contains(e.Blocked(), "Wi-Fi") {
		t.Fatalf("Wi-Fi only is the default; got %q", e.Blocked())
	}
	st.Set(SettingWifiOnly, "off")
	if e.Blocked() != "" {
		t.Fatal("mobile data allowed")
	}
	st.Set(SettingChargingOnly, "on")
	e.SetConditions(Conditions{Unmetered: true, Charging: false})
	if !strings.Contains(e.Blocked(), "charge") {
		t.Fatalf("charging only: got %q", e.Blocked())
	}
}

func TestIdleAndSkippedFolders(t *testing.T) {
	e, st := newTestEngine(t)
	dl := filepath.Join(t.TempDir(), "Download")
	write(t, filepath.Join(dl, "a.pdf"), "a")
	write(t, filepath.Join(dl, "tgvault", "restored.jpg"), "r")
	e.Configure(Config{Device: meta.DevicePhone, SkipDirs: []string{filepath.Join(dl, "tgvault")}})
	rt, err := e.AddRoot(dl)
	if err != nil {
		t.Fatal(err)
	}
	<-e.scanCh // AddRoot queued a scan; run it directly
	e.scanRoot(rt)
	if got := paths(t, st, rt.ID); len(got) != 1 || got[0] != "Phone/Download/a.pdf=pending" {
		t.Fatalf("restored files must not be backed up again: %v", got)
	}
	if e.Idle() {
		t.Fatal("a pending file means not idle")
	}
	it, _ := st.ItemByPath("Phone/Download/a.pdf")
	st.FinishItem(it.ID, store.StateSynced, "", 0, 0, time.Now().Unix()+10)
	if !e.Idle() {
		t.Fatal("nothing left to do should be idle")
	}
}
