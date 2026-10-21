package main

// Clip downloads: only the part of a video you want (say 2:30 to 5:00), so you
// don't pay for the whole thing. Jasqet downloads just the chunks that cover
// that time, then trims to the nearest picture boundary (a key frame) before
// the start, and exactly at the end. Subtitles can come along as an .srt file.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type ClipSpec struct {
	Start float64 `json:"start"` // seconds
	End   float64 `json:"end"`   // seconds (0 = to the end)
}

// label is used in file names (Windows doesn't allow ":"), e.g. "2m30s-5m00s".
func (c *ClipSpec) label() string {
	if c == nil {
		return ""
	}
	name := func(s float64) string {
		t := int(math.Round(s))
		if t >= 3600 {
			return fmt.Sprintf("%dh%02dm%02ds", t/3600, t/60%60, t%60)
		}
		return fmt.Sprintf("%dm%02ds", t/60, t%60)
	}
	end := "end"
	if c.End > 0 {
		end = name(c.End)
	}
	return name(c.Start) + "-" + end
}

// clock shows a time like a video player does: 2:30 or 1:02:30.
func clock(s float64) string {
	t := int(math.Round(s))
	if t >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", t/3600, t/60%60, t%60)
	}
	return fmt.Sprintf("%d:%02d", t/60, t%60)
}

// parseClock reads "2:30", "1:02:03", "150" or "2m30s".
func parseClock(s string) (float64, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, nil
	}
	if m := regexp.MustCompile(`^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+(?:\.\d+)?)s?)?$`).FindStringSubmatch(s); m != nil && (m[1] != "" || m[2] != "" || m[3] != "") && strings.ContainsAny(s, "hms") {
		h, _ := strconv.Atoi(m[1])
		mi, _ := strconv.Atoi(m[2])
		se, _ := strconv.ParseFloat(m[3], 64)
		return float64(h*3600+mi*60) + se, nil
	}
	parts := strings.Split(s, ":")
	if len(parts) > 3 {
		return 0, errors.New("write times like 2:30 or 1:02:30")
	}
	var total float64
	for _, p := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
		if err != nil || v < 0 {
			return 0, errors.New("write times like 2:30 or 1:02:30")
		}
		total = total*60 + v
	}
	return total, nil
}

func clipEstimateDur(full int64, dur float64, c ClipSpec) int64 {
	if dur <= 0 || full <= 0 {
		return full
	}
	end := c.End
	if end <= 0 || end > dur {
		end = dur
	}
	part := math.Max(0, end-c.Start) + 5 // chunks start a little early
	return int64(float64(full) * math.Min(1, part/dur))
}

var errNoClip = errors.New("this video can't be cut before downloading (the site doesn't say where each part is), so download it whole instead")

// applyClip keeps only the chunks that cover the clip.
func (m *Manager) applyClip(ctx context.Context, plan *MediaPlan, c *ClipSpec, o reqOpts) error {
	if c == nil {
		return nil
	}
	if plan.Fallback {
		return errNoClip
	}
	if plan.Duration > 0 && c.Start >= plan.Duration {
		return fmt.Errorf("the clip starts after the end of the video (it's only %s long)", clock(plan.Duration))
	}
	if c.End > 0 && c.End <= c.Start {
		return errors.New("the clip's end must be after its start")
	}
	for i := range plan.Tracks {
		t := &plan.Tracks[i]
		if oneFile(t) {
			if err := m.sidxClip(ctx, t, o, c, plan.Duration); err != nil {
				return err
			}
			continue
		}
		if err := clipSegs(t, c); err != nil {
			return err
		}
	}
	plan.Clip = c
	return nil
}

// oneFile: the track is a single file fetched in pieces (YouTube, DASH "on demand").
func oneFile(t *TrackPlan) bool {
	if len(t.Segs) == 0 || (t.Init != nil && t.Init.URL != t.Segs[0].URL) {
		return false
	}
	for _, s := range t.Segs {
		if s.URL != t.Segs[0].URL || s.D > 0 {
			return false
		}
	}
	return true
}

