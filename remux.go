package main

// Joining video chunks into one normal MP4 (or M4A for sound only), built in
// (no ffmpeg needed). Handles MPEG-TS chunks (most HLS), fragmented MP4 chunks
// (DASH and newer HLS) and raw AAC sound chunks, and lines the sound up with
// the picture. If something unusual comes along, it uses ffmpeg when present,
// and otherwise saves the joined stream as-is so you still get a playable file.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h265"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/fmp4/seekablebuffer"
	mp4codecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts"
	tscodecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mpegts/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/pmp4"
)

var errUnsupportedFormat = errors.New("unsupported video format")

type joinResult struct {
	path      string
	ext       string
	note      string
	extras    []joinExtra
	clipStart float64 // where a clip really starts (seconds into the video)
}

type joinExtra struct {
	path   string
	suffix string
}

type trackFiles struct {
	role     string
	files    []string
	format   string
	size     int64
	segStart float64 // clips: when the first chunk starts
}

func sniffFormat(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	b := make([]byte, 400)
	n, _ := io.ReadFull(f, b)
	b = b[:n]
	switch {
	case n >= 1 && b[0] == 0x47 && (n < 189 || b[188] == 0x47):
		return "ts"
	case n >= 8 && strings.Contains("|ftyp|styp|moof|sidx|moov|emsg|prft|free|", "|"+string(b[4:8])+"|"):
		return "fmp4"
	case bytes.HasPrefix(b, []byte("ID3")) || (n >= 2 && b[0] == 0xFF && b[1]&0xF6 == 0xF0):
		return "adts"
	case bytes.HasPrefix(b, []byte{0x1A, 0x45, 0xDF, 0xA3}):
		return "webm"
	case bytes.HasPrefix(b, []byte("FLV\x01")):
		return "flv"
	}
	return ""
}

