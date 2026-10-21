//go:build windows

package main

import "testing"

// The program has to start with jasqet.manifest's settings, or the window is
// blurry on scaled screens. A cgo build also links in MinGW's own manifest
// (ld warns "multiple non-default manifests" and keeps both, ours first).
func TestTheProgramRunsWithItsOwnManifest(t *testing.T) {
	ctx, _, _ := user32.NewProc("GetThreadDpiAwarenessContext").Call()
	v2, _, _ := user32.NewProc("AreDpiAwarenessContextsEqual").Call(ctx, ^uintptr(3)) // -4: per-monitor v2
	if v2 == 0 {
		t.Fatal("not per-monitor DPI aware: jasqet.manifest isn't the one Windows used (see rsrc_windows_*.syso)")
	}
}