func clipSegs(t *TrackPlan, c *ClipSpec) error {
	var cur float64
	var keep []SegRef
	first := true
	for _, s := range t.Segs {
		if s.D <= 0 {
			return errNoClip
		}
		end := cur + s.D
		if end > c.Start && (c.End <= 0 || cur < c.End) {
			if first {
				t.SegStart, first = cur, false
			}
			keep = append(keep, s)
		}
		cur = end
	}
	if len(keep) == 0 {
		return fmt.Errorf("the clip is outside the video (it's only %s long)", clock(cur))
	}
	t.Segs = keep
	return nil
}

type sidxRef struct {
	off, size int64
	start     float64
	dur       float64
}

// sidxClip reads the file's index (the "sidx" box at its start, a few KB)
// and keeps only the byte ranges for the clip.
func (m *Manager) sidxClip(ctx context.Context, t *TrackPlan, o reqOpts, c *ClipSpec, duration float64) error {
	u := t.Segs[0].URL
	var size int64
	for _, s := range t.Segs {
		if s.Len <= 0 {
			size = 0
			break
		}
		size = max(size, s.Off+s.Len)
	}
	if size <= 0 {
		s, err := m.remoteSize(ctx, u, o)
		if err != nil {
			return errNoClip
		}
		size = s
	}
	tmp := &Download{URL: u, Headers: o.headers, Cookies: o.cookies}
	head, err := m.rangeBytes(ctx, tmp, 0, min(size, 256<<10))
	if err != nil {
		return err
	}
	initEnd, refs := parseSidx(head)
	if initEnd <= 0 || len(refs) == 0 {
		return errNoClip
	}
	last := refs[len(refs)-1]
	if covered := last.start + last.dur; duration > 0 && covered < duration-1 {
		// The index only covers the start (some files have one index per
		// part): download the whole file and cut the clip out afterwards.
		t.SegStart = 0
		return nil
	}
	var a, b int64 = -1, 0
	for _, r := range refs {
		if r.start+r.dur > c.Start && (c.End <= 0 || r.start < c.End) {
			if a < 0 {
				a = r.off
				t.SegStart = r.start
			}
			b = r.off + r.size
		}
	}
	if a < 0 {
		return errors.New("the clip is outside the video")
	}
	t.Init = &SegRef{URL: u, Off: 0, Len: initEnd}
	t.Segs = nil
	const piece = 4 << 20
	for off := a; off < b; off += piece {
		t.Segs = append(t.Segs, SegRef{URL: u, Off: off, Len: min(piece, b-off)})
	}
	return nil
}

// parseSidx finds where the header (ftyp + moov) ends and reads the index of
// fragments: where each one is in the file and when it plays.
func parseSidx(b []byte) (int64, []sidxRef) {
	var initEnd int64
	var off int64
	for off+8 <= int64(len(b)) {
		size := int64(binary.BigEndian.Uint32(b[off:]))
		typ := string(b[off+4 : off+8])
		hdr := int64(8)
		if size == 1 && off+16 <= int64(len(b)) {
			size = int64(binary.BigEndian.Uint64(b[off+8:]))
			hdr = 16
		}
		if size < hdr || size > int64(len(b))-off && typ != "mdat" && typ != "moof" {
			return 0, nil
		}
		switch typ {
		case "ftyp", "moov":
			initEnd = off + size
		case "sidx":
			box := b[off+hdr : off+size]
			if len(box) < 12 {
				return 0, nil
			}
			ver := box[0]
			p := 4
			p += 4 // reference ID
			ts := float64(binary.BigEndian.Uint32(box[p:]))
			p += 4
			var ept, first uint64
			if ver == 0 {
				if len(box) < p+8 {
					return 0, nil
				}
				ept = uint64(binary.BigEndian.Uint32(box[p:]))
				first = uint64(binary.BigEndian.Uint32(box[p+4:]))
				p += 8
			} else {
				if len(box) < p+16 {
					return 0, nil
				}
				ept = binary.BigEndian.Uint64(box[p:])
				first = binary.BigEndian.Uint64(box[p+8:])
				p += 16
			}
			p += 2 // reserved
			if len(box) < p+2 || ts == 0 {
				return 0, nil
			}
			count := int(binary.BigEndian.Uint16(box[p:]))
			p += 2
			pos := off + size + int64(first)
			t := float64(ept) / ts
			var refs []sidxRef
			for i := 0; i < count && p+12 <= len(box); i++ {
				v := binary.BigEndian.Uint32(box[p:])
				dur := float64(binary.BigEndian.Uint32(box[p+4:])) / ts
				n := int64(v & 0x7fffffff)
				if v>>31 == 1 {
					return 0, nil // nested indexes: not used by the sites we handle
				}
				refs = append(refs, sidxRef{off: pos, size: n, start: t, dur: dur})
				pos += n
				t += dur
				p += 12
			}
			return initEnd, refs
		case "moof", "mdat":
			return initEnd, nil
		}
		off += size
	}
	return initEnd, nil
}

