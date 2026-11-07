package web

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// In the phone's WebView a height given in vh comes out as 0, so a list capped
// with max-height in vh disappears (the request window and the storage list both
// did). Cap heights in rem or px instead.
func TestNoViewportHeightsCapListsInTheStyles(t *testing.T) {
	files, _ := filepath.Glob("ui/*.css")
	if len(files) == 0 {
		t.Fatal("no style sheets found")
	}
	bad := regexp.MustCompile(`max-height:\s*[\d.]+v[hw]`)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if bad.MatchString(line) {
				t.Errorf("%s:%d caps a height in viewport units, which is 0 in the phone's WebView: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}
