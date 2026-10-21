package main

// Looking inside media files for the viewer: which tracks and formats they
// hold (so we know what the built-in player can play), title/artist/cover
// from music tags, and subtitles (files next to the video, or text tracks
// inside MKV files) turned into WebVTT for the player.

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
)

type MediaTrack struct {
	Type   string `json:"type"`  // "video", "audio", "subtitle"
	Codec  string `json:"codec"` // raw code (avc1, A_AC3…)
	Name   string `json:"name"`  // friendly name
	Lang   string `json:"lang,omitempty"`
	Title  string `json:"title,omitempty"`
	Played bool   `json:"played"` // the built-in player can play it
	num    int    // MKV track number
	text   string // MKV subtitle kind: "srt", "ass", "vtt"
}

// ---------- tracks ----------

func probeTracks(p string) []MediaTrack {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	head := make([]byte, 16)
	io.ReadFull(f, head)
	switch {
	case bytes.HasPrefix(head, []byte{0x1A, 0x45, 0xDF, 0xA3}):
		return mkvTracks(f)
	case string(head[4:8]) == "ftyp" || string(head[4:8]) == "moov" || string(head[4:8]) == "mdat" || string(head[4:8]) == "free" || string(head[4:8]) == "wide":
		return mp4Tracks(f)
	}
	return nil
}

var codecNames = map[string]string{
	"avc1": "H.264", "avc3": "H.264", "hvc1": "HEVC (H.265)", "hev1": "HEVC (H.265)", "av01": "AV1", "vp09": "VP9", "vp08": "VP8",
	"mp4v": "MPEG-4 Part 2 (DivX/Xvid)", "mp4a": "AAC", "ac-3": "Dolby AC-3", "ec-3": "Dolby Digital Plus", "Opus": "Opus", "fLaC": "FLAC",
	"alac": "Apple Lossless", ".mp3": "MP3", "tx3g": "subtitles", "wvtt": "WebVTT subtitles", "dtsc": "DTS", "dtsh": "DTS-HD", "mlpa": "Dolby TrueHD",
	"V_MPEG4/ISO/AVC": "H.264", "V_MPEGH/ISO/HEVC": "HEVC (H.265)", "V_AV1": "AV1", "V_VP9": "VP9", "V_VP8": "VP8", "V_THEORA": "Theora",
	"V_MPEG4/ISO/ASP": "MPEG-4 Part 2 (DivX/Xvid)", "V_MPEG4/ISO/SP": "MPEG-4 Part 2", "V_MPEG2": "MPEG-2", "V_MPEG1": "MPEG-1", "V_MS/VFW/FOURCC": "old Windows video (VfW)", "V_REAL/RV40": "RealVideo",
	"A_AAC": "AAC", "A_OPUS": "Opus", "A_VORBIS": "Vorbis", "A_FLAC": "FLAC", "A_MPEG/L3": "MP3", "A_MPEG/L2": "MP2", "A_AC3": "Dolby AC-3", "A_EAC3": "Dolby Digital Plus",
	"A_DTS": "DTS", "A_TRUEHD": "Dolby TrueHD", "A_PCM/INT/LIT": "PCM", "A_ALAC": "Apple Lossless",
	"S_TEXT/UTF8": "SRT subtitles", "S_TEXT/ASS": "ASS subtitles", "S_TEXT/SSA": "SSA subtitles", "S_ASS": "ASS subtitles", "S_SSA": "SSA subtitles", "S_TEXT/WEBVTT": "WebVTT subtitles",
	"S_HDMV/PGS": "picture subtitles (Blu-ray)", "S_VOBSUB": "picture subtitles (DVD)",
}

