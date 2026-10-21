package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---------- splitting pieces between connections ----------

func seg(start, end, pos int64, rate float64, age time.Duration) *Segment {
	s := &Segment{Start: start, End: end, Pos: pos, active: true, rate: rate}
	if age > 0 {
		s.began = time.Now().Add(-age)
	}
	return s
}

func TestSplitSharesSlowTail(t *testing.T) {
	const MB = 1 << 20
	// a piece with 3 MB left at 128 KB/s (24 s to go): the old rule (only over
	// 4 MB) left it alone; now a free connection takes half of it
	slow := seg(0, 4*MB-1, MB, 128<<10, 10*time.Second)
	d := &Download{Resumable: true, Segments: []*Segment{slow}}
	got := d.claim(false)
	if got == nil {
		t.Fatal("expected the slow tail to be shared")
	}
	if got.Start != MB+(3*MB)/2 || slow.End != got.Start-1 || got.End != 4*MB-1 {
		t.Fatalf("split at %d (victim end %d), want the middle of what's left", got.Start, slow.End)
	}
}

func TestSplitLeavesFastPieceAlone(t *testing.T) {
	const MB = 1 << 20
	// 3 MB left at 5 MB/s: done in 0.6 s, sharing would only add a request
	fast := seg(0, 4*MB-1, MB, 5*MB, 10*time.Second)
	d := &Download{Resumable: true, Segments: []*Segment{fast}}
	if got := d.claim(false); got != nil {
		t.Fatalf("a piece about to finish shouldn't be split: got %+v", got)
	}
}

func TestSplitTakesOverCrawlingConnection(t *testing.T) {
	const MB = 1 << 20
	a := seg(0, 8*MB-1, 2*MB, 2*MB, 10*time.Second)
	b := seg(8*MB, 16*MB-1, 9*MB, 2*MB, 10*time.Second)
	crawl := seg(16*MB, 17*MB-1, 16*MB+(100<<10), 20<<10, 10*time.Second) // 20 KB/s, 924 KB left
	d := &Download{Resumable: true, Segments: []*Segment{a, b, crawl}, connRate: 2 * MB}
	got := d.claim(false)
	if got == nil || got.Start != crawl.Pos-0 || crawl.End != got.Start-1 {
		t.Fatalf("the crawling connection's whole remainder should move: got %+v, crawl %+v", got, crawl)
	}
	if !crawl.done() {
		t.Fatal("the crawling piece should now count as finished")
	}
}

func TestSplitTakesOverLastCrawlingConnection(t *testing.T) {
	const MB = 1 << 20
	// the others have finished; only the crawler is left, with 300 KB to go at 2 KB/s
	crawl := seg(0, MB-1, MB-300<<10, 2<<10, 10*time.Second)
	d := &Download{Resumable: true, Segments: []*Segment{crawl}, connRate: 2 * MB}
	got := d.claim(false)
	if got == nil || got.Start != crawl.Pos {
		t.Fatalf("a crawling last connection should hand over its remainder: got %+v", got)
	}
}

func TestSplitWithSpeedLimitIsCoarse(t *testing.T) {
	const MB = 1 << 20
	slow := seg(0, 4*MB-1, MB, 16<<10, 10*time.Second) // 3 MB left, crawling because of the limit
	d := &Download{Resumable: true, Segments: []*Segment{slow}}
	if got := d.claim(true); got != nil {
		t.Fatal("with a speed limit, pieces under 4 MB aren't split")
	}
	big := seg(0, 10*MB-1, MB, 16<<10, 10*time.Second)
	d = &Download{Resumable: true, Segments: []*Segment{big}}
	if got := d.claim(true); got == nil {
		t.Fatal("with a speed limit, big pieces are still split")
	}
}

func TestSplitNeverMakesTinyPieces(t *testing.T) {
	s := seg(0, 400<<10, 0, 1<<10, 10*time.Second) // 400 KB left at 1 KB/s, but it's the only one
	d := &Download{Resumable: true, Segments: []*Segment{s}}
	if got := d.claim(false); got != nil {
		t.Fatalf("pieces under 2×256 KB aren't split: got %+v", got)
	}
}

func TestCompactMergesFinishedNeighbours(t *testing.T) {
	var segs []*Segment
	for i := int64(0); i < 60; i++ {
		segs = append(segs, &Segment{Start: i * 100, End: i*100 + 99, Pos: i*100 + 100})
	}
	segs[30].Pos = 3050 // one still unfinished in the middle
	d := &Download{Segments: segs}
	before := d.downloadedLocked()
	d.compactLocked()
	if len(d.Segments) != 3 {
		t.Fatalf("got %d pieces, want 3 (done, unfinished, done)", len(d.Segments))
	}
	if after := d.downloadedLocked(); after != before {
		t.Fatalf("progress changed: %d → %d", before, after)
	}
}

