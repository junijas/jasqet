package main

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
)

// Text pasted into Add with the site helper on must never reach yt-dlp unless
// it's a web address: "--config-locations=…" would be read as an option.
func TestProbeYTTakesOnlyWebAddresses(t *testing.T) {
	m := newTestManager(t)
	for _, bad := range []string{
		"--config-locations=\\\\host\\share\\x.conf", "--plugin-dirs=\\\\host\\share", "-J",
		"file:///C:/Windows/win.ini", `\\host\share\x`, "ftp://example.com/x", "javascript:alert(1)",
		"https://", "youtube.com/watch?v=x",
	} {
		if _, err := m.ProbeYT(context.Background(), bad, nil); err == nil || !strings.Contains(err.Error(), "web address") {
			t.Errorf("%q: want \"isn't a web address\", got %v", bad, err)
		}
	}
	for _, good := range []string{"https://www.youtube.com/watch?v=x", "http://example.com/video"} {
		if !webLink(good) {
			t.Errorf("%q should count as a web address", good)
		}
	}
}

// Every yt-dlp call puts "--" right before the page, so even a link that
// starts with "-" is taken as a link.
func TestYtDlpGetsThePageAfterDoubleDash(t *testing.T) {
	opts := []string{"-J", "--no-playlist"}
	got := pageArgs("--evil", opts...)
	if want := []string{"-J", "--no-playlist", "--", "--evil"}; !slices.Equal(got, want) {
		t.Fatalf("pageArgs = %q, want %q", got, want)
	}
	if !slices.Equal(opts, []string{"-J", "--no-playlist"}) {
		t.Fatalf("pageArgs changed its options: %q", opts)
	}
	// and no call site passes the page without it
	for _, f := range []string{"yt.go", "watch.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "ytRun(") && !strings.Contains(line, "func ") && !strings.Contains(line, "pageArgs(") {
				t.Errorf("%s:%d runs yt-dlp without pageArgs: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}
