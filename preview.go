package main

// Preview while downloading: watching a file that is still downloading.
// Two things work together:
//
//   - Where a player asks for a byte that hasn't arrived (an MP4's index at
//     the end of the file, or a jump ahead), the piece is cut exactly there and
//     a connection starts on it straight away (previewWant in engine.go).
//   - The connections follow the players: right after where each one reads,
//     they line up one after another in small pieces, instead of each working
//     on its own far-off piece. So even a server that limits every connection
//     (most file hosts do) feeds the player with all of them.
//
// When the viewer closes, the download goes back to its normal order.

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

var errYield = errors.New("making way for the part the viewer needs")

const previewSplit = 128 << 10 // the smallest step pieces are shared in near a player

const previewTailSplit = 16 << 10 // …and at the end of the file (previewStepLocked)

// previewWindowLocked: how far past what a player has counts as "needed
// soon": about 2.5% of the file (a few minutes of a film), 8 to 64 MB.
func (d *Download) previewWindowLocked() int64 {
	return min(max(d.Size/40, 8<<20), 64<<20)
}

// sequentialLocked: a player is reading this download right now (the viewer
// merely being open on it doesn't count: a paused or closed player leaves
// the download to its normal order after a few seconds). Caller holds d.mu.
func (d *Download) sequentialLocked() bool {
	if !d.Resumable || d.Size <= 0 || !time.Now().Before(d.previewUntil) {
		return false
	}
	for _, h := range d.previewHeads {
		if time.Since(h.at) < 20*time.Second {
			return true
		}
	}
	return false
}

// previewChunkLocked: how much one request asks for while someone watches.
// About a second of one connection, so the next one lines up right behind it.
func (d *Download) previewChunkLocked() int64 {
	return min(max(int64(d.connRate), previewSplit), 8<<20)
}

// previewStepLocked: how much a request from byte pos asks for while someone
// watches. A player reading the end of the file (an MP4's index there) needs
// all of the rest before it can start. In the last stretch (a step for
// every connection) the steps are about a second even below previewSplit,
// so the connections fetch it together. (At 25 KB/s per connection a 190 KB
// index was one 128 KB step plus 62 KB, too little to share: one connection
// fetched it all, 7 s.)
func (d *Download) previewStepLocked(pos int64) int64 {
	c := d.previewChunkLocked()
	last := d.Size - int64(d.Connections)*c // where the last stretch starts
	if d.connRate <= 0 || int64(d.connRate) >= previewSplit || pos < last {
		return c
	}
	for _, h := range d.previewHeads {
		if h.pos >= last {
			return max(int64(d.connRate), previewTailSplit)
		}
	}
	return c
}

// silent: nothing has arrived on this piece's connection for a while (the
// same rule splitPointLocked uses for a connection that died quietly).
func (s *Segment) silent(now time.Time) bool {
	return !s.lastData.IsZero() &&
		((!s.began.IsZero() && now.Sub(s.lastData) >= 4*time.Second) || now.Sub(s.lastData) >= 12*time.Second)
}

// lineUpCutLocked: where a free connection can join piece s so that it
// starts right after what s's connection is fetching now (0: not worth it).
// Cut where s's request ends, its answer is read to the end and its
// connection stays open; only a request that runs to the piece's end (one
// from before anyone watched) has to be cut short.
func (d *Download) lineUpCutLocked(s *Segment) int64 {
	if !s.active || s.failed || s.End < 0 || s.done() {
		return 0
	}
	c := d.previewStepLocked(s.Pos)
	at := s.Pos + c // not asking yet: its request will end there
	switch {
	case s.reqTo > s.Pos && s.reqTo <= s.End:
		at = s.reqTo // its request ends inside the piece
	case s.reqTo > s.Pos && s.End-s.Pos+1 <= 4*c:
		return 0 // its request runs to the piece's end, which is near: let it finish
	}
	if s.End-at+1 < min(64<<10, c/2) { // (64 KB, or half a step where steps are small)
		return 0
	}
	return at
}

// previewHead: where one player (one request from the viewer) is reading.
type previewHead struct {
	pos     int64
	at      time.Time
	waiting bool // it's waiting for this byte to arrive
}