// trimTracks cuts the collected samples to the clip. Video starts at the last
// key frame at or before the start (a clean picture); sound starts with it.
// Returns the clip's real start time.
func trimTracks(tracks []*outTrack, c *ClipSpec) ([]*outTrack, float64) {
	start := c.Start
	for _, t := range tracks {
		if !t.video || len(t.samples) == 0 {
			continue
		}
		ts := float64(t.timescale)
		best := 0
		for i, s := range t.samples {
			if s.sync && float64(s.pts)/ts+t.clipOffset <= c.Start+0.001 {
				best = i
			}
		}
		start = float64(t.samples[best].pts)/ts + t.clipOffset
		t.samples = t.samples[best:]
		break
	}
	var out []*outTrack
	for _, t := range tracks {
		ts := float64(t.timescale)
		if !t.video {
			i := 0
			for i < len(t.samples) && float64(t.samples[i].pts)/ts+t.clipOffset < start-0.0005 {
				i++
			}
			t.samples = t.samples[i:]
		}
		if c.End > 0 {
			n := len(t.samples)
			for n > 1 && float64(t.samples[n-1].dts)/ts+t.clipOffset >= c.End {
				n--
			}
			t.samples = t.samples[:n]
		}
		if len(t.samples) > 0 {
			out = append(out, t)
		}
	}
	return out, start
}

// setClipOffsets lines a group's own clock up with the video's timeline.
func setClipOffsets(got []*outTrack, segStart float64) {
	origin := math.Inf(1)
	for _, t := range got {
		if len(t.samples) == 0 {
			continue
		}
		first := t.samples[0].pts
		for k := 0; k < len(t.samples) && k < 16; k++ {
			first = min(first, t.samples[k].pts)
		}
		origin = math.Min(origin, float64(first)/float64(t.timescale))
	}
	if math.IsInf(origin, 1) {
		return
	}
	for _, t := range got {
		t.clipOffset = segStart - origin
	}
}

// ---------- subtitles ----------

type SubRef struct {
	Lang string `json:"lang"`
	Name string `json:"name"`
	Auto bool   `json:"auto"` // made by YouTube's speech recognition
	URL  string `json:"url,omitempty"`
	Ext  string `json:"ext,omitempty"`
}

type ytSubFormat struct {
	Ext  string `json:"ext"`
	URL  string `json:"url"`
	Name string `json:"name"`
}