func (m *Manager) joinMedia(ctx context.Context, plan *MediaPlan, dir string, progress func(float64)) (*joinResult, error) {
	var groups []trackFiles
	var total int64
	for ti, t := range plan.Tracks {
		g := trackFiles{role: t.Role, segStart: t.SegStart}
		if t.Init != nil {
			g.files = append(g.files, filepath.Join(dir, fmt.Sprintf("t%d_init", ti)))
		}
		for si := range t.Segs {
			g.files = append(g.files, filepath.Join(dir, fmt.Sprintf("t%d_%06d", ti, si)))
		}
		probe := g.files[0]
		if t.Init != nil && len(g.files) > 1 {
			if f := sniffFormat(g.files[0]); f == "fmp4" || f == "webm" {
				g.format = f
			} else {
				probe = g.files[1]
			}
		}
		if g.format == "" {
			g.format = sniffFormat(probe)
		}
		for _, f := range g.files {
			if st, err := os.Stat(f); err == nil {
				g.size += st.Size()
			}
		}
		total += g.size
		groups = append(groups, g)
	}

	out := filepath.Join(dir, "joined")
	ext, clipStart, err := remuxPure(ctx, groups, plan.AudioOnly, plan.Clip, dir, out, total, progress)
	if err == nil {
		return &joinResult{path: out + ext, ext: ext, clipStart: clipStart}, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	pureErr := err

	// Clips joined the plain way aren't trimmed: they start where the first chunk does.
	chunkStart := 0.0
	if plan.Clip != nil {
		chunkStart = math.Inf(1)
		for _, t := range plan.Tracks {
			chunkStart = math.Min(chunkStart, t.SegStart)
		}
	}
	// Plan B: ffmpeg, if the computer has it.
	if ff := ffmpegPath(); ff != "" {
		if res, err := joinWithFFmpeg(ctx, ff, groups, plan.AudioOnly, dir); err == nil {
			res.clipStart = chunkStart
			return res, nil
		}
	}
	// Plan C: keep the stream as it came (still plays in VLC and most players).
	res, err := joinRaw(ctx, groups, dir, pureErr)
	if res != nil {
		res.clipStart = chunkStart
	}
	return res, err
}

// ---------- collecting samples ----------

type sampleRec struct {
	dts, pts int64
	off      int64
	size     uint32
	sync     bool
}

type outTrack struct {
	codec     mp4codecs.Codec
	timescale uint32
	video     bool
	samples   []sampleRec
	relative  bool // timing unknown (raw AAC without timestamps): line it up with the video

	clipOffset    float64 // seconds to add to this track's clock to get video time (clips)
	editDelay     float64 // seconds of silence/black before this track starts (MP4 edit list)
	editMediaTime int64   // first media time shown (MP4 edit list), in track units

	lastRaw  int64
	wrapOff  int64
	haveRaw  bool
	adjust   int64
	audioDur int64
}

// unwrap turns 33-bit MPEG-TS clock values into a continuous count.
func (t *outTrack) unwrap(ts int64) int64 {
	if !t.haveRaw {
		t.haveRaw = true
		t.lastRaw = ts
		return ts
	}
	d := ts - t.lastRaw
	if d < -(1 << 32) {
		t.wrapOff += 1 << 33
	} else if d > (1 << 32) {
		t.wrapOff -= 1 << 33
	}
	t.lastRaw = ts
	return ts + t.wrapOff
}

func (t *outTrack) lastStep() int64 {
	n := len(t.samples)
	if n >= 2 {
		if s := t.samples[n-1].dts - t.samples[n-2].dts; s > 0 {
			return s
		}
	}
	if !t.video && t.audioDur > 0 {
		return t.audioDur
	}
	return int64(t.timescale) / 30
}

type rawStore struct {
	w   *bufio.Writer
	off int64
}

func (r *rawStore) put(b []byte) (int64, error) {
	off := r.off
	n, err := r.w.Write(b)
	r.off += int64(n)
	return off, err
}

// add appends a sample, smoothing over timestamp jumps (ad breaks, stream splices).
func (t *outTrack) add(raw *rawStore, dts, pts int64, payload []byte, sync, continuity bool) error {
	dts += t.adjust
	pts += t.adjust
	if n := len(t.samples); n > 0 && continuity {
		prev := t.samples[n-1].dts
		if dts <= prev || dts-prev > int64(t.timescale)*4 {
			fix := prev + t.lastStep() - dts
			t.adjust += fix
			dts += fix
			pts += fix
		}
	}
	if n := len(t.samples); n > 0 && !continuity && dts < t.samples[n-1].dts {
		dts = t.samples[n-1].dts
	}
	if pts < dts {
		pts = dts
	}
	off, err := raw.put(payload)
	if err != nil {
		return err
	}
	t.samples = append(t.samples, sampleRec{dts: dts, pts: pts, off: off, size: uint32(len(payload)), sync: sync})
	return nil
}

func (t *outTrack) addAudio(raw *rawStore, ts int64, payload []byte, frameDur int64) error {
	t.audioDur = frameDur
	if n := len(t.samples); n > 0 {
		expected := t.samples[n-1].dts + frameDur - t.adjust
		if d := ts - expected; d > -frameDur && d < frameDur {
			ts = expected // tiny rounding differences: keep frames back to back
		}
	}
	return t.add(raw, ts, ts, payload, true, true)
}

func cloneBytes(b []byte) []byte { return append([]byte(nil), b...) }

type countingReader struct {
	r    io.Reader
	n    int64
	ctx  context.Context
	tick func(int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	if c.tick != nil && n > 0 {
		c.tick(c.n)
	}
	return n, err
}

// seqReader reads a list of files one after another, opening each only when needed.
type seqReader struct {
	paths []string
	i     int
	cur   *os.File
}

func (s *seqReader) Read(p []byte) (int, error) {
	for {
		if s.cur == nil {
			if s.i >= len(s.paths) {
				return 0, io.EOF
			}
			f, err := os.Open(s.paths[s.i])
			if err != nil {
				return 0, err
			}
			s.i++
			s.cur = f
		}
		n, err := s.cur.Read(p)
		if n > 0 {
			return n, nil
		}
		s.cur.Close()
		s.cur = nil
		if err != nil && err != io.EOF {
			return 0, err
		}
	}
}

func (s *seqReader) Close() {
	if s.cur != nil {
		s.cur.Close()
	}
}

func collectTS(ctx context.Context, rd io.Reader, raw *rawStore, wantVideo, wantAudio bool) ([]*outTrack, error) {
	r := &mpegts.Reader{R: rd}
	if err := r.Initialize(); err != nil {
		return nil, fmt.Errorf("reading video chunks: %w", err)
	}
	r.OnDecodeError(func(error) {})
	var outs []*outTrack
	var finals []func()
	for _, tr := range r.Tracks() {
		switch c := tr.Codec.(type) {
		case *tscodecs.H264:
			if !wantVideo {
				continue
			}
			ot := &outTrack{video: true, timescale: 90000}
			var sps, pps []byte
			r.OnDataH264(tr, func(pts, dts int64, au [][]byte) error {
				pts, dts = ot.unwrapPair(pts, dts)
				var keep [][]byte
				for _, n := range au {
					if len(n) == 0 {
						continue
					}
					switch h264.NALUType(n[0] & 0x1F) {
					case h264.NALUTypeSPS:
						if sps == nil {
							sps = cloneBytes(n)
						}
						continue
					case h264.NALUTypePPS:
						if pps == nil {
							pps = cloneBytes(n)
						}
						continue
					case h264.NALUTypeAccessUnitDelimiter:
						continue
					}
					keep = append(keep, n)
				}
				if len(keep) == 0 {
					return nil
				}
				sync := h264.IsRandomAccess(au)
				if len(ot.samples) == 0 && (!sync || sps == nil || pps == nil) {
					return nil // start on a whole picture
				}
				payload, err := h264.AVCC(keep).Marshal()
				if err != nil {
					return nil
				}
				return ot.add(raw, dts, pts, payload, sync, true)
			})
			finals = append(finals, func() {
				if sps != nil && pps != nil {
					ot.codec = &mp4codecs.H264{SPS: sps, PPS: pps}
				}
			})
			outs = append(outs, ot)
		case *tscodecs.H265:
			if !wantVideo {
				continue
			}
			ot := &outTrack{video: true, timescale: 90000}
			var vps, sps, pps []byte
			r.OnDataH265(tr, func(pts, dts int64, au [][]byte) error {
				pts, dts = ot.unwrapPair(pts, dts)
				var keep [][]byte
				for _, n := range au {
					if len(n) == 0 {
						continue
					}
					switch h265.NALUType((n[0] >> 1) & 0x3F) {
					case h265.NALUType_VPS_NUT:
						if vps == nil {
							vps = cloneBytes(n)
						}
						continue
					case h265.NALUType_SPS_NUT:
						if sps == nil {
							sps = cloneBytes(n)
						}
						continue
					case h265.NALUType_PPS_NUT:
						if pps == nil {
							pps = cloneBytes(n)
						}
						continue
					case h265.NALUType_AUD_NUT:
						continue
					}
					keep = append(keep, n)
				}
				if len(keep) == 0 {
					return nil
				}
				sync := h265.IsRandomAccess(au)
				if len(ot.samples) == 0 && (!sync || vps == nil || sps == nil || pps == nil) {
					return nil
				}
				payload, err := h264.AVCC(keep).Marshal()
				if err != nil {
					return nil
				}
				return ot.add(raw, dts, pts, payload, sync, true)
			})
			finals = append(finals, func() {
				if vps != nil && sps != nil && pps != nil {
					ot.codec = &mp4codecs.H265{VPS: vps, SPS: sps, PPS: pps}
				}
			})
			outs = append(outs, ot)
		case *tscodecs.MPEG4Audio:
			if !wantAudio {
				continue
			}
			cfg := c.Config
			sr := int64(cfg.SampleRate)
			if sr <= 0 {
				continue
			}
			ot := &outTrack{timescale: uint32(sr), codec: &mp4codecs.MPEG4Audio{Config: cfg}}
			r.OnDataMPEG4Audio(tr, func(pts int64, aus [][]byte) error {
				base := ot.unwrap(pts) * sr / 90000
				for i, au := range aus {
					if err := ot.addAudio(raw, base+int64(i)*1024, au, 1024); err != nil {
						return err
					}
				}
				return nil
			})
			outs = append(outs, ot)
		}
	}
	if len(outs) == 0 {
		return nil, errUnsupportedFormat
	}
	for {
		err := r.Read()
		if err == io.EOF || errors.Is(err, io.ErrUnexpectedEOF) {
			break
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
	}
	for _, f := range finals {
		f()
	}
	return outs, nil
}

func (t *outTrack) unwrapPair(pts, dts int64) (int64, int64) {
	d := t.unwrap(dts)
	diff := pts - dts
	if diff < -(1 << 32) {
		diff += 1 << 33
	} else if diff > (1 << 32) {
		diff -= 1 << 33
	}
	return d + diff, d
}

// collectADTS reads raw AAC sound chunks (HLS "packed audio").
func collectADTS(ctx context.Context, paths []string, raw *rawStore) (*outTrack, error) {
	ot := &outTrack{relative: true}
	var sr int64
	next := int64(-1)
	for _, p := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		segStart := int64(-1)
		for len(data) >= 10 && bytes.HasPrefix(data, []byte("ID3")) {
			size := int(data[6]&0x7F)<<21 | int(data[7]&0x7F)<<14 | int(data[8]&0x7F)<<7 | int(data[9]&0x7F)
			end := 10 + size
			if data[5]&0x10 != 0 {
				end += 10 // footer
			}
			if end > len(data) {
				return nil, errUnsupportedFormat
			}
			if ts, ok := id3Timestamp(data[10 : 10+size]); ok {
				segStart = ts
			}
			data = data[end:]
		}
		if len(data) == 0 {
			continue
		}
		var pkts mpeg4audio.ADTSPackets
		if err := pkts.Unmarshal(data); err != nil {
			return nil, fmt.Errorf("reading sound chunks: %w", err)
		}
		if ot.codec == nil {
			pk := pkts[0]
			sr = int64(pk.SampleRate)
			ot.timescale = uint32(sr)
			ot.codec = &mp4codecs.MPEG4Audio{Config: mpeg4audio.AudioSpecificConfig{
				Type: pk.Type, SampleRate: pk.SampleRate, ChannelConfig: pk.ChannelConfig, ChannelCount: pk.ChannelCount, //nolint:staticcheck
			}}
		}
		t := next
		if segStart >= 0 {
			ot.relative = false
			t = ot.unwrap(segStart) * sr / 90000
		} else if t < 0 {
			t = 0
		}
		for _, pk := range pkts {
			if err := ot.addAudio(raw, t, pk.AU, 1024); err != nil {
				return nil, err
			}
			t += 1024
		}
		next = t
	}
	if ot.codec == nil {
		return nil, errUnsupportedFormat
	}
	return ot, nil
}

// id3Timestamp finds Apple's HLS timestamp (90 kHz clock) in an ID3 tag.
func id3Timestamp(tag []byte) (int64, bool) {
	const owner = "com.apple.streaming.transportStreamTimestamp"
	i := bytes.Index(tag, []byte(owner))
	if i < 0 || i+len(owner)+1+8 > len(tag) {
		return 0, false
	}
	v := binary.BigEndian.Uint64(tag[i+len(owner)+1:])
	return int64(v & (1<<33 - 1)), true
}

var errNotFragmented = errors.New("not a fragmented MP4")

// maxBox: the biggest piece of a video chunk read into memory at once.
var maxBox int64 = 512 << 20

func collectFMP4(ctx context.Context, rd io.Reader, raw *rawStore, wantVideo, wantAudio bool) ([]*outTrack, error) {
	br := bufio.NewReaderSize(rd, 1<<20)
	tracks := map[int]*outTrack{}
	known := map[int]bool{}
	var ordered []*outTrack
	var initBuf, moof []byte
	initDone := false
	hdr := make([]byte, 16)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(br, hdr[:8]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return nil, err
		}
		size := uint64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		hlen := uint64(8)
		if size == 1 {
			if _, err := io.ReadFull(br, hdr[8:16]); err != nil {
				return nil, err
			}
			size = binary.BigEndian.Uint64(hdr[8:16])
			hlen = 16
		}
		if typ == "mdat" && moof == nil {
			return nil, errNotFragmented // before reading what may be the whole film into memory
		}
		if size == 0 { // box runs to the end
			rest, err := io.ReadAll(io.LimitReader(br, maxBox+1))
			if err != nil {
				return nil, err
			}
			size = hlen + uint64(len(rest))
			br = bufio.NewReader(io.MultiReader(bytes.NewReader(rest), eofReader{}))
		}
		// a real chunk's box is a few MB; a damaged or made-up size must not use up the memory
		if size < hlen || size > uint64(maxBox) {
			return nil, errors.New("damaged video chunk")
		}
		box := make([]byte, size)
		copy(box, hdr[:hlen])
		if _, err := io.ReadFull(br, box[hlen:]); err != nil {
			if err == io.ErrUnexpectedEOF {
				break
			}
			return nil, err
		}
		switch typ {
		case "ftyp":
			if !initDone {
				initBuf = box
			}
		case "moov":
			if initDone {
				continue
			}
			if bytes.Contains(box, []byte("encv")) || bytes.Contains(box, []byte("enca")) || bytes.Contains(box, []byte("pssh")) {
				return nil, errDRM
			}
			var ini fmp4.Init
			if err := ini.Unmarshal(bytes.NewReader(append(initBuf, box...))); err != nil {
				return nil, fmt.Errorf("reading video header: %w", err)
			}
			edits := parseEditLists(box)
			for _, it := range ini.Tracks {
				known[it.ID] = true
				if it.Codec == nil {
					continue
				}
				video := it.Codec.IsVideo()
				if (video && !wantVideo) || (!video && !wantAudio) {
					continue
				}
				ot := &outTrack{codec: it.Codec, timescale: it.TimeScale, video: video}
				if e, ok := edits[it.ID]; ok {
					ot.editDelay, ot.editMediaTime = e.delay, e.mediaTime
				}
				tracks[it.ID] = ot
				ordered = append(ordered, ot)
			}
			initDone = true
		case "moof":
			moof = box
		case "mdat":
			if moof == nil {
				return nil, errNotFragmented
			}
			var parts fmp4.Parts
			if err := parts.Unmarshal(append(moof, box...)); err != nil {
				return nil, fmt.Errorf("reading video chunk: %w", err)
			}
			moof = nil
			for _, part := range parts {
				for _, pt := range part.Tracks {
					ot := tracks[pt.ID]
					if ot == nil {
						continue
					}
					t := int64(pt.BaseTime)
					for _, s := range pt.Samples {
						sync := !s.IsNonSyncSample
						switch ot.codec.(type) {
						case *mp4codecs.H264:
							var au h264.AVCC
							if au.Unmarshal(s.Payload) == nil {
								sync = h264.IsRandomAccess(au)
							}
						case *mp4codecs.H265:
							var au h264.AVCC
							if au.Unmarshal(s.Payload) == nil {
								sync = h265.IsRandomAccess(au)
							}
						}
						if ot.video && len(ot.samples) == 0 && !sync {
							t += int64(s.Duration)
							continue
						}
						if err := ot.add(raw, t, t+int64(s.PTSOffset), s.Payload, sync, false); err != nil {
							return nil, err
						}
						t += int64(s.Duration)
					}
				}
			}
		}
	}
	if !initDone {
		return nil, errors.New("the video header is missing")
	}
	if len(ordered) == 0 {
		return nil, errUnsupportedFormat
	}
	for _, ot := range ordered {
		spreadEqualTimes(ot)
	}
	return ordered, nil
}

type eofReader struct{}

func (eofReader) Read([]byte) (int, error) { return 0, io.EOF }

// spreadEqualTimes fixes chunks that don't state each frame's duration.
func spreadEqualTimes(t *outTrack) {
	s := t.samples
	for i := 0; i < len(s); {
		j := i + 1
		for j < len(s) && s[j].dts == s[i].dts {
			j++
		}
		if j-i > 1 {
			step := int64(0)
			if j < len(s) {
				step = (s[j].dts - s[i].dts) / int64(j-i)
			}
			if step <= 0 {
				step = t.lastStepBefore(i)
			}
			for k := i + 1; k < j; k++ {
				shift := int64(k-i) * step
				s[k].dts += shift
				s[k].pts += shift
			}
		}
		i = j
	}
}

func (t *outTrack) lastStepBefore(i int) int64 {
	if i >= 2 {
		if s := t.samples[i-1].dts - t.samples[i-2].dts; s > 0 {
			return s
		}
	}
	if t.video {
		return int64(t.timescale) / 30
	}
	return 1024
}

// ---------- writing the MP4 ----------

func remuxPure(ctx context.Context, groups []trackFiles, audioOnly bool, clip *ClipSpec, dir, out string, total int64, progress func(float64)) (string, float64, error) {
	ext, start, err := remuxPureInner(ctx, groups, audioOnly, clip, dir, out, total, progress)
	return ext, start, err
}

func remuxPureInner(ctx context.Context, groups []trackFiles, audioOnly bool, clip *ClipSpec, dir, out string, total int64, progress func(float64)) (string, float64, error) {
	rawPath := filepath.Join(dir, "samples.raw")
	rf, err := os.Create(rawPath)
	if err != nil {
		return "", 0, err
	}
	defer func() {
		rf.Close()
		os.Remove(rawPath)
	}()
	raw := &rawStore{w: bufio.NewWriterSize(rf, 4<<20)}

	separateAudio := false
	for _, g := range groups {
		if g.role == "audio" {
			separateAudio = true
		}
	}
	var tracks []*outTrack
	var done int64
	for _, g := range groups {
		wantVideo := g.role == "main" && !audioOnly
		wantAudio := g.role == "audio" || !separateAudio
		if !wantVideo && !wantAudio {
			done += g.size
			continue
		}
		base := done
		sr := &seqReader{paths: g.files}
		cr := &countingReader{r: sr, ctx: ctx, tick: func(n int64) {
			if total > 0 {
				progress(0.6 * float64(base+n) / float64(total))
			}
		}}
		var got []*outTrack
		switch g.format {
		case "ts":
			got, err = collectTS(ctx, cr, raw, wantVideo, wantAudio)
		case "fmp4":
			got, err = collectFMP4(ctx, cr, raw, wantVideo, wantAudio)
		case "flv":
			got, err = collectFLV(ctx, cr, raw, wantVideo, wantAudio)
		case "adts":
			if wantAudio {
				var ot *outTrack
				ot, err = collectADTS(ctx, g.files, raw)
				if ot != nil {
					got = []*outTrack{ot}
				}
			}
		default:
			err = errUnsupportedFormat
		}
		sr.Close()
		if err != nil {
			return "", 0, err
		}
		if clip != nil {
			setClipOffsets(got, g.segStart)
		}
		for _, t := range got {
			if t.codec != nil && len(t.samples) > 0 {
				tracks = append(tracks, t)
			}
		}
		done += g.size
	}
	if err := raw.w.Flush(); err != nil {
		return "", 0, err
	}
	clipStart := 0.0
	if clip != nil && len(tracks) > 0 {
		tracks, clipStart = trimTracks(tracks, clip)
	}
	if len(tracks) == 0 {
		return "", 0, errUnsupportedFormat
	}
	hasVideo := false
	var payload int64
	for _, t := range tracks {
		hasVideo = hasVideo || t.video
		for _, s := range t.samples {
			payload += int64(s.size)
		}
	}
	if !hasVideo && !audioOnly {
		return "", 0, errors.New("no picture found in this video")
	}
	ext := ".mp4"
	if !hasVideo {
		ext = ".m4a"
	}
	// Video first, then sound: what players expect.
	sort.SliceStable(tracks, func(i, j int) bool { return tracks[i].video && !tracks[j].video })

	tmp := out + ext + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return "", 0, err
	}
	bw := bufio.NewWriterSize(f, 4<<20)
	cw := &ctxWriter{w: bw, ctx: ctx, total: payload, tick: func(n, t int64) {
		if t > 0 {
			progress(0.6 + 0.4*math.Min(1, float64(n)/float64(t)))
		}
	}}
	if payload < 3_900_000_000 {
		err = writeProgressiveMP4(tracks, rf, cw)
	} else {
		err = writeFragmentedMP4(tracks, rf, cw)
	}
	if err == nil {
		err = bw.Flush()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return "", 0, err
	}
	if err := os.Rename(tmp, out+ext); err != nil {
		return "", 0, err
	}
	progress(1)
	return ext, clipStart, nil
}

