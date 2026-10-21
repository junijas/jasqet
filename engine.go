package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	readBufSize     = 256 * 1024
	minSplitSize    = 2 * 1024 * 1024 // pieces this big are split even before their speed is known
	minPiece        = 256 * 1024      // never make a piece smaller than this
	splitAfter      = 3.0             // seconds: only share a piece that would take longer than this
	minInitialChunk = 1 * 1024 * 1024
	seamCheck       = 16 * 1024 // bytes re-read when a cut-off piece is picked up again
	flushEvery      = 250 * time.Millisecond
)

// stallTimeout: a connection that delivers nothing for this long is reopened.
// (A variable so tests can shorten it.)
var stallTimeout = 30 * time.Second

// maxRetries: how often one connection retries a piece before giving up on it.
var maxRetries = 12

var errServerIgnoredRange = errors.New("server ignored the byte range request")

// errResolvedStale: a remembered redirect target stopped working; the next
// try goes through the original link again (not counted as a failure).
var errResolvedStale = errors.New("remembered redirect target stopped working")

var bufPool = sync.Pool{New: func() any { b := make([]byte, readBufSize); return &b }}

// statusError is an HTTP error answer from the server.
type statusError struct {
	Code   int
	Status string
	After  time.Duration // Retry-After, if the server sent one
}

func (e *statusError) Error() string { return "server replied " + e.Status }

func newStatusError(resp *http.Response) *statusError {
	e := &statusError{Code: resp.StatusCode, Status: resp.Status}
	if v := strings.TrimSpace(resp.Header.Get("Retry-After")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			e.After = time.Duration(min(n, 120)) * time.Second
		} else if t, err := http.ParseTime(v); err == nil { // the date form
			if wait := time.Until(t); wait > 0 {
				e.After = min(wait, 120*time.Second)
			}
		}
	}
	return e
}

// linkGone: the link stopped working (expired token, signed URL timed out…).
func linkGone(err error) bool {
	var se *statusError
	if errors.As(err, &se) {
		switch se.Code {
		case 401, 403, 404, 410, 498:
			return true
		}
	}
	return false
}

func busyErr(err error) (bool, time.Duration) {
	var se *statusError
	if errors.As(err, &se) && (se.Code == 429 || se.Code == 503) {
		return true, se.After
	}
	return false, 0
}

// Segment is one byte range of the file. End is inclusive; Pos is the next byte to write.
// End == -1 means "unknown size, read until the server closes".
type Segment struct {
	Start  int64 `json:"start"`
	End    int64 `json:"end"`
	Pos    int64 `json:"pos"`
	active bool
	src    int // 0 = the main link, 1… = mirrors

	rate      float64            // bytes per second this piece is arriving at (0 = not measured yet)
	began     time.Time          // when data started flowing on its current connection
	lastData  time.Time          // when it was last asked for or last got data
	failed    bool               // a connection gave up on it after its retries: not handed out again this run
	handovers int                // times this stretch was taken from a slow or silent connection
	stop      context.CancelFunc // ends its current request (when another connection takes all of it)
	yield     bool               // its connection should move to a more urgent piece (preview)
	yieldAt   time.Time          // next time to check whether the viewer needs this connection elsewhere
	reqTo     int64              // its current request asks for the bytes up to here (exclusive; 0 = none yet)
}

func (s *Segment) remaining() int64 {
	if s.End < 0 {
		return 1 << 62
	}
	return s.End - s.Pos + 1
}

func (s *Segment) done() bool { return s.End >= 0 && s.Pos > s.End }

// probe asks the server about the file: size, whether it supports ranges, and its name.
func (m *Manager) probe(ctx context.Context, d *Download) error {
	if isFTP(d.URL) {
		return m.probeFTP(ctx, d)
	}
	req, err := m.newRequest(ctx, d)
	if err != nil {
		return err
	}
	req.Header.Set("Range", "bytes=0-0") // one byte: enough to learn the size without using data
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	if googleDriveID(d.URL) != "" && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		// Google Drive's "can't scan this file for viruses" page: follow its button.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if next := driveConfirmURL(resp.Request.URL, body); next != "" && next != d.URL {
			d.mu.Lock()
			d.URL = next
			d.mu.Unlock()
			return m.probe(ctx, d)
		}
		return errors.New("Google Drive didn't give the file (it may be private, or over its download limit for today)")
	}
	if resp.ContentLength >= 0 && resp.ContentLength <= 64<<10 {
		// read the (tiny) answer to its end so this connection is reused by the
		// first piece instead of opening a new one (but never wait long for it)
		t := time.AfterFunc(5*time.Second, func() { resp.Body.Close() })
		io.Copy(io.Discard, resp.Body)
		t.Stop()
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return newStatusError(resp)
	}
	if resp.StatusCode == http.StatusPartialContent && resp.Request != nil && resp.Request.Response != nil {
		d.rememberResolved(0, req.URL.String(), resp.Request.URL.String())
	}

	size := int64(-1)
	resumable := false
	if resp.StatusCode == http.StatusPartialContent {
		if total := parseContentRangeTotal(resp.Header.Get("Content-Range")); total > 0 {
			size = total
			resumable = true
		}
	} else if resp.ContentLength > 0 {
		size = resp.ContentLength
	}

	name := filenameFromResponse(resp)

	d.mu.Lock()
	defer d.mu.Unlock()
	d.Size = size
	d.Resumable = resumable
	d.ETag, d.LastModified = validatorsOf(resp.Header)
	if d.FileName == "" {
		d.FileName = name
	}
	if !d.Placed { // once its .part file exists, the name stays (a resume must find it)
		d.FileName = sanitizeFilename(d.FileName)
	}
	return nil
}

// newRequest asks the download's main link (or the computer on the network
// it's copying from). The caller must NOT hold d.mu.
func (m *Manager) newRequest(ctx context.Context, d *Download) (*http.Request, error) {
	return m.newRequestFrom(ctx, d, 0)
}

