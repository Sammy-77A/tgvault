package backup

import "fmt"

// freeMargin is left free on top of a restored file, so restoring never
// fills the disk completely.
const freeMargin = 64 << 20

// checkFreeSpace refuses a restore that wouldn't fit in dir.
func checkFreeSpace(dir string, size int64) error {
	free, err := diskFree(dir)
	if err != nil {
		return nil // unknown: try anyway
	}
	if need := uint64(size) + freeMargin; free < need {
		return fmt.Errorf("not enough free space: it needs %s but only %s is free; free up %s and try again",
			humanBytes(int64(need)), humanBytes(int64(free)), humanBytes(int64(need-free)))
	}
	return nil
}
