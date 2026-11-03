package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// BenchmarkStateRealList: what the window's refresh costs with a real list.
// JASQET_STATE=path\to\state.json (a copy is used, the original isn't touched).
func BenchmarkStateRealList(b *testing.B) {
	src := os.Getenv("JASQET_STATE")
	if src == "" {
		b.Skip("set JASQET_STATE")
	}
	data, err := os.ReadFile(src)
	if err != nil {
		b.Fatal(err)
	}
	dir := b.TempDir()
	p := filepath.Join(dir, "state", "state.json")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, data, 0o644)
	m := NewManager(p)
	s := &Server{m: m, port: "9614", quit: make(chan struct{}), hub: newViewerHub()}
	h := s.routes()
	list, _ := m.Snapshot()
	b.Logf("%d downloads", len(list))
	b.Run("api-state", func(b *testing.B) {
		b.ReportAllocs()
		var n int
		for b.Loop() {
			r := httptest.NewRequest("GET", "http://127.0.0.1:9614/api/state", nil)
			r.Header.Set(keyHeader, s.key.window)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			n = w.Body.Len()
		}
		b.ReportMetric(float64(n), "bytes/answer")
	})
	b.Run("tick-walk", func(b *testing.B) { // what the twice-a-second loop does before its own checks
		b.ReportAllocs()
		for b.Loop() {
			m.mu.Lock()
			l := m.sortedLocked()
			m.mu.Unlock()
			for _, d := range l {
				d.mu.Lock()
				_ = d.downloadedLocked()
				d.mu.Unlock()
			}
		}
	})
	b.Run("summary", func(b *testing.B) { // the tray's tooltip, every 2 s
		b.ReportAllocs()
		for b.Loop() {
			m.Summary()
		}
	})
}

// TestListOrderSettlesTies: downloads added at the same moment keep one
// order, so the window doesn't move (and redraw) rows on every refresh.
func TestListOrderSettlesTies(t *testing.T) {
	m := newTestManager(t)
	at := time.Now()
	m.mu.Lock()
	for _, id := range []string{"c", "a", "e", "b", "d"} {
		m.downloads[id] = &Download{ID: id, AddedAt: at, Status: StDone}
	}
	m.mu.Unlock()
	first, _ := m.Snapshot()
	for range 50 {
		again, _ := m.Snapshot()
		for i := range first {
			if again[i].ID != first[i].ID {
				t.Fatalf("order changed between refreshes: %v then %v", ids(first), ids(again))
			}
		}
	}
	if got := ids(first); got != "abcde" {
		t.Fatalf("order = %s", got)
	}
}

func ids(l []DownloadView) string {
	s := ""
	for _, v := range l {
		s += v.ID
	}
	return s
}

// TestStateSaysSameWhenNothingChanged: the window's regular check gets an
// empty "the same" while nothing changes, and the whole list once it does.
func TestStateSaysSameWhenNothingChanged(t *testing.T) {
	s, h := newTestServer(t)
	const app = "http://127.0.0.1:9614"
	w := call(h, "GET", "/api/state", app, "", keyHeader, s.key.window)
	tag := w.Header().Get("X-Jasqet-State")
	if w.Code != 200 || tag == "" || w.Body.Len() == 0 {
		t.Fatalf("first: %d tag=%q", w.Code, tag)
	}
	if w := call(h, "GET", "/api/state", app, "", keyHeader, s.key.window, "X-Jasqet-Seen", tag); w.Code != 204 || w.Body.Len() != 0 {
		t.Fatalf("nothing changed: %d, %d bytes", w.Code, w.Body.Len())
	}
	if w := call(h, "GET", "/api/state", app, "", keyHeader, s.key.window, "X-Jasqet-Seen", "old"); w.Code != 200 {
		t.Fatalf("an older list: %d", w.Code)
	}
	s.m.mu.Lock()
	s.m.downloads["x"] = &Download{ID: "x", Status: StDone, FileName: "a.zip", AddedAt: time.Now()}
	s.m.mu.Unlock()
	if w := call(h, "GET", "/api/state", app, "", keyHeader, s.key.window, "X-Jasqet-Seen", tag); w.Code != 200 || w.Header().Get("X-Jasqet-State") == tag {
		t.Fatalf("after a change: %d", w.Code)
	}
	if w := call(h, "GET", "/api/state", app, "", "X-Jasqet-Seen", tag); w.Code == 204 {
		t.Fatal("answered without the window's key")
	}
}
