package main

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

const quarantineAttr = "com.apple.quarantine"

func defenderScan(ctx context.Context, p string) (string, bool) { return "", false }

// markFromInternet sets the same "downloaded from the internet" mark browsers
// set, so Gatekeeper checks apps and disk images before they're opened.
func markFromInternet(p string, d *Download) {
	tag := []byte(quarantineTag(time.Now()))
	st, err := os.Stat(p)
	if err != nil {
		return
	}
	if !st.IsDir() {
		unix.Setxattr(p, quarantineAttr, tag, 0)
		return
	}
	filepath.Walk(p, func(f string, info os.FileInfo, err error) error {
		if err == nil {
			unix.Setxattr(f, quarantineAttr, tag, 0) // folders too: an unpacked .app is one
		}
		return nil
	})
}

// keepInternetTag gives a copy the same mark as the original (a copy to
// another disk leaves it behind).
func keepInternetTag(src, dst string) {
	buf := make([]byte, 256)
	if n, err := unix.Getxattr(src, quarantineAttr, buf); err == nil && n > 0 {
		unix.Setxattr(dst, quarantineAttr, buf[:n], 0)
	}
}
