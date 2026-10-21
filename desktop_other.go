//go:build !windows

package main

// On Mac and Linux Jasqet opens in a browser app window (no tray icon yet).
// Each window's address carries a one-time code for the page's key to the
// API (apikey.go).

type desktop struct {
	url string
	key *apiKey
}

func startDesktop(appURL string, m *Manager, quit func(), showWindow bool, key *apiKey) *desktop {
	d := &desktop{url: appURL, key: key}
	if showWindow {
		d.Show()
	}
	return d
}

func (d *desktop) Show() { openBrowser(d.key.browserURL(d.url)) }

func allowSetForeground() {}
