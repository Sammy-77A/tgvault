package web

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/windows/registry"
)

const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

func autostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue("tgvault")
	return err == nil
}

func setAutostart(on bool) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !on {
		if err := k.DeleteValue("tgvault"); err != nil && !errors.Is(err, registry.ErrNotExist) {
			return err
		}
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return k.SetStringValue("tgvault", `"`+exe+`" --background`)
}

// pickFolder shows the Windows folder picker and returns the chosen path ("" if cancelled).
func pickFolder(ctx context.Context) (string, error) {
	script := `Add-Type -AssemblyName System.Windows.Forms
$f = New-Object System.Windows.Forms.FolderBrowserDialog
$f.Description = 'Choose a folder for tgvault'
$f.UseDescriptionForTitle = $true
$owner = New-Object System.Windows.Forms.Form -Property @{TopMost = $true}
if ($f.ShowDialog($owner) -eq 'OK') { [Console]::Out.Write($f.SelectedPath) }`
	cmd := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-STA", "-NonInteractive", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	out, err := cmd.Output()
	if err != nil {
		return "", errors.New("could not open the folder picker; type the path instead")
	}
	return strings.TrimSpace(string(out)), nil
}

// vlcPath is where VLC is installed ("" when it isn't).
func vlcPath() string {
	for _, key := range []string{`SOFTWARE\VideoLAN\VLC`, `SOFTWARE\WOW6432Node\VideoLAN\VLC`} {
		k, err := registry.OpenKey(registry.LOCAL_MACHINE, key, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		dir, _, err := k.GetStringValue("InstallDir")
		k.Close()
		if err == nil {
			if exe := filepath.Join(dir, "vlc.exe"); fileExists(exe) {
				return exe
			}
		}
	}
	for _, base := range []string{os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)")} {
		if base == "" {
			continue
		}
		if exe := filepath.Join(base, "VideoLAN", "VLC", "vlc.exe"); fileExists(exe) {
			return exe
		}
	}
	if p, err := exec.LookPath("vlc"); err == nil {
		return p
	}
	return ""
}

func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}
