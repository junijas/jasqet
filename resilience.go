package main

// Downloads that never die: fresh links fetched automatically when old ones
// expire, chunks checked before they're used, connection counts that tune
// themselves, and a quick restart when you switch from Wi-Fi to mobile data.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	errFileChanged = errors.New("the file changed on the server while downloading")
	errBadChunk    = errors.New("a video chunk arrived damaged (the server sent an error page instead). Fetching just that chunk again")
)

// ---------- automatic link refresh ----------

var hrefRe = regexp.MustCompile(`(?i)(?:href|src|data-url|data-href)\s*=\s*["']([^"']+)["']|(https?://[^\s"'<>]+)`)

// autoRefresh tries to get a fresh link for an expired download without
// asking: Google Drive links are rebuilt from the file ID, other links are
// found again on the page the download came from.
func (m *Manager) autoRefresh(ctx context.Context, d *Download) bool {
	d.mu.Lock()
	old, size, ref, name := d.URL, d.Size, d.Page, d.FileName
	if ref == "" {
		ref = d.Headers["Referer"]
	}
	if d.Kind != "" || time.Since(d.lastAutoRefresh) < 2*time.Minute {
		d.mu.Unlock()
		return false
	}
	d.lastAutoRefresh = time.Now()
	d.mu.Unlock()

	var cands []string
	if id := googleDriveID(old); id != "" {
		cands = append(cands, "https://drive.usercontent.google.com/download?id="+id+"&export=download&confirm=t")
	}
	if ref != "" && ref != old {
		cands = append(cands, m.linksOnPage(ctx, d, ref, old, name)...)
	}
	for _, c := range cands {
		if c == old {
			continue
		}
		tmp, err := m.checkLink(ctx, d, c, nil, true)
		if err != nil || (size > 0 && tmp.Size != size) || (size > 0 && !tmp.Resumable) {
			continue
		}
		d.mu.Lock()
		d.URL = tmp.URL
		d.Note = "The link had expired, so Jasqet fetched a fresh one by itself and carried on"
		if ms := d.mirrors; ms != nil && len(ms.ok) > 0 {
			ms.ok[0], ms.fails[0] = true, 0 // the main link works again
		}
		d.mu.Unlock()
		m.saveSoon()
		return true
	}
	return false
}

// driveConfirmURL follows the button on Google Drive's "this file is too big
// to scan for viruses" page.
func driveConfirmURL(base *url.URL, body []byte) string {
	form := regexp.MustCompile(`(?is)<form[^>]+id="download-form"[^>]*action="([^"]+)"[^>]*>(.*?)</form>`).FindSubmatch(body)
	if form == nil {
		return ""
	}
	q := url.Values{}
	for _, in := range regexp.MustCompile(`(?is)<input[^>]+type="hidden"[^>]*>`).FindAll(form[2], -1) {
		n := regexp.MustCompile(`name="([^"]+)"`).FindSubmatch(in)
		v := regexp.MustCompile(`value="([^"]*)"`).FindSubmatch(in)
		if n != nil && v != nil {
			q.Set(string(n[1]), string(v[1]))
		}
	}
	act := strings.ReplaceAll(string(form[1]), "&amp;", "&")
	return resolveRef(base, act) + "?" + q.Encode()
}

// directLink turns "share" links into links to the file itself.
func directLink(raw string) string {
	if id := googleDriveID(raw); id != "" && !strings.Contains(raw, "usercontent") {
		u, _ := url.Parse(raw)
		if u != nil && (strings.Contains(u.Path, "/file/d/") || strings.Contains(u.Path, "/open") || u.Path == "/uc") {
			return "https://drive.usercontent.google.com/download?id=" + id + "&export=download&confirm=t"
		}
	}
	if u, err := url.Parse(raw); err == nil && (u.Hostname() == "www.dropbox.com" || u.Hostname() == "dropbox.com") {
		q := u.Query()
		if q.Get("dl") == "0" || q.Get("dl") == "" {
			q.Set("dl", "1")
			u.RawQuery = q.Encode()
			return u.String()
		}
	}
	return raw
}

