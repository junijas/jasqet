//go:build tgbench

package main

// A test bench for the download engine: a local server that behaves like the
// awkward servers people meet in real life (per-connection speed caps, slow
// round trips, redirects, dropped connections, one crawling connection), and
// a set of scenarios that time a real download against it and check every byte.
//
// Run:  go test -tags tgbench -run TestBench -v -timeout 30m

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type benchServer struct {
	data       []byte
	perReqRate int64         // bytes per second per request (0 = no cap)
	rtt        time.Duration // added before every answer
	connSetup  time.Duration // added once per new connection (TCP + TLS handshakes)
	crawlReq   int64         // this numbered file request (1-based) crawls…
	crawlRate  int64         // …at this speed
	dropProb   float64       // chance per 64 KB of cutting the connection
	outage     atomic.Int64  // unix nanos: until then every request is cut off

	reqs, conns, served, redirects atomic.Int64
	mu                             sync.Mutex
	rnd                            *rand.Rand
}

type connKey struct{}

var benchT0 = time.Now()

type connInfo struct{ n atomic.Int64 }

func newBenchServer(size int) *benchServer {
	data := make([]byte, size)
	r := rand.New(rand.NewSource(int64(size)))
	r.Read(data)
	return &benchServer{data: data, rnd: rand.New(rand.NewSource(7))}
}

func (b *benchServer) start() *httptest.Server {
	srv := httptest.NewUnstartedServer(b)
	srv.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
		b.conns.Add(1)
		return context.WithValue(ctx, connKey{}, &connInfo{})
	}
	srv.Start()
	return srv
}

func (b *benchServer) chance(p float64) bool {
	if p <= 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.rnd.Float64() < p
}

