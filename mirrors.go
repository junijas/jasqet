package main

// Several links to the same file (mirrors): every connection takes a piece
// from whichever server is free, and a server that fails is simply dropped.
// Each mirror is checked against the main link before it's trusted, and the
// seams between pieces are compared, so a mirror with a different file can't
// slip bad data in.

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"sync"
)

type mirrorState struct {
	ok    []bool // per source (0 = main link): checked and working
	fails []int
	next  int
}

// pickSourceLocked chooses the server for the next piece. Caller holds d.mu.
func (d *Download) pickSourceLocked() int {
	ms := d.mirrors
	if ms == nil || d.lanSrc != "" {
		return 0
	}
	n := len(ms.ok)
	for i := 0; i < n; i++ {
		ms.next = (ms.next + 1) % n
		if ms.ok[ms.next] {
			return ms.next
		}
	}
	return 0
}

// sourceWorked: this server delivered data, so earlier hiccups are forgiven.
func (d *Download) sourceWorked(seg *Segment) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ms := d.mirrors; ms != nil && seg.src < len(ms.fails) {
		ms.fails[seg.src] = 0
	}
}

// sourceRequestLink: the link a source number stands for. Caller holds d.mu.
func (d *Download) sourceLinkLocked(src int) string {
	if src > 0 && src <= len(d.Mirrors) {
		return d.Mirrors[src-1]
	}
	return d.URL
}

// switchSource moves a piece to another server after this one failed.
func (d *Download) switchSource(seg *Segment, err error) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	ms := d.mirrors
	if ms == nil || seg.src >= len(ms.ok) {
		return false
	}
	ms.fails[seg.src]++
	if ms.fails[seg.src] < 2 && !linkGone(err) {
		return false
	}
	ms.ok[seg.src] = false
	alive := 0
	for _, ok := range ms.ok {
		if ok {
			alive++
		}
	}
	if alive == 0 {
		ms.ok[seg.src] = true // nothing else left: keep trying this one
		return false
	}
	seg.src = d.pickSourceLocked()
	return true
}

// checkMirrors asks every mirror for the file and keeps the ones that have
// exactly the same file as the main link (same size, same bytes in a sample).
func (m *Manager) checkMirrors(ctx context.Context, d *Download) {
	d.mu.Lock()
	mirrors := append([]string(nil), d.Mirrors...)
	size, resumable := d.Size, d.Resumable
	d.mu.Unlock()
	if len(mirrors) == 0 || !resumable || size <= 0 {
		return
	}
	ms := &mirrorState{ok: make([]bool, len(mirrors)+1), fails: make([]int, len(mirrors)+1)}
	ms.ok[0] = true
	const piece = 32 << 10
	off := max(0, size/2-piece/2)
	main, err := m.rangeBytes(ctx, d, off, min(piece, size))
	if err != nil {
		return
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	good := 0
	for i, link := range mirrors {
		wg.Add(1)
		go func(i int, link string) {
			defer wg.Done()
			tmp, err := m.checkLink(ctx, d, link, nil, true)
			if err != nil || tmp.Size != size || !tmp.Resumable {
				return
			}
			b, err := m.rangeBytes(ctx, tmp, off, int64(len(main)))
			if err != nil || !bytes.Equal(b, main) {
				return
			}
			mu.Lock()
			ms.ok[i+1] = true
			good++
			mu.Unlock()
		}(i, link)
	}
	wg.Wait()
	d.mu.Lock()
	d.mirrors = ms
	if good > 0 {
		d.Note = fmt.Sprintf("Downloading from %d servers at once", good+1)
	} else {
		d.Note = "None of the other links had the same file, so only the main link is used"
	}
	if good < len(mirrors) && good > 0 {
		d.Note += fmt.Sprintf(" (%d of the links didn't match and were left out)", len(mirrors)-good)
	}
	d.mu.Unlock()
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Hostname()
}
