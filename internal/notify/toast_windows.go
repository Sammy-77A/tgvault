package notify

import (
	"encoding/base64"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"unicode/utf16"
)

// powershellAppID lets an unpackaged app show toasts under Windows PowerShell's
// registered identity, which needs no installer.
const powershellAppID = `{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}\WindowsPowerShell\v1.0\powershell.exe`

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}

// send shows a Windows notification; clicking it opens url.
func send(title, body, url string) error {
	toastXML := fmt.Sprintf(`<toast activationType="protocol" launch="%s" duration="long">`+
		`<visual><binding template="ToastGeneric"><text>%s</text><text>%s</text><text placement="attribution">tgvault</text></binding></visual>`+
		`<actions><action content="Open tgvault" activationType="protocol" arguments="%s"/></actions></toast>`,
		xmlEscape(url), xmlEscape(title), xmlEscape(body), xmlEscape(url))
	script := `$ErrorActionPreference = 'Stop'
[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] | Out-Null
[Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType = WindowsRuntime] | Out-Null
$xml = New-Object Windows.Data.Xml.Dom.XmlDocument
$xml.LoadXml('` + strings.ReplaceAll(toastXML, "'", "''") + `')
$toast = New-Object Windows.UI.Notifications.ToastNotification $xml
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier('` + powershellAppID + `').Show($toast)`
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-EncodedCommand", encodePS(script))
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// encodePS encodes a script for -EncodedCommand (base64 of UTF-16LE).
func encodePS(s string) string {
	u := utf16.Encode([]rune(s))
	b := make([]byte, len(u)*2)
	for i, v := range u {
		b[2*i] = byte(v)
		b[2*i+1] = byte(v >> 8)
	}
	return base64.StdEncoding.EncodeToString(b)
}