// newRequestFrom asks a particular source (main link or a mirror) for the file.
// The caller must NOT hold d.mu (lanSettings takes m.mu; holding d.mu here
// would invert the lock order and could freeze the app).
func (m *Manager) newRequestFrom(ctx context.Context, d *Download, src int) (*http.Request, error) {
	d.mu.Lock()
	lan, link, o := d.lanSrc, d.URL, d.reqOpts()
	if lan == "" && src > 0 && src <= len(d.Mirrors) {
		link = d.Mirrors[src-1]
	}
	d.mu.Unlock()
	if lan != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, lan, nil)
		if err == nil {
			lanSignReq(req, m.lanSettings().Code)
		}
		return req, err
	}
	return m.newReq(ctx, link, o)
}

// ---------- remembered redirects ----------
//
// Many download links redirect (GitHub releases, SourceForge, file hosts, CDN
// sign-in links). Following the redirect for every piece costs an extra round
// trip, and often a new connection to a second server. So once a link has
// redirected to the file, the pieces go straight to where it pointed; if that
// stops working (signed links expire), the next try goes through the original
// link again. Not used with a site login, so a password is never sent to a
// server the original link didn't lead to by itself (nor a token the site
// gave in a header).

type resolvedLink struct {
	from, to string
	worked   bool // a piece came through it at least once
}

// rememberResolved notes that link `from` (source src) led to `to`.
func (d *Download) rememberResolved(src int, from, to string) {
	if from == "" || to == "" || from == to {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lanSrc != "" || d.User != "" || d.noResolve[src] || hasAuthHeader(d.Headers) {
		return
	}
	if d.resolved == nil {
		d.resolved = map[int]*resolvedLink{}
	}
	d.resolved[src] = &resolvedLink{from: from, to: to}
}

// hasAuthHeader: the site gave a login token in a header. Go drops it when a
// redirect leads to another server, so going there directly must not send it.
func hasAuthHeader(h map[string]string) bool {
	for k := range h {
		if strings.EqualFold(k, "Authorization") {
			return true
		}
	}
	return false
}

// forgetResolved drops a remembered target that failed. One that never worked
// (single-use links) isn't remembered again for this run.
func (d *Download) forgetResolved(src int, target string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if r := d.resolved[src]; r != nil && r.to == target {
		if !r.worked {
			if d.noResolve == nil {
				d.noResolve = map[int]bool{}
			}
			d.noResolve[src] = true
		}
		delete(d.resolved, src)
	}
}

// segmentRequest is newRequestFrom, but goes straight to a remembered redirect
// target when there is one. cached says whether it did.
func (m *Manager) segmentRequest(ctx context.Context, d *Download, src int) (req *http.Request, link, target string, err error) {
	d.mu.Lock()
	lan, link, o := d.lanSrc, d.URL, d.reqOpts()
	if lan == "" && src > 0 && src <= len(d.Mirrors) {
		link = d.Mirrors[src-1]
	}
	if r := d.resolved[src]; lan == "" && r != nil && r.from == link && o.user == "" && !hasAuthHeader(o.headers) {
		target = r.to
	}
	d.mu.Unlock()
	if target == "" {
		req, err = m.newRequestFrom(ctx, d, src)
		return req, link, "", err
	}
	req, err = m.newReq(ctx, target, o)
	return req, link, target, err
}

func (m *Manager) clientFor(d *Download) *http.Client {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lanSrc != "" {
		return lanClient
	}
	return m.client
}

func (d *Download) reqOpts() reqOpts {
	return reqOpts{headers: d.Headers, cookies: d.Cookies, user: d.User, pass: d.Password, authHost: hostOf(d.URL)}
}

// execute runs (or resumes) a download until it completes, fails or ctx is cancelled.
func (m *Manager) execute(ctx context.Context, d *Download) error {
	if d.isStream() {
		return m.executeMedia(ctx, d)
	}
	if d.Kind == "torrent" {
		return m.executeTorrent(ctx, d)
	}
	if d.Kind == "subs" {
		return m.executeSubs(ctx, d)
	}
	d.mu.Lock()
	fresh := len(d.Segments) == 0
	d.resolved, d.noResolve = nil, nil // redirect targets are only trusted within one run
	d.connRate = 0                     // the network may be a different one now
	d.mu.Unlock()

	if fresh && d.Kind == "zipentry" {
		m.placeFile(d)
		d.mu.Lock()
		if d.Zip != nil && d.Zip.Packed > 0 {
			d.Size = d.Zip.Packed // "Download again" forgot the size: it's known
		}
		d.Segments = initialSegments(d.Size, true, d.Connections)
		d.mu.Unlock()
		m.saveSoon()
	} else if fresh {
		if err := m.probe(ctx, d); err != nil {
			return err
		}
		m.mu.Lock()
		findSums := m.settings.Safety.FindChecksums
		m.mu.Unlock()
		if findSums {
			go m.findChecksum(ctx, d)
		}
		if dup := m.findDuplicate(ctx, d); dup != "" {
			d.mu.Lock()
			d.DupPath = dup
			d.mu.Unlock()
			return errDuplicate
		}
		m.placeFile(d)
		if src, peer := m.lanFind(ctx, d); src != "" {
			d.mu.Lock()
			d.lanSrc = src
			d.Note = "Copying from " + peerName(m, peer) + " on your network (no internet data used)"
			d.mu.Unlock()
		}
		d.mu.Lock()
		d.Segments = initialSegments(d.Size, d.Resumable, d.Connections)
		d.mu.Unlock()
		m.saveSoon()
	}

	d.mu.Lock()
	if !d.Resumable {
		// Nothing to resume from: start again at byte zero with one connection.
		d.Segments = []*Segment{{Start: 0, End: d.Size - 1, Pos: 0}}
		if d.Size <= 0 {
			d.Segments[0].End = -1
		}
	}
	for _, s := range d.Segments {
		s.active, s.failed, s.handovers = false, false, 0
	}
	workers := d.Connections
	if !d.Resumable {
		workers = 1
	}
	part := d.partPath()
	d.Status = StDownloading
	need := d.Size - d.downloadedLocked()
	if !d.Resumable {
		need = d.Size
		if st, err := os.Stat(part); err == nil {
			need -= st.Size() // emptied before it starts again
		}
	}
	d.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(part), 0o755); err != nil {
		return err
	}
	if need > 0 {
		// Better to say so now than to fail at 95% with the data already spent.
		if free := diskFree(filepath.Dir(part)); free >= 0 && free < need+diskSpareBytes {
			return &noSpaceError{need: need, free: free}
		}
	}
	flags := os.O_RDWR | os.O_CREATE
	if !d.Resumable {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return err
	}
	// Windows (NTFS) would otherwise fill the gap with zeros before the first
	// write far into the file: seconds of stalled disk for big files.
	makeSparse(f)
	d.mu.Lock()
	d.file = f
	d.mu.Unlock()

	d.mu.Lock()
	needMirrors := len(d.Mirrors) > 0 && d.mirrors == nil && d.lanSrc == ""
	d.mu.Unlock()
	if needMirrors {
		m.checkMirrors(ctx, d)
	}
	tuneCtx, stopTune := context.WithCancel(ctx)
	m.mu.Lock()
	auto, maxConns := m.settings.AutoConnections, m.settings.Connections
	m.mu.Unlock()
	d.mu.Lock()
	tune := auto && !d.FixedConns && d.Resumable && d.Size >= 8<<20 && d.lanSrc == "" && !isFTP(d.URL) && maxConns > 2
	if tune {
		d.Connections = autoStartConns(maxConns)
		workers = d.Connections
	}
	d.mu.Unlock()
	if tune {
		go m.autoTune(tuneCtx, d, maxConns)
	}
	var lastErr error
	for attempt := 0; ; attempt++ {
		lastErr = m.superviseWorkers(ctx, d, f, workers)
		if ctx.Err() == nil && !d.hasWork() && !m.lanCopyOK(ctx, d, f) {
			continue // the copy from the network didn't pass its check: download it from the internet
		}
		if ctx.Err() != nil || attempt >= 4 {
			break
		}
		d.mu.Lock()
		fromLAN := d.lanSrc != ""
		d.mu.Unlock()
		if fromLAN && d.hasWork() {
			// The other computer went away: carry on from the internet.
			d.mu.Lock()
			d.lanSrc, d.Note = "", ""
			d.mu.Unlock()
		} else if linkGone(lastErr) && m.autoRefresh(ctx, d) {
			// got a fresh link: carry on where it stopped
		} else if errors.Is(lastErr, errFileChanged) && d.Restarts < 2 {
			// The file on the server isn't the one we started with: mixing the two
			// would give a broken file, so start this one again.
			d.mu.Lock()
			d.Restarts++
			d.Note = "The file changed on the server while downloading, so Jasqet started it again (a mix of old and new would be broken)"
			d.ETag, d.LastModified, d.okValidators = "", "", nil
			zp := d.Zip
			d.mu.Unlock()
			if zp != nil {
				// a file inside a ZIP: its place in the ZIP may have changed too
				lastErr = errors.New("the ZIP changed on the server. Open it again with \"Look inside\" to take the file out")
				break
			}
			if err := m.probe(ctx, d); err != nil {
				lastErr = err
				break
			}
			if err := f.Truncate(0); err != nil {
				lastErr = err
				break
			}
			d.mu.Lock()
			d.Segments = initialSegments(d.Size, d.Resumable, d.Connections)
			d.mu.Unlock()
			m.saveSoon()
		} else {
			break
		}
		d.mu.Lock()
		for _, s := range d.Segments {
			s.active, s.failed = false, false
		}
		d.mu.Unlock()
	}
	stopTune()
	d.mu.Lock()
	if d.lanSrc != "" {
		d.lanSrc = ""
		d.Note = "Copied from another computer on your network (no internet data used)"
	}
	complete := true
	for _, s := range d.Segments {
		if !s.done() {
			complete = false
		}
	}
	d.mu.Unlock()
	syncErr := f.Sync()
	if complete {
		unsparse(f) // every byte is written: back to an ordinary file
	}
	d.mu.Lock()
	d.file = nil
	d.mu.Unlock()
	f.Close()

	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !complete {
		if lastErr == nil {
			lastErr = errors.New("download stopped before finishing")
		}
		return lastErr
	}
	if syncErr != nil {
		return syncErr
	}
	return m.finish(d)
}

