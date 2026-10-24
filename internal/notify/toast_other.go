//go:build !windows

package notify

import "log"

func send(title, body, _ string) error {
	log.Printf("notification: %s: %s", title, body)
	return nil
}