type ctxWriter struct {
	w     io.Writer
	ctx   context.Context
	n     int64
	total int64
	tick  func(n, total int64)
}

func (c *ctxWriter) Write(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.w.Write(p)
	c.n += int64(n)
	c.tick(c.n, c.total)
	return n, err
}

// alignment works out where each track starts so sound and picture line up.
func alignment(tracks []*outTrack) []float64 {
	ref := math.NaN()
	starts := make([]float64, len(tracks))
	for i, t := range tracks {
		minPTS := t.samples[0].pts
		for k := 0; k < len(t.samples) && k < 16; k++ {
			if t.samples[k].pts < minPTS {
				minPTS = t.samples[k].pts
			}
		}
		starts[i] = t.editDelay + float64(minPTS-t.editMediaTime)/float64(t.timescale)
		if t.video && math.IsNaN(ref) {
			ref = starts[i]
		}
	}
	if math.IsNaN(ref) {
		ref = math.Inf(1)
		for i, t := range tracks {
			if !t.relative && starts[i] < ref {
				ref = starts[i]
			}
		}
		if math.IsInf(ref, 1) {
			ref = 0
		}
	}
	delays := make([]float64, len(tracks))
	for i, t := range tracks {
		if t.relative {
			delays[i] = 0
			continue
		}
		delays[i] = starts[i] - ref
	}
	return delays
}