// initialSegments splits the file evenly across connections.
func initialSegments(size int64, resumable bool, conns int) []*Segment {
	if !resumable || size <= 0 {
		end := size - 1
		if size <= 0 {
			end = -1
		}
		return []*Segment{{Start: 0, End: end}}
	}
	n := int64(conns)
	if n < 1 {
		n = 1
	}
	if size/n < minInitialChunk {
		n = size / minInitialChunk
		if n < 1 {
			n = 1
		}
	}
	chunk := size / n
	segs := make([]*Segment, 0, n)
	for i := int64(0); i < n; i++ {
		start := i * chunk
		end := start + chunk - 1
		if i == n-1 {
			end = size - 1
		}
		segs = append(segs, &Segment{Start: start, End: end, Pos: start})
	}
	return segs
}

// claim gives a worker something to do: an idle unfinished piece if there is
// one, otherwise it shares the piece that would take longest to finish with
// this connection ("work stealing"), so every connection stays busy until the
// very last byte. limited: a speed limit is on, so sharing pieces can't make
// the download faster and only the old, coarse splitting is done.
func (d *Download) claim(limited bool) *Segment {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.compactLocked()
	d.noLineUp = limited
	if d.sequentialLocked() {
		// someone is watching: first where the player jumped to, then the
		// connections line up right after the playing point (preview.go)
		if s := d.sequentialClaimLocked(); s != nil {
			s.src = d.pickSourceLocked()
			s.rate, s.began, s.lastData, s.reqTo = 0, time.Time{}, time.Time{}, 0
			return s
		}
	}
	preview := time.Now().Before(d.previewUntil)
	var best *Segment
	for _, s := range d.Segments {
		if !s.active && !s.done() && !s.failed {
			if best == nil {
				best = s
				continue
			}
			if preview {
				if a, b := d.fromPlayheadLocked(s), d.fromPlayheadLocked(best); a != b {
					if a < b {
						best = s
					}
					continue
				}
			}
			if s.remaining() > best.remaining() {
				best = s
			}
		}
	}
	if best != nil {
		best.active = true
		best.src = d.pickSourceLocked()
		best.rate, best.began, best.lastData, best.reqTo = 0, time.Time{}, time.Time{}, 0
		return best
	}
	if !d.Resumable {
		return nil
	}
	victim, at := d.splitPointLocked(time.Now(), limited)
	if victim == nil {
		return nil
	}
	ns := &Segment{Start: at, End: victim.End, Pos: at, active: true, src: d.pickSourceLocked()}
	if at == victim.Pos {
		ns.handovers = victim.handovers + 1
	}
	victim.End = at - 1
	d.Segments = append(d.Segments, ns)
	return ns
}