// checkVersion: is the server still sending the same file we started with?
// Different version tags alone aren't trusted (mirrors and CDN servers often
// label the same file differently), so a sample of the data is compared.
func (m *Manager) checkVersion(ctx context.Context, d *Download, src int, h http.Header, f *os.File) error {
	etag, lm := validatorsOf(h)
	d.mu.Lock()
	differs := (d.ETag != "" && etag != "" && etag != d.ETag) ||
		(d.ETag == "" && d.LastModified != "" && lm != "" && lm != d.LastModified)
	key := etag + "|" + lm
	if !differs || d.okValidators[key] {
		d.mu.Unlock()
		return nil
	}
	// Compare a piece we already have with what this same server sends now.
	// The piece comes from the middle of the biggest finished stretch (file
	// headers often stay the same between versions).
	var off, best int64 = -1, 0
	for _, s := range d.Segments {
		if done := s.Pos - s.Start; done >= 64<<10 && done > best {
			best, off = done, s.Start+done/2-32<<10
		}
	}
	probe := &Download{URL: d.sourceLinkLocked(src), Headers: d.Headers, Cookies: d.Cookies, User: d.User, Password: d.Password, Offset: d.Offset}
	d.mu.Unlock()
	if off < 0 {
		return nil // nothing downloaded yet worth comparing
	}
	theirs, err := m.rangeBytes(ctx, probe, off, 64<<10)
	if err != nil {
		return nil
	}
	ours := make([]byte, len(theirs))
	if _, err := f.ReadAt(ours, off); err != nil {
		return nil
	}
	if !bytes.Equal(ours, theirs) {
		return errFileChanged
	}
	d.mu.Lock()
	if d.okValidators == nil {
		d.okValidators = map[string]bool{}
	}
	d.okValidators[key] = true
	d.mu.Unlock()
	return nil
}

func googleDriveID(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	h := strings.ToLower(u.Hostname())
	if !strings.Contains(h, "drive.google.com") && !strings.Contains(h, "docs.google.com") && !strings.Contains(h, "drive.usercontent.google.com") {
		return ""
	}
	if id := u.Query().Get("id"); id != "" {
		return id
	}
	if m := regexp.MustCompile(`/file/d/([\w-]+)`).FindStringSubmatch(u.Path); m != nil {
		return m[1]
	}
	return ""
}

// linksOnPage fetches the page a download came from and returns links that
// point to the same file (same name or path), best matches first.
func (m *Manager) linksOnPage(ctx context.Context, d *Download, pageURL, oldURL, fileName string) []string {
	d.mu.Lock()
	o := d.reqOpts()
	d.mu.Unlock()
	body, final, _, err := m.fetchSmall(ctx, pageURL, o, 8<<20)
	if err != nil {
		return nil
	}
	ou, _ := url.Parse(oldURL)
	oldBase := ""
	if ou != nil {
		oldBase = path.Base(ou.Path)
	}
	var exact, byName []string
	seen := map[string]bool{}
	for _, mm := range hrefRe.FindAllSubmatch(body, -1) {
		ref := string(mm[1])
		if ref == "" {
			ref = string(mm[2])
		}
		ref = strings.ReplaceAll(ref, "&amp;", "&")
		abs := resolveRef(final, ref)
		u, err := url.Parse(abs)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || seen[abs] {
			continue
		}
		seen[abs] = true
		base := path.Base(u.Path)
		switch {
		case ou != nil && u.Path == ou.Path && u.Path != "/":
			exact = append(exact, abs)
		case base != "" && (strings.EqualFold(base, oldBase) || strings.EqualFold(base, fileName)):
			byName = append(byName, abs)
		}
	}
	out := append(exact, byName...)
	if len(out) > 6 {
		out = out[:6]
	}
	return out
}

// ---------- checking chunks before they're used ----------

// validChunk catches the classic broken chunk: a server that answers with an
// error page (HTML) instead of video, or a cut-off file.
func validChunk(data []byte) bool {
	if len(data) == 0 {
		return false
	}
	head := bytes.TrimLeft(data[:min(len(data), 512)], " \r\n\t\uFEFF")
	if bytes.HasPrefix(head, []byte("<")) || bytes.HasPrefix(head, []byte("{\"")) {
		return false // HTML / JSON error page
	}
	if data[0] == 0x47 && len(data) >= 188 { // MPEG-TS: every packet starts with 0x47
		for i := 0; i+188 <= len(data); i += 188 {
			if data[i] != 0x47 {
				return false
			}
		}
		return true
	}
	if len(data) >= 8 && isBoxType(string(data[4:8])) { // MP4 pieces: boxes must add up
		var off uint64
		total := uint64(len(data))
		for off+8 <= total {
			size := uint64(data[off])<<24 | uint64(data[off+1])<<16 | uint64(data[off+2])<<8 | uint64(data[off+3])
			if size == 1 && off+16 <= total {
				size = 0
				for k := 0; k < 8; k++ {
					size = size<<8 | uint64(data[off+8+uint64(k)])
				}
			}
			if size == 0 {
				return true // runs to the end
			}
			if size < 8 || size > total-off {
				return false // too small, or claims more than there is (cut off)
			}
			off += size
		}
		return off == total
	}
	return true
}