func (b *benchServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ci, _ := r.Context().Value(connKey{}).(*connInfo)
	if ci != nil && ci.n.Add(1) == 1 && b.connSetup > 0 {
		time.Sleep(b.connSetup)
	}
	if until := b.outage.Load(); until > 0 && time.Now().UnixNano() < until {
		panic(http.ErrAbortHandler) // the network is down: the connection just dies
	}
	time.Sleep(b.rtt)
	if strings.HasPrefix(r.URL.Path, "/r/") {
		b.redirects.Add(1)
		http.Redirect(w, r, strings.TrimPrefix(r.URL.Path, "/r"), http.StatusFound)
		return
	}
	if !strings.HasPrefix(r.URL.Path, "/file") {
		http.NotFound(w, r)
		return
	}
	n := b.reqs.Add(1)
	size := int64(len(b.data))
	start, end := int64(0), size-1
	status := http.StatusOK
	if rg := r.Header.Get("Range"); strings.HasPrefix(rg, "bytes=") {
		parts := strings.SplitN(strings.TrimPrefix(rg, "bytes="), "-", 2)
		s, err := strconv.ParseInt(parts[0], 10, 64)
		if err != nil || s >= size {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return
		}
		start = s
		if parts[1] != "" {
			if e, err := strconv.ParseInt(parts[1], 10, 64); err == nil && e < size {
				end = e
			}
		}
		status = http.StatusPartialContent
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("ETag", `"bench-1"`)
	w.Header().Set("Last-Modified", "Mon, 05 Oct 2026 10:00:00 GMT")
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.WriteHeader(status)
	rate := b.perReqRate
	if b.crawlReq > 0 && n == b.crawlReq {
		rate = b.crawlRate
	}
	const chunk = 16 << 10
	began := time.Now()
	sent := int64(0)
	if os.Getenv("BENCH_LOG") != "" {
		defer func() {
			fmt.Printf("  req #%d %-24s rate=%-8d sent=%-9d at=%v took=%v\n", n, r.Header.Get("Range"), rate, sent, began.Sub(benchT0).Round(time.Millisecond), time.Since(began).Round(time.Millisecond))
		}()
	}
	for pos := start; pos <= end; {
		k := min(int64(chunk), end-pos+1)
		if sent%(64<<10) == 0 && sent > 0 && b.chance(b.dropProb) {
			panic(http.ErrAbortHandler)
		}
		if until := b.outage.Load(); until > 0 && time.Now().UnixNano() < until {
			panic(http.ErrAbortHandler)
		}
		if _, err := w.Write(b.data[pos : pos+k]); err != nil {
			return
		}
		b.served.Add(k)
		pos += k
		sent += k
		if rate > 0 {
			due := began.Add(time.Duration(float64(sent) / float64(rate) * float64(time.Second)))
			if d := time.Until(due); d > 0 {
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				time.Sleep(d)
			}
		}
	}
}

type benchResult struct {
	name     string
	took     time.Duration
	ok       bool
	reqs     int64
	conns    int64
	served   int64
	size     int64
	redirect int64
	err      string
}

func benchManager(t *testing.T, tune func(*Settings)) (*Manager, string) {
	dir := t.TempDir()
	m := NewManager(filepath.Join(dir, "state", "state.json"))
	_, s := m.Snapshot()
	s.DownloadDir = filepath.Join(dir, "dl")
	s.Categorize = false
	s.DupCheck = false
	s.Safety.FindChecksums = false
	s.Safety.Scan = false
	s.MaxConcurrent = 3
	s.Connections = 16
	s.AutoConnections = true
	s.SpeedLimitKB = 0
	if tune != nil {
		tune(&s)
	}
	m.SetSettings(s)
	return m, s.DownloadDir
}

func waitDone(m *Manager, ids []string, limit time.Duration) (bool, string) {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		all := true
		for _, id := range ids {
			d := m.get(id)
			d.mu.Lock()
			st, e, running := d.Status, d.Error, d.running
			d.mu.Unlock()
			if st == StError {
				return false, e
			}
			if st != StDone || running {
				all = false
			}
		}
		if all {
			return true, ""
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false, "timed out"
}

func checkFile(m *Manager, id string, want []byte) error {
	d := m.get(id)
	d.mu.Lock()
	p := filepath.Join(d.Dir, d.FileName)
	d.mu.Unlock()
	got, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, want) {
		return fmt.Errorf("file differs (got %d bytes, sha %x; want %d bytes, sha %x)", len(got), sha256.Sum256(got), len(want), sha256.Sum256(want))
	}
	return nil
}

func runOne(t *testing.T, name string, b *benchServer, link string, limit time.Duration, tune func(*Settings), during func(srv *httptest.Server)) benchResult {
	srv := b.start()
	defer srv.Close()
	m, _ := benchManager(t, tune)
	began := time.Now()
	benchT0 = began
	ids, err := m.Add(AddRequest{URL: srv.URL + link})
	if err != nil {
		t.Fatal(err)
	}
	if during != nil {
		go during(srv)
	}
	ok, e := waitDone(m, ids, limit)
	res := benchResult{name: name, took: time.Since(began), ok: ok, err: e, reqs: b.reqs.Load(), conns: b.conns.Load(), served: b.served.Load(), size: int64(len(b.data)), redirect: b.redirects.Load()}
	if ok {
		if err := checkFile(m, ids[0], b.data); err != nil {
			res.ok, res.err = false, err.Error()
		}
	}
	// stop whatever is still running so it doesn't eat the next scenario's bandwidth
	if d := m.get(ids[0]); d != nil {
		m.Pause(d)
	}
	return res
}

func report(t *testing.T, r benchResult) {
	status := "OK"
	if !r.ok {
		status = "FAIL: " + r.err
	}
	extra := float64(r.served-r.size) / float64(r.size) * 100
	t.Logf("%-34s %7.2fs  requests=%-4d connections=%-4d redirects=%-3d served=%5.1f MB (%+.1f%% over file)  %s",
		r.name, r.took.Seconds(), r.reqs, r.conns, r.redirect, float64(r.served)/(1<<20), extra, status)
}

func TestBench(t *testing.T) {
	only := os.Getenv("BENCH_ONLY")
	want := func(name string) bool { return only == "" || strings.Contains(name, only) }
	var results []benchResult

	if want("capped") {
		b := newBenchServer(48 << 20)
		b.perReqRate, b.rtt, b.connSetup = 1<<20, 40*time.Millisecond, 120*time.Millisecond
		results = append(results, runOne(t, "1 capped host (1 MB/s per conn)", b, "/file.bin", 3*time.Minute, nil, nil))
	}
	if want("crawl") {
		b := newBenchServer(32 << 20)
		b.perReqRate, b.rtt, b.connSetup = 2<<20, 40*time.Millisecond, 120*time.Millisecond
		b.crawlReq, b.crawlRate = 3, 24<<10
		results = append(results, runOne(t, "2 one connection crawls (24 KB/s)", b, "/file.bin", 4*time.Minute, func(s *Settings) {
			s.AutoConnections, s.Connections = false, 8
		}, nil))
	}
	if want("redirect") {
		b := newBenchServer(16 << 20)
		b.perReqRate, b.rtt, b.connSetup = 1<<20, 100*time.Millisecond, 250*time.Millisecond
		results = append(results, runOne(t, "3 redirect + slow round trips", b, "/r/file.bin", 3*time.Minute, func(s *Settings) {
			s.AutoConnections, s.Connections = false, 8
		}, nil))
	}
	if want("flaky") {
		b := newBenchServer(24 << 20)
		b.perReqRate, b.rtt, b.connSetup = 2<<20, 40*time.Millisecond, 120*time.Millisecond
		b.dropProb = 0.03
		results = append(results, runOne(t, "4 flaky connections (3% drops/64KB)", b, "/file.bin", 4*time.Minute, nil, nil))
	}
	if want("limit") {
		// stall timeout shortened to 3 s (from 30 s), with speeds scaled up to match
		setBenchStall(3 * time.Second)
		b := newBenchServer(8 << 20)
		b.rtt, b.connSetup = 40*time.Millisecond, 120*time.Millisecond
		results = append(results, runOne(t, "5 speed limit 640 KB/s, 8 conns", b, "/file.bin", 3*time.Minute, func(s *Settings) {
			s.AutoConnections, s.Connections, s.SpeedLimitKB = false, 8, 640
		}, nil))
		setBenchStall(30 * time.Second)
	}
	if want("outage") {
		b := newBenchServer(8 << 20)
		b.perReqRate, b.rtt, b.connSetup = 128<<10, 40*time.Millisecond, 120*time.Millisecond
		results = append(results, runOne(t, "6 internet drops for 100 s", b, "/file.bin", 6*time.Minute, func(s *Settings) {
			s.AutoConnections, s.Connections = false, 8
		}, func(srv *httptest.Server) {
			time.Sleep(3 * time.Second)
			b.outage.Store(time.Now().Add(100 * time.Second).UnixNano())
		}))
	}
	if want("batch") {
		// many small files: start-up cost per download matters most here
		b := newBenchServer(512 << 10)
		b.perReqRate, b.rtt, b.connSetup = 2<<20, 100*time.Millisecond, 250*time.Millisecond
		srv := b.start()
		m, _ := benchManager(t, nil)
		var links []string
		for i := 0; i < 30; i++ {
			links = append(links, fmt.Sprintf("%s/r/file%02d.bin", srv.URL, i))
		}
		began := time.Now()
		ids, err := m.Add(AddRequest{URLs: strings.Join(links, "\n")})
		if err != nil {
			t.Fatal(err)
		}
		ok, e := waitDone(m, ids, 5*time.Minute)
		r := benchResult{name: "7 batch: 30 x 512 KB via redirect", took: time.Since(began), ok: ok, err: e, reqs: b.reqs.Load(), conns: b.conns.Load(), served: b.served.Load(), size: int64(len(b.data)) * 30, redirect: b.redirects.Load()}
		for _, id := range ids {
			if err := checkFile(m, id, b.data); err != nil && r.ok {
				r.ok, r.err = false, err.Error()
			}
		}
		srv.Close()
		results = append(results, r)
	}
	t.Log("")
	for _, r := range results {
		report(t, r)
	}
}
