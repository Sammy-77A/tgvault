package media

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// Prints how real file names are read; run with REAL_NAMES=path to a list.
func TestRealNames(t *testing.T) {
	p := os.Getenv("REAL_NAMES")
	if p == "" {
		t.Skip("REAL_NAMES not set")
	}
	b, _ := os.ReadFile(p)
	seen := map[string]int{}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		in := ParseIn(strings.TrimSpace(l))
		key := fmt.Sprintf("%s | %q y%d s%d e%v weak=%v", in.Kind, in.Title, in.Year, in.Season, in.Episodes, in.Weak)
		seen[key]++
		if in.Kind == Unknown || in.Title == "" {
			fmt.Printf("UNREAD: %s\n", l)
		}
	}
	for k, n := range seen {
		if n >= 1 {
			fmt.Printf("%3d x %s\n", n, k)
		}
	}
}