func sampleDurations(t *outTrack) []int64 {
	n := len(t.samples)
	durs := make([]int64, n)
	for k := 0; k < n-1; k++ {
		durs[k] = t.samples[k+1].dts - t.samples[k].dts
		if durs[k] <= 0 {
			durs[k] = 1
		}
	}
	if n >= 2 {
		durs[n-1] = durs[n-2]
	} else if t.video {
		durs[n-1] = int64(t.timescale) / 30
	} else {
		durs[n-1] = 1024
	}
	return durs
}

func writeProgressiveMP4(tracks []*outTrack, raw *os.File, w io.Writer) error {
	delays := alignment(tracks)
	pres := pmp4.Presentation{}
	for i, t := range tracks {
		durs := sampleDurations(t)
		ps := make([]*pmp4.Sample, len(t.samples))
		for k := range t.samples {
			s := t.samples[k]
			ptsOff := s.pts - s.dts
			if ptsOff > math.MaxInt32 {
				ptsOff = 0
			}
			ps[k] = &pmp4.Sample{
				Duration:        uint32(durs[k]),
				PTSOffset:       int32(ptsOff),
				IsNonSyncSample: !s.sync,
				PayloadSize:     s.size,
				GetPayload: func() ([]byte, error) {
					buf := make([]byte, s.size)
					_, err := raw.ReadAt(buf, s.off)
					return buf, err
				},
			}
		}
		// Time offset: skip the decoder lead-in and add/trim any start delay.
		lead := t.samples[0].pts - t.samples[0].dts
		for k := 0; k < len(t.samples) && k < 16; k++ {
			if d := t.samples[k].pts - t.samples[0].dts; d < lead {
				lead = d
			}
		}
		offset := int64(math.Round(delays[i]*float64(t.timescale))) - lead
		if offset > math.MaxInt32 || offset < math.MinInt32 {
			offset = 0
		}
		pres.Tracks = append(pres.Tracks, &pmp4.Track{
			ID:         i + 1,
			TimeScale:  t.timescale,
			TimeOffset: int32(offset),
			Codec:      t.codec,
			Samples:    ps,
		})
	}
	return pres.Marshal(w)
}

