//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// registerProtocol lets the buttons on Jasqet's notifications (jasqet://
// links) open the finished file. Only for this Windows user; the uninstaller
// removes it.
func registerProtocol() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Classes\jasqet`, registry.ALL_ACCESS)
	if err != nil {
		return
	}
	defer k.Close()
	k.SetStringValue("", "URL:Jasqet")
	k.SetStringValue("URL Protocol", "")
	if ic, _, err := registry.CreateKey(k, `DefaultIcon`, registry.ALL_ACCESS); err == nil {
		ic.SetStringValue("", `"`+exe+`",0`)
		ic.Close()
	}
	if c, _, err := registry.CreateKey(k, `shell\open\command`, registry.ALL_ACCESS); err == nil {
		c.SetStringValue("", `"`+exe+`" "%1"`)
		c.Close()
	}
	// the name and icon on Jasqet's notifications (see notify_windows.go)
	if a, _, err := registry.CreateKey(registry.CURRENT_USER, `Software\Classes\AppUserModelId\`+toastAppID, registry.ALL_ACCESS); err == nil {
		a.SetStringValue("DisplayName", "Jasqet")
		if ico := filepath.Join(filepath.Dir(exe), "icon.ico"); fileExists(ico) {
			a.SetStringValue("IconUri", ico)
		}
		a.Close()
	}
	forgetTurboGet(exe)
}

// forgetTurboGet removes what this app registered under its old name (the
// turboget:// links and "TurboGet" on notifications), so it doesn't hold on
// to another app's name. Only when they're clearly its own: they start
// TurboGet.exe from this folder, or a program that's gone.
func forgetTurboGet(exe string) {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software\Classes\turboget\shell\open\command`, registry.QUERY_VALUE)
	if err != nil {
		return
	}
	cmd, _, _ := k.GetStringValue("")
	k.Close()
	if !oldProtocolIsOurs(cmd, exe, fileExists) {
		return
	}
	for _, p := range []string{`turboget\shell\open\command`, `turboget\shell\open`, `turboget\shell`, `turboget\DefaultIcon`, `turboget`, `AppUserModelId\TurboGet`} {
		registry.DeleteKey(registry.CURRENT_USER, `Software\Classes\`+p)
	}
}

// oldProtocolIsOurs: the turboget:// command (`"C:\…\TurboGet.exe" "%1"`)
// starts TurboGet.exe from exe's folder, or a program that's no longer there.
func oldProtocolIsOurs(command, exe string, exists func(string) bool) bool {
	old, _, ok := strings.Cut(strings.TrimPrefix(command, `"`), `"`)
	if !ok || !strings.EqualFold(filepath.Base(old), "TurboGet.exe") {
		return false
	}
	return strings.EqualFold(filepath.Dir(old), filepath.Dir(exe)) || !exists(old)
}
