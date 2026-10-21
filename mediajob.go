package main

// Downloading a video stream: fetch every chunk (many at once), then join them.

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

func (d *Download) partsDir() string { return filepath.Join(d.Dir, d.FileName) + ".parts" }

func loadPlan(dir string) *MediaPlan {
	b, err := os.ReadFile(filepath.Join(dir, "plan.json"))
	if err != nil {
		return nil
	}
	var p MediaPlan
	if json.Unmarshal(b, &p) != nil || len(p.Tracks) == 0 {
		return nil
	}
	return &p
}

func savePlan(dir string, p *MediaPlan) error {
	b, _ := json.Marshal(p)
	return os.WriteFile(filepath.Join(dir, "plan.json"), b, 0o644)
}

type segTask struct {
	path  string
	ref   SegRef
	whole bool // the only piece of its track, size not given: a single-file stream
}

// segCap: a piece whose size the playlist doesn't give stops here. Real HLS
// and DASH pieces last seconds and stay far below it (even 8K video is a few
// tens of MB a piece), so more means a broken or hostile server sending
// endless data. A single-file stream has no cap: it's written straight to
// disk like any download, and never read whole into memory.
var segCap int64 = 128 << 20

var errSegTooBig = errors.New("a piece of this video is far bigger than video pieces ever are, so Jasqet stopped (the site may be sending endless data)")

type keyCache struct {
	mu   sync.Mutex
	keys map[string][]byte
}

func (k *keyCache) get(ctx context.Context, m *Manager, rawURL string, o reqOpts) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if b, ok := k.keys[rawURL]; ok {
		return b, nil
	}
	b, _, _, err := m.fetchSmall(ctx, rawURL, o, 1024)
	if err != nil {
		return nil, fmt.Errorf("couldn't get the video's decryption key: %w", err)
	}
	if len(b) != 16 {
		return nil, errDRM
	}
	if k.keys == nil {
		k.keys = map[string][]byte{}
	}
	k.keys[rawURL] = b
	return b, nil
}

func (m *Manager) executeMedia(ctx context.Context, d *Download) error {
	for attempt := 0; ; attempt++ {
		err := m.executeMediaOnce(ctx, d)
		d.mu.Lock()
		kind := d.Kind
		d.mu.Unlock()
		if kind != "yt" || err == nil || ctx.Err() != nil || attempt >= 2 || !linkExpired(err) {
			return err
		}
		// The site refused the links: get fresh ones; if that fails too,
		// let yt-dlp download the parts itself.
		if attempt == 1 {
			d.mu.Lock()
			d.YTFallback = true
			d.mu.Unlock()
		}
	}
}

func linkExpired(err error) bool {
	s := err.Error()
	return strings.Contains(s, "403") || strings.Contains(s, "410") || strings.Contains(s, "wrong size")
}

