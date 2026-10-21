//go:build !windows

package main

// On Mac and Linux the viewer opens as a browser app window.
func runViewer(u string) { openBrowser(u) }

func registerProtocol() {}