// playable: what the Edge/WebView2 player can decode.
func playable(codec string) bool {
	c := codec
	if strings.HasPrefix(c, "A_AAC") {
		c = "A_AAC"
	}
	switch c {
	case "avc1", "avc3", "av01", "vp09", "vp08", "hvc1", "hev1", "mp4a", "Opus", "fLaC", ".mp3",
		"V_MPEG4/ISO/AVC", "V_AV1", "V_VP9", "V_VP8", "V_MPEGH/ISO/HEVC",
		"A_AAC", "A_OPUS", "A_VORBIS", "A_FLAC", "A_MPEG/L3":
		return true
	}
	return false
}

func friendly(codec string) string {
	c := codec
	if strings.HasPrefix(c, "A_AAC") {
		c = "A_AAC"
	}
	if n, ok := codecNames[c]; ok {
		return n
	}
	return codec
}

// mp4Tracks lists the tracks of an MP4/MOV file (reads only the index).
func mp4Tracks(f *os.File) []MediaTrack {
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	moov := findBox(f, 0, st.Size(), "moov")
	if moov == nil {
		return nil
	}
	var out []MediaTrack
	for _, trak := range childBoxes(moov, "trak") {
		mdia := firstChild(trak, "mdia")
		if mdia == nil {
			continue
		}
		var t MediaTrack
		if hdlr := firstChild(mdia, "hdlr"); len(hdlr) >= 12 {
			switch string(hdlr[8:12]) {
			case "vide":
				t.Type = "video"
			case "soun":
				t.Type = "audio"
			case "subt", "text", "sbtl", "clcp":
				t.Type = "subtitle"
			default:
				continue
			}
		}
		if mdhd := firstChild(mdia, "mdhd"); len(mdhd) >= 24 {
			off := 20
			if mdhd[0] == 1 {
				off = 32
			}
			if len(mdhd) >= off+2 {
				l := binary.BigEndian.Uint16(mdhd[off:])
				t.Lang = string([]byte{byte(l>>10&31) + 0x60, byte(l>>5&31) + 0x60, byte(l&31) + 0x60})
				if t.Lang == "und" || t.Lang == "```" {
					t.Lang = ""
				}
			}
		}
		if stbl := firstChild(firstChild(firstChild(mdia, "minf"), "stbl"), "stsd"); len(stbl) >= 16 {
			t.Codec = string(stbl[12:16])
			if t.Codec == "mp4a" {
				if ot := esdsObjectType(stbl); ot == 0x69 || ot == 0x6b {
					t.Codec = ".mp3" // MP3 sound inside an MP4
				}
			}
		}
		if t.Type == "" || t.Codec == "" {
			continue
		}
		t.Name = friendly(t.Codec)
		t.Played = playable(t.Codec) || t.Type == "subtitle"
		out = append(out, t)
	}
	return out
}

// esdsObjectType reads the codec number from an MP4 "esds" box (0x40 = AAC, 0x6B = MP3).
func esdsObjectType(stsd []byte) byte {
	i := bytes.Index(stsd, []byte("esds"))
	if i < 0 {
		return 0
	}
	b := stsd[i+4:]
	p := 4 // version + flags
	descLen := func() int {
		n := 0
		for k := 0; k < 4 && p < len(b); k++ {
			c := b[p]
			p++
			n = n<<7 | int(c&0x7f)
			if c&0x80 == 0 {
				break
			}
		}
		return n
	}
	if p >= len(b) || b[p] != 0x03 {
		return 0
	}
	p++
	descLen()
	if p+3 > len(b) {
		return 0
	}
	flags := b[p+2]
	p += 3
	if flags&0x80 != 0 {
		p += 2
	}
	if flags&0x40 != 0 && p < len(b) {
		p += 1 + int(b[p])
	}
	if flags&0x20 != 0 {
		p += 2
	}
	if p >= len(b) || b[p] != 0x04 {
		return 0
	}
	p++
	descLen()
	if p < len(b) {
		return b[p]
	}
	return 0
}

