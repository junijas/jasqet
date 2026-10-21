package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Programs and scripts never get the green "✓ Checked": a .bat, .ps1 or .hta
// is plain text, so before this they passed as harmless.
func TestProgramsAndScriptsAreNeverJustChecked(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"report.bat": "@echo off\r\ndel /q %USERPROFILE%\\*", "run.ps1": "Remove-Item -Recurse ~",
		"invoice.vbs": "CreateObject(\"WScript.Shell\")", "page.hta": "<html><script>x()</script></html>",
		"open.url": "[InternetShortcut]\r\nURL=file://x", "setup.exe": "MZ\x90\x00 a program", "disk.iso": "CD001",
	} {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(body), 0o644)
		s := &Safety{}
		checkType(p, name, s)
		if s.Level != "warn" && s.Level != "danger" {
			t.Errorf("%s: level %q, want at least warn", name, s.Level)
		}
		if strings.Count(strings.Join(s.Notes, "\n"), "trust the website") > 1 {
			t.Errorf("%s: the same warning twice: %q", name, s.Notes)
		}
	}
	// ordinary files stay green
	p := filepath.Join(dir, "notes.txt")
	os.WriteFile(p, []byte("just text"), 0o644)
	s := &Safety{}
	checkType(p, "notes.txt", s)
	if s.Level == "warn" || s.Level == "danger" {
		t.Errorf("notes.txt: level %q", s.Level)
	}
}

// Only a checksum you typed proves anything: one found on the same website
// could have been changed along with the file, so it shows "site", not "ok".
func TestOnlyYourOwnChecksumCountsAsProof(t *testing.T) {
	m := newTestManager(t)
	p := filepath.Join(t.TempDir(), "tool.zip")
	data := []byte("PK\x03\x04 pretend zip")
	os.WriteFile(p, data, 0o644)
	h := sha256.Sum256(data)
	sum := "sha256:" + hex.EncodeToString(h[:])
	for from, want := range map[string]string{
		checksumTyped: "ok", "": "ok", // typed in the details panel, or into the Add box
		"the .sha256 file next to the download": "site", "the download page": "site",
	} {
		d := &Download{URL: "https://example.com/tool.zip", FileName: "tool.zip", Checksum: sum, ChecksumFrom: from}
		if got := m.checkSafety(context.Background(), d, p).HashMatch; got != want {
			t.Errorf("checksum from %q: hashMatch %q, want %q", from, got, want)
		}
	}
}

// A file that's dangerous for another reason is renamed to .blocked even when
// its checksum is wrong too; one that's only damaged keeps its name.
func TestDangerousFileIsBlockedEvenWithAWrongChecksum(t *testing.T) {
	m := newTestManager(t)
	_, st := m.Snapshot()
	st.Notify.DesktopAll, st.Notify.TelegramAll = false, false
	m.SetSettings(st)
	dir := t.TempDir()
	wrong := "sha256:" + strings.Repeat("0", 64)
	cases := []struct {
		name, body string
		blocked    bool
	}{
		{"movie.mp4.exe", "MZ\x90\x00 a program pretending to be a video", true},
		{"song.mp3", "ID3\x03\x00 not quite the published file", false},
	}
	for _, c := range cases {
		os.WriteFile(filepath.Join(dir, c.name), []byte(c.body), 0o644)
		d := &Download{ID: newID(), URL: "https://example.com/" + c.name, Dir: dir, FileName: c.name, Checksum: wrong, Status: StDone}
		m.afterDownload(d)
		if d.Safety == nil || d.Safety.HashMatch != "bad" || d.Safety.Level != "danger" {
			t.Fatalf("%s: safety %+v", c.name, d.Safety)
		}
		renamed := fileExists(filepath.Join(dir, c.name+".blocked"))
		if renamed != c.blocked {
			t.Errorf("%s: renamed to .blocked = %v, want %v", c.name, renamed, c.blocked)
		}
		if c.blocked && d.FileName != c.name+".blocked" {
			t.Errorf("%s: the list says %s", c.name, d.FileName)
		}
	}
}
