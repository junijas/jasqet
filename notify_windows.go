//go:build windows

package main

import (
	"os"
	"os/exec"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// toastAppID: the name Windows files Jasqet's notifications under (with
// its icon), registered for this user by registerProtocol. Without it they'd
// show up as "Windows PowerShell".
const toastAppID = "Jasqet"

const powershellAppID = `{1AC14E77-02E7-4E5D-B744-2EB1AE5198B7}\WindowsPowerShell\v1.0\powershell.exe`

func toastSender() string {
	if k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Classes\AppUserModelId\`+toastAppID, registry.QUERY_VALUE); err == nil {
		k.Close()
		return toastAppID
	}
	return powershellAppID
}

// desktopNotify shows a Windows notification (bottom-right corner).
func desktopNotify(title, text string) { desktopNotifyFile(title, text, "") }

// desktopNotifyFile: the same, with Open and Show in folder buttons for a
// finished download (they come back to Jasqet as jasqet:// links).
func desktopNotifyFile(title, text, id string) {
	esc := func(s string) string {
		s = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;").Replace(s)
		return s
	}
	open, actions := ` activationType="protocol" launch="jasqet://show/"`, "" // a click shows Jasqet
	if id != "" && isSafeID(id) {
		open = ` activationType="protocol" launch="jasqet://open/` + id + `"`
		actions = `<actions><action content="Open" activationType="protocol" arguments="jasqet://open/` + id + `"/>` +
			`<action content="Show in folder" activationType="protocol" arguments="jasqet://folder/` + id + `"/></actions>`
	}
	xml := `<toast` + open + `><visual><binding template="ToastGeneric"><text>` + esc(title) + `</text><text>` + esc(text) + `</text></binding></visual>` + actions + `</toast>`
	// The text travels in an environment variable, never inside the script,
	// so nothing in a file name can be run as a command.
	script := `[Windows.UI.Notifications.ToastNotificationManager, Windows.UI.Notifications, ContentType = WindowsRuntime] > $null
[Windows.Data.Xml.Dom.XmlDocument, Windows.Data.Xml.Dom.XmlDocument, ContentType = WindowsRuntime] > $null
$x = New-Object Windows.Data.Xml.Dom.XmlDocument
$x.LoadXml($env:JASQET_TOAST)
$t = New-Object Windows.UI.Notifications.ToastNotification $x
[Windows.UI.Notifications.ToastNotificationManager]::CreateToastNotifier($env:JASQET_TOAST_APP).Show($t)`
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
	cmd.Env = append(os.Environ(), "JASQET_TOAST="+xml, "JASQET_TOAST_APP="+toastSender())
	hideWindow(cmd)
	cmd.Run()
}

func isSafeID(id string) bool {
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return len(id) > 0 && len(id) < 64
}
