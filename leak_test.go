package main

import (
	"bytes"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// TestNoLeaksAfterManyDownloads: downloads, pauses, resumes and removes many
// files, then checks the goroutines and memory go back to where they started.
// A leak there grows with every download while Jasqet runs for weeks.
func TestNoLeaksAfterManyDownloads(t *testing.T) {
	if testing.Short() {
		t.Skip("long")
	}
	body := make([]byte, 3<<20)
	rand.Read(body)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Millisecond)
		http.ServeContent(w, r, "f.bin", time.Time{}, bytes.NewReader(body))
	}))
	defer srv.Close()
	m := newTestManager(t)
	_, st := m.Snapshot()
	st.MaxConcurrent = 4
	m.SetSettings(st)

	round := func(n, from int) {
		var ids []string
		for i := range n {
			got, err := m.Add(AddRequest{URL: srv.URL + "/f" + strconv.Itoa(from+i) + ".bin"})
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, got...)
		}
		// pause and resume a few on the way, as people do
		time.Sleep(30 * time.Millisecond)
		for _, id := range ids[:n/4] {
			m.Pause(m.get(id))
		}
		time.Sleep(30 * time.Millisecond)
		for _, id := range ids[:n/4] {
			m.Resume(m.get(id))
		}
		for _, id := range ids {
			for end := time.Now().Add(60 * time.Second); ; time.Sleep(20 * time.Millisecond) {
				d := m.get(id)
				d.mu.Lock()
				st, running, note, e := d.Status, d.running, d.Note, d.Error
				d.mu.Unlock()
				if st == StDone && !running {
					break
				}
				if time.Now().After(end) || st == StError {
					m.mu.Lock()
					holds, rates := m.holds, m.rates
					m.mu.Unlock()
					t.Fatalf("a download is stuck: %s running=%v note=%q error=%q holds=%v limits=%v", st, running, note, e, holds, rates)
				}
			}
		}
		for _, id := range ids {
			m.Delete(m.get(id), true)
		}
	}
	settle := func() (int, uint64) {
		var ms runtime.MemStats
		for range 3 { // finished downloads' checks, idle connections closing
			time.Sleep(700 * time.Millisecond)
			srv.CloseClientConnections()
			runtime.GC()
		}
		runtime.ReadMemStats(&ms)
		return runtime.NumGoroutine(), ms.HeapAlloc
	}

	round(8, 0) // warm up: pools, the HTTP client, the loop
	g0, h0 := settle()
	var g1 int
	var h1 uint64
	for r := range 3 { // three rounds: a leak keeps growing, a pool filling up doesn't
		round(40, 100+100*r)
		g1, h1 = settle()
		t.Logf("after %d more downloads: goroutines %d → %d, heap %.1f MB → %.1f MB", 40*(r+1), g0, g1, float64(h0)/1e6, float64(h1)/1e6)
	}
	if g1 > g0+3 {
		buf := make([]byte, 1<<20)
		t.Fatalf("goroutines grew from %d to %d after 40 downloads:\n%s", g0, g1, buf[:runtime.Stack(buf, true)])
	}
	if h1 > h0+8<<20 {
		t.Fatalf("memory grew by %.1f MB after 120 downloads", float64(h1-h0)/1e6)
	}
}

// TestResumeWhileStillPausing: Resume pressed while a download is still
// stopping from Pause used to be ignored, and it stayed paused.
func TestResumeWhileStillPausing(t *testing.T) {
	body := make([]byte, 4<<20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond) // slow to answer: Pause and Resume land while it's starting
		http.ServeContent(w, r, "f.bin", time.Time{}, bytes.NewReader(body))
	}))
	defer srv.Close()
	m := newTestManager(t)
	for i := range 5 {
		ids, err := m.Add(AddRequest{URL: srv.URL + "/p" + strconv.Itoa(i) + ".bin"})
		if err != nil {
			t.Fatal(err)
		}
		d := m.get(ids[0])
		for end := time.Now().Add(5 * time.Second); ; time.Sleep(time.Millisecond) {
			d.mu.Lock()
			r := d.running
			d.mu.Unlock()
			if r || time.Now().After(end) {
				break
			}
		}
		m.Pause(d)
		m.Resume(d) // at once: it hasn't stopped yet
		waitStatus(t, m, ids[0], StDone, 30*time.Second)
	}
}