// ---------- preview while downloading ----------

// fromPlayheadLocked: how far a piece is from where someone is watching (0 for
// the piece holding the playhead, then the pieces after it; the pieces behind
// it come last).
func (d *Download) fromPlayheadLocked(s *Segment) int64 {
	x := d.previewPos
	switch {
	case s.End >= 0 && x > s.End:
		return 1 << 62
	case s.Start <= x:
		return 0
	}
	return s.Start - x
}

// previewAvailable: how many bytes from x on are already on the disk (within
// one piece; ask again at the end of it).
func (m *Manager) previewAvailable(d *Download, x int64) int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.Status == StDone {
		return max(0, d.Size-x)
	}
	for _, s := range d.Segments {
		if x < s.Start || (s.End >= 0 && x > s.End) {
			continue
		}
		end := s.Pos
		if s.End >= 0 && end > s.End+1 {
			end = s.End + 1
		}
		return max(0, end-x)
	}
	return 0
}

// previewWant: someone is watching at byte x. If no connection will get
// there soon, the piece holding x is cut there, so the next free connection
// starts exactly where it's needed (one extra connection is added while
// someone watches). Jumping ahead in a video works the same way.
func (m *Manager) previewWant(d *Download, x int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.previewWantLocked(x)
}

func (d *Download) previewWantLocked(x int64) {
	d.previewPos, d.previewUntil = x, time.Now().Add(20*time.Second)
	if !d.Resumable || d.Status == StDone {
		return
	}
	for _, s := range d.Segments {
		if x < s.Start || (s.End >= 0 && x > s.End) {
			continue
		}
		if x < s.Pos || s.End < 0 {
			return
		}
		if s.handovers < 3 {
			switch {
			case s.active && s.stop != nil && s.silent(time.Now()):
				// Its connection has gone silent (a dead mobile connection, say):
				// don't keep the player waiting for the stall timeout. That
				// connection drops its request and starts again right here.
				s.stop()
				d.retakeLocked(s)
				return
			case s.failed && !s.active:
				// A connection gave up on it, but the player needs it now: one
				// more go (a few at most, since the server may be refusing it).
				d.retakeLocked(s)
				d.makeWayLocked(nil)
				return
			}
		}
		gap := x - s.Pos
		if gap < minPiece {
			return // whoever has (or takes) this piece gets there in a moment
		}
		if s.active && s.rate > 0 && float64(gap)/s.rate < 2 {
			return
		}
		ns := &Segment{Start: x, End: s.End, Pos: x}
		s.End = x - 1
		d.Segments = append(d.Segments, ns)
		d.pieceChangedLocked()
		d.makeWayLocked(s)
		return
	}
}

// retakeLocked: the rest of s, from its next byte, becomes a fresh piece for
// the next free connection.
func (d *Download) retakeLocked(s *Segment) {
	ns := &Segment{Start: s.Pos, End: s.End, Pos: s.Pos, handovers: s.handovers + 1}
	s.End = s.Pos - 1
	d.Segments = append(d.Segments, ns)
	d.pieceChangedLocked()
}

// makeWayLocked: if every connection is busy, the one working furthest from
// the playhead leaves its piece (to finish later) for what the player needs.
func (d *Download) makeWayLocked(skip *Segment) {
	if d.waiting != 0 {
		return // a connection with nothing to do takes it
	}
	var far *Segment
	for _, o := range d.Segments {
		if o.active && !o.done() && o != skip && o.stop != nil &&
			(far == nil || d.fromPlayheadLocked(o) > d.fromPlayheadLocked(far)) {
			far = o
		}
	}
	if far != nil && d.fromPlayheadLocked(far) > 0 {
		far.yield = true
		far.stop()
	}
}

// splitPointLocked chooses which running piece to share with a free
// connection, and where to cut it. Caller holds d.mu.
//
//   - A piece is shared when, at its own speed, it would still take more than
//     a few seconds: the free connection takes its second half. (The old rule,
//     "only pieces over 4 MB", left one connection finishing the last few MB
//     alone while the others sat idle, which was slow when servers cap each
//     connection.)
//   - A connection crawling far behind the others (a bad route, an overloaded
//     server) hands its whole remainder to the free connection.
func (d *Download) splitPointLocked(now time.Time, limited bool) (*Segment, int64) {
	var sum float64
	var measured int
	for _, s := range d.Segments {
		if s.active && s.rate > 0 {
			sum += s.rate
			measured++
		}
	}
	avg := 0.0
	if measured > 0 {
		avg = sum / float64(measured)
	}
	typical := d.connRate // also knows the pieces that already finished
	var best *Segment
	var bestETA float64
	takeAll := false
	for _, s := range d.Segments {
		if !s.active || s.End < 0 || s.done() {
			continue
		}
		rem := s.remaining()
		if s.failed {
			continue
		}
		if limited {
			// with a speed limit the only reason to split is to keep the pieces
			// reasonably sized
			if rem >= 2*minSplitSize && (best == nil || rem > best.remaining()) {
				best, takeAll = s, false
			}
			continue
		}
		// silent: nothing has arrived for a while (a connection that died
		// without saying so, as often happens on mobile networks, or a server
		// that takes long to answer); its old speed means nothing now
		quiet := !s.lastData.IsZero() && rem >= 64<<10 &&
			((!s.began.IsZero() && now.Sub(s.lastData) >= 4*time.Second) || now.Sub(s.lastData) >= 12*time.Second)
		var eta float64
		switch {
		case quiet:
			eta = math.Inf(1)
		case s.rate > 0:
			eta = float64(rem) / s.rate
		case avg > 0:
			eta = float64(rem) / avg
		case rem >= 2*minSplitSize:
			eta = math.Inf(1) // no speeds yet: split big pieces as before
		default:
			continue
		}
		settled := s.rate > 0 && !s.began.IsZero() && now.Sub(s.began) >= 3*time.Second
		crawling := quiet || (settled && typical > 0 && s.rate < typical/5 && eta > 2*splitAfter && rem >= 64<<10)
		if crawling && s.handovers >= 3 {
			crawling = false // it moved three times already: the server is just slow there
		}
		if !crawling && (rem < 2*minPiece || eta < splitAfter) {
			continue
		}
		if best == nil || eta > bestETA {
			best, bestETA, takeAll = s, eta, crawling
		}
	}
	if best == nil {
		return nil, 0
	}
	if takeAll {
		if best.stop != nil {
			best.stop() // its connection has nothing left to do: don't wait for it to notice
		}
		return best, best.Pos
	}
	return best, best.Pos + best.remaining()/2
}

