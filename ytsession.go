package main

// When YouTube asks "confirm you're not a robot". This happens to whole
// internet addresses, and mobile networks share one address between many
// people, so it comes and goes. Jasqet then:
//  1. tries YouTube's other apps (TV, Android) that are often not asked;
//  2. if you allowed it in the browser extension, uses your browser's own
//     YouTube session (only when YouTube asks, and kept encrypted).

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

var errBotCheck = errors.New("YouTube wants to check you're not a robot (it does this on busy mobile networks)")

// ytSessionKey is the cookie key the extension uses for your YouTube session.
const ytSessionKey = "youtube.com"

type ytSessionState struct {
	mu       sync.Mutex
	cookie   string
	saved    time.Time
	botUntil time.Time // YouTube asked recently: use the session first
}

func (m *Manager) saveYTSession(cookie string) {
	ys := m.ytsess
	ys.mu.Lock()
	changed := ys.cookie != cookie
	ys.cookie, ys.saved = cookie, time.Now()
	ys.mu.Unlock()
	if changed {
		m.saveSoon()
	}
}

func (m *Manager) ytSessionCookie() string {
	ys := m.ytsess
	ys.mu.Lock()
	defer ys.mu.Unlock()
	return ys.cookie
}

// ForgetYTSession removes the saved YouTube session.
func (m *Manager) ForgetYTSession() {
	ys := m.ytsess
	ys.mu.Lock()
	ys.cookie, ys.saved = "", time.Time{}
	ys.mu.Unlock()
	m.mu.Lock()
	for _, d := range m.downloads {
		d.mu.Lock()
		delete(d.Cookies, ytSessionKey)
		d.mu.Unlock()
	}
	m.dirty = true
	m.mu.Unlock()
}

func (m *Manager) YTSessionInfo() (bool, time.Time) {
	ys := m.ytsess
	ys.mu.Lock()
	defer ys.mu.Unlock()
	return ys.cookie != "", ys.saved
}

func withYTSession(cookies map[string]string, s string) map[string]string {
	out := map[string]string{}
	for k, v := range cookies {
		out[k] = v
	}
	out[ytSessionKey] = s
	return out
}

// ytRun runs yt-dlp for a page, getting past YouTube's robot check if it can.
// Your YouTube session is only used once YouTube has asked.
func (m *Manager) ytRun(ctx context.Context, pageURL string, cookies map[string]string, args ...string) ([]byte, error) {
	if !isYouTube(pageURL) {
		return m.runYtDlp(ctx, cookies, args...)
	}
	plain := map[string]string{}
	for k, v := range cookies {
		if k != ytSessionKey {
			plain[k] = v
		}
	}
	session := cookies[ytSessionKey]
	if session != "" {
		m.saveYTSession(session) // you allowed it in the extension: keep it for scheduled downloads
	} else {
		session = m.ytSessionCookie()
	}
	ys := m.ytsess
	ys.mu.Lock()
	recent := time.Now().Before(ys.botUntil)
	ys.mu.Unlock()

	type try struct {
		withSession, otherApps bool
	}
	var tries []try
	switch {
	case recent && session != "":
		tries = []try{{true, false}, {true, true}}
	case recent:
		tries = []try{{false, true}, {false, false}}
	default:
		tries = []try{{false, false}, {false, true}}
		if session != "" {
			tries = append(tries, try{true, false})
		}
	}
	var lastErr error
	for i, t := range tries {
		c := plain
		if t.withSession {
			c = withYTSession(plain, session)
		}
		a := args
		if t.otherApps {
			// YouTube's TV and Android apps are often let through
			a = append([]string{"--extractor-args", "youtube:player_client=tv_simply,tv,android_vr,web_safari"}, args...)
		}
		out, err := m.runYtDlp(ctx, c, a...)
		if err == nil {
			return out, nil
		}
		if !errors.Is(err, errBotCheck) {
			if i == 0 || lastErr == nil {
				return nil, err // a real error (private video, no internet…)
			}
			continue
		}
		lastErr = err
		ys.mu.Lock()
		ys.botUntil = time.Now().Add(2 * time.Hour)
		ys.mu.Unlock()
	}
	return nil, lastErr
}

func isBotCheck(msg string) bool {
	low := strings.ToLower(msg)
	return strings.Contains(low, "not a bot") || strings.Contains(low, "not a robot") || strings.Contains(low, "confirm you")
}
