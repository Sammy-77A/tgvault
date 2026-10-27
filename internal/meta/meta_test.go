package meta

import (
	"strings"
	"testing"
)

func TestCaptionRoundTrip(t *testing.T) {
	in := Info{Kind: "f", Path: "Documents/My Projects/notes 2026.txt", Size: 42, Mt: 1700000000, Hash: strings.Repeat("a", 64)}
	c, err := Caption(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c, "📄 Documents/My Projects/notes 2026.txt\n#Documents #My_Projects\n") {
		t.Fatalf("unexpected caption:\n%s", c)
	}
	got, ok := Parse(c)
	if !ok {
		t.Fatal("caption did not parse")
	}
	in.V = 1
	if got != in {
		t.Fatalf("got %+v, want %+v", got, in)
	}
}

func TestCaptionLongPathDropsTags(t *testing.T) {
	long := strings.Repeat("folder/", 60) + "f.txt" // ~425 chars
	c, err := Caption(Info{Kind: "f", Path: long, Size: 1, Hash: strings.Repeat("b", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if len(c) > MaxCaption {
		t.Fatalf("caption is %d bytes", len(c))
	}
	if _, ok := Parse(c); !ok {
		t.Fatal("long caption did not parse")
	}
	if _, err := Caption(Info{Kind: "f", Path: strings.Repeat("x", 1100)}); err == nil {
		t.Fatal("expected an error for a path that can't fit")
	}
}

func TestParseRejectsOtherMessages(t *testing.T) {
	for _, c := range []string{"", "hello", "tgvault:{bad json", `tgvault:{"v":2,"k":"f","p":"a"}`, `tgvault:{"v":1,"k":"x","p":"a"}`} {
		if _, ok := Parse(c); ok {
			t.Errorf("Parse(%q) should fail", c)
		}
	}
}

func TestHashtags(t *testing.T) {
	cases := map[string]string{
		"Documents/a.txt":                "#Documents",
		"Documents/2024/Taxes & Bills/x": "#Documents #f2024 #Taxes_Bills",
		"Docs/Docs/x":                    "#Docs",
	}
	for p, want := range cases {
		if got := Hashtags(p, false); got != want {
			t.Errorf("Hashtags(%q) = %q, want %q", p, got, want)
		}
	}
	if got := Hashtags("Documents/Projects/shop-site", true); got != "#Documents #Projects #shop_site" {
		t.Errorf("project hashtags = %q", got)
	}
}

func TestSkipDir(t *testing.T) {
	no := func(string) bool { return false }
	yes := func(string) bool { return true }
	cases := []struct {
		name    string
		markers map[string]bool
		has     func(string) bool
		want    bool
	}{
		{"node_modules", nil, no, true},
		{".tmp.driveupload", nil, no, true}, // Google Drive's temp folder
		{"vendor", nil, no, false},          // could be a real folder outside projects
		{"vendor", map[string]bool{"composer.json": true}, no, true},
		{"build", nil, no, false},
		{"build", map[string]bool{"build.gradle": true}, no, true},
		{"target", map[string]bool{"Cargo.toml": true}, no, true},
		{"venv", nil, yes, true}, // has pyvenv.cfg
		{"venv", nil, no, false},
		{"src", map[string]bool{"package.json": true}, no, false},
		{".git", map[string]bool{".git": true}, no, false}, // history is kept
	}
	for _, c := range cases {
		if got := SkipDir(c.name, c.markers, c.has); got != c.want {
			t.Errorf("SkipDir(%q, %v) = %v, want %v", c.name, c.markers, got, c.want)
		}
	}
}

func TestSkipFile(t *testing.T) {
	for _, n := range []string{"~$report.docx", "desktop.ini", "Thumbs.db", "movie.mp4.crdownload", "x.tmp"} {
		if !SkipFile(n) {
			t.Errorf("SkipFile(%q) should be true", n)
		}
	}
	for _, n := range []string{"report.docx", ".env", "temp.txt"} {
		if SkipFile(n) {
			t.Errorf("SkipFile(%q) should be false", n)
		}
	}
}

func TestProjectMarkers(t *testing.T) {
	got := ProjectMarkers([]string{"src", "package.json", ".git", "App.csproj"})
	want := ".git,package.json,.csproj"
	if strings.Join(got, ",") != want {
		t.Fatalf("got %v, want %s", got, want)
	}
	if len(ProjectMarkers([]string{"notes.txt", "photos"})) != 0 {
		t.Fatal("plain folder detected as project")
	}
}

func TestDevices(t *testing.T) {
	cases := map[string]string{"Documents/a.txt": DevicePC, "Phone": DevicePhone, "Phone/Camera/x.jpg": DevicePhone, "Phones/x": DevicePC}
	for p, want := range cases {
		if got := DeviceOf(p); got != want {
			t.Errorf("DeviceOf(%q) = %s, want %s", p, got, want)
		}
	}
	if DevicePrefix(DevicePC) != "" || DevicePrefix(DevicePhone) != "Phone" {
		t.Fatal("PC paths stay unprefixed; phone paths go under Phone/")
	}
	in := Info{Kind: "f", Path: "Phone/Camera/x.jpg", Size: 1, Mt: 1, Hash: "h", Dev: DevicePhone}
	c, _ := Caption(in)
	out, ok := Parse(c)
	if !ok || out.Dev != DevicePhone {
		t.Fatalf("device lost in caption round trip: %+v", out)
	}
}

func TestAndroidSkips(t *testing.T) {
	for _, n := range []string{".trashed-1700000000-IMG_1.jpg", ".pending-1700000000-VID_2.mp4"} {
		if !SkipFile(n) {
			t.Errorf("%s should be skipped", n)
		}
	}
	for _, rel := range []string{"Android/data", "Android/obb", "x/Android/data"} {
		if !SkipPath(rel) {
			t.Errorf("%s should be skipped", rel)
		}
	}
	if SkipPath("Android/media") {
		t.Error("Android/media holds WhatsApp media and must be backed up")
	}
	if !SkipDir(".thumbnails", nil, func(string) bool { return false }) {
		t.Error(".thumbnails should be skipped")
	}
}

func TestSplitCaption(t *testing.T) {
	in := Info{Kind: "f", Path: "Videos/movie.mkv", Size: 5_000_000_000, Mt: 1700000000, Hash: strings.Repeat("a", 64),
		Group: "0123456789abcdef", Part: 3, Parts: 3, PartOff: 2 * SplitPartSize, PartSize: 5_000_000_000 - 2*SplitPartSize,
		PartHash: strings.Repeat("b", 64), EarlierID: "101,102"}
	c, err := Caption(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c, "📄 Videos/movie.mkv (part 3 of 3)\n") || !strings.Contains(c, `"v":2`) {
		t.Fatalf("unexpected caption:\n%s", c)
	}
	got, ok := Parse(c)
	in.V = 2
	if !ok || got != in {
		t.Fatalf("got %+v (%v), want %+v", got, ok, in)
	}
	// Inconsistent part fields are rejected rather than half-trusted.
	for _, bad := range []Info{
		{Kind: "f", Path: "a", Size: 10, Hash: "h", Group: "g", Part: 4, Parts: 3, PartSize: 1, PartHash: "x"},
		{Kind: "f", Path: "a", Size: 10, Hash: "h", Group: "g", Part: 1, Parts: 3, PartOff: 8, PartSize: 5, PartHash: "x"},
		{Kind: "f", Path: "a", Size: 10, Hash: "h", Part: 1, Parts: 3, PartSize: 5, PartHash: "x"},
	} {
		c, _ := Caption(bad)
		if _, ok := Parse(c); ok {
			t.Errorf("Parse accepted %+v", bad)
		}
	}
}

func TestSplitPartSizeFitsTelegram(t *testing.T) {
	if SplitPartSize > 4000*512*1024 || SplitPartSize%(512*1024) != 0 || SplitPartSize > MaxFileSize {
		t.Fatalf("SplitPartSize %d must be whole 512 KiB pieces under Telegram's limit", SplitPartSize)
	}
}

func TestChangeCaption(t *testing.T) {
	in := Info{Kind: "d", Path: "Documents/shop", Size: 900, Mt: 1700000000, Hash: strings.Repeat("c", 64), FP: "fp",
		Base: 10, Prev: 12, Seq: 2, Tree: strings.Repeat("d", 64), Changed: 3, Deleted: 1}
	c, err := Caption(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(c, "📦 Documents/shop (change 2 after the full backup: 3 files changed, 1 file deleted)\n#Documents #shop\n") {
		t.Fatalf("unexpected caption:\n%s", c)
	}
	got, ok := Parse(c)
	in.V = 1
	if !ok || got != in {
		t.Fatalf("got %+v %v, want %+v", got, ok, in)
	}
	// A change ZIP missing its links is not accepted.
	for _, bad := range []Info{
		{Kind: "d", Path: "a", Prev: 1, Seq: 1, Tree: "t"},
		{Kind: "d", Path: "a", Base: 1, Seq: 1, Tree: "t"},
		{Kind: "d", Path: "a", Base: 1, Prev: 1, Tree: "t"},
		{Kind: "d", Path: "a", Base: 1, Prev: 1, Seq: 1},
	} {
		c, _ := Caption(bad)
		if _, ok := Parse(c); ok {
			t.Fatalf("accepted %+v", bad)
		}
	}
}
