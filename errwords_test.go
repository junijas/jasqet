package main

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
)

// A number in a file or folder name isn't a server's status code: a missing
// file in "Movies 2404" isn't "File not found on the server (404)".
func TestStatusCodesOnlyFromTheServer(t *testing.T) {
	for _, code := range []string{"401", "403", "404", "407", "410"} {
		pe := &fs.PathError{Op: "open", Path: `D:\Movies 2` + code + `\film.mp4`, Err: errors.New("The system cannot find the path specified.")}
		if got := friendlyError(pe); strings.Contains(got, code) || !strings.Contains(got, "film.mp4") {
			t.Errorf("file error in a folder with %s: %q", code, got)
		}
		le := &os.LinkError{Op: "rename", Old: `D:\clip` + code + `.part`, New: `D:\clip` + code + `.mp4`, Err: errors.New("Access is denied.")}
		if got := friendlyError(le); strings.Contains(got, "("+code+")") || strings.Contains(got, "server") || strings.Contains(got, "login") {
			t.Errorf("rename error for a file with %s: %q", code, got)
		}
	}
	// the server's own words still count
	if got := friendlyError(&statusError{Code: 404, Status: "404 Not Found"}); !strings.Contains(got, "not found on the server (404)") {
		t.Errorf("a real 404: %q", got)
	}
	if got := friendlyError(errors.New("server replied 403 Forbidden")); !strings.Contains(got, "refused (403)") {
		t.Errorf("a real 403: %q", got)
	}
}
