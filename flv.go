package main

// FLV (old Flash videos, still common on some sites): read H.264 video and
// AAC/MP3 sound so the file can be re-wrapped as MP4 for the built-in viewer.
// Nothing is re-encoded.

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	mp4codecs "github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
)

func collectFLV(ctx context.Context, rd io.Reader, raw *rawStore, wantVideo, wantAudio bool) ([]*outTrack, error) {
	br := bufio.NewReaderSize(rd, 1<<20)
	hdr := make([]byte, 9)
	if _, err := io.ReadFull(br, hdr); err != nil || string(hdr[:3]) != "FLV" {
		return nil, errUnsupportedFormat
	}
	skip := int(binary.BigEndian.Uint32(hdr[5:9])) - 9
	if skip < 0 || skip > 1<<20 {
		return nil, errUnsupportedFormat
	}
	if _, err := br.Discard(skip + 4); err != nil { // rest of header + first PreviousTagSize
		return nil, err
	}
	video := &outTrack{video: true, timescale: 1000}
	audio := &outTrack{timescale: 1000}
	var sps, pps []byte
	lengthSize := 4
	th := make([]byte, 11)
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if _, err := io.ReadFull(br, th); err != nil {
			break // end of file (or cut off): keep what we have
		}
		typ := th[0] & 0x1f
		size := int(th[1])<<16 | int(th[2])<<8 | int(th[3])
		ts := int64(th[4])<<16 | int64(th[5])<<8 | int64(th[6]) | int64(th[7])<<24
		data := make([]byte, size)
		if _, err := io.ReadFull(br, data); err != nil {
			break
		}
		br.Discard(4) // PreviousTagSize
		switch {
		case typ == 9 && wantVideo && size >= 5:
			frameType, codecID := data[0]>>4, data[0]&0x0f
			if codecID != 7 { // only H.264 (others can't be played in MP4 here)
				if len(video.samples) == 0 {
					return nil, errors.New("this FLV video uses an old format the built-in viewer can't play")
				}
				continue
			}
			cts := int64(int32(uint32(data[2])<<24|uint32(data[3])<<16|uint32(data[4])<<8) >> 8)
			body := data[5:]
			switch data[1] {
			case 0: // AVCDecoderConfigurationRecord
				s, p, ls := parseAVCC(body)
				if s != nil {
					sps, pps, lengthSize = s, p, ls
				}
			case 1:
				if sps == nil {
					continue
				}
				payload := toAVCC4(body, lengthSize)
				sync := frameType == 1
				if len(video.samples) == 0 && !sync {
					continue
				}
				if err := video.add(raw, ts, ts+cts, payload, sync, false); err != nil {
					return nil, err
				}
			}
		case typ == 8 && wantAudio && size >= 2:
			format := data[0] >> 4
			switch format {
			case 10: // AAC
				if data[1] == 0 {
					var cfg mpeg4audio.AudioSpecificConfig
					if cfg.Unmarshal(data[2:]) == nil && cfg.SampleRate > 0 {
						audio.codec = &mp4codecs.MPEG4Audio{Config: cfg}
						audio.timescale = uint32(cfg.SampleRate)
					}
					continue
				}
				if audio.codec == nil {
					continue
				}
				t := ts * int64(audio.timescale) / 1000
				if err := audio.addAudio(raw, t, data[2:], 1024); err != nil {
					return nil, err
				}
			case 2: // MP3
				if audio.codec == nil {
					rates := []int{5512, 11025, 22050, 44100}
					ch := 1
					if data[0]&1 == 1 {
						ch = 2
					}
					audio.codec = &mp4codecs.MPEG1Audio{SampleRate: rates[(data[0]>>2)&3], ChannelCount: ch}
				}
				if err := audio.add(raw, ts, ts, data[1:], true, false); err != nil {
					return nil, err
				}
			}
		}
	}
	if sps != nil && pps != nil {
		video.codec = &mp4codecs.H264{SPS: sps, PPS: pps}
	}
	var out []*outTrack
	for _, t := range []*outTrack{video, audio} {
		if t.codec != nil && len(t.samples) > 0 {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil, errUnsupportedFormat
	}
	return out, nil
}

// parseAVCC reads the first SPS and PPS out of an H.264 configuration record.
func parseAVCC(b []byte) (sps, pps []byte, lengthSize int) {
	if len(b) < 7 {
		return nil, nil, 4
	}
	lengthSize = int(b[4]&3) + 1
	n := int(b[5] & 0x1f)
	p := 6
	for i := 0; i < n && p+2 <= len(b); i++ {
		l := int(binary.BigEndian.Uint16(b[p:]))
		p += 2
		if p+l > len(b) {
			return nil, nil, 4
		}
		if sps == nil {
			sps = cloneBytes(b[p : p+l])
		}
		p += l
	}
	if p >= len(b) {
		return nil, nil, 4
	}
	n = int(b[p])
	p++
	for i := 0; i < n && p+2 <= len(b); i++ {
		l := int(binary.BigEndian.Uint16(b[p:]))
		p += 2
		if p+l > len(b) {
			break
		}
		if pps == nil {
			pps = cloneBytes(b[p : p+l])
		}
		p += l
	}
	return sps, pps, lengthSize
}

// toAVCC4 makes NAL units 4-byte length-prefixed (what MP4 uses).
func toAVCC4(b []byte, lengthSize int) []byte {
	if lengthSize == 4 {
		return cloneBytes(b)
	}
	var out []byte
	for p := 0; p+lengthSize <= len(b); {
		l := 0
		for i := 0; i < lengthSize; i++ {
			l = l<<8 | int(b[p+i])
		}
		p += lengthSize
		if p+l > len(b) {
			break
		}
		out = binary.BigEndian.AppendUint32(out, uint32(l))
		out = append(out, b[p:p+l]...)
		p += l
	}
	return out
}
