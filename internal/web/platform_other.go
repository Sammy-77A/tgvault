//go:build !windows

package web

import (
	"context"
	"errors"
	"os/exec"
)

func autostartEnabled() bool { return false }

func setAutostart(bool) error {
	return errors.New("start-with-computer is only supported on Windows for now")
}

func pickFolder(context.Context) (string, error) {
	return "", errors.New("the folder picker is only available on Windows; type the path instead")
}

// vlcPath is where VLC is installed ("" when it isn't, or can't be told).
func vlcPath() string {
	if p, err := exec.LookPath("vlc"); err == nil {
		return p
	}
	return ""
}