// compactLocked merges neighbouring finished pieces, so a download that was
// split many times doesn't carry hundreds of them around. Caller holds d.mu.
func (d *Download) compactLocked() {
	if len(d.Segments) < 48 {
		return
	}
	segs := append([]*Segment(nil), d.Segments...)
	sort.Slice(segs, func(i, j int) bool { return segs[i].Start < segs[j].Start })
	out := segs[:0]
	for _, s := range segs {
		if n := len(out); n > 0 {
			p := out[n-1]
			if !p.active && !s.active && p.done() && s.done() && p.End >= 0 && p.End+1 == s.Start {
				p.End, p.Pos = s.End, s.End+1
				continue
			}
		}
		out = append(out, s)
	}
	d.Segments = out
}

// waitForWork returns a channel closed when a piece finishes or fails, or nil
// when no connection is working on anything (so nothing can turn up).
// The caller counts as waiting until it calls doneWaiting.
func (d *Download) waitForWork() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, s := range d.Segments {
		if s.active && !s.done() {
			if d.changed == nil {
				d.changed = make(chan struct{})
			}
			d.waiting++
			return d.changed
		}
	}
	return nil
}

func (d *Download) doneWaiting() {
	d.mu.Lock()
	d.waiting--
	d.mu.Unlock()
}

// pieceChangedLocked wakes connections waiting for work. Caller holds d.mu.
func (d *Download) pieceChangedLocked() {
	if d.changed != nil {
		close(d.changed)
		d.changed = nil
	}
}

// limited: a speed limit applies to this download.
func (m *Manager) limited(d *Download) bool {
	d.mu.Lock()
	lan, own := d.lanSrc != "", d.SpeedLimitKB > 0
	d.mu.Unlock()
	if lan {
		return false // copies from the local network aren't limited
	}
	return own || m.limiter.Rate() > 0
}

