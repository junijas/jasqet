//go:build !windows

package main

import (
	"os/exec"
	"runtime"
	"strings"
)

func desktopNotify(title, text string) {
	switch runtime.GOOS {
	case "darwin":
		q := strings.NewReplacer(`\`, `\\`, `"`, `\"`) // AppleScript string escaping
		exec.Command("osascript", "-e", `display notification "`+q.Replace(text)+`" with title "`+q.Replace(title)+`"`).Run()
	default:
		exec.Command("notify-send", title, text).Run()
	}
}

func desktopNotifyFile(title, text, id string) { desktopNotify(title, text) }
