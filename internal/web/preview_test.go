package web

import "testing"

// A backed-up web page or script must never be served as something the
// browser would run.
func TestPreviewTypes(t *testing.T) {
	for name, want := range map[string]string{
		"photo.JPG":   "image/jpeg",
		"clip.mov":    "video/mp4",
		"song.mp3":    "audio/mpeg",
		"doc.pdf":     "application/pdf",
		"page.html":   "text/plain; charset=utf-8",
		"app.js":      "text/plain; charset=utf-8",
		"data.xml":    "text/plain; charset=utf-8",
		"report.docx": "application/octet-stream",
		"noext":       "application/octet-stream",
		"page.xhtml":  "application/octet-stream",
	} {
		if got := previewType(name); got != want {
			t.Errorf("%s: %s, want %s", name, got, want)
		}
	}
}