func (m *Manager) executeMediaOnce(ctx context.Context, d *Download) error {
	d.mu.Lock()
	o := d.reqOpts()
	placed := d.Placed
	rawURL, variant, kind := d.URL, d.Variant, d.Kind
	d.mu.Unlock()

	if !placed {
		// Another Jasqet on the network may already have this video.
		if src, peer := m.lanFind(ctx, d); src != "" {
			m.placeFile(d)
			d.mu.Lock()
			dest := filepath.Join(d.Dir, d.FileName)
			parts := d.partsDir()
			d.segState = []byte{1}
			d.Status = StDownloading
			d.Note = "Copying from " + peerName(m, peer) + " on your network (no internet data used)"
			d.mu.Unlock()
			os.RemoveAll(parts)
			if err := m.lanCopyWhole(ctx, d, src, dest); err == nil {
				st, _ := os.Stat(dest)
				d.mu.Lock()
				if st != nil {
					d.Size = st.Size()
				}
				d.segState, d.segInflight = nil, 0
				d.Note = "Copied from another computer on your network (no internet data used)"
				d.mu.Unlock()
				return nil
			} else if ctx.Err() != nil {
				return ctx.Err()
			}
			// copy failed: download it normally
			d.mu.Lock()
			d.Placed, d.Note, d.segState = false, "", nil
			os.Remove(dest)
			d.mu.Unlock()
			placed = false
		}
	}

	var plan *MediaPlan
	if placed && kind != "yt" {
		d.mu.Lock()
		plan = loadPlan(d.partsDir())
		d.mu.Unlock()
	}
	if plan == nil {
		// Read the playlist first, so refused videos (DRM, live) leave nothing behind.
		var err error
		if kind == "yt" {
			plan, err = m.buildYTPlan(ctx, d) // fresh links every start
		} else {
			plan, err = m.BuildMediaPlan(ctx, rawURL, variant, o)
		}
		if err != nil {
			return err
		}
		d.mu.Lock()
		o = d.reqOpts()
		clip := d.Clip
		d.mu.Unlock()
		if clip != nil {
			if err := m.applyClip(ctx, plan, clip, o); err != nil {
				return err
			}
		}
	}
	if !placed {
		d.mu.Lock()
		dupOK, name := d.DupOK, d.FileName
		d.mu.Unlock()
		if !dupOK {
			if dup := m.findDuplicateName(name); dup != "" {
				d.mu.Lock()
				d.DupPath = dup
				d.mu.Unlock()
				return errDuplicate
			}
		}
		if plan.AudioOnly {
			d.mu.Lock()
			if strings.HasSuffix(d.FileName, ".mp4") {
				d.FileName = strings.TrimSuffix(d.FileName, ".mp4") + ".m4a"
			}
			d.mu.Unlock()
		}
		m.placeFile(d)
	}
	d.mu.Lock()
	dir := d.partsDir()
	d.mu.Unlock()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if kind == "yt" {
		// Same video and quality gives the same pieces, so finished ones are kept.
		old := loadPlan(dir)
		if old != nil && (old.chunkCount() != plan.chunkCount() || old.Fallback != plan.Fallback) {
			if olds, _ := filepath.Glob(filepath.Join(dir, "t*_*")); len(olds) > 0 {
				for _, f := range olds {
					os.Remove(f)
				}
			}
		}
		if err := savePlan(dir, plan); err != nil {
			return err
		}
	} else if loadPlan(dir) == nil {
		// A new link (refresh) for the same video should give the same chunks.
		// If it doesn't line up with what we have, start the chunks over.
		n := plan.chunkCount()
		d.mu.Lock()
		old := d.PlanChunks
		d.PlanChunks = n
		d.mu.Unlock()
		if old > 0 && old != n {
			if olds, _ := filepath.Glob(filepath.Join(dir, "t*_*")); len(olds) > 0 {
				for _, f := range olds {
					os.Remove(f)
				}
			}
		}
		if err := savePlan(dir, plan); err != nil {
			return err
		}
	}

	if plan.Fallback {
		if err := m.ytFallbackDownload(ctx, d, plan, dir); err != nil {
			return err
		}
		return m.joinAndFinish(ctx, d, plan, dir)
	}

	var tasks []segTask
	var place []float64 // how far into its track each piece is (0 = start, 1 = end)
	for ti, t := range plan.Tracks {
		if t.Init != nil {
			tasks = append(tasks, segTask{path: filepath.Join(dir, fmt.Sprintf("t%d_init", ti)), ref: *t.Init})
			place = append(place, -1)
		}
		for si, s := range t.Segs {
			whole := len(t.Segs) == 1 && s.Len == 0
			tasks = append(tasks, segTask{path: filepath.Join(dir, fmt.Sprintf("t%d_%06d", ti, si)), ref: s, whole: whole})
			place = append(place, float64(si)/float64(len(t.Segs)))
		}
	}
	// Video and sound pieces are fetched side by side, from the start onward,
	// so what's downloaded so far can be watched (Preview) at any moment.
	order := make([]int, len(tasks))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return place[order[a]] < place[order[b]] })
	slot := make([]int, len(tasks))
	for k, i := range order {
		slot[i] = k
	}

	d.mu.Lock()
	d.segState = make([]byte, len(tasks))
	d.segDone, d.segBytes, d.segInflight = 0, 0, 0
	d.sizeSum, d.sizeCount = 0, 0
	for i, t := range tasks {
		if st, err := os.Stat(t.path); err == nil {
			d.segState[i] = 2
			d.segDone++
			d.segBytes += st.Size()
			d.sizeSum += st.Size()
			d.sizeCount++
		} else if st, err := os.Stat(t.path + ".tmp"); err == nil {
			d.segInflight += st.Size() // half-finished chunk: we'll continue it
		}
	}
	d.Status = StDownloading
	workers := d.Connections
	d.mu.Unlock()
	if workers < 1 {
		workers = 1
	}
	if workers > 32 {
		workers = 32
	}

	keys := &keyCache{}
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error
	work := make(chan int)
	// too many workers after the user lowered the connection count? this one stops.
	surplus := func() bool {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.mediaWorkers > max(1, min(d.Connections, 32)) {
			d.mediaWorkers--
			return true
		}
		return false
	}
	spawn := func() {
		d.mu.Lock()
		d.mediaWorkers++
		d.mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				if err := m.fetchSegmentTask(wctx, d, i, tasks[i], keys, o); err != nil {
					if wctx.Err() == nil {
						errMu.Lock()
						if firstErr == nil {
							firstErr = err
						}
						errMu.Unlock()
						cancel()
					}
				}
				if surplus() {
					return
				}
			}
			d.mu.Lock()
			d.mediaWorkers--
			d.mu.Unlock()
		}()
	}
	d.mu.Lock()
	d.mediaWorkers = 0
	d.mu.Unlock()
	for w := 0; w < workers; w++ {
		spawn()
	}
	feederDone := make(chan struct{})
	spawnerDone := make(chan struct{})
	go func() { // more connections requested while downloading
		defer close(spawnerDone)
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				d.mu.Lock()
				need := max(1, min(d.Connections, 32)) - d.mediaWorkers
				d.mu.Unlock()
				for ; need > 0; need-- {
					spawn()
				}
			case <-feederDone:
				return
			case <-wctx.Done():
				return
			}
		}
	}()
	sent := make([]bool, len(tasks))
	cursor := 0
