//go:build tgbench

package main

import "time"

func setBenchStall(d time.Duration) { stallTimeout = d }