// ytSubtitleChoices lists the subtitles worth offering: all real ones, and
// the automatic ones in the video's own language and in English.
func ytSubtitleChoices(info *ytInfo) []SubRef {
	var out []SubRef
	seen := map[string]bool{}
	langs := make([]string, 0, len(info.Subtitles))
	for l := range info.Subtitles {
		langs = append(langs, l)
	}
	sort.Strings(langs)
	for _, l := range langs {
		if l == "live_chat" || len(info.Subtitles[l]) == 0 {
			continue
		}
		name := info.Subtitles[l][0].Name
		if name == "" {
			name = l
		}
		out = append(out, SubRef{Lang: l, Name: name})
		seen[l] = true
	}
	for l, fs := range info.AutoCaptions {
		if len(fs) == 0 {
			continue
		}
		orig := strings.HasSuffix(l, "-orig")
		base := strings.TrimSuffix(l, "-orig")
		if !(orig || (base == "en" && info.AutoCaptions["en-orig"] == nil)) || seen[base] {
			continue
		}
		name := fs[0].Name
		if name == "" {
			name = base
		}
		out = append(out, SubRef{Lang: l, Name: strings.TrimSuffix(name, " (Original)") + " (automatic)", Auto: true})
		seen[base] = true
	}
	if info.AutoCaptions["en"] != nil && !seen["en"] {
		out = append(out, SubRef{Lang: "en", Name: "English (automatic)", Auto: true})
	}
	return out
}

func pickSubtitle(info *ytInfo, lang string) *SubRef {
	if lang == "" {
		return nil
	}
	try := func(list map[string][]ytSubFormat, auto bool) *SubRef {
		fs := list[lang]
		for _, want := range []string{"json3", "vtt", "srv1"} {
			for _, f := range fs {
				if f.Ext == want && f.URL != "" {
					return &SubRef{Lang: strings.TrimSuffix(lang, "-orig"), URL: f.URL, Ext: f.Ext, Auto: auto}
				}
			}
		}
		return nil
	}
	if s := try(info.Subtitles, false); s != nil {
		return s
	}
	return try(info.AutoCaptions, true)
}

type cue struct {
	start, end float64
	text       string
}

var (
	vttTimeRe = regexp.MustCompile(`((?:\d+:)?\d{1,2}:\d{2}[.,]\d{3})\s*-->\s*((?:\d+:)?\d{1,2}:\d{2}[.,]\d{3})`)
	tagRe     = regexp.MustCompile(`<[^>]*>`)
)

func vttSeconds(s string) float64 {
	s = strings.ReplaceAll(s, ",", ".")
	parts := strings.Split(s, ":")
	var t float64
	for _, p := range parts {
		v, _ := strconv.ParseFloat(p, 64)
		t = t*60 + v
	}
	return t
}

func parseVTT(body string) []cue { return dedupeRolling(parseVTTCues(body)) }

// parseVTTCues reads SRT or WebVTT cues as they are.
func parseVTTCues(body string) []cue {
	var out []cue
	blocks := regexp.MustCompile(`\r?\n\r?\n`).Split(strings.ReplaceAll(body, "\r\n", "\n"), -1)
	for _, b := range blocks {
		lines := strings.Split(strings.TrimSpace(b), "\n")
		for i, l := range lines {
			m := vttTimeRe.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			var text []string
			for _, t := range lines[i+1:] {
				t = strings.TrimSpace(htmlUnescape(tagRe.ReplaceAllString(t, "")))
				if t != "" {
					text = append(text, t)
				}
			}
			if len(text) > 0 {
				out = append(out, cue{vttSeconds(m[1]), vttSeconds(m[2]), strings.Join(text, "\n")})
			}
			break
		}
	}
	return out
}

// dedupeRolling cleans YouTube's automatic captions, where each line is
// repeated as the next one scrolls in.
func dedupeRolling(cs []cue) []cue {
	var out []cue
	prevLast := ""
	for _, c := range cs {
		lines := strings.Split(c.text, "\n")
		var keep []string
		for _, l := range lines {
			if l != prevLast {
				keep = append(keep, l)
			}
		}
		if len(lines) > 0 {
			prevLast = lines[len(lines)-1]
		}
		if len(keep) == 0 || c.end-c.start < 0.02 {
			continue
		}
		c.text = strings.Join(keep, "\n")
		out = append(out, c)
	}
	return out
}