// findBox finds a top-level box and returns its body (up to 8 MB).
func findBox(f *os.File, off, end int64, typ string) []byte {
	h := make([]byte, 16)
	for off+8 <= end {
		if _, err := f.ReadAt(h, off); err != nil && err != io.EOF {
			return nil
		}
		size := int64(binary.BigEndian.Uint32(h[:4]))
		hl := int64(8)
		if size == 1 {
			size = int64(binary.BigEndian.Uint64(h[8:16]))
			hl = 16
		} else if size == 0 {
			size = end - off
		}
		if size < hl || size > end-off {
			return nil
		}
		if string(h[4:8]) == typ {
			if size-hl > 8<<20 {
				return nil
			}
			b := make([]byte, size-hl)
			n, _ := f.ReadAt(b, off+hl)
			return b[:n]
		}
		off += size
	}
	return nil
}

func eachBox(b []byte, fn func(typ string, body []byte) bool) {
	for p := 0; p+8 <= len(b); {
		size := int(binary.BigEndian.Uint32(b[p:]))
		hl := 8
		if size == 1 && p+16 <= len(b) {
			size = int(binary.BigEndian.Uint64(b[p+8:]))
			hl = 16
		} else if size == 0 {
			size = len(b) - p
		}
		if size < hl || size > len(b)-p {
			return
		}
		if !fn(string(b[p+4:p+8]), b[p+hl:p+size]) {
			return
		}
		p += size
	}
}

func childBoxes(b []byte, typ string) [][]byte {
	var out [][]byte
	eachBox(b, func(t string, body []byte) bool {
		if t == typ {
			out = append(out, body)
		}
		return true
	})
	return out
}

func firstChild(b []byte, typ string) []byte {
	var out []byte
	eachBox(b, func(t string, body []byte) bool {
		if t == typ {
			out = body
			return false
		}
		return true
	})
	return out
}

// ---------- Matroska (MKV / WebM) ----------

type ebml struct {
	r   io.ReaderAt
	end int64
}

// readVint reads an EBML variable-length number at off.
func (e *ebml) vint(off int64, keepMarker bool) (uint64, int, error) {
	var b [8]byte
	if _, err := e.r.ReadAt(b[:1], off); err != nil {
		return 0, 0, err
	}
	first := b[0]
	n := 1
	for mask := byte(0x80); n <= 8 && first&mask == 0; mask >>= 1 {
		n++
	}
	if n > 8 {
		return 0, 0, errors.New("bad ebml")
	}
	if n > 1 {
		if _, err := e.r.ReadAt(b[1:n], off+1); err != nil {
			return 0, 0, err
		}
	}
	v := uint64(first)
	if !keepMarker {
		v &= uint64(0xff >> n)
	}
	for i := 1; i < n; i++ {
		v = v<<8 | uint64(b[i])
	}
	return v, n, nil
}

// element reads an element header: id, data start, data size (-1 = unknown).
func (e *ebml) element(off int64) (id uint64, data int64, size int64, err error) {
	id, n, err := e.vint(off, true)
	if err != nil {
		return
	}
	sz, m, err := e.vint(off+int64(n), false)
	if err != nil {
		return
	}
	size = int64(sz)
	if sz == (uint64(1)<<(7*uint(m)))-1 {
		size = -1 // unknown size
	}
	return id, off + int64(n+m), size, nil
}

func (e *ebml) bytesAt(off, n int64) []byte {
	if n < 0 || n > 16<<20 {
		return nil
	}
	b := make([]byte, n)
	k, _ := e.r.ReadAt(b, off)
	return b[:k]
}

