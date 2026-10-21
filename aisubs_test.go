package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// pcm16 makes interleaved 16-bit samples of a tone.
func pcm16(rate, channels int, seconds, hz, amp float64) []byte {
	n := int(float64(rate) * seconds)
	b := make([]byte, 0, n*channels*2)
	for i := 0; i < n; i++ {
		v := int16(amp * math.Sin(2*math.Pi*hz*float64(i)/float64(rate)))
		for c := 0; c < channels; c++ {
			b = binary.LittleEndian.AppendUint16(b, uint16(v))
		}
	}
	return b
}

// readSpeechWAV checks a 16 kHz mono WAV's header and gives its samples.
func readSpeechWAV(t *testing.T, p string) []int16 {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil || len(b) < 44 || string(b[0:4]) != "RIFF" || string(b[8:16]) != "WAVEfmt " || string(b[36:40]) != "data" {
		t.Fatalf("not a WAV: %v", err)
	}
	if ch, rate, bits := binary.LittleEndian.Uint16(b[22:]), binary.LittleEndian.Uint32(b[24:]), binary.LittleEndian.Uint16(b[34:]); ch != 1 || rate != 16000 || bits != 16 {
		t.Fatalf("format %d ch %d Hz %d bit", ch, rate, bits)
	}
	if n := binary.LittleEndian.Uint32(b[40:]); int(n) != len(b)-44 || binary.LittleEndian.Uint32(b[4:]) != uint32(len(b)-8) {
		t.Fatalf("sizes in the header don't match the file")
	}
	s := make([]int16, (len(b)-44)/2)
	for i := range s {
		s[i] = int16(binary.LittleEndian.Uint16(b[44+2*i:]))
	}
	return s
}

func peak(s []int16) int {
	p := 0
	for _, v := range s {
		p = max(p, int(math.Abs(float64(v))))
	}
	return p
}

func TestSpeechWAVMixesAndResamples(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		rate, ch int
		block    int // fed in pieces of this size, frames split across them
	}{{48000, 2, 4093}, {44100, 2, 1000}, {8000, 1, 333}, {16000, 1, 4096}} {
		p := filepath.Join(dir, "out.wav")
		w, err := newSpeechWAV(p)
		if err != nil {
			t.Fatal(err)
		}
		w.format(c.rate, c.ch)
		data := pcm16(c.rate, c.ch, 2, 300, 12000)
		for i := 0; i < len(data); i += c.block {
			w.write(data[i:min(len(data), i+c.block)])
		}
		if err := w.finish(); err != nil {
			t.Fatal(err)
		}
		s := readSpeechWAV(t, p)
		if len(s) < 31990 || len(s) > 32010 {
			t.Errorf("%d Hz × %d: %d samples for 2 s, want 32000", c.rate, c.ch, len(s))
		}
		if pk := peak(s); pk < 10500 || pk > 12100 {
			t.Errorf("%d Hz × %d: a 300 Hz tone at 12000 came out at %d", c.rate, c.ch, pk)
		}
	}
}

func TestLanguageFromSeveralSamples(t *testing.T) {
	// the interview test: an accent made one sample sound like Yoruba
	out := "processing 'a.wav'\nwhisper_full_with_state: auto-detected language: en (p = 0.894239)\n" +
		"processing 'b.wav'\nwhisper_full_with_state: auto-detected language: yo (p = 0.830169)\n" +
		"processing 'c.wav'\nwhisper_full_with_state: auto-detected language: en (p = 0.744907)\n"
	if got := pickLanguage(out); got != "en" {
		t.Fatalf("picked %q", got)
	}
	if got := pickLanguage("nothing detected"); got != "" {
		t.Fatalf("picked %q from nothing", got)
	}
	// samples are cut from the right place, as WAVs Whisper can read
	dir := t.TempDir()
	src := filepath.Join(dir, "all.wav")
	w, _ := newSpeechWAV(src)
	w.format(speechRate, 1)
	for s := 0; s < 100; s++ { // second s is filled with the value s
		w.write(bytes.Repeat(binary.LittleEndian.AppendUint16(nil, uint16(s)), speechRate))
	}
	w.finish()
	cut := filepath.Join(dir, "cut.wav")
	if err := cutSpeechWAV(src, cut, 40, 30); err != nil {
		t.Fatal(err)
	}
	s := readSpeechWAV(t, cut)
	if len(s) != 30*speechRate || s[0] != 40 || s[len(s)-1] != 69 {
		t.Fatalf("%d samples from %d to %d", len(s), s[0], s[len(s)-1])
	}
	if err := cutSpeechWAV(src, cut, 90, 30); err != nil || len(readSpeechWAV(t, cut)) != 10*speechRate {
		t.Fatalf("a sample past the end should be cut short: %v", err)
	}
}

