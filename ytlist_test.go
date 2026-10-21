package main

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// fakeYtDlp: tools that count as set up, and stand-ins for yt-dlp: a video
// answers at once, a list only when release is closed (a Mix can take a minute).
func fakeYtDlp(t *testing.T) (m *Manager, release chan struct{}, lists *atomic.Int32) {
	t.Helper()
	isolateConfig(t)
	m = newTestManager(t)
	os.MkdirAll(toolsDir(), 0o755)
	os.WriteFile(ytdlpPath(), []byte("stand-in"), 0o644)
	os.WriteFile(filepath.Join(toolsDir(), exeName("deno")), []byte("stand-in"), 0o644)
	m.tools.mu.Lock()
	m.tools.lastUpdate = time.Now() // no self-update in a test
	m.tools.mu.Unlock()
	release, lists = make(chan struct{}), &atomic.Int32{}
	oldV, oldL := ytVideoInfo, ytListOf
	ytVideoInfo = func(_ *Manager, _ context.Context, page string, _ map[string]string) (*ytInfo, error) {
		return &ytInfo{Title: "A video", Duration: 60}, nil
	}
	ytListOf = func(_ *Manager, ctx context.Context, page string, _ map[string]string) (*YTPlaylist, error) {
		lists.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &YTPlaylist{Title: "A mix", Count: 3, Entries: make([]ytEntry, 3)}, nil
	}
	ytProbeMu.Lock()
	ytProbeCache = map[string]struct {
		p  *YTProbe
		at time.Time
	}{}
	ytLists = map[string]*ytListJob{}
	ytProbeMu.Unlock()
	t.Cleanup(func() { ytVideoInfo, ytListOf = oldV, oldL })
	return m, release, lists
}

// A video opened from a list (watch?v=…&list=…) answers as soon as its own
// qualities are known: before, it waited for the whole list (52 s for a Mix).
// The list comes with a later answer, read only once.
func TestVideoFromAListDoesntWaitForTheList(t *testing.T) {
	m, release, lists := fakeYtDlp(t)
	page := "https://www.youtube.com/watch?v=abc&list=RDabc&start_radio=1"
	start := time.Now()
	p, err := m.ProbeYT(context.Background(), page, nil)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("the video waited %v for its list", took)
	}
	if p.Title != "A video" || p.Playlist != nil || !p.Listing {
		t.Fatalf("first answer: title %q, playlist %v, loading %v", p.Title, p.Playlist, p.Listing)
	}
	// asked again while the list is still being read: same answer, no second read
	if p2, _ := m.ProbeYT(context.Background(), page, nil); p2.Playlist != nil || !p2.Listing {
		t.Fatal("the list appeared before it was read")
	}
	close(release)
	deadline := time.Now().Add(3 * time.Second)
	var p3 *YTProbe
	for time.Now().Before(deadline) {
		if p3, _ = m.ProbeYT(context.Background(), page, nil); p3.Playlist != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if p3 == nil || p3.Playlist == nil || p3.Playlist.Count != 3 || p3.Listing || p3.Title != "A video" {
		t.Fatalf("after the list was read: %+v", p3)
	}
	if n := lists.Load(); n != 1 {
		t.Fatalf("the list was read %d times, want once", n)
	}
}

// A plain video reads no list; a playlist page still waits for its list (it's
// what you asked for).
func TestPlainVideoAndPlaylistPage(t *testing.T) {
	m, release, lists := fakeYtDlp(t)
	p, err := m.ProbeYT(context.Background(), "https://www.youtube.com/watch?v=plain", nil)
	if err != nil || p.Listing || p.Playlist != nil || lists.Load() != 0 {
		t.Fatalf("plain video: %+v %v, lists read %d", p, err, lists.Load())
	}
	close(release)
	p, err = m.ProbeYT(context.Background(), "https://www.youtube.com/playlist?list=PL123", nil)
	if err != nil || p.Playlist == nil || p.Playlist.Count != 3 || p.Listing {
		t.Fatalf("playlist page: %+v %v", p, err)
	}
}