func parseJSON3(body []byte) []cue {
	var j struct {
		Events []struct {
			Start int64 `json:"tStartMs"`
			Dur   int64 `json:"dDurationMs"`
			Segs  []struct {
				Text string `json:"utf8"`
			} `json:"segs"`
			Append int `json:"aAppend"`
		} `json:"events"`
	}
	if json.Unmarshal(body, &j) != nil {
		return nil
	}
	var out []cue
	for _, e := range j.Events {
		if e.Append == 1 || len(e.Segs) == 0 {
			continue
		}
		var sb strings.Builder
		for _, s := range e.Segs {
			sb.WriteString(s.Text)
		}
		text := strings.TrimSpace(sb.String())
		if text == "" {
			continue
		}
		out = append(out, cue{float64(e.Start) / 1000, float64(e.Start+e.Dur) / 1000, text})
	}
	// automatic captions overlap: end each line when the next one starts
	for i := 0; i+1 < len(out); i++ {
		if out[i].end > out[i+1].start {
			out[i].end = out[i+1].start
		}
	}
	return out
}

func htmlUnescape(s string) string {
	return strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", "\"", "&#39;", "'", "&nbsp;", " ").Replace(s)
}

// toSRT writes cues as an .srt file, moved by shift seconds and cut to the clip.
func toSRT(cs []cue, shift, length float64) string {
	var sb strings.Builder
	n := 0
	ts := func(t float64) string {
		ms := int64(math.Round(t * 1000))
		return fmt.Sprintf("%02d:%02d:%02d,%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
	}
	for _, c := range cs {
		s, e := c.start-shift, c.end-shift
		if e <= 0 || (length > 0 && s >= length) {
			continue
		}
		s = math.Max(0, s)
		if length > 0 {
			e = math.Min(e, length)
		}
		n++
		fmt.Fprintf(&sb, "%d\n%s --> %s\n%s\n\n", n, ts(s), ts(e), c.text)
	}
	return sb.String()
}

// saveSubtitles downloads the chosen subtitles and saves them next to the video.
func (m *Manager) saveSubtitles(ctx context.Context, sub *SubRef, videoPath string, shift, length float64) (string, error) {
	lang := sub.Lang
	if !subLangRe.MatchString(lang) { // the site names its languages: never a path
		lang = "subs"
	}
	out := strings.TrimSuffix(videoPath, filepath.Ext(videoPath)) + "." + lang + ".srt"
	return out, m.saveSubtitlesTo(ctx, sub, out, shift, length)
}

// executeSubs downloads only a video's subtitles, as an .srt file.
func (m *Manager) executeSubs(ctx context.Context, d *Download) error {
	d.mu.Lock()
	pageURL, lang, cookies := d.URL, d.Subs, d.Cookies
	d.Status = StDownloading
	d.mu.Unlock()
	if err := m.toolsFor(ctx, d); err != nil {
		return err
	}
	info, err := m.ytInfoFor(ctx, pageURL, cookies)
	if err != nil {
		return err
	}
	sub := pickSubtitle(info, lang)
	if sub == nil {
		return errors.New("this video has no subtitles in that language")
	}
	m.placeFile(d)
	d.mu.Lock()
	p := filepath.Join(d.Dir, d.FileName)
	d.mu.Unlock()
	os.Remove(p + ".part")
	if err := m.saveSubtitlesTo(ctx, sub, p, 0, 0); err != nil {
		return err
	}
	if st, err := os.Stat(p); err == nil {
		d.mu.Lock()
		d.Size = st.Size()
		d.mu.Unlock()
	}
	return nil
}

func (m *Manager) saveSubtitlesTo(ctx context.Context, sub *SubRef, out string, shift, length float64) error {
	ctx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	body, _, _, err := m.fetchSmall(ctx, sub.URL, reqOpts{}, 16<<20)
	if err != nil {
		return err
	}
	m.countBytes(len(body))
	var cs []cue
	if sub.Ext == "json3" {
		cs = parseJSON3(body)
	} else {
		cs = parseVTT(string(body))
	}
	if len(cs) == 0 {
		return errors.New("the subtitles were empty")
	}
	return writeFileAtomic(out, []byte(toSRT(cs, shift, length)))
}

func writeFileAtomic(p string, b []byte) error {
	if err := os.WriteFile(p+".tmp", b, 0o644); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}
