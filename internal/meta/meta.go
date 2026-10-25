// Package meta defines what tgvault writes into each Telegram caption, and the
// rules for which files and folders are backed up.
package meta

import (
	"encoding/json"
	"errors"
	"path"
	"strings"
	"unicode"
)

// MaxFileSize is the largest file Telegram accepts (4000 parts of 512 KiB),
// rounded down for safety.
const MaxFileSize = 2_000_000_000

// MaxCaption is Telegram's caption limit for regular accounts.
const MaxCaption = 1024

const marker = "tgvault:"

// Info is the machine-readable part of a caption. It lets tgvault rebuild its
// whole file list from Telegram alone.
type Info struct {
	V    int    `json:"v"`
	Kind string `json:"k"`           // "f" file, "p" project ZIP
	Path string `json:"p"`           // cloud path, e.g. "Documents/Notes/a.txt"
	Size int64  `json:"s"`           // size of the uploaded bytes
	Mt   int64  `json:"m"`           // local modification time (unix seconds)
	Hash string `json:"h"`           // sha256 of the uploaded bytes
	FP   string `json:"t,omitempty"` // project fingerprint
	Dev  string `json:"d,omitempty"` // device that uploaded it: "PC" or "Phone"
}

// Device names. The PC's backups keep their original paths ("Documents/…");
// every other device's backups live under a folder named after it ("Phone/…").
const (
	DevicePC    = "PC"
	DevicePhone = "Phone"
)

// DeviceOf returns which device a cloud path belongs to.
func DeviceOf(cloudPath string) string {
	if cloudPath == DevicePhone || strings.HasPrefix(cloudPath, DevicePhone+"/") {
		return DevicePhone
	}
	return DevicePC
}

// DevicePrefix is the folder a device's backups go under ("" for the PC).
func DevicePrefix(device string) string {
	if device == DevicePC || device == "" {
		return ""
	}
	return device
}

// Caption builds the caption shown in Telegram: a readable path, hashtags for
// each folder, and the machine-readable line.
func Caption(in Info) (string, error) {
	in.V = 1
	js, err := json.Marshal(in)
	if err != nil {
		return "", err
	}
	machine := marker + string(js)
	if len(machine) > MaxCaption {
		return "", errors.New("the file path is too long to store in Telegram (over about 900 characters)")
	}
	icon := "📄 "
	if in.Kind == "p" {
		icon = "📦 "
	}
	human := icon + in.Path
	tags := Hashtags(in.Path, in.Kind == "p")
	caption := human + "\n" + tags + "\n" + machine
	if len(caption) <= MaxCaption {
		return caption, nil
	}
	caption = human + "\n" + machine
	if len(caption) <= MaxCaption {
		return caption, nil
	}
	return machine, nil
}

// Parse extracts Info from a caption. ok is false for messages tgvault didn't write.
func Parse(caption string) (Info, bool) {
	for _, line := range strings.Split(caption, "\n") {
		if !strings.HasPrefix(line, marker) {
			continue
		}
		var in Info
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, marker)), &in); err != nil {
			return Info{}, false
		}
		if in.V != 1 || in.Path == "" || (in.Kind != "f" && in.Kind != "p") {
			return Info{}, false
		}
		return in, true
	}
	return Info{}, false
}

// Hashtags turns each folder of a cloud path into a Telegram hashtag, so the
// channel can be filtered by folder inside the Telegram app.
func Hashtags(cloudPath string, isProject bool) string {
	dir := path.Dir(cloudPath)
	parts := strings.Split(dir, "/")
	if isProject {
		parts = strings.Split(cloudPath, "/")
	}
	var tags []string
	seen := map[string]bool{}
	for _, p := range parts {
		t := tagify(p)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		tags = append(tags, "#"+t)
	}
	return strings.Join(tags, " ")
}

func tagify(s string) string {
	var b strings.Builder
	lastUnderscore := false
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastUnderscore = false
		} else if !lastUnderscore && b.Len() > 0 {
			b.WriteRune('_')
			lastUnderscore = true
		}
	}
	t := strings.TrimRight(b.String(), "_")
	if t == "" || t == "." {
		return ""
	}
	// A hashtag made only of digits isn't clickable in Telegram.
	if strings.IndexFunc(t, func(r rune) bool { return !unicode.IsDigit(r) && r != '_' }) < 0 {
		t = "f" + t
	}
	return t
}

// ---- backup rules ----