// ---------- reading a response ----------

// dribble delivers its data a little at a time.
type dribble struct {
	data  []byte
	chunk int
	every time.Duration
	ctx   context.Context // like a real connection: once cut, reads fail
}

func (r *dribble) Read(p []byte) (int, error) {
	if r.ctx != nil && r.ctx.Err() != nil {
		return 0, r.ctx.Err()
	}
	if len(r.data) == 0 {
		return 0, io.EOF
	}
	time.Sleep(r.every)
	n := min(len(p), r.chunk, len(r.data))
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	dir := tempDirRetry(t) // the manager may still be saving as the test ends
	m := NewManager(filepath.Join(dir, "state", "state.json"))
	_, s := m.Snapshot()
	s.DownloadDir = filepath.Join(dir, "dl")
	s.Categorize, s.DupCheck = false, false
	s.Safety.FindChecksums, s.Safety.Scan = false, false
	m.SetSettings(s)
	return m
}

func TestPumpSlowConnectionIsNotAStall(t *testing.T) {
	old := stallTimeout
	stallTimeout = 400 * time.Millisecond
	defer func() { stallTimeout = old }()
	m := newTestManager(t)
	data := bytes.Repeat([]byte("x"), 64<<10)
	// 2 KB every 50 ms (40 KB/s): filling a 256 KB buffer would take far longer
	// than the stall timeout, but bytes keep arriving, so it must not count as stalled
	f, _ := os.CreateTemp(t.TempDir(), "p")
	defer f.Close()
	d := &Download{Resumable: true, Size: int64(len(data))}
	s := &Segment{Start: 0, End: int64(len(data)) - 1, active: true}
	d.Segments = []*Segment{s}
	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := &dribble{data: append([]byte(nil), data...), chunk: 2 << 10, every: 50 * time.Millisecond, ctx: reqCtx}
	stall := time.AfterFunc(stallTimeout, cancel)
	defer stall.Stop()
	_, err := m.pump(context.Background(), reqCtx, d, s, f, body, stall, s.End, true)
	if err != nil {
		t.Fatalf("slow but steady connection failed: %v", err)
	}
	got, _ := os.ReadFile(f.Name())
	if !bytes.Equal(got, data) {
		t.Fatal("data differs")
	}
}

func TestPumpStopsWhereAnotherConnectionTookOver(t *testing.T) {
	m := newTestManager(t)
	data := make([]byte, 1<<20)
	rand.New(rand.NewSource(1)).Read(data)
	f, _ := os.CreateTemp(t.TempDir(), "p")
	defer f.Close()
	d := &Download{Resumable: true, Size: int64(len(data))}
	s := &Segment{Start: 0, End: int64(len(data)) - 1, active: true}
	d.Segments = []*Segment{s}
	// halfway through, another connection takes the second half
	body := &dribble{data: append([]byte(nil), data...), chunk: 32 << 10, every: time.Millisecond}
	go func() {
		time.Sleep(5 * time.Millisecond)
		d.mu.Lock()
		s.End = 700<<10 - 1
		d.mu.Unlock()
	}()
	reqCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stall := time.AfterFunc(time.Minute, cancel)
	defer stall.Stop()
	if _, err := m.pump(context.Background(), reqCtx, d, s, f, body, stall, int64(len(data))-1, true); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(f.Name())
	if st.Size() != 700<<10 || s.Pos != 700<<10 {
		t.Fatalf("wrote %d bytes, pos %d; want exactly the first 700 KB", st.Size(), s.Pos)
	}
	got, _ := os.ReadFile(f.Name())
	if !bytes.Equal(got, data[:700<<10]) {
		t.Fatal("data differs")
	}
}

// ---------- which errors fix themselves ----------