feed:
	for {
		d.mu.Lock()
		want := d.previewChunk - 1
		d.previewChunk = 0
		d.mu.Unlock()
		if want >= 0 && want < len(tasks) && !sent[want] {
			cursor = slot[want] // the viewer jumped ahead: fetch from there on
		}
		i := -1
		for n := 0; n < len(order); n++ {
			k := (cursor + n) % len(order)
			if !sent[order[k]] {
				i, cursor = order[k], k+1
				break
			}
		}
		if i < 0 {
			break
		}
		sent[i] = true
		d.mu.Lock()
		skip := d.segState[i] == 2
		d.mu.Unlock()
		if skip {
			continue
		}
		select {
		case work <- i:
		case <-wctx.Done():
			break feed
		}
	}
	close(feederDone)
	<-spawnerDone
	close(work)
	wg.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if firstErr != nil {
		return firstErr
	}

	return m.joinAndFinish(ctx, d, plan, dir)
}

// joinAndFinish joins the chunks into one file and puts it in place.
func (m *Manager) joinAndFinish(ctx context.Context, d *Download, plan *MediaPlan, dir string) error {
	d.mu.Lock()
	d.Status = StMerging
	d.mergePct = 0
	stem := strings.TrimSuffix(d.FileName, filepath.Ext(d.FileName))
	outDir := d.Dir
	d.mu.Unlock()

	res, err := m.joinMedia(ctx, plan, dir, func(p float64) {
		d.mu.Lock()
		d.mergePct = p
		d.mu.Unlock()
	})
	if err != nil {
		return err
	}
	final := uniqueFinal(outDir, stem+res.ext)
	if err := os.Rename(res.path, final); err != nil {
		return err
	}
	if plan.Subs != nil && plan.Subs.URL != "" {
		length := 0.0
		if plan.Clip != nil && plan.Clip.End > 0 {
			length = plan.Clip.End - res.clipStart
		}
		if p, err := m.saveSubtitles(ctx, plan.Subs, final, res.clipStart, length); err == nil {
			res.note = strings.TrimSpace(res.note + " Subtitles saved as " + filepath.Base(p) + ".")
		} else {
			res.note = strings.TrimSpace(res.note + " Couldn't get the subtitles: " + friendlyError(err) + ".")
		}
	}
	if plan.Clip != nil && res.clipStart > 0 && plan.Clip.Start-res.clipStart > 0.05 {
		res.note = strings.TrimSpace(res.note + fmt.Sprintf(" The clip starts at %s, the nearest clean picture before %s.", clock(res.clipStart), clock(plan.Clip.Start)))
	}
	for _, extra := range res.extras {
		os.Rename(extra.path, uniqueFinal(outDir, stem+extra.suffix))
	}
	st, _ := os.Stat(final)
	d.mu.Lock()
	d.FileName = filepath.Base(final)
	if st != nil {
		d.Size = st.Size()
	}
	d.Note = res.note
	d.mu.Unlock()
	os.RemoveAll(dir)
	return nil
}

func uniqueFinal(dir, name string) string {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	p := filepath.Join(dir, name)
	for i := 1; ; i++ {
		if _, err := os.Stat(p); err != nil {
			return p
		}
		p = filepath.Join(dir, fmt.Sprintf("%s (%d)%s", stem, i, ext))
	}
}

