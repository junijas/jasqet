//go:build windows

package main

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// a WAV file as any program would write it
func writeWAV(t *testing.T, p string, rate, channels int, data []byte) {
	t.Helper()
	var h [44]byte
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+len(data)))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], uint16(channels))
	binary.LittleEndian.PutUint32(h[24:], uint32(rate))
	binary.LittleEndian.PutUint32(h[28:], uint32(rate*channels*2))
	binary.LittleEndian.PutUint16(h[32:], uint16(channels*2))
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(len(data)))
	if err := os.WriteFile(p, append(h[:], data...), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsDecodersTakeTheSoundOut(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "talk.wav")
	writeWAV(t, in, 44100, 2, pcm16(44100, 2, 3, 250, 10000))
	out := filepath.Join(dir, "speech.wav")
	var last float64
	if err := decodeAudioOS(context.Background(), in, out, func(p float64) { last = p }); err != nil {
		t.Fatal(err)
	}
	s := readSpeechWAV(t, out)
	if len(s) < 47900 || len(s) > 48100 {
		t.Fatalf("%d samples for 3 s, want 48000", len(s))
	}
	if pk := peak(s); pk < 8800 || pk > 10100 {
		t.Fatalf("a tone at 10000 came out at %d", pk)
	}
	if last <= 0.5 {
		t.Fatalf("progress only reached %.2f", last)
	}
	// not a sound file: a clear error, no half-written WAV left behind
	bad := filepath.Join(dir, "notes.txt")
	os.WriteFile(bad, []byte("just text"), 0o644)
	os.Remove(out)
	if err := decodeAudioOS(context.Background(), bad, out, func(float64) {}); err == nil {
		t.Fatal("a text file decoded as sound")
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("a broken WAV was left behind")
	}
}
