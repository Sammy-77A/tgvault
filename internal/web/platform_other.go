//go:build !windows

package web

import (
	"context"
	"errors"
)

func autostartEnabled() bool { return false }

func setAutostart(bool) error {
	return errors.New("start-with-computer is only supported on Windows for now")
}

func pickFolder(context.Context) (string, error) {
	return "", errors.New("the folder picker is only available on Windows; type the path instead")
}