func (m *Manager) fetchSegmentTask(ctx context.Context, d *Download, i int, t segTask, keys *keyCache, o reqOpts) error {
	d.mu.Lock()
	d.segState[i] = 1
	d.mu.Unlock()
	tmp := t.path + ".tmp"
	var counted int64 // bytes of this chunk included in segInflight
	if st, err := os.Stat(tmp); err == nil {
		counted = st.Size()
	}
	limit := segCap
	if t.whole {
		limit = 0 // a single-file stream is the whole video
	}
	var lastErr error
	for attempt := 0; attempt < 10; attempt++ {
		if attempt > 0 {
			wait := time.Duration(attempt) * time.Second
			if wait > 8*time.Second {
				wait = 8 * time.Second
			}
			select {
			case <-time.After(wait):
			case <-ctx.Done():
			}
		}
		if ctx.Err() != nil {
			break
		}
		err := m.fetchSeg(ctx, d, t.ref, tmp, o, limit, func(n int64) {
			counted += n
			d.mu.Lock()
			d.segInflight += n
			d.mu.Unlock()
		}, func(total int64) {
			d.mu.Lock()
			d.sizeSum += total
			d.sizeCount++
			d.mu.Unlock()
		})
		if err == nil {
			err = finishSegment(ctx, m, t, tmp, keys, o)
		}
		if err == nil {
			st, serr := os.Stat(t.path)
			var size int64
			if serr == nil {
				size = st.Size()
			}
			d.mu.Lock()
			d.segInflight -= counted
			d.segState[i] = 2
			d.segDone++
			d.segBytes += size
			d.mu.Unlock()
			return nil
		}
		if errors.Is(err, errBadChunk) {
			// only this chunk is fetched again
			d.mu.Lock()
			d.segInflight -= counted
			d.badChunks++
			d.mu.Unlock()
			counted = 0
		}
		if errors.Is(err, errDRM) || isDiskFull(err) || errors.Is(err, errSegTooBig) {
			lastErr = err
			break
		}
		lastErr = err
		if refusedErr(err) && attempt >= 1 {
			break // the link itself was refused: retrying won't help, a fresh link will
		}
	}
	d.mu.Lock()
	if d.segState[i] != 2 {
		d.segState[i] = 0
	}
	d.mu.Unlock()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return lastErr
}

// finishSegment decrypts (if needed) and moves the finished chunk into place.
func finishSegment(ctx context.Context, m *Manager, t segTask, tmp string, keys *keyCache, o reqOpts) error {
	if t.ref.Key == nil {
		if err := os.Rename(tmp, t.path); err != nil {
			return err
		}
		return checkSizeless(t.path, t.ref.Len == 0)
	}
	key, err := keys.get(ctx, m, t.ref.Key.URL, o)
	if err != nil {
		return err
	}
	if err := decryptFileAES128(tmp, t.path+".dec", key, hexIV(t.ref.Key.IV)); err != nil {
		os.Remove(t.path + ".dec")
		if errors.Is(err, errEncSize) {
			os.Remove(tmp) // damaged: fetch it again
		}
		return err
	}
	os.Remove(tmp)
	if err := os.Rename(t.path+".dec", t.path); err != nil {
		return err
	}
	return checkSizeless(t.path, t.ref.Len == 0)
}

// checkSizeless checks a chunk the way checkChunkFile does, which reads it
// whole into memory. Only a single-file stream can be bigger than segCap: it
// isn't checked that way, just like the byte-range pieces of a big file.
func checkSizeless(p string, wholeChunk bool) error {
	if st, err := os.Stat(p); err == nil && st.Size() > segCap {
		return nil
	}
	return checkChunkFile(p, wholeChunk)
}

// fetchSegToFile downloads one chunk into tmp, continuing a half-finished one
// if tmp already has data, so pausing or a dropped connection wastes nothing.
// A chunk whose size isn't given stops at segCap.
func (m *Manager) fetchSegToFile(ctx context.Context, d *Download, ref SegRef, tmp string, o reqOpts, onBytes func(int64), onTotal func(int64)) error {
	return m.fetchSeg(ctx, d, ref, tmp, o, segCap, onBytes, onTotal)
}

