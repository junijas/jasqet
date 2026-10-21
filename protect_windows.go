//go:build windows

package main

import (
	"encoding/base64"
	"unsafe"

	"golang.org/x/sys/windows"
)

// protect encrypts a secret so only this Windows user can read it (the same
// protection browsers use for saved cookies).
func protect(s string) string {
	if s == "" {
		return ""
	}
	in := []byte(s)
	var out windows.DataBlob
	if err := windows.CryptProtectData(&windows.DataBlob{Size: uint32(len(in)), Data: &in[0]}, nil, nil, 0, nil, 0x1, &out); err != nil {
		return ""
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return "dpapi:" + base64.StdEncoding.EncodeToString(unsafe.Slice(out.Data, out.Size))
}

func unprotect(s string) string {
	const p = "dpapi:"
	if len(s) <= len(p) || s[:len(p)] != p {
		return ""
	}
	in, err := base64.StdEncoding.DecodeString(s[len(p):])
	if err != nil || len(in) == 0 {
		return ""
	}
	var out windows.DataBlob
	if err := windows.CryptUnprotectData(&windows.DataBlob{Size: uint32(len(in)), Data: &in[0]}, nil, nil, 0, nil, 0x1, &out); err != nil {
		return ""
	}
	defer windows.LocalFree(windows.Handle(unsafe.Pointer(out.Data)))
	return string(unsafe.Slice(out.Data, out.Size))
}