// writeFragmentedMP4 handles files over 4 GB, which the classic MP4 layout can't index.
func writeFragmentedMP4(tracks []*outTrack, raw *os.File, w io.Writer) error {
	delays := alignment(tracks)
	ini := fmp4.Init{}
	for i, t := range tracks {
		ini.Tracks = append(ini.Tracks, &fmp4.InitTrack{ID: i + 1, TimeScale: t.timescale, Codec: t.codec})
	}
	var sb seekablebuffer.Buffer
	if err := ini.Marshal(&sb); err != nil {
		return err
	}
	if _, err := w.Write(sb.Bytes()); err != nil {
		return err
	}
	durs := make([][]int64, len(tracks))
	pos := make([]int, len(tracks))
	base := make([]int64, len(tracks))
	for i, t := range tracks {
		durs[i] = sampleDurations(t)
		shift := int64(math.Round(delays[i] * float64(t.timescale)))
		// drop samples that come before the picture starts
		for pos[i] < len(t.samples) && t.samples[pos[i]].dts-t.samples[0].dts+shift < 0 {
			pos[i]++
		}
		if pos[i] < len(t.samples) {
			base[i] = t.samples[pos[i]].dts - t.samples[0].dts + shift
		}
	}
	seq := uint32(1)
	for {
		best := -1
		bestT := 0.0
		for i, t := range tracks {
			if pos[i] < len(t.samples) {
				tt := float64(base[i]) / float64(t.timescale)
				if best < 0 || tt < bestT {
					best, bestT = i, tt
				}
			}
		}
		if best < 0 {
			return nil
		}
		t := tracks[best]
		pt := &fmp4.PartTrack{ID: best + 1, BaseTime: uint64(base[best])}
		limit := int64(t.timescale) * 2
		var span int64
		for pos[best] < len(t.samples) && span < limit {
			s := t.samples[pos[best]]
			buf := make([]byte, s.size)
			if _, err := raw.ReadAt(buf, s.off); err != nil {
				return err
			}
			d := durs[best][pos[best]]
			pt.Samples = append(pt.Samples, &fmp4.Sample{Duration: uint32(d), PTSOffset: int32(s.pts - s.dts), IsNonSyncSample: !s.sync, Payload: buf})
			span += d
			pos[best]++
		}
		base[best] += span
		part := fmp4.Part{SequenceNumber: seq, Tracks: []*fmp4.PartTrack{pt}}
		seq++
		var pb seekablebuffer.Buffer
		if err := part.Marshal(&pb); err != nil {
			return err
		}
		if _, err := w.Write(pb.Bytes()); err != nil {
			return err
		}
	}
}