// fetchSeg is fetchSegToFile with its own cap for a chunk whose size isn't
// given (0: no cap, for a single-file stream).
func (m *Manager) fetchSeg(ctx context.Context, d *Download, ref SegRef, tmp string, o reqOpts, limit int64, onBytes func(int64), onTotal func(int64)) error {
	capped := ref.Len == 0 && limit > 0
	var have int64
	if st, err := os.Stat(tmp); err == nil {
		have = st.Size()
	}
	if ref.Len > 0 && have >= ref.Len {
		if have == ref.Len {
			return nil
		}
		os.Remove(tmp)
		onBytes(-have)
		have = 0
	}
	if capped && have > limit {
		os.Remove(tmp)
		onBytes(-have)
		return errSegTooBig
	}

	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stall := time.AfterFunc(stallTimeout, cancel)
	defer stall.Stop()

	req, err := m.newReq(reqCtx, ref.URL, o)
	if err != nil {
		return err
	}
	start := ref.Off + have
	if ref.Len > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, ref.Off+ref.Len-1))
	} else if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := m.client.Do(req)
	if err != nil {
		if ctx.Err() == nil && reqCtx.Err() != nil {
			return errors.New("connection stalled")
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable && have > 0 && ref.Len == 0 {
		return nil // we already had the whole chunk
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("server replied %s", resp.Status)
	}

	skip := int64(0) // bytes to throw away when the server ignores our range
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if resp.StatusCode == http.StatusOK && (have > 0 || ref.Len > 0) {
		if have > 0 {
			onBytes(-have)
			have = 0
		}
		flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
		skip = ref.Off
	}
	expect := int64(-1)
	if ref.Len > 0 {
		expect = ref.Len
	} else if resp.StatusCode == http.StatusOK && resp.ContentLength >= 0 {
		expect = resp.ContentLength
	} else if total := parseContentRangeTotal(resp.Header.Get("Content-Range")); total > 0 {
		expect = total
	}
	if capped && expect > limit {
		return errSegTooBig // the server says so itself: nothing needs downloading to know
	}
	if expect > 0 && have == 0 {
		onTotal(expect)
	}
	f, err := os.OpenFile(tmp, flags, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	written := have
	bp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bp)
	buf := *bp
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			// waiting for a speed limit isn't a stall
			stall.Stop()
			m.throttle(ctx, d, n)
			stall.Reset(stallTimeout)
			chunk := buf[:n]
			if skip > 0 {
				drop := min(skip, int64(len(chunk)))
				chunk = chunk[drop:]
				skip -= drop
			}
			if ref.Len > 0 && written+int64(len(chunk)) > ref.Len {
				chunk = chunk[:ref.Len-written]
			}
			if capped && written+int64(len(chunk)) > limit {
				f.Close()
				os.Remove(tmp) // never a real piece: nothing to continue later
				onBytes(-written)
				return errSegTooBig
			}
			if len(chunk) > 0 {
				if _, werr := f.Write(chunk); werr != nil {
					return werr
				}
				written += int64(len(chunk))
				onBytes(int64(len(chunk)))
			}
			if ref.Len > 0 && written >= ref.Len {
				return nil
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			if ctx.Err() == nil && reqCtx.Err() != nil {
				return errors.New("connection stalled")
			}
			return rerr
		}
	}
	if expect > 0 && written < expect {
		return errors.New("connection closed early")
	}
	return nil
}

var errEncSize = errors.New("encrypted chunk has the wrong size")

// decryptFileAES128 decrypts an HLS chunk file into dst piece by piece, so
// even a big one is never held in memory whole.
func decryptFileAES128(src, dst string, key, iv []byte) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	left := st.Size()
	if left == 0 || left%aes.BlockSize != 0 {
		return errEncSize
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return err
	}
	cbc := cipher.NewCBCDecrypter(block, iv)
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	buf := make([]byte, 1<<20) // a whole number of AES blocks
	for left > 0 && err == nil {
		piece := buf[:min(int64(len(buf)), left)]
		if _, err = io.ReadFull(in, piece); err != nil {
			break
		}
		left -= int64(len(piece))
		cbc.CryptBlocks(piece, piece)
		if left == 0 {
			piece = unpadAES(piece)
		}
		_, err = out.Write(piece)
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// unpadAES takes the padding off the end of the last piece, if it has any.
func unpadAES(b []byte) []byte {
	p := int(b[len(b)-1])
	if p < 1 || p > aes.BlockSize || p > len(b) {
		return b
	}
	for _, c := range b[len(b)-p:] {
		if int(c) != p {
			return b
		}
	}
	return b[:len(b)-p]
}

func isDiskFull(err error) bool {
	s := err.Error()
	return strings.Contains(s, "no space left") || strings.Contains(s, "not enough space") || strings.Contains(s, "disk is full")
}

func (p *MediaPlan) chunkCount() int {
	n := 0
	for _, t := range p.Tracks {
		n += len(t.Segs)
		if t.Init != nil {
			n++
		}
	}
	return n
}

func refusedErr(err error) bool {
	s := err.Error()
	return strings.Contains(s, " 401 ") || strings.Contains(s, " 403 ") || strings.Contains(s, " 404 ") || strings.Contains(s, " 410 ")
}
