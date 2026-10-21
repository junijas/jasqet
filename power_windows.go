//go:build windows

package main

import (
	"os/exec"
	"runtime"
	"sync"
	"syscall"
)

var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procSetThreadExecState = kernel32.NewProc("SetThreadExecutionState")
	awakeCh                = make(chan bool, 1)
	awakeOnce              sync.Once
	awakeMu                sync.Mutex // one caller at a time hands over the latest wish
)

const (
	esContinuous     = 0x80000000
	esSystemRequired = 0x00000001
)

// setAwake asks Windows not to go to sleep (the screen may still turn off).
// The request belongs to one OS thread, so a single locked goroutine owns it.
func setAwake(on bool) {
	awakeOnce.Do(func() {
		go func() {
			runtime.LockOSThread()
			for v := range awakeCh {
				if v {
					procSetThreadExecState.Call(uintptr(esContinuous | esSystemRequired))
				} else {
					procSetThreadExecState.Call(uintptr(esContinuous))
				}
			}
		}()
	})
	awakeMu.Lock()
	defer awakeMu.Unlock()
	select {
	case awakeCh <- on:
	default: // an older wish still waits: replace it
		select {
		case <-awakeCh:
		default: // taken meanwhile
		}
		awakeCh <- on
	}
}

func doPower(action string) error {
	var cmd *exec.Cmd
	switch action {
	case "shutdown":
		cmd = exec.Command("shutdown", "/s", "/t", "0")
	case "sleep":
		cmd = exec.Command("powershell", "-NoProfile", "-Command",
			"Add-Type -AssemblyName System.Windows.Forms; [System.Windows.Forms.Application]::SetSuspendState('Suspend', $false, $false)")
	default:
		return nil
	}
	hideWindow(cmd)
	return cmd.Run()
}
