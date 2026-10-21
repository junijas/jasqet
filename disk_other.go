//go:build !windows && !darwin

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// diskFree: bytes free for this user on the drive holding dir (-1 if unknown).
func diskFree(dir string) int64 {
	var st unix.Statfs_t
	if unix.Statfs(dir, &st) != nil {
		return -1
	}
	return int64(uint64(st.Bavail) * uint64(st.Bsize))
}

// Files on macOS and Linux are sparse already: nothing to do.
func makeSparse(f *os.File) {}
func unsparse(f *os.File)   {}

// openShared opens a file for reading (files can be renamed while open here).
func openShared(p string) (*os.File, error) { return os.Open(p) }