func TestSubtitleFileNames(t *testing.T) {
	for _, c := range []struct {
		lang      string
		translate bool
		want      string
	}{
		{"en", false, "Lecture.English (auto-generated).srt"},
		{"fr", true, "Lecture.English (auto-translated).srt"},
		{"tw", false, "Lecture.Twi (auto-generated).srt"},
		{"xx", false, "Lecture.XX (auto-generated).srt"},
		{"", false, "Lecture.Auto-generated.srt"},
	} {
		if got := filepath.Base(subsName(filepath.Join("x", "Lecture.mp4"), c.lang, c.translate)); got != c.want {
			t.Errorf("%q translate=%v: %q, want %q", c.lang, c.translate, got, c.want)
		}
	}
	// the viewer finds them and shows the language
	dir := t.TempDir()
	video := filepath.Join(dir, "Lecture.mp4")
	os.WriteFile(subsName(video, "en", false), []byte("1\n"), 0o644)
	if subs := subtitleFilesFor(video); len(subs) != 1 || subtitleLabel(video, subs[0]) != "English (auto-generated)" {
		t.Fatalf("viewer sees %q", subs)
	}
}

func TestAISubtitlesSetUpCheckedAndOnlyWhatsNeeded(t *testing.T) {
	home := t.TempDir()
	for _, k := range []string{"APPDATA", "XDG_CONFIG_HOME", "HOME", "USERPROFILE", "PATH"} {
		t.Setenv(k, home)
	}
	if !strings.HasPrefix(whisperDir(), home) {
		t.Fatalf("the engine would land in %s", whisperDir())
	}
	// a pretend engine zip: the program, its libraries, and things to leave out
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	for _, n := range []string{"Release/" + exeName("whisper-cli"), "Release/whisper.dll", "Release/ggml-cpu-haswell.dll", "Release/wchess.exe", "Release/../../evil.exe", "Release/SDL2.dll"} {
		f, _ := zw.Create(n)
		io.WriteString(f, "pretend "+n)
	}
	zw.Close()
	model := []byte("pretend model")
	sum := func(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/engine.zip":
			w.Write(zb.Bytes())
		case "/model.bin":
			w.Write(model)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	key := runtime.GOOS + "/" + runtime.GOARCH
	oldBuild, oldZip, oldModel := whisperBuild, whisperZips[key], whisperModel
	defer func() { whisperBuild, whisperModel = oldBuild, oldModel; whisperZips[key] = oldZip }()
	whisperBuild = srv.URL + "/"
	whisperModel.url, whisperModel.sum = srv.URL+"/model.bin", strings.Repeat("0", 64) // a model that doesn't match
	whisperZips[key] = struct{ name, sum string }{"engine.zip", sum(zb.Bytes())}
	m := newTestManager(t)
	ctx := context.Background()
	if runtime.GOOS != "windows" {
		t.Skip("Jasqet only downloads the engine on Windows")
	}
	if err := m.installWhisper(ctx, func(float64, string) {}); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("a model that doesn't match was accepted: %v", err)
	}
	if fileExists(whisperModelPath()) || whisperReady() {
		t.Fatal("the unchecked model was kept")
	}
	whisperModel.sum = sum(model)
	var last float64
	if err := m.installWhisper(ctx, func(p float64, _ string) { last = p }); err != nil {
		t.Fatal(err)
	}
	if !whisperReady() || last < 0.99 {
		t.Fatalf("not ready after setup (progress %.2f)", last)
	}
	var names []string
	entries, _ := os.ReadDir(whisperDir())
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if got := strings.Join(names, ","); got != strings.Join([]string{"ggml-base-q8_0.bin", "ggml-cpu-haswell.dll", exeName("whisper-cli"), "whisper.dll"}, ",") {
		t.Fatalf("in the engine's folder: %s", got)
	}
	if _, err := os.Stat(filepath.Join(home, "evil.exe")); err == nil {
		t.Fatal("a name in the zip wrote outside the engine's folder")
	}
}

func TestMakeSubtitlesOnlyForFinishedSpeech(t *testing.T) {
	m := newTestManager(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "doc.pdf"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "talk.mp4"), []byte("x"), 0o644)
	for _, c := range []struct {
		d    *Download
		want string
	}{
		{&Download{ID: "1", Dir: dir, FileName: "talk.mp4", Status: StDownloading}, "finished"},
		{&Download{ID: "2", Dir: dir, FileName: "doc.pdf", Status: StDone}, "speech"},
		{&Download{ID: "3", Dir: dir, FileName: "gone.mp4", Status: StDone}, "isn't there"},
	} {
		if err := m.MakeSubtitles(c.d, false); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.d.FileName, err, c.want)
		}
	}
}
