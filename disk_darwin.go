//go:build darwin

package main

import "os"

// diskFree: on macOS the free space the system reports leaves out "purgeable"
// space that macOS frees on demand, so a check would refuse downloads that fit.
// Unknown (-1) means no check.
func diskFree(dir string) int64 { return -1 }

// Files on macOS are sparse already: nothing to do.
func makeSparse(f *os.File) {}
func unsparse(f *os.File)   {}

// openShared opens a file for reading (files can be renamed while open here).
func openShared(p string) (*os.File, error) { return os.Open(p) }
