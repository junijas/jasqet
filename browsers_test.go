package main

import (
	"strings"
	"testing"
	"time"
)

func TestExtensionCheckInIsRememberedAfterARestart(t *testing.T) {
	forget := func() {
		extStatus.mu.Lock()
		extStatus.at, extStatus.browser = time.Time{}, ""
		extStatus.mu.Unlock()
	}
	old := seenSnapshot()
	defer func() { forget(); restoreSeen(old) }()
	forget()
	m := newTestManager(t)
	noteExtension("chrome-extension://abcdefghijklmnopabcdefghijklmnop", "Mozilla/5.0 (Windows NT 10.0) Chrome/155.0 Safari/537.36 Edg/155.0")
	m.save()
	forget() // Jasqet restarts…
	NewManager(m.stateFile)
	if seen, browser := extensionSeen(); !seen || browser != "Microsoft Edge" {
		t.Fatalf("after a restart: seen %v, %q", seen, browser)
	}
}

func TestBrowsersSayWhichPageToPaste(t *testing.T) {
	for _, b := range knownBrowsers() {
		want := b.ID + "://extensions/"
		if b.Firefox {
			want = "about:debugging"
		}
		if !strings.HasPrefix(b.Page, want) {
			t.Errorf("%s: page %q", b.Name, b.Page)
		}
	}
}