func TestTransientErr(t *testing.T) {
	yes := []error{
		errors.New("connection stalled"),
		errors.New("connection closed early"),
		&statusError{Code: 503, Status: "503 Service Unavailable"},
		&statusError{Code: 429},
		&net.OpError{Op: "dial", Err: errors.New("connect: connection refused")},
		&net.DNSError{Err: "no such host", Name: "example.com"},
		fmt.Errorf("server replied %s", "502 Bad Gateway"),
		io.ErrUnexpectedEOF,
		errors.New("read tcp 1.2.3.4:5->6.7.8.9:443: wsarecv: An existing connection was forcibly closed by the remote host."),
	}
	no := []error{
		&statusError{Code: 404},
		&statusError{Code: 403},
		errFileChanged,
		errDuplicate,
		&noSpaceError{need: 1, free: 0},
		errors.New("write C:\\x.part: There is not enough space on the disk."),
		errors.New("tls: failed to verify certificate: x509: certificate signed by unknown authority"),
		context.Canceled,
	}
	for _, e := range yes {
		if !transientErr(e) {
			t.Errorf("should retry by itself: %v", e)
		}
	}
	for _, e := range no {
		if transientErr(e) {
			t.Errorf("shouldn't retry by itself: %v", e)
		}
	}
}

func TestAutoRetryWaitsGrowAndReset(t *testing.T) {
	// a server that can't resume starts from zero every time: only a few tries
	nr := &Download{Size: 100}
	tries := 0
	for {
		if _, ok := nr.autoRetryLocked(errors.New("connection stalled")); !ok {
			break
		}
		tries++
	}
	if tries != 3 {
		t.Fatalf("non-resumable: %d tries, want 3", tries)
	}

	d := &Download{Size: 100, Resumable: true}
	var waits []time.Duration
	for i := 0; i < 9; i++ {
		w, ok := d.autoRetryLocked(errors.New("connection stalled"))
		if !ok {
			t.Fatal("should retry")
		}
		waits = append(waits, w)
	}
	if waits[0] != 5*time.Second || waits[1] != 10*time.Second || waits[8] != 5*time.Minute {
		t.Fatalf("waits %v", waits)
	}
	// progress was made in the last run: waits start small again
	d.Segments = []*Segment{{Start: 0, End: 99, Pos: 50}}
	if w, _ := d.autoRetryLocked(errors.New("connection stalled")); w != 5*time.Second {
		t.Fatalf("after progress want 5s, got %v", w)
	}
	d.startBytes = 50
	d.autoRetries = maxAutoRetries
	if _, ok := d.autoRetryLocked(errors.New("connection stalled")); ok {
		t.Fatal("should give up after the last try")
	}
	if _, ok := d.autoRetryLocked(&statusError{Code: 404}); ok {
		t.Fatal("404 isn't retried")
	}
}

// ---------- the saved list ----------

func TestLoadFallsBackToPreviousCopy(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	m := NewManager(p)
	_, s := m.Snapshot()
	s.DownloadDir = filepath.Join(dir, "dl")
	s.MaxConcurrent = 7
	m.SetSettings(s)
	m.save()
	m.save()                                                      // the first copy becomes state.json.bak
	os.WriteFile(p, []byte(`{"settings":{"downloadDir":`), 0o644) // cut short by a power cut
	m2 := NewManager(p)
	_, s2 := m2.Snapshot()
	if s2.MaxConcurrent != 7 {
		t.Fatalf("settings lost after a damaged save: MaxConcurrent=%d", s2.MaxConcurrent)
	}
}

// ---------- end to end against a local server ----------

type testServer struct {
	data       []byte
	rate       int64
	redirects  atomic.Int64
	reqs       atomic.Int64
	targetUses atomic.Int64
	maxUses    int64 // the redirect target stops working after this many requests (0 = never)
	down       atomic.Bool
}