func beUint(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

const (
	idSegment  = 0x18538067
	idTracks   = 0x1654AE6B
	idTrack    = 0xAE
	idInfo     = 0x1549A966
	idCluster  = 0x1F43B675
	idTimecode = 0xE7
	idSimple   = 0xA3
	idGroup    = 0xA0
	idBlock    = 0xA1
	idDuration = 0x9B
)

// mkvSegment finds the Segment's data range.
func mkvSegment(f *os.File) (*ebml, int64, int64, bool) {
	st, err := f.Stat()
	if err != nil {
		return nil, 0, 0, false
	}
	e := &ebml{r: f, end: st.Size()}
	id, data, size, err := e.element(0) // EBML header
	if err != nil || id != 0x1A45DFA3 || size < 0 {
		return nil, 0, 0, false
	}
	off := data + size
	id, data, size, err = e.element(off)
	if err != nil || id != idSegment {
		return nil, 0, 0, false
	}
	end := e.end
	if size >= 0 && data+size < end {
		end = data + size
	}
	return e, data, end, true
}

func mkvTracks(f *os.File) []MediaTrack {
	e, off, end, ok := mkvSegment(f)
	if !ok {
		return nil
	}
	for off < end {
		id, data, size, err := e.element(off)
		if err != nil || size < 0 {
			return nil
		}
		if id == idCluster {
			return nil // tracks should come before the video data
		}
		if id == idTracks {
			return mkvParseTracks(e, data, data+size)
		}
		off = data + size
	}
	return nil
}

func mkvParseTracks(e *ebml, off, end int64) []MediaTrack {
	var out []MediaTrack
	for off < end {
		id, data, size, err := e.element(off)
		if err != nil || size < 0 {
			break
		}
		if id == idTrack {
			var t MediaTrack
			var kind uint64
			t.Lang = "eng" // Matroska's default
			for p := data; p < data+size; {
				cid, cdata, csize, err := e.element(p)
				if err != nil || csize < 0 {
					break
				}
				v := e.bytesAt(cdata, csize)
				switch cid {
				case 0xD7:
					t.num = int(beUint(v))
				case 0x83:
					kind = beUint(v)
				case 0x86:
					t.Codec = string(v)
				case 0x22B59C:
					t.Lang = string(v)
				case 0x536E:
					t.Title = string(v)
				}
				p = cdata + csize
			}
			switch kind {
			case 1:
				t.Type = "video"
			case 2:
				t.Type = "audio"
			case 17:
				t.Type = "subtitle"
				switch t.Codec {
				case "S_TEXT/UTF8":
					t.text = "srt"
				case "S_TEXT/ASS", "S_TEXT/SSA", "S_ASS", "S_SSA":
					t.text = "ass"
				case "S_TEXT/WEBVTT":
					t.text = "vtt"
				}
			}
			if t.Type != "" {
				t.Name = friendly(t.Codec)
				t.Played = playable(t.Codec) || t.text != ""
				if t.Lang == "und" {
					t.Lang = ""
				}
				out = append(out, t)
			}
		}
		off = data + size
	}
	return out
}

type subCue struct {
	start, end float64
	text       string
}

// mkvSubtitles reads one text subtitle track out of an MKV file. It has to
// look through the whole file (subtitles are spread through it), but it
// skips over the picture and sound without reading them.
func mkvSubtitles(p string, track int, kind string) ([]subCue, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	e, off, end, ok := mkvSegment(f)
	if !ok {
		return nil, errors.New("not an MKV file")
	}
	scale := 1e6 // nanoseconds per timecode unit
	var cues []subCue
	for off < end {
		id, data, size, err := e.element(off)
		if err != nil {
			break
		}
		switch id {
		case idInfo:
			for q := data; size >= 0 && q < data+size; {
				cid, cdata, csize, err := e.element(q)
				if err != nil || csize < 0 {
					break
				}
				if cid == 0x2AD7B1 {
					scale = float64(beUint(e.bytesAt(cdata, csize)))
				}
				q = cdata + csize
			}
		case idCluster:
			cend := end
			if size >= 0 {
				cend = data + size
			}
			var ctc float64
			q := data
			for q < cend {
				cid, cdata, csize, err := e.element(q)
				if err != nil {
					break
				}
				if csize < 0 || cid == idCluster || cid == 0x1C53BB6B /* Cues */ {
					break // next cluster (unknown-size clusters end where the next begins)
				}
				switch cid {
				case idTimecode:
					ctc = float64(beUint(e.bytesAt(cdata, csize)))
				case idSimple:
					if c, ok := mkvBlock(e, cdata, csize, track, ctc, 0, scale, kind); ok {
						cues = append(cues, c)
					}
				case idGroup:
					var bOff, bSize int64 = -1, 0
					var dur float64
					for r := cdata; r < cdata+csize; {
						gid, gdata, gsize, err := e.element(r)
						if err != nil || gsize < 0 {
							break
						}
						if gid == idBlock {
							bOff, bSize = gdata, gsize
						} else if gid == idDuration {
							dur = float64(beUint(e.bytesAt(gdata, gsize)))
						}
						r = gdata + gsize
					}
					if bOff >= 0 {
						if c, ok := mkvBlock(e, bOff, bSize, track, ctc, dur, scale, kind); ok {
							cues = append(cues, c)
						}
					}
				}
				q = cdata + csize
			}
			off = q
			continue
		}
		if size < 0 {
			off = data
			continue
		}
		off = data + size
	}
	sort.SliceStable(cues, func(i, j int) bool { return cues[i].start < cues[j].start })
	for i := range cues {
		if cues[i].end <= cues[i].start {
			cues[i].end = cues[i].start + 3
			if i+1 < len(cues) && cues[i+1].start > cues[i].start {
				cues[i].end = min(cues[i].end, cues[i+1].start)
			}
		}
	}
	return cues, nil
}

func mkvBlock(e *ebml, off, size int64, track int, ctc, dur, scale float64, kind string) (subCue, bool) {
	num, n, err := e.vint(off, false)
	if err != nil || int(num) != track || size < int64(n)+3 {
		return subCue{}, false
	}
	hdr := e.bytesAt(off+int64(n), 3)
	if len(hdr) < 3 {
		return subCue{}, false
	}
	rel := float64(int16(binary.BigEndian.Uint16(hdr)))
	text := string(e.bytesAt(off+int64(n)+3, size-int64(n)-3))
	if kind == "ass" { // ReadOrder, Layer, Style, Name, MarginL, MarginR, MarginV, Effect, Text
		parts := strings.SplitN(text, ",", 9)
		if len(parts) == 9 {
			text = parts[8]
		}
		text = assText(text)
	}
	start := (ctc + rel) * scale / 1e9
	return subCue{start: start, end: start + dur*scale/1e9, text: strings.TrimSpace(text)}, strings.TrimSpace(text) != ""
}

// ---------- subtitle files ----------

var assTagRe = regexp.MustCompile(`\{[^}]*\}`)

func assText(s string) string {
	s = assTagRe.ReplaceAllString(s, "")
	s = strings.NewReplacer(`\N`, "\n", `\n`, "\n", `\h`, " ").Replace(s)
	return s
}

func assTime(s string) float64 {
	parts := strings.Split(strings.TrimSpace(s), ":")
	var t float64
	for _, p := range parts {
		v, _ := strconv.ParseFloat(p, 64)
		t = t*60 + v
	}
	return t
}

// parseSubtitleFile reads SRT, VTT, ASS or SSA.
func parseSubtitleFile(p string) ([]subCue, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	text := decodeText(b)
	switch extOf(p) {
	case ".ass", ".ssa":
		return parseASS(text), nil
	}
	var out []subCue
	for _, c := range parseVTTCues(text) { // SRT and VTT share the cue layout
		out = append(out, subCue{c.start, c.end, c.text})
	}
	return out, nil
}

func parseASS(text string) []subCue {
	var out []subCue
	format := []string{"layer", "start", "end", "style", "name", "marginl", "marginr", "marginv", "effect", "text"}
	inEvents := false
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		low := strings.ToLower(line)
		switch {
		case strings.HasPrefix(line, "["):
			inEvents = low == "[events]"
		case inEvents && strings.HasPrefix(low, "format:"):
			format = nil
			for _, f := range strings.Split(line[7:], ",") {
				format = append(format, strings.ToLower(strings.TrimSpace(f)))
			}
		case inEvents && strings.HasPrefix(low, "dialogue:"):
			parts := strings.SplitN(line[9:], ",", len(format))
			if len(parts) != len(format) {
				continue
			}
			var c subCue
			for i, f := range format {
				switch f {
				case "start":
					c.start = assTime(parts[i])
				case "end":
					c.end = assTime(parts[i])
				case "text":
					c.text = strings.TrimSpace(assText(parts[i]))
				}
			}
			if c.text != "" {
				out = append(out, c)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out
}

// decodeText handles UTF-8 (with or without BOM), UTF-16, and old
// single-byte files (Windows-1252).
func decodeText(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		return string(b[3:])
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}), bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		be := b[0] == 0xFE
		u := make([]uint16, 0, len(b)/2)
		for i := 2; i+1 < len(b); i += 2 {
			if be {
				u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
			} else {
				u = append(u, uint16(b[i+1])<<8|uint16(b[i]))
			}
		}
		return string(utf16.Decode(u))
	}
	if isUTF8(b) {
		return string(b)
	}
	r := make([]rune, len(b))
	for i, c := range b {
		r[i] = rune(c)
	}
	return string(r)
}