func isBoxType(t string) bool {
	return strings.Contains("|ftyp|styp|moof|mdat|sidx|moov|emsg|prft|free|skip|", "|"+t+"|")
}

// checkChunkFile validates a finished chunk file; a bad one is deleted so only
// that chunk is downloaded again.
func checkChunkFile(p string, wholeChunk bool) error {
	if !wholeChunk {
		return nil // a byte-range piece of a bigger file can't be judged alone
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	if !validChunk(data) {
		os.Remove(p)
		return errBadChunk
	}
	return nil
}

// ---------- validators: did the file change on the server? ----------

func validatorsOf(h http.Header) (etag, lastMod string) {
	etag = h.Get("ETag")
	if strings.HasPrefix(etag, "W/") {
		etag = "" // weak tags can change while the bytes stay the same
	}
	return etag, h.Get("Last-Modified")
}

// ---------- adaptive connections ----------

// autoStartConns picks the first connection count for auto mode; autoTune
// then adjusts it. Caller holds d.mu.
func autoStartConns(maxConns int) int {
	return min(8, maxConns)
}

// autoTune adjusts the connection count to what actually makes the download
// faster: it tries more while each step helps, goes back when it doesn't, and
// halves when the server says "too many requests" (never going that high again).
func (m *Manager) autoTune(ctx context.Context, d *Download, maxConns int) {
	const window = 4 * time.Second
	t := time.NewTicker(window)
	defer t.Stop()
	d.mu.Lock()
	lastBytes := d.downloadedLocked()
	d.mu.Unlock()
	var prevRate float64
	ceiling := maxConns
	lastStep := 0 // >0: the last change added this many connections
	var cooldown time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		d.mu.Lock()
		if d.FixedConns {
			d.mu.Unlock()
			return
		}
		now := d.downloadedLocked()
		left := d.Size - now
		pushback := d.pushback
		d.pushback = 0
		cur := d.Connections
		d.mu.Unlock()
		rate := float64(now-lastBytes) / window.Seconds()
		lastBytes = now
		next := cur
		switch {
		case pushback > 0:
			ceiling = max(1, cur-1)
			next = max(1, min(cur/2, ceiling))
			cooldown = time.Now().Add(30 * time.Second)
			lastStep = 0
		case left < int64(rate*8): // nearly done: changes won't matter now
		case lastStep > 0 && rate < prevRate*1.07:
			next = max(1, cur-lastStep) // that didn't help: give them back
			cooldown = time.Now().Add(40 * time.Second)
			lastStep = 0
		case cur < ceiling && (lastStep > 0 || time.Now().After(cooldown)):
			step := max(2, cur/2)
			next = min(ceiling, cur+step)
			lastStep = next - cur
		default:
			lastStep = 0
		}
		if lastStep == 0 || next != cur {
			prevRate = rate
		}
		if next != cur {
			d.mu.Lock()
			d.Connections = next
			d.mu.Unlock()
		}
		prevRate = rate
	}
}

// ---------- switching networks (Wi-Fi ⇄ mobile) ----------

// netSignature: the computer's IPv4 addresses. It changes when you move from
// Wi-Fi to a phone hotspot or mobile modem.
func netSignature() string {
	ifs, _ := net.Interfaces()
	var parts []string
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLinkLocalUnicast() {
				parts = append(parts, ipn.IP.String())
			}
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func (m *Manager) checkNetChange() {
	sig := netSignature()
	m.mu.Lock()
	old := m.netSig
	m.netSig = sig
	m.mu.Unlock()
	if old == "" || old == sig || sig == "" {
		return
	}
	m.networkChanged()
}

// networkChanged restarts running connections at once on the new network
// instead of waiting for them to time out. Progress is kept.
func (m *Manager) networkChanged() {
	if t, ok := m.client.Transport.(*authTransport); ok {
		if tr, ok := t.base.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	}
	m.retryNow() // downloads waiting to try again after a network problem: now
	m.SetHold("network", true)
	time.AfterFunc(1500*time.Millisecond, func() { m.SetHold("network", false) })
}

var _ = io.EOF