// ---------- fallbacks ----------

func ffmpegPath() string {
	if exe, err := os.Executable(); err == nil {
		for _, n := range []string{"ffmpeg.exe", "ffmpeg"} {
			p := filepath.Join(filepath.Dir(exe), n)
			if st, err := os.Stat(p); err == nil && !st.IsDir() {
				return p
			}
		}
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	return ""
}

func concatFiles(ctx context.Context, paths []string, out string) error {
	f, err := os.Create(out)
	if err != nil {
		return err
	}
	sr := &seqReader{paths: paths}
	defer sr.Close()
	_, err = io.Copy(f, &countingReader{r: sr, ctx: ctx})
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

var formatExt = map[string]string{"ts": ".ts", "fmp4": ".mp4", "adts": ".aac", "webm": ".webm"}

func joinWithFFmpeg(ctx context.Context, ff string, groups []trackFiles, audioOnly bool, dir string) (*joinResult, error) {
	args := []string{"-y", "-hide_banner", "-loglevel", "error"}
	separateAudio := false
	for i, g := range groups {
		in := filepath.Join(dir, fmt.Sprintf("track%d%s", i, formatExt[g.format]))
		if err := concatFiles(ctx, g.files, in); err != nil {
			return nil, err
		}
		args = append(args, "-i", in)
		if g.role == "audio" {
			separateAudio = true
		}
	}
	for i, g := range groups {
		if g.role == "main" && !audioOnly {
			args = append(args, "-map", fmt.Sprintf("%d:v:0?", i))
		}
		if g.role == "audio" || (g.role == "main" && !separateAudio) {
			args = append(args, "-map", fmt.Sprintf("%d:a:0?", i))
		}
	}
	ext := ".mp4"
	if audioOnly {
		ext = ".m4a"
	}
	out := filepath.Join(dir, "joined"+ext)
	args = append(args, "-c", "copy", "-movflags", "+faststart", out)
	cmd := exec.CommandContext(ctx, ff, args...)
	hideWindow(cmd)
	if b, err := cmd.CombinedOutput(); err != nil {
		os.Remove(out)
		return nil, fmt.Errorf("ffmpeg: %v %s", err, strings.TrimSpace(string(b)))
	}
	return &joinResult{path: out, ext: ext}, nil
}

func joinRaw(ctx context.Context, groups []trackFiles, dir string, cause error) (*joinResult, error) {
	if errors.Is(cause, errDRM) {
		return nil, errDRM
	}
	res := &joinResult{}
	for i, g := range groups {
		ext, ok := formatExt[g.format]
		if !ok {
			return nil, fmt.Errorf("couldn't join this video (%v)", cause)
		}
		out := filepath.Join(dir, fmt.Sprintf("raw%d%s", i, ext))
		if err := concatFiles(ctx, g.files, out); err != nil {
			return nil, err
		}
		if res.path == "" {
			res.path, res.ext = out, ext
		} else {
			suffix := " (sound)" + ext
			res.extras = append(res.extras, joinExtra{path: out, suffix: suffix})
		}
	}
	if len(res.extras) > 0 {
		res.note = "Picture and sound were saved as two files. Install ffmpeg next to Jasqet to get them joined"
	} else if res.ext != ".mp4" {
		res.note = "Saved in the stream's own format (" + strings.TrimPrefix(res.ext, ".") + "). It plays in VLC and most players"
	}
	return res, nil
}

type editInfo struct {
	delay     float64
	mediaTime int64
}

// parseEditLists reads each track's edit list from an MP4 "moov" box. Some
// encoders say "this track starts 0.5 s late" there instead of in the chunk times.
func parseEditLists(moov []byte) map[int]editInfo {
	out := map[int]editInfo{}
	children := func(b []byte, fn func(typ string, body []byte)) {
		for len(b) >= 8 {
			size := int(binary.BigEndian.Uint32(b[:4]))
			typ := string(b[4:8])
			h := 8
			if size == 1 && len(b) >= 16 {
				size = int(binary.BigEndian.Uint64(b[8:16]))
				h = 16
			} else if size == 0 {
				size = len(b)
			}
			if size < h || size > len(b) {
				return
			}
			fn(typ, b[h:size])
			b = b[size:]
		}
	}
	if len(moov) < 8 {
		return out
	}
	movieTS := uint32(1000)
	children(moov[8:], func(typ string, body []byte) {
		switch typ {
		case "mvhd":
			if len(body) >= 24 {
				if body[0] == 1 && len(body) >= 24 {
					movieTS = binary.BigEndian.Uint32(body[20:24])
				} else if len(body) >= 16 {
					movieTS = binary.BigEndian.Uint32(body[12:16])
				}
			}
		case "trak":
			id := 0
			var e editInfo
			has := false
			children(body, func(t2 string, b2 []byte) {
				switch t2 {
				case "tkhd":
					if len(b2) >= 24 && b2[0] == 1 {
						id = int(binary.BigEndian.Uint32(b2[20:24]))
					} else if len(b2) >= 16 {
						id = int(binary.BigEndian.Uint32(b2[12:16]))
					}
				case "edts":
					children(b2, func(t3 string, el []byte) {
						if t3 != "elst" || len(el) < 8 {
							return
						}
						v := el[0]
						n := int(binary.BigEndian.Uint32(el[4:8]))
						p := el[8:]
						for i := 0; i < n; i++ {
							var dur uint64
							var mt int64
							if v == 1 {
								if len(p) < 20 {
									return
								}
								dur = binary.BigEndian.Uint64(p[:8])
								mt = int64(binary.BigEndian.Uint64(p[8:16]))
								p = p[20:]
							} else {
								if len(p) < 12 {
									return
								}
								dur = uint64(binary.BigEndian.Uint32(p[:4]))
								mt = int64(int32(binary.BigEndian.Uint32(p[4:8])))
								p = p[12:]
							}
							if mt == -1 {
								if movieTS > 0 {
									e.delay += float64(dur) / float64(movieTS)
								}
								continue
							}
							e.mediaTime = mt
							has = true
							return
						}
					})
				}
			})
			if id > 0 && (has || e.delay > 0) {
				out[id] = e
			}
		}
	})
	return out
}
