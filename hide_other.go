//go:build !windows

package main

import "os/exec"

func hideWindow(cmd *exec.Cmd) {}

func lowPriority(cmd *exec.Cmd) {}
