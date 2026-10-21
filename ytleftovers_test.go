package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Cookie files a crash left in the temp folder hold logins: the sweep at
// start-up removes the ones older than 10 minutes and keeps newer ones.
func TestStartUpSweepRemovesOldCookieFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMP", dir) // where os.TempDir looks on Windows
	t.Setenv("TEMP", dir)
	t.Setenv("TMPDIR", dir) // and on Linux and Mac
	if os.TempDir() != dir {
		t.Fatalf("the temp folder is %s, want %s", os.TempDir(), dir)
	}
	old := time.Now().Add(-20 * time.Minute)
	files := map[string]bool{ // name: should be gone
		"jasqet-cookies-111.txt":   true,
		"turboget-cookies-222.txt": true,
		"jasqet-cookies-333.txt":   false, // just made
		"other-file.txt":           false, // not one of ours
	}
	for name := range files {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte("# Netscape HTTP Cookie File\n"), 0o600)
		if name != "jasqet-cookies-333.txt" {
			os.Chtimes(p, old, old)
		}
	}
	sweepCookieFiles(10 * time.Minute)
	for name, gone := range files {
		if _, err := os.Stat(filepath.Join(dir, name)); (err != nil) != gone {
			t.Errorf("%s: gone = %v, want %v", name, err != nil, gone)
		}
	}
	// and Jasqet does it when it starts
	b, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "sweepCookieFiles(10 * time.Minute)") {
		t.Fatal("main.go doesn't sweep the cookie files at start-up")
	}
}

// Other people on the same computer can see a program's command line, so
// the proxy's password goes to yt-dlp in its environment only.
func TestYtDlpGetsTheProxyPasswordOnlyInItsEnvironment(t *testing.T) {
	m := newTestManager(t)
	for _, c := range []struct{ setting, want string }{
		{"user:s3cret@proxy.example:8080", "http://user:s3cret@proxy.example:8080"},
		{"socks5://user:s3cret@proxy.example:1080", "socks5://user:s3cret@proxy.example:1080"},
	} {
		m.mu.Lock()
		m.settings.Proxy = c.setting
		m.mu.Unlock()
		cmd := m.ytdlpCmd(context.Background(), append(m.ytdlpArgs("cookies.txt"), pageArgs("https://www.youtube.com/watch?v=x", "-J")...)...)
		for _, a := range cmd.Args {
			if strings.Contains(a, "s3cret") || a == "--proxy" {
				t.Fatalf("the proxy is on the command line: %q", cmd.Args)
			}
		}
		if !slices.Contains(cmd.Args, "--ignore-config") {
			t.Fatalf("--ignore-config is missing: %q", cmd.Args)
		}
		for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY"} {
			if !slices.Contains(cmd.Env, k+"="+c.want) {
				t.Fatalf("%s=%s isn't in yt-dlp's environment", k, c.want)
			}
		}
	}
	// no proxy set: yt-dlp gets the computer's own environment, as before
	m.mu.Lock()
	m.settings.Proxy = ""
	m.mu.Unlock()
	if cmd := m.ytdlpCmd(context.Background(), "-U"); cmd.Env != nil {
		t.Fatal("without a proxy the environment should be left alone")
	}
	// and every place that starts yt-dlp goes through ytdlpCmd
	starts := 0
	for _, f := range []string{"yt.go", "tools.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, `"--proxy"`) {
				t.Errorf("%s:%d passes --proxy: %s", f, i+1, strings.TrimSpace(line))
			}
			if strings.Contains(line, "exec.Command") && strings.Contains(line, "ytdlpPath()") {
				starts++
			}
		}
	}
	if starts != 1 {
		t.Fatalf("yt-dlp is started in %d places, want only in ytdlpCmd", starts)
	}
}

// A page on a look-alike host must not get your saved YouTube session.
func TestLookAlikeHostIsNotYouTube(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://www.youtube.com/watch?v=x":         true,
		"https://youtube.com/watch?v=x":             true,
		"https://youtu.be/x":                        true,
		"https://www.youtube-nocookie.com/embed/x":  true,
		"https://YouTube-NoCookie.com/embed/x":      true,
		"https://evil-youtube-nocookie.com/embed/x": false,
		"https://www.evil-youtube-nocookie.com/x":   false,
		"https://evilyoutube-nocookie.com/embed/x":  false,
		"https://youtube-nocookie.com.evil.test/x":  false,
		"https://evil-youtube.com/watch?v=x":        false,
		"https://evil.test/youtube-nocookie.com/x":  false,
	} {
		if got := isYouTube(raw); got != want {
			t.Errorf("isYouTube(%q) = %v, want %v", raw, got, want)
		}
	}
	// cookies for a look-alike of Google aren't left out as Google's are
	dir := t.TempDir()
	t.Setenv("TMP", dir)
	t.Setenv("TEMP", dir)
	t.Setenv("TMPDIR", dir)
	cf := writeCookieFile(map[string]string{"evilgoogle.com": "a=1", "accounts.google.com": "b=2"})
	defer os.Remove(cf)
	b, _ := os.ReadFile(cf)
	if !strings.Contains(string(b), "evilgoogle.com\t") || strings.Contains(string(b), "accounts.google.com") {
		t.Fatalf("cookie file:\n%s", b)
	}
}