// alwaysSkipDirs are never backed up anywhere: they are caches or downloaded
// dependencies that can be recreated.
var alwaysSkipDirs = map[string]bool{
	"node_modules":              true,
	"__pycache__":               true,
	".pytest_cache":             true,
	".mypy_cache":               true,
	".ruff_cache":               true,
	".gradle":                   true,
	".next":                     true,
	".nuxt":                     true,
	".svelte-kit":               true,
	".parcel-cache":             true,
	".turbo":                    true,
	".angular":                  true,
	"$RECYCLE.BIN":              true,
	"System Volume Information": true,
	// Temporary folders of other sync apps; their contents are never real files.
	".tmp.driveupload":   true, // Google Drive
	".tmp.drivedownload": true, // Google Drive
	".dropbox.cache":     true,
	// Android: generated thumbnails and WhatsApp's temporary statuses.
	".thumbnails": true,
	".Statuses":   true,
}

// SkipPath reports whether a folder, by its path inside a backup folder, is one
// Android keeps private to other apps (and doesn't let tgvault read anyway).
func SkipPath(rel string) bool {
	return rel == "Android/data" || rel == "Android/obb" ||
		strings.HasSuffix(rel, "/Android/data") || strings.HasSuffix(rel, "/Android/obb")
}

// SkipDir reports whether a folder should be skipped because its contents can
// be recreated. markers are the project markers of the enclosing code project
// (nil outside projects); hasFile reports whether the folder contains a file.
// Rules only fire when the project type makes the folder's purpose certain.
func SkipDir(name string, markers map[string]bool, hasFile func(string) bool) bool {
	if alwaysSkipDirs[name] {
		return true
	}
	switch name {
	case ".venv", "venv", "env", ".env":
		return hasFile("pyvenv.cfg") // a Python virtual environment
	case "vendor":
		return markers["composer.json"] || markers["go.mod"]
	case "target":
		return markers["Cargo.toml"] || markers["pom.xml"]
	case "bin", "obj":
		return markers[".csproj"]
	case "build":
		return markers["build.gradle"] || markers["build.gradle.kts"] || markers["pubspec.yaml"]
	case ".dart_tool":
		return markers["pubspec.yaml"]
	case "bower_components":
		return true
	}
	return false
}

// SkipFile reports whether a file is temporary or system junk.
func SkipFile(name string) bool {
	lower := strings.ToLower(name)
	switch lower {
	case "desktop.ini", "thumbs.db", ".ds_store", "ntuser.dat":
		return true
	}
	if strings.HasPrefix(name, "~$") || strings.HasPrefix(name, ".~lock.") {
		return true // Office / LibreOffice lock files
	}
	if strings.HasPrefix(name, ".trashed-") || strings.HasPrefix(name, ".pending-") {
		return true // Android: in the trash, or still being written (e.g. a video recording)
	}
	for _, ext := range []string{".tmp", ".temp", ".crdownload", ".part", ".partial", ".download", ".tgvault-tmp"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// projectMarkers are files or folders whose presence means "this is a code project".
var projectMarkers = []string{
	".git", "package.json", "composer.json", "go.mod", "Cargo.toml",
	"pyproject.toml", "requirements.txt", "pom.xml", "build.gradle",
	"build.gradle.kts", "pubspec.yaml", "Gemfile", "artisan", "CMakeLists.txt",
}

// ProjectMarkers returns which project markers are among a folder's entries.
func ProjectMarkers(entries []string) []string {
	have := map[string]bool{}
	for _, e := range entries {
		have[e] = true
	}
	var found []string
	for _, m := range projectMarkers {
		if have[m] {
			found = append(found, m)
		}
	}
	for _, e := range entries {
		if strings.HasSuffix(e, ".csproj") {
			found = append(found, ".csproj")
			break
		}
	}
	return found
}

// File types used to filter the file browser.
const (
	TypeAudio    = "audio"
	TypeVideo    = "video"
	TypeImage    = "image"
	TypeDocument = "document"
	TypeArchive  = "archive"
	TypeOther    = "other"
)

var typeByExt = map[string]string{}

func init() {
	for t, exts := range map[string]string{
		TypeAudio:    "mp3 m4a aac wav flac ogg opus oga wma amr 3ga aiff mid midi",
		TypeVideo:    "mp4 mkv mov avi wmv webm m4v 3gp flv mpg mpeg ts",
		TypeImage:    "jpg jpeg png gif webp heic heif bmp tif tiff svg raw dng cr2 nef ico",
		TypeDocument: "pdf doc docx xls xlsx ppt pptx odt ods odp txt rtf md csv epub json xml html htm",
		TypeArchive:  "zip rar 7z tar gz tgz bz2 xz apk exe msi iso dmg",
	} {
		for _, e := range strings.Fields(exts) {
			typeByExt[e] = t
		}
	}
}

// FileType sorts a file name into audio, video, image, document, archive or other.
func FileType(name string) string {
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))
	if t, ok := typeByExt[ext]; ok {
		return t
	}
	return TypeOther
}