func (m *Manager) worker(ctx context.Context, d *Download, f *os.File) error {
	for {
		// FTP pieces each need a new login, so they're only split coarsely too
		seg := d.claim(m.limited(d) || isFTP(d.URL))
		if seg == nil {
			// Nothing to take right now. While other connections are still
			// working, one of their pieces may become worth sharing (or one may
			// fail and need picking up), so wait instead of leaving.
			wake := d.waitForWork()
			if wake == nil {
				return nil
			}
			select {
			case <-ctx.Done():
			case <-wake:
			case <-time.After(300 * time.Millisecond):
			}
			d.doneWaiting()
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		err := m.runSegment(ctx, d, seg, f)
		d.mu.Lock()
		seg.active = false
		if err != nil && ctx.Err() == nil && !seg.done() {
			// This connection used up its retries on it. Handing it to each
			// other connection in turn would just repeat that for minutes.
			seg.failed = true
		}
		d.pieceChangedLocked()
		d.mu.Unlock()
		if err != nil {
			return err
		}
	}
}

func (m *Manager) runSegment(ctx context.Context, d *Download, seg *Segment, f *os.File) error {
	retries := 0
	for {
		d.mu.Lock()
		finished, yield := seg.done(), seg.yield
		seg.yield = false
		d.mu.Unlock()
		if finished || yield {
			return nil // done, or someone watching needs this connection for another part
		}
		progressed, err := m.fetch(ctx, d, seg, f)
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		d.mu.Lock()
		takenOver, yield := seg.done(), seg.yield
		seg.yield = false
		resumable := d.Resumable
		d.mu.Unlock()
		if takenOver || yield {
			return nil // another connection took the rest of it (or needs this one): not a failure
		}
		if errors.Is(err, errResolvedStale) {
			continue // straight on through the original link
		}
		if errors.Is(err, errServerIgnoredRange) || errors.Is(err, errFileChanged) || isDiskFull(err) {
			return err
		}
		if progressed {
			if resumable {
				retries = 0 // it carries on from where it got to
			}
			d.sourceWorked(seg)
		}
		retries++
		limit := maxRetries
		if !resumable {
			// every try starts again from the first byte: don't spend the
			// whole file's data over and over on a link that keeps dropping
			limit = min(limit, 3)
		}
		if d.switchSource(seg, err) {
			retries = 0 // carry on from another mirror
			continue
		}
		if retries > limit || (linkGone(err) && retries >= 2) {
			return err // an expired link won't come back by itself: get a fresh one
		}
		wait := time.Duration(retries) * time.Second
		if wait > 10*time.Second {
			wait = 10 * time.Second
		}
		if busy, after := busyErr(err); busy {
			// "Too many requests": slow down and use fewer connections.
			d.mu.Lock()
			d.pushback++
			d.mu.Unlock()
			wait = max(after, time.Duration(retries)*3*time.Second)
			retries = min(retries, maxRetries-2) // the server is busy, not broken
		}
		if progressed && wait > time.Second {
			wait = time.Second // it was working a moment ago: reconnect quickly
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// fetch streams one HTTP response into the segment's part of the file.
// It returns nil when the segment is finished.
func (m *Manager) fetch(ctx context.Context, d *Download, seg *Segment, f *os.File) (bool, error) {
	if isFTP(d.URL) {
		return m.fetchFTP(ctx, d, seg, f)
	}
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stall := time.AfterFunc(stallTimeout, cancel)
	defer stall.Stop()

	d.mu.Lock()
	src := seg.src
	seg.stop = cancel
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		seg.stop = nil
		d.mu.Unlock()
	}()
	req, link, target, err := m.segmentRequest(reqCtx, d, src)
	if err != nil {
		return false, err
	}
	cached := target != ""
	d.mu.Lock()
	pos, end, resumable, start := seg.Pos, seg.End, d.Resumable, seg.Start
	lan := d.lanSrc != ""
	seg.lastData = time.Now()
	// Straight on from this connection's own last request on this piece: the
	// seam was just written by it, nothing to compare.
	carryOn := seg.reqTo == pos
	if end >= 0 && !d.noLineUp && d.sequentialLocked() {
		// Someone is watching: ask for about a second's worth at a time, so the
		// next connection can start exactly where this answer ends, and this
		// one is read to the end, which keeps its connection open for the next.
		if c := d.previewStepLocked(pos); end-pos+1 > c {
			end = pos + c - 1
		}
	}
	seg.reqTo = end + 1
	d.mu.Unlock()
	// Picking up a piece that was cut off: ask for a little of what we already
	// have too and compare, so a file that changed (or got damaged) is caught.
	overlap := int64(0)
	if resumable && !lan && pos-start >= seamCheck && !carryOn {
		overlap = seamCheck
	}
	base := int64(0)
	if resumable {
		if !lan {
			base = d.Offset // a file inside a ZIP on the server
		}
		if end >= 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", base+pos-overlap, base+end))
		} else {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", base+pos-overlap))
		}
	} else if pos != 0 {
		// can't resume: start again from the first byte, with an empty file so
		// nothing of the earlier try is left at the end
		d.mu.Lock()
		seg.Pos = 0
		d.mu.Unlock()
		pos = 0
		if err := f.Truncate(0); err != nil {
			return false, err
		}
	}

	resp, err := m.clientFor(d).Do(req)
	if err != nil {
		if ctx.Err() == nil && reqCtx.Err() != nil {
			err = errors.New("connection stalled")
		}
		if cached && ctx.Err() == nil {
			d.forgetResolved(src, target)
			return false, fmt.Errorf("%w: %v", errResolvedStale, err)
		}
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		if cached {
			d.forgetResolved(src, target)
			return false, fmt.Errorf("%w: %s", errResolvedStale, resp.Status)
		}
		return false, newStatusError(resp)
	}
	if resumable && resp.StatusCode != http.StatusPartialContent {
		if cached {
			d.forgetResolved(src, target)
			return false, errResolvedStale
		}
		return false, errServerIgnoredRange
	}
	if cached {
		d.mu.Lock()
		if r := d.resolved[src]; r != nil && r.to == target {
			r.worked = true
		}
		d.mu.Unlock()
	} else if resp.Request != nil && resp.Request.Response != nil && !lan {
		d.rememberResolved(src, link, resp.Request.URL.String()) // it really was redirected
	}
	if !lan {
		if err := m.checkVersion(ctx, d, src, resp.Header, f); err != nil {
			return false, err
		}
	}
	if overlap > 0 {
		theirs := make([]byte, overlap)
		if _, err := io.ReadFull(resp.Body, theirs); err != nil {
			return false, err
		}
		stall.Stop() // waiting for a speed limit isn't a stall
		m.throttle(ctx, d, int(overlap))
		stall.Reset(stallTimeout)
		ours := make([]byte, overlap)
		if _, err := f.ReadAt(ours, pos-overlap); err != nil {
			return false, err
		}
		if !bytes.Equal(ours, theirs) {
			d.mu.Lock()
			if ms := d.mirrors; ms != nil {
				// one of the links may have a different file: use only the main one now
				for i := 1; i < len(ms.ok); i++ {
					ms.ok[i] = false
				}
				ms.ok[0] = true
			}
			d.mu.Unlock()
			return false, errFileChanged
		}
	}
	// Did the server send exactly what was asked? Then, once the piece is done,
	// its answer has been read to the end and the connection can be reused.
	exact := resumable && end >= 0 && resp.ContentLength == end-(pos-overlap)+1
	return m.pump(ctx, reqCtx, d, seg, f, resp.Body, stall, end, exact)
}

// pump copies a stream into the segment's part of the file until the segment
// is done. askedEnd is the last byte the request asked for; exact means the
// answer is exactly that long.
//
// It reads whatever arrives (rather than waiting to fill a big buffer), so the
// stall timer only fires when nothing arrives at all; time spent waiting for a
// speed limit doesn't count as a stall. (Before, a connection slower than
// 8 KB/s, which is normal with a speed limit or a weak mobile signal, was cut
// off and reopened every 30 seconds, throwing away the data already on its way.)
// Data is written in batches of up to 256 KB, at least four times a second.
func (m *Manager) pump(ctx, reqCtx context.Context, d *Download, seg *Segment, f *os.File, body io.Reader, stall *time.Timer, askedEnd int64, exact bool) (bool, error) {
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	buf := *bp
	progressed := false
	filled := 0
	uncounted := 0 // bytes read but not yet counted / limited
	defer func() {
		if uncounted > 0 {
			m.throttle(ctx, d, uncounted) // everything that arrived counts toward the data budget
		}
	}()
	lastFlush := time.Now()
	winStart, winBytes := time.Now(), int64(0) // for this piece's speed
	d.mu.Lock()
	seg.began = time.Now()
	d.mu.Unlock()

	flush := func() error {
		if filled == 0 {
			return nil
		}
		d.mu.Lock()
		p, e := seg.Pos, seg.End
		d.mu.Unlock()
		w := int64(filled)
		if e >= 0 && p+w-1 > e {
			w = e - p + 1 // another connection took over the rest of this range
		}
		filled = 0
		lastFlush = time.Now()
		if w <= 0 {
			return nil
		}
		if _, err := f.WriteAt(buf[:w], p); err != nil {
			return err
		}
		progressed = true
		winBytes += w
		d.mu.Lock()
		seg.Pos += w
		seg.lastData = time.Now()
		if el := time.Since(winStart); el >= 500*time.Millisecond {
			inst := float64(winBytes) / el.Seconds()
			if seg.rate == 0 {
				seg.rate = inst
			} else {
				seg.rate = seg.rate*0.5 + inst*0.5
			}
			if d.connRate == 0 {
				d.connRate = seg.rate
			} else {
				d.connRate = d.connRate*0.9 + seg.rate*0.1
			}
			winStart, winBytes = time.Now(), 0
		}
		d.mu.Unlock()
		return nil
	}

	for {
		// Read no further than this piece now reaches: another connection may
		// have taken over the rest of it.
		d.mu.Lock()
		p, e := seg.Pos, seg.End
		checkYield := time.Now().After(seg.yieldAt)
		if checkYield {
			seg.yieldAt = time.Now().Add(250 * time.Millisecond)
			if d.shouldYieldLocked(seg) {
				seg.yield = true // runSegment sees this: not a failure
				d.mu.Unlock()
				if err := flush(); err != nil {
					return progressed, err
				}
				return progressed, errYield
			}
		}
		d.mu.Unlock()
		want := len(buf) - filled
		if askedEnd >= 0 && (e < 0 || askedEnd < e) {
			e = askedEnd // the request asked for less than the piece (preview asks in steps)
		}
		if e >= 0 {
			left := e - (p + int64(filled)) + 1
			if left <= 0 {
				if err := flush(); err != nil {
					return progressed, err
				}
				if exact && e == askedEnd {
					var one [1]byte
					body.Read(one[:]) // the end of the answer: keeps the connection reusable
				}
				return progressed, nil
			}
			if left < int64(want) {
				want = int(left)
			}
		}
		n, rerr := body.Read(buf[filled : filled+want])
		if n > 0 {
			filled += n
			uncounted += n
			stall.Reset(stallTimeout) // something arrived: not stalled
			if uncounted >= 64<<10 || rerr != nil {
				// waiting for a speed limit isn't a stall
				stall.Stop()
				m.throttle(ctx, d, uncounted)
				stall.Reset(stallTimeout)
				uncounted = 0
			}
			if filled == len(buf) || time.Since(lastFlush) >= flushEvery {
				if err := flush(); err != nil {
					return progressed, err
				}
			}
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			if err := flush(); err != nil {
				return progressed, err
			}
			d.mu.Lock()
			defer d.mu.Unlock()
			if seg.End < 0 {
				// unknown size: a clean end of the stream is the end of the file;
				// a cut-off one is not (the file would be silently short)
				if rerr != io.EOF {
					return progressed, errors.New("connection closed early")
				}
				seg.End = seg.Pos - 1
				d.Size = seg.Pos
				return progressed, nil
			}
			if seg.done() {
				return progressed, nil
			}
			if rerr == io.EOF && askedEnd >= 0 && seg.Pos > askedEnd {
				// the whole answer to a request for part of the piece (preview
				// asks in steps; Go gives its last bytes with the EOF): not a
				// dropped connection, so no waiting before the next step
				return progressed, nil
			}
			return progressed, errors.New("connection closed early")
		}
		if rerr != nil {
			if err := flush(); err != nil { // keep what arrived
				return progressed, err
			}
			if ctx.Err() == nil && reqCtx.Err() != nil {
				return progressed, errors.New("connection stalled")
			}
			return progressed, rerr
		}
	}
}

// finish moves the .part file to its final name.
func (m *Manager) finish(d *Download) error {
	d.mu.Lock()
	part := d.partPath()
	dir, name := d.Dir, d.FileName
	d.mu.Unlock()

	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	final := filepath.Join(dir, name)
	for i := 1; ; i++ {
		if _, err := os.Stat(final); err != nil {
			break
		}
		final = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
	}
	d.mu.Lock()
	zp := d.Zip
	d.mu.Unlock()
	if zp != nil {
		if err := unpackZipPart(part, final, zp); err != nil {
			return err
		}
		d.mu.Lock()
		d.Note = fmt.Sprintf("Taken out of the ZIP on the server: downloaded %s instead of the whole ZIP", humanSize(d.Size))
		d.Size = zp.Size
		d.mu.Unlock()
	} else if err := renamePatiently(part, final); err != nil {
		return err
	}
	d.mu.Lock()
	d.FileName = filepath.Base(final)
	d.mu.Unlock()
	return nil
}

// ---------- helpers ----------

var contentRangeRe = regexp.MustCompile(`/(\d+)\s*$`)

func parseContentRangeTotal(v string) int64 {
	m := contentRangeRe.FindStringSubmatch(v)
	if m == nil {
		return -1
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return -1
	}
	return n
}

func filenameFromResponse(resp *http.Response) string {
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if _, params, err := mime.ParseMediaType(cd); err == nil {
			if fn := params["filename"]; fn != "" {
				return fn
			}
		}
	}
	name := ""
	if resp.Request != nil && resp.Request.URL != nil {
		name = filenameFromURL(resp.Request.URL.String())
	}
	if filepath.Ext(name) == "" {
		ct, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
		if exts, _ := mime.ExtensionsByType(ct); len(exts) > 0 && ct != "application/octet-stream" && ct != "text/plain" {
			if name == "" {
				name = "download"
			}
			name += exts[len(exts)-1]
		}
	}
	if name == "" {
		name = "download"
	}
	return name
}

func filenameFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	base := path.Base(u.Path)
	if base == "/" || base == "." {
		return ""
	}
	if un, err := url.PathUnescape(base); err == nil {
		base = un
	}
	return base
}

