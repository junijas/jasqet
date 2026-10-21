//go:build !windows

package main

import "encoding/base64"

// On Mac and Linux the state file is already private to your user account.
func protect(s string) string {
	if s == "" {
		return ""
	}
	return "b64:" + base64.StdEncoding.EncodeToString([]byte(s))
}

func unprotect(s string) string {
	if len(s) < 4 || s[:4] != "b64:" {
		return ""
	}
	b, _ := base64.StdEncoding.DecodeString(s[4:])
	return string(b)
}