// previewAt: a player is reading at byte x (waiting: the byte hasn't arrived
// yet). The connections follow the players: wherever one reads, the part
// right after it comes first.
func (m *Manager) previewAt(d *Download, who any, x int64, waiting bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	d.previewUntil = now.Add(20 * time.Second)
	if d.previewHeads == nil {
		d.previewHeads = map[any]*previewHead{}
	}
	h := d.previewHeads[who]
	if h == nil {
		h = &previewHead{}
		d.previewHeads[who] = h
	}
	moved := h.pos != x || h.waiting != waiting
	h.pos, h.at, h.waiting = x, now, waiting
	for k, o := range d.previewHeads {
		if now.Sub(o.at) > 10*time.Minute {
			delete(d.previewHeads, k)
		}
	}
	if waiting {
		// cut the piece exactly there (engine.go), now that the player is known
		// to wait there, so a connection freed by it goes straight to it
		d.previewWantLocked(x)
		if moved {
			d.pieceChangedLocked() // wake connections with nothing to do
		}
	}
}

// previewDone: that player stopped reading (it closed its request).
func (m *Manager) previewDone(d *Download, who any) {
	d.mu.Lock()
	delete(d.previewHeads, who)
	d.mu.Unlock()
}

// readyEndLocked: where the unbroken downloaded part from byte x ends
// (segs sorted by Start).
func readyEnd(x int64, segs []*Segment) int64 {
	f := x
	for _, s := range segs {
		if s.Start > f {
			break
		}
		hi := s.Pos
		if s.End >= 0 && hi > s.End+1 {
			hi = s.End + 1
		}
		f = max(f, hi)
	}
	return f
}

func (d *Download) sortedSegmentsLocked() []*Segment {
	segs := append([]*Segment(nil), d.Segments...)
	sort.Slice(segs, func(i, j int) bool { return segs[i].Start < segs[j].Start })
	return segs
}

// frontierLocked is where the unbroken downloaded part from the start ends.
func (d *Download) frontierLocked() int64 { return readyEnd(0, d.sortedSegmentsLocked()) }

// zone: a stretch needed soon, from where a player's downloaded data runs
// out to a window ahead.
type zone struct{ from, to int64 }

// zonesLocked: the stretches the players need soon.
func (d *Download) zonesLocked() []zone {
	segs := d.sortedSegmentsLocked()
	win := d.previewWindowLocked()
	var zs []zone
	for _, h := range d.previewHeads {
		if h.pos >= d.Size {
			continue
		}
		f := readyEnd(h.pos, segs)
		zs = append(zs, zone{f, f + win})
	}
	return zs
}

// distance: how far a piece's next byte is past the nearest zone's start
// (-1: not needed soon).
func distance(s *Segment, zs []zone) int64 {
	best := int64(-1)
	for _, z := range zs {
		if (s.End >= 0 && s.End < z.from) || s.Pos > z.to {
			continue
		}
		dd := max(0, s.Pos-z.from)
		if best < 0 || dd < best {
			best = dd
		}
	}
	return best
}

// sequentialClaimLocked picks work for a connection while someone watches;
// nil means nothing is needed soon (the normal rules take over).
func (d *Download) sequentialClaimLocked() *Segment {
	// 1. a player waiting where no connection is: start one exactly there
	var waiting []*previewHead
	for _, h := range d.previewHeads {
		if h.waiting {
			waiting = append(waiting, h)
		}
	}
	sort.Slice(waiting, func(i, j int) bool { return waiting[i].at.After(waiting[j].at) })
	for _, h := range waiting {
		x := h.pos
		for _, s := range d.Segments {
			if s.done() || x < s.Start || (s.End >= 0 && x > s.End) {
				continue
			}
			switch {
			// already here, or a connection is on it (one that went silent is
			// dealt with by previewWantLocked, while the player waits)
			case x < s.Pos, s.active, s.failed, s.End < 0:
			case x-s.Pos < previewSplit:
				s.active = true
				return s
			default:
				ns := &Segment{Start: x, End: s.End, Pos: x, active: true}
				s.End = x - 1
				d.Segments = append(d.Segments, ns)
				return ns
			}
			break
		}
	}
	zs := d.zonesLocked()
	// 2. an idle piece a player needs soon, the nearest first
	var best *Segment
	bestD := int64(-1)
	for _, s := range d.Segments {
		if s.active || s.done() || s.failed {
			continue
		}
		if dd := distance(s, zs); dd >= 0 && (best == nil || dd < bestD) {
			best, bestD = s, dd
		}
	}
	if best != nil {
		best.active = true
		return best
	}
	// 3. help with the piece nearest a player: the new connection starts just
	// past what that one fetches now, so they line up. (Not with a speed
	// limit, where it can't make anything faster, nor over FTP, where every
	// piece needs a new login.)
	if d.noLineUp {
		return nil
	}
	if best, at := d.nearestCutLocked(zs, nil); best != nil {
		ns := &Segment{Start: at, End: best.End, Pos: at, active: true}
		best.End = at - 1
		d.Segments = append(d.Segments, ns)
		return ns
	}
	return nil
}