func (ts *testServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if ts.down.Load() {
		panic(http.ErrAbortHandler)
	}
	if r.URL.Path == "/start/file.bin" {
		ts.redirects.Add(1)
		ts.targetUses.Store(0)
		http.Redirect(w, r, "/signed/file.bin?token="+strconv.FormatInt(ts.redirects.Load(), 10), http.StatusFound)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/signed/") && ts.maxUses > 0 && ts.targetUses.Add(1) > ts.maxUses {
		http.Error(w, "expired", http.StatusForbidden)
		return
	}
	ts.reqs.Add(1)
	size := int64(len(ts.data))
	start, end := int64(0), size-1
	if rg := r.Header.Get("Range"); rg != "" {
		fmt.Sscanf(rg, "bytes=%d-%d", &start, &end)
		if end >= size || end < start {
			end = size - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
	began := time.Now()
	for pos := start; pos <= end; {
		if ts.down.Load() {
			panic(http.ErrAbortHandler)
		}
		k := min(16<<10, end-pos+1)
		if _, err := w.Write(ts.data[pos : pos+k]); err != nil {
			return
		}
		pos += k
		if ts.rate > 0 {
			due := began.Add(time.Duration(float64(pos-start) / float64(ts.rate) * float64(time.Second)))
			if d := time.Until(due); d > 0 {
				w.(http.Flusher).Flush()
				time.Sleep(d)
			}
		}
	}
}

func waitStatus(t *testing.T, m *Manager, id string, want Status, limit time.Duration) *Download {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		d := m.get(id)
		d.mu.Lock()
		st, running, e := d.Status, d.running, d.Error
		d.mu.Unlock()
		if st == want && !running {
			return d
		}
		if st == StError && want != StError {
			t.Fatalf("download failed: %s", e)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", want)
	return nil
}

func fileOf(d *Download) []byte {
	d.mu.Lock()
	p := filepath.Join(d.Dir, d.FileName)
	d.mu.Unlock()
	b, _ := os.ReadFile(p)
	return b
}

func TestRedirectFollowedOncePerRun(t *testing.T) {
	ts := &testServer{data: make([]byte, 6<<20), rate: 3 << 20}
	rand.New(rand.NewSource(3)).Read(ts.data)
	srv := httptest.NewServer(ts)
	defer srv.Close()
	m := newTestManager(t)
	ids, err := m.Add(AddRequest{URL: srv.URL + "/start/file.bin", Connections: 6})
	if err != nil {
		t.Fatal(err)
	}
	d := waitStatus(t, m, ids[0], StDone, time.Minute)
	if !bytes.Equal(fileOf(d), ts.data) {
		t.Fatal("file differs")
	}
	if n := ts.redirects.Load(); n > 2 {
		t.Fatalf("followed the redirect %d times; the pieces should go straight to where it led", n)
	}
}

func TestExpiredRedirectTargetFallsBack(t *testing.T) {
	ts := &testServer{data: make([]byte, 6<<20), rate: 2 << 20, maxUses: 3}
	rand.New(rand.NewSource(4)).Read(ts.data)
	srv := httptest.NewServer(ts)
	defer srv.Close()
	m := newTestManager(t)
	ids, err := m.Add(AddRequest{URL: srv.URL + "/start/file.bin", Connections: 6})
	if err != nil {
		t.Fatal(err)
	}
	d := waitStatus(t, m, ids[0], StDone, time.Minute)
	if !bytes.Equal(fileOf(d), ts.data) {
		t.Fatal("file differs")
	}
}

func TestDownloadRetriesByItselfAfterOutage(t *testing.T) {
	oldWaits, oldStall, oldRetries := autoRetryWaits, stallTimeout, maxRetries
	autoRetryWaits = []time.Duration{300 * time.Millisecond, 600 * time.Millisecond}
	stallTimeout, maxRetries = 2*time.Second, 2
	defer func() { autoRetryWaits, stallTimeout, maxRetries = oldWaits, oldStall, oldRetries }()

	ts := &testServer{data: make([]byte, 3<<20), rate: 1 << 20}
	rand.New(rand.NewSource(5)).Read(ts.data)
	srv := httptest.NewServer(ts)
	defer srv.Close()
	m := newTestManager(t)
	ids, err := m.Add(AddRequest{URL: srv.URL + "/file.bin", Connections: 4})
	if err != nil {
		t.Fatal(err)
	}
	// the internet goes away mid-download and stays away longer than the
	// connections' own retries last
	time.Sleep(700 * time.Millisecond)
	ts.down.Store(true)
	deadline := time.Now().Add(3 * time.Minute)
	sawRetry := false
	for time.Now().Before(deadline) && !sawRetry {
		d := m.get(ids[0])
		d.mu.Lock()
		sawRetry = d.Status == StQueued && !d.retryAt.IsZero()
		failed := d.Status == StError
		d.mu.Unlock()
		if failed {
			t.Fatal("went to error instead of waiting to retry")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !sawRetry {
		t.Fatal("never scheduled a retry")
	}
	ts.down.Store(false)
	d := waitStatus(t, m, ids[0], StDone, time.Minute)
	if !bytes.Equal(fileOf(d), ts.data) {
		t.Fatal("file differs")
	}
}

// Video chunks fetched one after another should share one connection: a new
// connection per chunk costs several round trips (TCP + TLS) every time.
func TestVideoChunksReuseTheConnection(t *testing.T) {
	ts := &testServer{data: make([]byte, 2<<20)}
	rand.New(rand.NewSource(6)).Read(ts.data)
	var conns atomic.Int64
	srv := httptest.NewUnstartedServer(ts)
	srv.Config.ConnState = func(c net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()
	m := newTestManager(t)
	d := &Download{}
	dir := t.TempDir()
	const piece = 256 << 10
	for i := int64(0); i < 8; i++ {
		tmp := filepath.Join(dir, fmt.Sprintf("c%d", i))
		ref := SegRef{URL: srv.URL + "/file.bin", Off: i * piece, Len: piece}
		if err := m.fetchSegToFile(context.Background(), d, ref, tmp, reqOpts{}, func(int64) {}, func(int64) {}); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(tmp)
		if !bytes.Equal(got, ts.data[i*piece:(i+1)*piece]) {
			t.Fatalf("chunk %d differs", i)
		}
	}
	if n := conns.Load(); n != 1 {
		t.Fatalf("8 chunks used %d connections, want 1", n)
	}
}

func TestSplitTakesOverSilentConnection(t *testing.T) {
	const MB = 1 << 20
	// was fast, but nothing has arrived for 5 s: its old speed means nothing now
	silent := seg(0, 4*MB-1, MB, 3*MB, 10*time.Second)
	silent.lastData = time.Now().Add(-5 * time.Second)
	d := &Download{Resumable: true, Segments: []*Segment{silent}, connRate: 3 * MB}
	got := d.claim(false)
	if got == nil || got.Start != silent.Pos {
		t.Fatalf("a silent connection should hand over its whole remainder: got %+v", got)
	}
	// …but not endlessly: a stretch that moved three times stays where it is
	again := seg(0, 4*MB-1, MB, 3*MB, 10*time.Second)
	again.lastData, again.handovers = time.Now().Add(-5*time.Second), 3
	d = &Download{Resumable: true, Segments: []*Segment{again}, connRate: 3 * MB}
	if got := d.claim(false); got != nil && got.Start == again.Pos {
		t.Fatal("handed over a fourth time")
	}
}

func TestFailedPieceNotHandedOutAgain(t *testing.T) {
	failed := &Segment{Start: 0, End: 1 << 20, Pos: 100, failed: true}
	d := &Download{Resumable: true, Segments: []*Segment{failed}}
	if got := d.claim(false); got != nil {
		t.Fatalf("a piece a connection gave up on was handed out again: %+v", got)
	}
}

// A file of unknown size whose connection is cut halfway must not end up
// "completed" with half the file.
func TestUnknownSizeCutOffIsNotComplete(t *testing.T) {
	data := make([]byte, 2<<20)
	rand.New(rand.NewSource(8)).Read(data)
	var cut atomic.Bool
	cut.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// chunked, no size, no ranges
		for i := 0; i < len(data); i += 64 << 10 {
			if cut.Load() && i >= len(data)/2 {
				panic(http.ErrAbortHandler)
			}
			w.Write(data[i : i+64<<10])
			w.(http.Flusher).Flush()
		}
	}))
	defer srv.Close()
	oldWaits := autoRetryWaits
	autoRetryWaits = []time.Duration{200 * time.Millisecond}
	defer func() { autoRetryWaits = oldWaits }()
	m := newTestManager(t)
	ids, err := m.Add(AddRequest{URL: srv.URL + "/stream.bin"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	d := m.get(ids[0])
	d.mu.Lock()
	st := d.Status
	d.mu.Unlock()
	if st == StDone {
		t.Fatal("a cut-off stream was marked as finished")
	}
	cut.Store(false) // the next try gets the whole stream
	d = waitStatus(t, m, ids[0], StDone, 30*time.Second)
	if !bytes.Equal(fileOf(d), data) {
		t.Fatalf("file differs: %d bytes, want %d", len(fileOf(d)), len(data))
	}
}

func TestRetryMessages(t *testing.T) {
	if got := withoutResumeHint("The connection kept dropping. Press Resume to continue where it stopped"); got != "The connection kept dropping" {
		t.Fatal(got)
	}
	if got := withoutResumeHint("Connection timed out. Check your internet and press Resume"); got != "Connection timed out. Check your internet" {
		t.Fatal(got)
	}
	for _, e := range []error{
		errors.New("421 Too many connections from your IP"),
		errors.New("the FTP server didn't accept the login (421 Service not available, closing control connection). Add your username and password in Add link → Login"),
		errors.New("426 Connection closed; transfer aborted"),
	} {
		if !transientErr(e) {
			t.Errorf("should retry by itself: %v", e)
		}
	}
	for _, e := range []error{
		&statusError{Code: 501}, &statusError{Code: 505},
		errors.New("remote error: tls: handshake failure"),
		errors.New("530 Login incorrect"),
	} {
		if transientErr(e) {
			t.Errorf("shouldn't retry by itself: %v", e)
		}
	}
}
