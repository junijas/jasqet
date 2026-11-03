//go:build !windows

package main

// knownDownloadsDir: on a Mac and Linux, browsers use ~/Downloads
// (browserDownloadDirs adds it).
func knownDownloadsDir() string { return "" }
