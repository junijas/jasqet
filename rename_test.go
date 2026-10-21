package main

// Jasqet was called TurboGet up to 1.6.0: what people had keeps working.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// oldTurboGetFolder makes a TurboGet folder as 1.6.0 left it: a list with one
// download, tools, the window's data and where you stopped in a video.
func oldTurboGetFolder(t *testing.T, dir string) string {
	t.Helper()
	old := filepath.Join(dir, "TurboGet")
	// saved by a manager elsewhere, so nothing keeps writing into the old folder
	m := NewManager(filepath.Join(tempDirRetry(t), "state.json"))
	_, s := m.Snapshot()
	s.DownloadDir = filepath.Join(dir, "Downloads", "TurboGet")
	s.MaxConcurrent = 7
	m.SetSettings(s)
	m.mu.Lock()
	m.downloads["o1"] = &Download{ID: "o1", URL: "https://a.example/f.zip", FileName: "f.zip", Status: StPaused}
	m.dirty = true
	m.mu.Unlock()
	m.save()
	list, err := os.ReadFile(m.stateFile)
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"state.json":                   string(list),
		"tools/yt-dlp.exe":             "tool",
		"WebView2/Default/Preferences": "{}",
		"viewer-positions.json":        `{"x":1}`,
	} {
		p := filepath.Join(old, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	return old
}

func readText(p string) string { b, _ := os.ReadFile(p); return string(b) }

func TestMoveOldStateBringsEverything(t *testing.T) {
	dir := tempDirRetry(t) // the list loaded at the end may still be saving
	old := oldTurboGetFolder(t, dir)
	nu := filepath.Join(dir, "Jasqet")
	if err := moveOldState(old, nu); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("the TurboGet folder should be gone after the move: %v", err)
	}
	for _, name := range []string{"tools/yt-dlp.exe", "WebView2/Default/Preferences", "viewer-positions.json"} {
		if !fileExists(filepath.Join(nu, filepath.FromSlash(name))) {
			t.Errorf("%s didn't come along", name)
		}
	}
	m := NewManager(filepath.Join(nu, "state.json"))
	if d := m.get("o1"); d == nil || d.FileName != "f.zip" {
		t.Fatalf("the download list didn't come along: %+v", d)
	}
	_, s := m.Snapshot()
	if s.MaxConcurrent != 7 || s.DownloadDir != filepath.Join(dir, "Downloads", "TurboGet") {
		t.Fatalf("settings didn't come along (downloads stay where they are): %d %s", s.MaxConcurrent, s.DownloadDir)
	}
}

func TestMoveOldStateIntoAnExistingFolder(t *testing.T) {
	dir := tempDirRetry(t) // the list loaded at the end may still be saving
	old := oldTurboGetFolder(t, dir)
	nu := filepath.Join(dir, "Jasqet")
	// Jasqet's window already made its own data, but there's no list yet
	os.MkdirAll(filepath.Join(nu, "WebView2"), 0o755)
	os.WriteFile(filepath.Join(nu, "WebView2", "mine"), []byte("new"), 0o644)
	if err := moveOldState(old, nu); err != nil {
		t.Fatal(err)
	}
	if NewManager(filepath.Join(nu, "state.json")).get("o1") == nil {
		t.Fatal("the download list didn't come along")
	}
	if !fileExists(filepath.Join(nu, "tools", "yt-dlp.exe")) || !fileExists(filepath.Join(nu, "viewer-positions.json")) {
		t.Error("tools or viewer positions didn't come along")
	}
	if readText(filepath.Join(nu, "WebView2", "mine")) != "new" {
		t.Error("Jasqet's own folder was replaced")
	}
	if !fileExists(filepath.Join(old, "WebView2", "Default", "Preferences")) {
		t.Error("what couldn't move should stay in the old folder, not be deleted")
	}
}

func TestMoveOldStateLeavesJasqetsOwnList(t *testing.T) {
	dir := t.TempDir()
	old := oldTurboGetFolder(t, dir)
	nu := filepath.Join(dir, "Jasqet")
	os.MkdirAll(nu, 0o755)
	os.WriteFile(filepath.Join(nu, "state.json"), []byte(`{"downloads":[]}`), 0o644)
	if err := moveOldState(old, nu); err != nil {
		t.Fatal(err)
	}
	if readText(filepath.Join(nu, "state.json")) != `{"downloads":[]}` {
		t.Error("Jasqet's own list was replaced by TurboGet's")
	}
	if !fileExists(filepath.Join(old, "state.json")) || fileExists(filepath.Join(nu, "viewer-positions.json")) {
		t.Error("the old folder should be left alone once Jasqet has a list")
	}
}

func TestMoveOldStateWithoutTurboGet(t *testing.T) {
	dir := t.TempDir()
	nu := filepath.Join(dir, "Jasqet")
	if err := moveOldState(filepath.Join(dir, "TurboGet"), nu); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(nu); !os.IsNotExist(err) {
		t.Error("nothing to move, so no folder should be made")
	}
}

func TestStateFolderIsJasqets(t *testing.T) {
	if filepath.Base(filepath.Dir(stateFilePath())) != "Jasqet" || filepath.Base(oldStateDir()) != "TurboGet" ||
		filepath.Dir(oldStateDir()) != filepath.Dir(filepath.Dir(stateFilePath())) {
		t.Fatalf("state %s, old %s", stateFilePath(), oldStateDir())
	}
	if filepath.Base(defaultDownloadDir()) != "Jasqet" {
		t.Errorf("new downloads go to %s", defaultDownloadDir())
	}
}

// Cookie files a crash left behind go, also the ones from TurboGet's days.
func TestOldCookieFilesAreRemoved(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
	t.Setenv("TMPDIR", dir)
	stale := time.Now().Add(-7 * time.Hour)
	for _, n := range []string{"turboget-cookies-1.txt", "jasqet-cookies-2.txt"} {
		p := filepath.Join(dir, n)
		os.WriteFile(p, []byte("sid=old"), 0o600)
		os.Chtimes(p, stale, stale)
	}
	p := writeCookieFile(map[string]string{"a.example": "sid=new"})
	if p == "" || !strings.HasPrefix(filepath.Base(p), "jasqet-cookies-") {
		t.Fatalf("new cookie file: %q", p)
	}
	defer os.Remove(p)
	for _, n := range []string{"turboget-cookies-1.txt", "jasqet-cookies-2.txt"} {
		if _, err := os.Stat(filepath.Join(dir, n)); !os.IsNotExist(err) {
			t.Errorf("%s was left behind", n)
		}
	}
}
