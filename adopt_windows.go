//go:build windows

package main

import "golang.org/x/sys/windows"

// knownDownloadsDir: the Downloads folder, also when you've moved it (its
// Location tab in Properties).
func knownDownloadsDir() string {
	p, err := windows.KnownFolderPath(windows.FOLDERID_Downloads, 0)
	if err != nil {
		return ""
	}
	return p
}