func isUTF8(b []byte) bool {
	return bytes.Equal([]byte(strings.ToValidUTF8(string(b), "\x00\x00")), b)
}

// toVTT writes cues as WebVTT for the player.
func toVTT(cs []subCue) string {
	var sb strings.Builder
	sb.WriteString("WEBVTT\n\n")
	ts := func(t float64) string {
		ms := int64(t*1000 + 0.5)
		if ms < 0 {
			ms = 0
		}
		return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
	}
	for _, c := range cs {
		if c.end <= c.start {
			continue
		}
		t := strings.ReplaceAll(c.text, "-->", "→")
		fmt.Fprintf(&sb, "%s --> %s\n%s\n\n", ts(c.start), ts(c.end), t)
	}
	return sb.String()
}

// subtitleFilesFor finds subtitle files that belong to a video: same name,
// optionally with a language ("Video.en.srt").
func subtitleFilesFor(video string) []string {
	dir := filepath.Dir(video)
	stem := strings.ToLower(strings.TrimSuffix(filepath.Base(video), filepath.Ext(video)))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		n := strings.ToLower(e.Name())
		switch filepath.Ext(n) {
		case ".srt", ".vtt", ".ass", ".ssa":
		default:
			continue
		}
		if strings.HasPrefix(n, stem+".") || strings.HasPrefix(n, stem+" ") || strings.HasPrefix(n, stem+"_") || strings.TrimSuffix(n, filepath.Ext(n)) == stem {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

func subtitleLabel(video, sub string) string {
	stem := strings.TrimSuffix(filepath.Base(video), filepath.Ext(video))
	s := strings.TrimSuffix(filepath.Base(sub), filepath.Ext(sub))
	s = strings.Trim(strings.TrimPrefix(s, stem), " ._-")
	if s == "" {
		return "Subtitles (" + strings.ToUpper(strings.TrimPrefix(filepath.Ext(sub), ".")) + ")"
	}
	return s
}

// ---------- music tags ----------

type MediaTags struct {
	Title, Artist, Album string
	Cover                []byte
	CoverType            string
}

func readTags(p string) *MediaTags {
	f, err := os.Open(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	head := make([]byte, 12)
	io.ReadFull(f, head)
	f.Seek(0, io.SeekStart)
	switch {
	case bytes.HasPrefix(head, []byte("ID3")):
		return id3Tags(f)
	case bytes.HasPrefix(head, []byte("fLaC")):
		return flacTags(f)
	case bytes.HasPrefix(head, []byte("OggS")):
		return oggTags(f)
	case string(head[4:8]) == "ftyp":
		return mp4Tags(f)
	}
	return nil
}

func id3Text(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	enc, b := b[0], b[1:]
	var s string
	switch enc {
	case 1, 2:
		be := enc == 2
		if len(b) >= 2 && (b[0] == 0xFE && b[1] == 0xFF) {
			be, b = true, b[2:]
		} else if len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE {
			be, b = false, b[2:]
		}
		u := make([]uint16, 0, len(b)/2)
		for i := 0; i+1 < len(b); i += 2 {
			if be {
				u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
			} else {
				u = append(u, uint16(b[i+1])<<8|uint16(b[i]))
			}
		}
		s = string(utf16.Decode(u))
	case 3:
		s = string(b)
	default:
		r := make([]rune, len(b))
		for i, c := range b {
			r[i] = rune(c)
		}
		s = string(r)
	}
	return strings.TrimRight(s, "\x00 ")
}

func syncsafe(b []byte) int {
	return int(b[0])<<21 | int(b[1])<<14 | int(b[2])<<7 | int(b[3])
}

func id3Tags(f *os.File) *MediaTags {
	h := make([]byte, 10)
	if _, err := io.ReadFull(f, h); err != nil {
		return nil
	}
	ver := h[3]
	size := syncsafe(h[6:10])
	if size > 32<<20 {
		return nil
	}
	b := make([]byte, size)
	n, _ := io.ReadFull(f, b)
	b = b[:n]
	t := &MediaTags{}
	idLen, hdrLen := 4, 10
	if ver == 2 {
		idLen, hdrLen = 3, 6
	}
	for p := 0; p+hdrLen <= len(b); {
		id := string(b[p : p+idLen])
		if id[0] == 0 {
			break
		}
		var fs int
		switch ver {
		case 2:
			fs = int(b[p+3])<<16 | int(b[p+4])<<8 | int(b[p+5])
		case 4:
			fs = syncsafe(b[p+4 : p+8])
		default:
			fs = int(binary.BigEndian.Uint32(b[p+4 : p+8]))
		}
		p += hdrLen
		if fs <= 0 || p+fs > len(b) {
			break
		}
		data := b[p : p+fs]
		switch id {
		case "TIT2", "TT2":
			t.Title = id3Text(data)
		case "TPE1", "TP1":
			t.Artist = id3Text(data)
		case "TALB", "TAL":
			t.Album = id3Text(data)
		case "APIC", "PIC":
			if t.Cover == nil && len(data) > 4 {
				enc := data[0]
				q := 1
				if id == "PIC" {
					q += 3 // image format
				} else {
					z := bytes.IndexByte(data[q:], 0)
					if z < 0 {
						break
					}
					q += z + 1
				}
				q++ // picture type
				if q >= len(data) {
					break
				}
				// description, ended by 0 (or 00 00 for UTF-16)
				if enc == 1 || enc == 2 {
					for q+1 < len(data) && !(data[q] == 0 && data[q+1] == 0) {
						q += 2
					}
					q += 2
				} else {
					z := bytes.IndexByte(data[q:], 0)
					if z < 0 {
						break
					}
					q += z + 1
				}
				if q < len(data) {
					t.Cover = data[q:]
				}
			}
		}
		p += fs
	}
	return t
}

func vorbisComments(b []byte, t *MediaTags) {
	if len(b) < 8 {
		return
	}
	vl := int(binary.LittleEndian.Uint32(b))
	p := 4 + vl
	if p+4 > len(b) {
		return
	}
	n := int(binary.LittleEndian.Uint32(b[p:]))
	p += 4
	for i := 0; i < n && p+4 <= len(b); i++ {
		l := int(binary.LittleEndian.Uint32(b[p:]))
		p += 4
		if l < 0 || p+l > len(b) {
			return
		}
		kv := string(b[p : p+l])
		p += l
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch strings.ToUpper(k) {
		case "TITLE":
			t.Title = v
		case "ARTIST":
			t.Artist = v
		case "ALBUM":
			t.Album = v
		case "METADATA_BLOCK_PICTURE":
			if raw, err := base64.StdEncoding.DecodeString(v); err == nil && t.Cover == nil {
				t.Cover = flacPicture(raw)
			}
		}
	}
}

func flacPicture(b []byte) []byte {
	p := 4
	if p+4 > len(b) {
		return nil
	}
	ml := int(binary.BigEndian.Uint32(b[p:]))
	p += 4 + ml
	if p+4 > len(b) {
		return nil
	}
	dl := int(binary.BigEndian.Uint32(b[p:]))
	p += 4 + dl + 16
	if p+4 > len(b) {
		return nil
	}
	n := int(binary.BigEndian.Uint32(b[p:]))
	p += 4
	if n < 0 || p+n > len(b) {
		return nil
	}
	return b[p : p+n]
}

func flacTags(f *os.File) *MediaTags {
	r := bufio.NewReader(f)
	r.Discard(4)
	t := &MediaTags{}
	for i := 0; i < 64; i++ {
		h := make([]byte, 4)
		if _, err := io.ReadFull(r, h); err != nil {
			break
		}
		last, typ := h[0]&0x80 != 0, h[0]&0x7f
		n := int(h[1])<<16 | int(h[2])<<8 | int(h[3])
		if n > 32<<20 {
			break
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(r, b); err != nil {
			break
		}
		switch typ {
		case 4:
			vorbisComments(b, t)
		case 6:
			if t.Cover == nil {
				t.Cover = flacPicture(b)
			}
		}
		if last {
			break
		}
	}
	return t
}

// oggTags reads the comment packet (second packet) of an Ogg Vorbis / Opus file.
func oggTags(f *os.File) *MediaTags {
	r := bufio.NewReader(f)
	var packets [][]byte
	var cur []byte
	for pages := 0; pages < 200 && len(packets) < 2; pages++ {
		h := make([]byte, 27)
		if _, err := io.ReadFull(r, h); err != nil || string(h[:4]) != "OggS" {
			break
		}
		segs := make([]byte, h[26])
		if _, err := io.ReadFull(r, segs); err != nil {
			break
		}
		for _, l := range segs {
			b := make([]byte, l)
			if _, err := io.ReadFull(r, b); err != nil {
				return nil
			}
			cur = append(cur, b...)
			if l < 255 {
				packets = append(packets, cur)
				cur = nil
				if len(packets) == 2 {
					break
				}
			}
			if len(cur) > 32<<20 {
				return nil
			}
		}
	}
	if len(packets) < 2 {
		return nil
	}
	t := &MediaTags{}
	p := packets[1]
	switch {
	case bytes.HasPrefix(p, []byte("\x03vorbis")):
		vorbisComments(p[7:], t)
	case bytes.HasPrefix(p, []byte("OpusTags")):
		vorbisComments(p[8:], t)
	}
	return t
}

func mp4Tags(f *os.File) *MediaTags {
	st, err := f.Stat()
	if err != nil {
		return nil
	}
	moov := findBox(f, 0, st.Size(), "moov")
	meta := firstChild(firstChild(moov, "udta"), "meta")
	if len(meta) < 4 {
		return nil
	}
	ilst := firstChild(meta[4:], "ilst") // meta is a "full box": 4 bytes of version/flags first
	if ilst == nil {
		return nil
	}
	t := &MediaTags{}
	eachBox(ilst, func(typ string, body []byte) bool {
		data := firstChild(body, "data")
		if len(data) < 8 {
			return true
		}
		v := data[8:]
		switch typ {
		case "\xa9nam":
			t.Title = string(v)
		case "\xa9ART", "aART":
			if t.Artist == "" {
				t.Artist = string(v)
			}
		case "\xa9alb":
			t.Album = string(v)
		case "covr":
			t.Cover = v
		}
		return true
	})
	return t
}
