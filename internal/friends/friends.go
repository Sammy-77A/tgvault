// Package friends lets a few people store and fetch files through tgvault
// running on an always-on computer. Each friend has an API key, a folder of
// their own inside the owner's Telegram backup ("Friends/<name>/…") and a
// storage limit. Uploads wait in a local folder until they are safely in
// Telegram, then the local copy is removed.
package friends

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Folder is the top-level cloud folder that holds every friend's files.
const Folder = "Friends"

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{1,31}$`)

// ValidName reports whether name can be a friend's name: 2 to 32 lowercase
// letters, digits, "-" or "_".
func ValidName(name string) bool { return nameRe.MatchString(name) }

// NewKey makes a new API key and the hash that is stored for it.
func NewKey() (key, hash string) {
	key = "tgv_" + randomToken(24)
	return key, HashKey(key)
}

// HashKey is how keys and share tokens are stored: only this hash is kept.
func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system's random source is broken; nothing safe can continue
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// ParseSize reads sizes such as "50GB", "500 MB", "1.5tb" or "0" (no limit).
func ParseSize(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || s == "0" || s == "none" || s == "unlimited" {
		return 0, nil
	}
	mult := int64(1)
	for _, u := range []struct {
		suffix string
		mult   int64
	}{{"tb", 1 << 40}, {"gb", 1 << 30}, {"mb", 1 << 20}, {"kb", 1 << 10}, {"t", 1 << 40}, {"g", 1 << 30}, {"m", 1 << 20}, {"k", 1 << 10}, {"b", 1}} {
		if strings.HasSuffix(s, u.suffix) {
			s, mult = strings.TrimSpace(strings.TrimSuffix(s, u.suffix)), u.mult
			break
		}
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f < 0 {
		return 0, fmt.Errorf("%q is not a size like 50GB", s)
	}
	return int64(f * float64(mult)), nil
}

// FormatSize writes bytes the way people read them.
func FormatSize(n int64) string {
	switch {
	case n >= 1<<40:
		return fmt.Sprintf("%.1f TB", float64(n)/(1<<40))
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// cleanRel checks a path a friend sent and returns it in its stored form, or
// false when it is not allowed: absolute, parent folders, odd characters.
func cleanRel(p string) (string, bool) {
	p = strings.Trim(p, "/")
	if p == "" {
		return "", true
	}
	if len(p) > 600 || strings.ContainsAny(p, "\\:*?\"<>|\x00") {
		return "", false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." || strings.TrimSpace(seg) != seg || strings.HasSuffix(seg, ".") {
			return "", false
		}
		for _, r := range seg {
			if r < 0x20 || r == 0x7f {
				return "", false
			}
		}
	}
	return p, true
}
