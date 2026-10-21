//go:build !windows

package main

import (
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
)

var (
	awakeMu  sync.Mutex
	awakeCmd *exec.Cmd
)

// setAwake keeps a Mac awake with caffeinate while needed (no-op elsewhere).
func setAwake(on bool) {
	if runtime.GOOS != "darwin" {
		return
	}
	awakeMu.Lock()
	defer awakeMu.Unlock()
	if on && awakeCmd == nil {
		c := exec.Command("caffeinate", "-i", "-w", strconv.Itoa(os.Getpid()))
		if c.Start() == nil {
			awakeCmd = c
		}
	} else if !on && awakeCmd != nil {
		awakeCmd.Process.Kill()
		awakeCmd.Wait()
		awakeCmd = nil
	}
}

func doPower(action string) error {
	var cmd *exec.Cmd
	switch {
	case runtime.GOOS == "darwin" && action == "sleep":
		cmd = exec.Command("pmset", "sleepnow")
	case runtime.GOOS == "darwin" && action == "shutdown":
		cmd = exec.Command("osascript", "-e", `tell app "System Events" to shut down`)
	case action == "sleep":
		cmd = exec.Command("systemctl", "suspend")
	case action == "shutdown":
		cmd = exec.Command("systemctl", "poweroff")
	default:
		return nil
	}
	return cmd.Run()
}