// nearestCutLocked: the running piece nearest a player that a free
// connection can join, and where (skip: a piece not to consider).
func (d *Download) nearestCutLocked(zs []zone, skip *Segment) (*Segment, int64) {
	var best *Segment
	var bestAt, bestD int64
	for _, s := range d.Segments {
		if s == skip {
			continue
		}
		at := d.lineUpCutLocked(s)
		if at == 0 {
			continue
		}
		// what the new connection would fetch must itself be needed soon
		if dd := distance(&Segment{Pos: at, End: s.End}, zs); dd >= 0 && (best == nil || dd < bestD) {
			best, bestAt, bestD = s, at, dd
		}
	}
	return best, bestAt
}

func (d *Download) rangeDoneLocked(from, to int64) bool {
	for _, s := range d.Segments {
		if s.End < 0 {
			continue
		}
		if max(s.Pos, from) < min(s.End+1, to) {
			return false // part of [from, to) still to download in this piece
		}
	}
	return true
}

// shouldYieldLocked: while someone watches, a connection working on a part
// nobody needs soon hands its place to one that is (what it got is kept).
func (d *Download) shouldYieldLocked(seg *Segment) bool {
	if !d.sequentialLocked() {
		return false
	}
	zs := d.zonesLocked()
	if distance(seg, zs) >= 0 {
		return false
	}
	for _, h := range d.previewHeads {
		if h.waiting && h.pos >= seg.Pos && (seg.End < 0 || h.pos <= seg.End) {
			return false // a player is waiting for this very piece
		}
	}
	// is there near work this connection would get (the same as claim gives)?
	for _, s := range d.Segments {
		if s != seg && !s.active && !s.done() && !s.failed && distance(s, zs) >= 0 {
			return true
		}
	}
	if d.noLineUp {
		return false
	}
	s, _ := d.nearestCutLocked(zs, seg)
	return s != nil
}

// previewReader reads a file that's still downloading. Reading past what's
// downloaded asks for that part first and waits for it (the viewer shows
// "Waiting for download").
type previewReader struct {
	ctx  context.Context
	m    *Manager
	d    *Download
	size int64
	pos  int64

	f     *os.File // kept open between reads (shared, so the file can still be renamed when it finishes)
	fpath string
}

func (r *previewReader) Close() error {
	if r.f != nil {
		r.f.Close()
		r.f = nil
	}
	return nil
}

func (r *previewReader) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		r.pos = off
	case io.SeekCurrent:
		r.pos += off
	case io.SeekEnd:
		r.pos = r.size + off
	}
	if r.pos < 0 {
		r.pos = 0
	}
	return r.pos, nil
}

func (r *previewReader) Read(p []byte) (int, error) {
	if r.pos >= r.size {
		return 0, io.EOF
	}
	d := r.d
	setWait := func(on bool) {
		d.mu.Lock()
		if on {
			d.previewWait = r.pos + 1
		} else if d.previewWait == r.pos+1 {
			d.previewWait = 0
		}
		d.mu.Unlock()
	}
	for idle := time.Duration(0); ; idle += 150 * time.Millisecond {
		if n := r.m.previewAvailable(d, r.pos); n > 0 {
			setWait(false)
			r.m.previewAt(d, r, r.pos, false)
			k := int(min(int64(len(p)), n, 1<<20))
			for try := 0; try < 2; try++ { // the file may be renamed as it finishes
				d.mu.Lock()
				path := d.partPath()
				if d.Status == StDone {
					path = filepath.Join(d.Dir, d.FileName)
				}
				d.mu.Unlock()
				if r.f == nil || r.fpath != path {
					r.Close()
					f, err := openShared(path)
					if err != nil {
						if try == 0 {
							time.Sleep(200 * time.Millisecond)
							continue
						}
						return 0, err
					}
					r.f, r.fpath = f, path
				}
				got, err := r.f.ReadAt(p[:k], r.pos)
				r.pos += int64(got)
				if err == io.EOF && got > 0 {
					err = nil
				}
				if err != nil && got == 0 && try == 0 {
					r.Close() // moved or finished under us: open it again
					continue
				}
				return got, err
			}
		}
		if r.m.get(d.ID) == nil {
			return 0, errors.New("the download was removed")
		}
		setWait(true)
		r.m.previewAt(d, r, r.pos, true)
		select {
		case <-r.ctx.Done():
			setWait(false)
			return 0, r.ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
		if idle > 10*time.Minute {
			setWait(false)
			return 0, errors.New("the download isn't moving")
		}
	}
}
