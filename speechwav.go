package main

// speechWAV turns decoded sound (16-bit PCM, any rate and number of channels)
// into what Whisper listens to: 16 kHz, mono, 16-bit WAV. The channels are
// mixed, and each output sample is the average of the input it covers, which
// is enough smoothing for speech.

import (
	"bufio"
	"encoding/binary"
	"errors"
	"os"
)

const speechRate = 16000

type speechWAV struct {
	f     *os.File
	w     *bufio.Writer
	ch    int
	ratio float64 // input frames per output sample
	carry []byte  // part of a frame left from the last block

	frame      int64   // input frames so far
	k          int64   // output samples so far
	end        float64 // where output sample k ends, in input frames
	acc        float64
	cnt        int
	last       float64
	dataLength int64
}

func newSpeechWAV(path string) (*speechWAV, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	s := &speechWAV{f: f, w: bufio.NewWriterSize(f, 256<<10)}
	s.w.Write(make([]byte, 44)) // the header, filled in at the end
	return s, nil
}

// format sets (or changes) what comes in.
func (s *speechWAV) format(rate, channels int) {
	s.ch = channels
	s.ratio = float64(rate) / speechRate
	// carry on from where the output is
	s.frame = 0
	s.end = s.ratio
	s.k = 0
	s.carry = nil
}

// write takes a block of 16-bit samples, the channels interleaved.
func (s *speechWAV) write(b []byte) error {
	if s.ch <= 0 {
		return errors.New("the sound's format isn't known")
	}
	fb := 2 * s.ch
	if len(s.carry) > 0 {
		need := fb - len(s.carry)
		if len(b) < need {
			s.carry = append(s.carry, b...)
			return nil
		}
		frame := append(s.carry, b[:need]...)
		s.carry = nil
		b = b[need:]
		s.frameIn(frame)
	}
	n := len(b) / fb * fb
	for i := 0; i < n; i += fb {
		s.frameIn(b[i : i+fb])
	}
	if n < len(b) {
		s.carry = append([]byte(nil), b[n:]...)
	}
	return nil
}

func (s *speechWAV) frameIn(fr []byte) {
	var v float64
	for c := 0; c < s.ch; c++ {
		v += float64(int16(binary.LittleEndian.Uint16(fr[2*c:])))
	}
	v /= float64(s.ch)
	for float64(s.frame) >= s.end { // this frame belongs to the next output sample
		s.emit()
	}
	s.acc += v
	s.cnt++
	s.frame++
}

func (s *speechWAV) emit() {
	v := s.last
	if s.cnt > 0 {
		v = s.acc / float64(s.cnt)
	}
	s.last, s.acc, s.cnt = v, 0, 0
	var b [2]byte
	binary.LittleEndian.PutUint16(b[:], uint16(int16(max(-32768, min(32767, v)))))
	s.w.Write(b[:])
	s.dataLength += 2
	s.k++
	s.end = float64(s.k+1) * s.ratio
}

// finish writes the last sample and the header.
func (s *speechWAV) finish() error {
	if s.cnt > 0 {
		s.emit()
	}
	if err := s.w.Flush(); err != nil {
		s.f.Close()
		return err
	}
	h := speechHeader(s.dataLength)
	if _, err := s.f.WriteAt(h[:], 0); err != nil {
		s.f.Close()
		return err
	}
	return s.f.Close()
}

// speechHeader: a WAV header for n bytes of 16 kHz mono 16-bit sound.
func speechHeader(n int64) [44]byte {
	var h [44]byte
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+n))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1) // PCM
	binary.LittleEndian.PutUint16(h[22:], 1) // mono
	binary.LittleEndian.PutUint32(h[24:], speechRate)
	binary.LittleEndian.PutUint32(h[28:], speechRate*2)
	binary.LittleEndian.PutUint16(h[32:], 2)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(n))
	return h
}

// cutSpeechWAV copies secs seconds from second from of a speechWAV file.
func cutSpeechWAV(src, dst string, from, secs int64) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	b := make([]byte, secs*2*speechRate)
	n, err := in.ReadAt(b, 44+from*2*speechRate)
	if n == 0 {
		return err
	}
	n &^= 1
	h := speechHeader(int64(n))
	return os.WriteFile(dst, append(h[:], b[:n]...), 0o644)
}

// abort drops an unfinished file.
func (s *speechWAV) abort() {
	s.f.Close()
	os.Remove(s.f.Name())
}