var badChars = regexp.MustCompile(`[<>:"/\\|?*\x00-\x1f]`)

// invisChars make a name look like something else: a right-to-left mark turns
// "invoice" + U+202E + "fdp.exe" into "invoiceexe.pdf" on screen.
var invisChars = regexp.MustCompile(`[\x{200B}-\x{200F}\x{202A}-\x{202E}\x{2066}-\x{2069}\x{FEFF}]`)

// deviceName: Windows treats these as devices, not files ("nul.mp4" too).
var deviceName = regexp.MustCompile(`(?i)^(con|prn|aux|nul|conin\$|conout\$|com[0-9¹²³]|lpt[0-9¹²³])$`)

var titleFixer = strings.NewReplacer(": ", " - ", ":", "-", " | ", " - ", "|", "-", "\"", "'", "/", "-", "\\", "-", "?", "", "*", "", "<", "", ">", "")

func sanitizeFilename(name string) string {
	name = titleFixer.Replace(name)
	name = badChars.ReplaceAllString(name, "_")
	name = invisChars.ReplaceAllString(name, "")
	name = strings.Join(strings.Fields(name), " ")
	name = strings.Trim(name, " .")
	if name == "" {
		name = "download"
	}
	if len(name) > 200 {
		ext := filepath.Ext(name)
		if len(ext) > 20 {
			ext = ""
		}
		cut := 200 - len(ext)
		for cut > 0 && !utf8.RuneStart(name[cut]) { // don't cut a letter in half
			cut--
		}
		name = strings.TrimRight(name[:cut], " .") + ext
	}
	if stem, _, _ := strings.Cut(name, "."); deviceName.MatchString(strings.TrimRight(stem, " ")) {
		name = "_" + name
	}
	return name
}

