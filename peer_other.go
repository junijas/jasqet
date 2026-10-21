//go:build !linux

package main

import "net/http"

// otherUser: on Windows and Mac other accounts can't read the browser's
// command line, so the one-time code needs no extra check.
func otherUser(*http.Request) bool { return false }
