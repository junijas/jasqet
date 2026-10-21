package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBlockReason(t *testing.T) {
	exe := append([]byte("MZ\x90\x00"), make([]byte, 200)...)
	zip := append([]byte("PK\x03\x04"), make([]byte, 100)...)
	html := []byte("<!DOCTYPE html><html><body>Sign in</body></html>")
	script := []byte("@echo off\r\npowershell -c \"Start-Process a.exe\"\r\n")
	mp4 := append([]byte{0, 0, 0, 24}, []byte("ftypisom")...)
	pdf := []byte("%PDF-1.7\n%\xe2\xe3\xcf\xd3\n1 0 obj")
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`)
	ts := []byte("export function hello(): string { return 'hi' }\n")
	cases := []struct {
		name    string
		head    []byte
		blocked bool
	}{
		{"movie.mp4", exe, true},      // a program dressed as a video
		{"song.mp3", zip, true},       // an archive dressed as music
		{"report.pdf", html, true},    // a login page saved as a PDF
		{"invoice.pdf", script, true}, // a script dressed as a PDF
		{"movie.mp4.exe", exe, true},  // the double-extension trick
		{"photo.jpg.scr", exe, true},  //
		{"report.docx", exe, true},    // a program dressed as a document
		{"clip.mp4", mp4, false},      // the real thing
		{"paper.pdf", pdf, false},     //
		{"logo.svg", svg, false},      // SVGs are shown locked down, not blocked
		{"app.ts", ts, false},         // TypeScript, not a video: not blocked, not viewed
		{"movie.ts", exe, true},       // a program named .ts is still a program
		{"setup.exe", exe, false},     // an honest program: Open asks before running it
		{"archive.zip", zip, false},   // an honest archive goes to the default program
		{"notes.txt", []byte("hello"), false},
	}
	for _, c := range cases {
		reason, _ := blockReason(c.name, c.head)
		if (reason != "") != c.blocked {
			t.Errorf("%s: blocked=%v (%q), want %v", c.name, reason != "", reason, c.blocked)
		}
	}
}

func TestIsProgram(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "setup.exe")
	os.WriteFile(exe, append([]byte("MZ"), make([]byte, 100)...), 0o644)
	bat := filepath.Join(dir, "run.bat")
	os.WriteFile(bat, []byte("@echo off"), 0o644)
	doc := filepath.Join(dir, "a.docx")
	os.WriteFile(doc, []byte("PK\x03\x04"), 0o644)
	if !isProgram(exe, "setup.exe") || !isProgram(bat, "run.bat") || isProgram(doc, "a.docx") {
		t.Fatal("isProgram wrong")
	}
}

func TestEngineFor(t *testing.T) {
	want := map[string]string{"a.mp4": "native", "a.MKV": "native", "a.webm": "native", "a.mov": "native", "a.m4v": "native",
		"a.ts": "remux", "a.flv": "remux", "a.avi": "", "a.mp3": "native", "a.flac": "native", "a.jpg": "native",
		"a.svg": "native", "a.pdf": "native", "a.docx": "", "a.zip": "", "a.heic": ""}
	for n, w := range want {
		if g := engineFor(n); g != w {
			t.Errorf("%s: %q want %q", n, g, w)
		}
	}
}

func TestNaturalLess(t *testing.T) {
	names := []string{"Episode 10.mp4", "Episode 2.mp4", "episode 1.mp4", "Episode 02b.mp4", "a.mp4"}
	sort.Slice(names, func(i, j int) bool { return naturalLess(names[i], names[j]) })
	got := strings.Join(names, "|")
	if got != "a.mp4|episode 1.mp4|Episode 2.mp4|Episode 02b.mp4|Episode 10.mp4" {
		t.Fatal(got)
	}
}

// In preview mode the connections line up from the start of the file, and an
// MP4 index at the end is fetched first.
func TestSequentialClaim(t *testing.T) {
	const size = 100 << 20
	m := &Manager{}
	d := &Download{Resumable: true, Size: size}
	// four connections spread over the file, as normal downloading leaves them
	for i := 0; i < 4; i++ {
		s := &Segment{Start: int64(i) * size / 4, End: int64(i+1)*size/4 - 1, active: true}
		s.Pos = s.Start + 1<<20
		d.Segments = append(d.Segments, s)
	}
	// one player reads the start, another asks for the MP4 index at the end
	tail := int64(size - 64<<10)
	m.previewAt(d, "start", 1<<20, true)
	m.previewAt(d, "index", tail, true)
	far := d.Segments[2]
	if !d.shouldYieldLocked(far) {
		t.Fatal("a far connection should make way while the index is missing")
	}
	m.previewAt(d, "start", 1<<20, true) // the start keeps asking: it mustn't crowd out the index
	s := d.claim(false)
	if s == nil || s.Start != tail {
		t.Fatalf("first claim should fetch the index exactly where it was asked for, got %+v", s)
	}
	s.Pos = s.End + 1 // index done
	s.active = false
	m.previewDone(d, "index")
	// next: connections line up right after the playing point
	s = d.claim(false)
	if s == nil || s.Start != 1<<20+previewSplit {
		t.Fatalf("second claim should start just past the first connection, got %+v", s)
	}
	s2 := d.claim(false)
	if s2 == nil || s2.Start != s.Start+previewSplit {
		t.Fatalf("third claim should line up behind the second, got %+v", s2)
	}
	if !d.rangeDoneLocked(tail, size) {
		t.Fatal("tail should be done")
	}
	// the player jumps to the middle: a connection starts right there…
	m.previewDone(d, "start")
	m.previewAt(d, "jump", 60<<20, true)
	s = d.claim(false)
	if s == nil || s.Start != 60<<20 {
		t.Fatalf("jump: got %+v", s)
	}
	// …and as it plays on from there, the connections follow it, not the start
	s.Pos += 512 << 10
	m.previewAt(d, "jump", s.Pos, true)
	if !d.shouldYieldLocked(d.Segments[0]) {
		t.Fatal("the connection at the start should move to where the player is now")
	}
	s3 := d.claim(false)
	if s3 == nil || s3.Start < 60<<20 || s3.Start > 61<<20 {
		t.Fatalf("help should go to the new playing point, got %+v", s3)
	}
	// not watching: normal rules
	d.previewUntil = time.Time{}
	if d.shouldYieldLocked(far) {
		t.Fatal("no yielding outside preview mode")
	}
}

func TestCompactAndFrontier(t *testing.T) {
	d := &Download{}
	var pos int64
	for i := 0; i < 100; i++ {
		s := &Segment{Start: pos, End: pos + 99, Pos: pos + 100}
		if i == 70 {
			s.Pos = s.Start + 10 // a gap here
		}
		d.Segments = append(d.Segments, s)
		pos += 100
	}
	// shuffle the order a bit
	d.Segments[3], d.Segments[50] = d.Segments[50], d.Segments[3]
	if f := d.frontierLocked(); f != 70*100+10 {
		t.Fatalf("frontier %d", f)
	}
	d.compactLocked()
	if len(d.Segments) > 3 {
		t.Fatalf("not compacted: %d pieces", len(d.Segments))
	}
	if f := d.frontierLocked(); f != 70*100+10 {
		t.Fatalf("frontier after compact %d", f)
	}
	var total int64
	for _, s := range d.Segments {
		total += s.End - s.Start + 1
	}
	if total != 100*100 {
		t.Fatalf("compact lost bytes: %d", total)
	}
	// a finished download is saved as one piece covering the whole file, even
	// though its pieces were added out of order
	one := compactSegments(d.Segments)
	if len(one) != 1 || one[0].End != 100*100-1 {
		t.Fatalf("finished pieces compacted to %+v", one[0])
	}
}

// The part being watched is fetched first; with every connection busy, the
// one furthest from the playhead moves over (from 1.5.0).
func TestPreviewFetchesThePartBeingWatched(t *testing.T) {
	const MB = 1 << 20
	m := &Manager{}
	a := &Segment{Start: 0, End: 5*MB - 1, Pos: MB, active: true, rate: 256 << 10}
	b := &Segment{Start: 5 * MB, End: 10*MB - 1, Pos: 6 * MB, active: true, rate: 256 << 10}
	stopped := false
	a.stop = func() { stopped = true }
	d := &Download{Resumable: true, Size: 10 * MB, Segments: []*Segment{a, b}}
	m.previewWant(d, 7*MB)
	if b.End != 7*MB-1 {
		t.Fatalf("the piece holding the playhead wasn't cut there: %+v", b)
	}
	var cut *Segment
	for _, s := range d.Segments {
		if s.Start == 7*MB {
			cut = s
		}
	}
	if cut == nil || cut.active {
		t.Fatal("no free piece starting at the playhead")
	}
	if !stopped || !a.yield {
		t.Fatal("with every connection busy, the one furthest from the playhead should move over")
	}
	a.active = false
	if got := d.claim(false); got != cut {
		t.Fatalf("claimed %+v, want the piece at the playhead", got)
	}
	before := len(d.Segments)
	m.previewWant(d, 7*MB+100<<10)
	if len(d.Segments) != before {
		t.Fatal("cut again for a point a connection is about to reach")
	}
	if n := m.previewAvailable(d, MB-10); n != 10 {
		t.Fatalf("available just before a piece's edge: %d, want 10", n)
	}
	if n := m.previewAvailable(d, 9*MB); n != 0 {
		t.Fatalf("available where nothing has arrived: %d", n)
	}
}

// Only a player that is reading switches the download to the viewer's order:
// the viewer merely being open on it (previewUntil refreshed) doesn't.
func TestSequentialNeedsAPlayer(t *testing.T) {
	d := &Download{Resumable: true, Size: 100 << 20, previewUntil: time.Now().Add(20 * time.Second)}
	if d.sequentialLocked() {
		t.Fatal("no player is reading")
	}
	m := &Manager{}
	m.previewAt(d, "p", 0, false)
	if !d.sequentialLocked() {
		t.Fatal("a player is reading")
	}
	d.previewHeads["p"].at = time.Now().Add(-time.Minute) // paused for a minute
	if d.sequentialLocked() {
		t.Fatal("a paused player shouldn't keep the download in the viewer's order")
	}
}

// Connections line up where the one in front's request ends (so its answer
// is read to the end and its connection kept), and not at all with a speed
// limit or over FTP.
func TestLineUpAtRequestEnd(t *testing.T) {
	const MB = 1 << 20
	m := &Manager{}
	a := &Segment{Start: 0, End: 50*MB - 1, Pos: MB, active: true, reqTo: MB + 300<<10}
	d := &Download{Resumable: true, Size: 50 * MB, Segments: []*Segment{a}}
	m.previewAt(d, "p", MB, true)
	d.noLineUp = true // what claim(true) sets: a speed limit or FTP
	if d.sequentialClaimLocked() != nil {
		t.Fatal("with a speed limit nothing is shared out behind the player")
	}
	s := d.claim(false)
	if s == nil || s.Start != MB+300<<10 || a.End != MB+300<<10-1 {
		t.Fatalf("should start where the request ends: %+v / %+v", s, a)
	}
	// the new one hasn't asked yet: its first request will be one step long
	s2 := d.claim(false)
	if s2 == nil || s2.Start != s.Start+previewSplit {
		t.Fatalf("third: %+v", s2)
	}
	// a far connection moves over only when there's near work it would get
	far := &Segment{Start: 40 * MB, End: 50*MB - 1, Pos: 41 * MB, active: true}
	d.Segments = append(d.Segments, far)
	s2.End = 40*MB - 1
	if !d.shouldYieldLocked(far) {
		t.Fatal("near work: the far connection should move over")
	}
	d.noLineUp = true
	if d.shouldYieldLocked(far) {
		t.Fatal("with a speed limit there's no near work it would get: it shouldn't move")
	}
}

// An MP4's index at the end of the file is fetched by several connections in
// steps of about a second, not by one: at 25 KB/s per connection a 190 KB
// index was one 128 KB step plus 62 KB, too little to share, and took 7 s.
// A player reading the start meanwhile keeps its usual steps.
func TestLineUpAcrossTheIndexAtTheEnd(t *testing.T) {
	const KB, MB = 1 << 10, 1 << 20
	const size = 32 * MB
	ts := &testServer{data: make([]byte, size)}
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		asked = append(asked, r.Header.Get("Range"))
		mu.Unlock()
		ts.ServeHTTP(w, r)
	}))
	defer srv.Close()
	m := newTestManager(t)
	d := &Download{URL: srv.URL + "/file.bin", Resumable: true, Size: size, Connections: 16, connRate: 25 * KB}
	// 16 connections a fifth into their pieces, each asking to its piece's end
	for i := int64(0); i < 16; i++ {
		s := &Segment{Start: i * size / 16, End: (i+1)*size/16 - 1, active: true, rate: 25 * KB, stop: func() {}}
		s.Pos, s.reqTo = s.Start+size/80, s.End+1
		d.Segments = append(d.Segments, s)
	}
	// the player read the start, then asks for the index at the end
	index := int64(size - 190*KB)
	m.previewAt(d, "start", KB, false)
	m.previewAt(d, "index", index, true)
	first := d.claim(false)
	if first == nil || first.Start != index {
		t.Fatalf("the index should get a connection exactly where it starts: %+v", first)
	}
	// it asks for about a second's worth (not 128 KB); the answer ending there
	// isn't a dropped connection (that cost a 1 s wait before the next step)
	f, _ := os.CreateTemp(t.TempDir(), "p")
	defer f.Close()
	if _, err := m.fetch(context.Background(), d, first, f); err != nil {
		t.Fatalf("a whole answer to one step: %v", err)
	}
	mu.Lock()
	sent := append([]string(nil), asked...)
	mu.Unlock()
	if want := fmt.Sprintf("bytes=%d-%d", index, index+25*KB-1); len(sent) != 1 || sent[0] != want {
		t.Fatalf("asked %v, want %s", sent, want)
	}
	d.connRate = 25 * KB                     // (a slow moment in that fetch may have measured it)
	m.previewAt(d, "index", first.Pos, true) // the player read it and waits for more
	// the connection told to make way leaves its piece; others move over too
	for _, s := range d.Segments {
		if s.yield {
			s.active, s.yield = false, false
		}
	}
	var got []*Segment
	for i := 0; i < 8; i++ {
		if s := d.claim(false); s != nil {
			got = append(got, s)
		}
	}
	tail := 0
	for _, s := range d.Segments {
		if s.End < index || s.done() {
			continue
		}
		if !s.active {
			t.Errorf("part of the index has no connection: %+v", s)
		}
		tail++
		if s.remaining() > 25*KB {
			t.Errorf("a piece of the index is more than a second's worth: %+v", s)
		}
	}
	if tail < 6 {
		t.Fatalf("the index is fetched by %d connections, want several", tail)
	}
	start := d.Segments[0].Pos
	usual := false
	for _, s := range got {
		usual = usual || s.Start == start+previewSplit
	}
	if !usual {
		t.Fatal("the start player's connections should still line up in the usual steps")
	}
	// the index is read: steps at the end are the usual ones again
	m.previewDone(d, "index")
	if c := d.previewStepLocked(size - KB); c != previewSplit {
		t.Fatalf("step at the end after the index was read: %d", c)
	}
}

// A connection that went silent right where the player waits is taken over
// at once, not after the stall timeout; a piece a connection gave up on is
// tried again (a few times) when the player needs it.
func TestPreviewTakesOverAtThePlayhead(t *testing.T) {
	const MB = 1 << 20
	m := &Manager{}
	stopped := false
	a := &Segment{Start: 0, End: 10*MB - 1, Pos: 2 * MB, active: true,
		began: time.Now().Add(-10 * time.Second), lastData: time.Now().Add(-5 * time.Second)}
	a.stop = func() { stopped = true }
	d := &Download{Resumable: true, Size: 20 * MB, Segments: []*Segment{a,
		{Start: 10 * MB, End: 20*MB - 1, Pos: 15 * MB, failed: true}}}
	m.previewAt(d, "p", 2*MB, true)
	s := d.claim(false)
	if !stopped || s == nil || s.Start != 2*MB || s.handovers != 1 || !a.done() {
		t.Fatalf("silent connection at the playhead not taken over: %+v", s)
	}
	m.previewAt(d, "p", 15*MB, true)
	s = d.claim(false)
	if s == nil || s.Start != 15*MB || s.failed {
		t.Fatalf("the failed piece the player needs should be tried again: %+v", s)
	}
	s.active, s.failed = false, true // failed again…
	s.handovers = 3                  // …a few times: leave it now
	m.previewAt(d, "p", 15*MB, true)
	if got := d.claim(false); got != nil && got.Start == 15*MB {
		t.Fatal("tried a piece the server keeps refusing again")
	}
}

func TestViewerSettingsNormalize(t *testing.T) {
	v := ViewerSettings{OpenWith: "nonsense", Video: "default", HWDecode: ""}
	v.normalize()
	if v.OpenWith != "viewer" || v.Video != "default" || v.Audio != "viewer" || v.HWDecode != "auto" {
		t.Fatalf("%+v", v)
	}
}

func TestSubtitleFiles(t *testing.T) {
	dir := t.TempDir()
	ass := "[Script Info]\nTitle: x\n\n[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n" +
		"Dialogue: 0,0:00:01.50,0:00:03.00,Default,,0,0,0,,{\\i1}Hello{\\i0}\\Nworld, again\n"
	os.WriteFile(filepath.Join(dir, "film.ass"), []byte(ass), 0o644)
	os.WriteFile(filepath.Join(dir, "film.en.srt"), []byte("\xef\xbb\xbf1\r\n00:00:01,000 --> 00:00:02,500\r\nHi <b>there</b>\r\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "film.mkv"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, "other.srt"), nil, 0o644)
	files := subtitleFilesFor(filepath.Join(dir, "film.mkv"))
	if len(files) != 2 {
		t.Fatalf("found %v", files)
	}
	for _, f := range files {
		cues, err := parseSubtitleFile(f)
		if err != nil || len(cues) != 1 {
			t.Fatalf("%s: %v %v", f, cues, err)
		}
		vtt := toVTT(cues)
		if !strings.HasPrefix(vtt, "WEBVTT") || !strings.Contains(vtt, "-->") {
			t.Fatalf("bad vtt %q", vtt)
		}
		if strings.HasSuffix(f, ".ass") && (!strings.Contains(vtt, "Hello") || !strings.Contains(vtt, "world, again") || strings.Contains(vtt, "{")) {
			t.Fatalf("ass text %q", vtt)
		}
	}
}

// Real files made with ffmpeg (skipped if the test media isn't there).
func TestRealMedia(t *testing.T) {
	dir := "/tmp/claude-0/vw/files"
	if _, err := os.Stat(dir); err != nil {
		t.Skip("no test media")
	}
	tr := probeTracks(filepath.Join(dir, "film.mkv"))
	audio, subs := 0, 0
	for _, x := range tr {
		if x.Type == "audio" {
			audio++
		}
		if x.Type == "subtitle" && x.text != "" {
			subs++
			cues, err := mkvSubtitles(filepath.Join(dir, "film.mkv"), x.num, x.text)
			if err != nil || len(cues) != 2 || !strings.Contains(cues[0].text, "Embedded") {
				t.Fatalf("mkv subs %v %v", cues, err)
			}
		}
	}
	if audio != 2 || subs != 1 {
		t.Fatalf("mkv tracks %+v", tr)
	}
	for _, f := range []string{"song.mp3", "tune.flac"} {
		tg := readTags(filepath.Join(dir, f))
		if tg == nil || tg.Title == "" || tg.Artist == "" || len(tg.Cover) == 0 || sniff(tg.Cover) != kJPG {
			t.Fatalf("%s tags %+v", f, tg)
		}
	}
	for _, f := range []string{"clip.mp4", "big.mp4"} {
		for _, x := range probeTracks(filepath.Join(dir, f)) {
			if !x.Played {
				t.Fatalf("%s: %+v should be playable", f, x)
			}
		}
	}
}

// Settings saved by version 1.5.0 carry over.
func TestViewerSettingsFrom150(t *testing.T) {
	v := defaultViewerSettings()
	if err := json.Unmarshal([]byte(`{"external":false,"noVideo":true,"noPdf":true,"noPreview":true}`), &v); err != nil {
		t.Fatal(err)
	}
	v.normalize()
	if v.OpenWith != "viewer" || v.Video != "default" || v.PDF != "default" || v.Audio != "viewer" || v.Preview || v.Window != "inside" {
		t.Fatalf("%+v", v)
	}
	b, _ := json.Marshal(v)
	if strings.Contains(string(b), "noVideo") {
		t.Fatalf("old names kept: %s", b)
	}
	v = defaultViewerSettings()
	json.Unmarshal([]byte(`{"external":true}`), &v)
	v.normalize()
	if v.OpenWith != "default" {
		t.Fatalf("%+v", v)
	}
}
