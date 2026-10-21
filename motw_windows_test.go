//go:build windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A "Move to" rule onto another drive copies the file: the "downloaded from
// the internet" tag must come along, or SmartScreen won't check it there.
func TestCopyToAnotherDriveKeepsTheInternetTag(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "setup.exe"), filepath.Join(dir, "moved", "setup.exe")
	os.MkdirAll(filepath.Dir(dst), 0o755)
	os.WriteFile(src, []byte("MZ program"), 0o644)
	tag := "[ZoneTransfer]\r\nZoneId=3\r\nHostUrl=https://example.com/setup.exe\r\n"
	if err := os.WriteFile(src+":Zone.Identifier", []byte(tag), 0o644); err != nil {
		t.Skip("this drive can't hold the tag:", err)
	}
	if err := copyMove(src, dst); err != nil {
		t.Fatal(err)
	}
	if fileExists(src) {
		t.Fatal("the original is still there")
	}
	got, err := os.ReadFile(dst + ":Zone.Identifier")
	if err != nil || string(got) != tag {
		t.Fatalf("the copy's tag = %q, %v; want %q", got, err, tag)
	}
}

// A finished download is tagged by the safety check.
func TestFinishedDownloadIsTagged(t *testing.T) {
	m := newTestManager(t)
	p := filepath.Join(t.TempDir(), "report.pdf")
	os.WriteFile(p, []byte("%PDF-1.7 test"), 0o644)
	d := &Download{URL: "https://user:secret@example.com/report.pdf", Page: "https://example.com/", FileName: "report.pdf"}
	m.checkSafety(context.Background(), d, p)
	got, err := os.ReadFile(p + ":Zone.Identifier")
	if err != nil {
		t.Skip("this drive can't hold the tag:", err)
	}
	s := string(got)
	if !strings.Contains(s, "ZoneId=3") || !strings.Contains(s, "HostUrl=https://example.com/report.pdf") {
		t.Fatalf("tag = %q", s)
	}
	if strings.Contains(s, "secret") {
		t.Fatalf("the tag holds the password: %q", s)
	}
}
