package main

// Helping you connect the browser extension: find installed browsers, open
// their extensions page, show the extension folder, and notice when the
// extension checks in.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type browserInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Found   bool   `json:"found"`
	Firefox bool   `json:"firefox,omitempty"`
	// Its extensions page. Chrome, Edge and the like don't open their own pages
	// for other programs (they show a new tab instead), so the window copies
	// this for you to paste.
	Page string `json:"page"`
	exe  string
}

func knownBrowsers() []browserInfo {
	pf, pf86, local := os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LocalAppData")
	list := []struct {
		id, name, page string
		paths          []string
	}{
		{"chrome", "Google Chrome", "chrome://extensions/", []string{pf + `\Google\Chrome\Application\chrome.exe`, pf86 + `\Google\Chrome\Application\chrome.exe`, local + `\Google\Chrome\Application\chrome.exe`}},
		{"edge", "Microsoft Edge", "edge://extensions/", []string{pf86 + `\Microsoft\Edge\Application\msedge.exe`, pf + `\Microsoft\Edge\Application\msedge.exe`}},
		{"brave", "Brave", "brave://extensions/", []string{pf + `\BraveSoftware\Brave-Browser\Application\brave.exe`, local + `\BraveSoftware\Brave-Browser\Application\brave.exe`}},
		{"opera", "Opera", "opera://extensions/", []string{local + `\Programs\Opera\opera.exe`, local + `\Programs\Opera GX\opera.exe`}},
		{"vivaldi", "Vivaldi", "vivaldi://extensions/", []string{local + `\Vivaldi\Application\vivaldi.exe`, pf + `\Vivaldi\Application\vivaldi.exe`}},
		{"firefox", "Firefox", "about:debugging#/runtime/this-firefox", []string{pf + `\Mozilla Firefox\firefox.exe`, pf86 + `\Mozilla Firefox\firefox.exe`}},
	}
	var out []browserInfo
	for _, b := range list {
		bi := browserInfo{ID: b.id, Name: b.name, Page: b.page, Firefox: b.id == "firefox"}
		if runtime.GOOS == "windows" {
			for _, p := range b.paths {
				if fileExists(p) {
					bi.Found, bi.exe = true, p
					break
				}
			}
		}
		out = append(out, bi)
	}
	return out
}

func openBrowserExtensions(id string) bool {
	for _, b := range knownBrowsers() {
		if b.ID == id && b.Found {
			return exec.Command(b.exe, b.Page).Start() == nil
		}
	}
	return false
}

// extensionDir finds the extension folder shipped next to Jasqet.exe.
func extensionDir(firefox bool) string {
	name := "extension"
	if firefox {
		name = "extension-firefox"
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	p := filepath.Join(filepath.Dir(exe), name)
	if st, err := os.Stat(p); err == nil && st.IsDir() {
		return p
	}
	return ""
}

// ---------- has the extension checked in? ----------

type extSeen struct {
	mu      sync.Mutex
	at      time.Time
	browser string
}

var extStatus extSeen

func noteExtension(origin, ua string) {
	b := browserName(origin, ua)
	extStatus.mu.Lock()
	extStatus.at, extStatus.browser = time.Now(), b
	extStatus.mu.Unlock()
}

func browserName(origin, ua string) string {
	switch {
	case strings.HasPrefix(origin, "moz-extension://"):
		return "Firefox"
	case strings.Contains(ua, "Edg/"):
		return "Microsoft Edge"
	case strings.Contains(ua, "OPR/"):
		return "Opera"
	case strings.Contains(ua, "Vivaldi"):
		return "Vivaldi"
	case strings.Contains(ua, "Chrome/"):
		return "Chrome (or Brave)"
	}
	return "Chrome-based browser"
}

func extensionSeen() (bool, string) {
	extStatus.mu.Lock()
	defer extStatus.mu.Unlock()
	return !extStatus.at.IsZero(), extStatus.browser
}

// extSeenSaved: when the extension last checked in. It's kept with the
// download list, so after a restart the window doesn't say "not connected"
// until the browser's extension next wakes up.
type extSeenSaved struct {
	At      time.Time `json:"at,omitzero"`
	Browser string    `json:"browser,omitempty"`
}

func seenSnapshot() extSeenSaved {
	extStatus.mu.Lock()
	defer extStatus.mu.Unlock()
	return extSeenSaved{extStatus.at, extStatus.browser}
}

func restoreSeen(s extSeenSaved) {
	extStatus.mu.Lock()
	defer extStatus.mu.Unlock()
	if s.At.After(extStatus.at) {
		extStatus.at, extStatus.browser = s.At, s.Browser
	}
}
