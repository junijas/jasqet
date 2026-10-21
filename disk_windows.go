//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

const fsctlSetSparse = 0x000900c4 // FSCTL_SET_SPARSE

// diskFree: bytes free for this user on the drive holding dir (-1 if unknown).
func diskFree(dir string) int64 {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return -1
	}
	var avail, total, free uint64
	if windows.GetDiskFreeSpaceEx(p, &avail, &total, &free) != nil {
		return -1
	}
	return int64(avail)
}

// makeSparse marks the file as sparse. On NTFS, writing far into a new file
// otherwise makes Windows fill everything before that point with zeros first,
// which stalls the download (and doubles the disk writes) for big files.
// Drives that can't do it (FAT32, exFAT) are simply left as they are.
func makeSparse(f *os.File) {
	var n uint32
	windows.DeviceIoControl(windows.Handle(f.Fd()), fsctlSetSparse, nil, 0, nil, 0, &n, nil)
}

// unsparse turns a finished file back into an ordinary one.
func unsparse(f *os.File) {
	var n uint32
	set := byte(0) // FILE_SET_SPARSE_BUFFER{SetSparse: FALSE}
	windows.DeviceIoControl(windows.Handle(f.Fd()), fsctlSetSparse, &set, 1, nil, 0, &n, nil)
}

// openShared opens a file for reading while letting it be renamed or deleted
// (a download finishing, an after-download rule moving it) as it's being viewed.
func openShared(p string) (*os.File, error) {
	u, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(u, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: p, Err: err}
	}
	return os.NewFile(uintptr(h), p), nil
}
