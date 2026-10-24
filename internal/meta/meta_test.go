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