// uniquePath returns dir/name, adding " (1)", " (2)"... if something already uses that name.
func uniquePath(dir, name string, taken func(string) bool) string {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 0; ; i++ {
		candidate := name
		if i > 0 {
			candidate = fmt.Sprintf("%s (%d)%s", stem, i, ext)
		}
		p := filepath.Join(dir, candidate)
		if _, err := os.Stat(p); err == nil {
			continue
		}
		if _, err := os.Stat(p + ".part"); err == nil {
			continue
		}
		if _, err := os.Stat(p + ".parts"); err == nil {
			continue
		}
		if taken != nil && taken(p) {
			continue
		}
		return p
	}
}

// Limiter is a shared token bucket that caps total download speed (0 = unlimited).
type Limiter struct {
	mu     sync.Mutex
	rate   int64 // bytes per second
	tokens float64
	last   time.Time
}

func (l *Limiter) Rate() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rate
}

func (l *Limiter) SetRate(bps int64) {
	l.mu.Lock()
	l.rate = bps
	l.tokens = 0
	l.last = time.Now()
	l.mu.Unlock()
}

func (l *Limiter) Wait(ctx context.Context, n int) {
	l.mu.Lock()
	if l.rate <= 0 {
		l.mu.Unlock()
		return
	}
	now := time.Now()
	l.tokens += now.Sub(l.last).Seconds() * float64(l.rate)
	if l.tokens > float64(l.rate) {
		l.tokens = float64(l.rate)
	}
	l.last = now
	l.tokens -= float64(n)
	var wait time.Duration
	if l.tokens < 0 {
		wait = time.Duration(-l.tokens / float64(l.rate) * float64(time.Second))
	}
	l.mu.Unlock()
	if wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
		}
	}
}

// superviseWorkers runs the connections for one download and adds or removes
// connections when the user changes the number while it's downloading.
func (m *Manager) superviseWorkers(outer context.Context, d *Download, f *os.File, workers int) error {
	ctx, stopAll := context.WithCancel(outer)
	defer stopAll()
	var errMu sync.Mutex
	var lastErr error
	exited := make(chan int, 64)
	cancels := map[int]context.CancelFunc{}
	nextID := 0
	spawn := func() {
		wctx, wcancel := context.WithCancel(ctx)
		id := nextID
		nextID++
		cancels[id] = wcancel
		go func() {
			err := m.worker(wctx, d, f)
			if err != nil && wctx.Err() == nil {
				errMu.Lock()
				lastErr = err
				errMu.Unlock()
				if linkGone(err) || errors.Is(err, errFileChanged) {
					stopAll() // no point in the other connections carrying on
				}
			}
			wcancel()
			exited <- id
		}()
	}
	want := func() int {
		d.mu.Lock()
		defer d.mu.Unlock()
		if !d.Resumable {
			return 1
		}
		n := max(1, d.Connections)
		if time.Now().Before(d.previewUntil) {
			n++ // someone is watching: one more connection fetches the part they need
		}
		return n
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	stopping := map[int]bool{}
	for {
		for i := 0; i < workers; i++ {
			spawn()
		}
		for len(cancels) > 0 {
			select {
			case id := <-exited:
				delete(cancels, id)
				delete(stopping, id)
			case <-tick.C:
				w := want()
				alive := len(cancels) - len(stopping)
				errMu.Lock()
				failing := lastErr != nil
				errMu.Unlock()
				if w > alive && !failing && ctx.Err() == nil && d.hasWork() {
					// more connections asked for: add them (not to replace ones that
					// gave up on an error: that's decided below, once all have stopped)
					for i := alive; i < w; i++ {
						spawn()
					}
				} else if w < alive {
					// stop the newest connections; their unfinished parts go to the others
					ids := make([]int, 0, len(cancels))
					for id := range cancels {
						if !stopping[id] {
							ids = append(ids, id)
						}
					}
					sort.Sort(sort.Reverse(sort.IntSlice(ids)))
					for i := 0; i < alive-w && i < len(ids); i++ {
						cancels[ids[i]]()
						stopping[ids[i]] = true
					}
				}
			}
		}
		errMu.Lock()
		failed := lastErr != nil
		errMu.Unlock()
		if outer.Err() != nil || failed || !d.hasWork() {
			break
		}
		workers = 1 // a stopped connection left work behind: carry on
	}
	errMu.Lock()
	defer errMu.Unlock()
	return lastErr
}

func (d *Download) hasWork() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, s := range d.Segments {
		if !s.done() {
			return true
		}
	}
	return false
}

var patternRe = regexp.MustCompile(`\[(\d+)-(\d+)\]|\[([a-zA-Z])-([a-zA-Z])\]`)

// expandPattern turns https://site/part[1-3].zip into part1, part2, part3.
// [01-10] keeps the leading zero; [a-e] works for letters. One range per link.
func expandPattern(link string) ([]string, error) {
	m := patternRe.FindStringSubmatchIndex(link)
	if m == nil {
		return []string{link}, nil
	}
	pre, post := link[:m[0]], link[m[1]:]
	var out []string
	if m[2] >= 0 {
		a, b := link[m[2]:m[3]], link[m[4]:m[5]]
		from, _ := strconv.Atoi(a)
		to, _ := strconv.Atoi(b)
		if to < from {
			from, to = to, from
		}
		if to-from >= 2000 {
			return nil, errors.New("that pattern makes too many links (2000 at most)")
		}
		width := 0
		if len(a) > 1 && a[0] == '0' {
			width = len(a)
		}
		for i := from; i <= to; i++ {
			out = append(out, fmt.Sprintf("%s%0*d%s", pre, width, i, post))
		}
	} else {
		a, b := link[m[6]], link[m[8]]
		if b < a {
			a, b = b, a
		}
		for c := a; c <= b; c++ {
			out = append(out, pre+string(c)+post)
		}
	}
	return out, nil
}

func peerName(m *Manager, addr string) string {
	m.lan.mu.Lock()
	defer m.lan.mu.Unlock()
	if p, ok := m.lan.peers[addr]; ok && p.Name != "" {
		return p.Name
	}
	return addr
}
