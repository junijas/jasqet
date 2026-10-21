package main

import (
	"strings"
	"testing"
)

func TestParseClock(t *testing.T) {
	for in, want := range map[string]float64{"2:30": 150, "1:02:03": 3723, "90": 90, "2m30s": 150, "1h": 3600, "": 0, "0:05.5": 5.5} {
		got, err := parseClock(in)
		if err != nil || got != want {
			t.Errorf("parseClock(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseClock("abc"); err == nil {
		t.Error("abc should fail")
	}
}

func TestSubtitles(t *testing.T) {
	vtt := "WEBVTT\nKind: captions\n\n00:00:01.000 --> 00:00:03.000\nhello <c>there</c>\n\n00:00:03.000 --> 00:00:03.010\nhello there\n\n00:00:03.010 --> 00:00:05.000\nhello there\nhow are you\n"
	cs := parseVTT(vtt)
	if len(cs) != 2 || cs[0].text != "hello there" || cs[1].text != "how are you" {
		t.Fatalf("vtt: %+v", cs)
	}
	srt := toSRT(cs, 2, 0)
	if !strings.Contains(srt, "00:00:00,000 --> 00:00:01,000\nhello there") || !strings.Contains(srt, "00:00:01,010 --> 00:00:03,000\nhow are you") {
		t.Fatalf("srt:\n%s", srt)
	}
	j := `{"events":[{"tStartMs":0,"dDurationMs":2000,"segs":[{"utf8":"good "},{"utf8":"morning"}]},{"tStartMs":1500,"dDurationMs":10,"aAppend":1,"segs":[{"utf8":"\n"}]},{"tStartMs":1500,"dDurationMs":3000,"segs":[{"utf8":"class"}]}]}`
	cs = parseJSON3([]byte(j))
	if len(cs) != 2 || cs[0].text != "good morning" || cs[0].end != 1.5 {
		t.Fatalf("json3: %+v", cs)
	}
}

func TestSniff(t *testing.T) {
	cases := map[string]string{
		"MZ\x90\x00":                       kWinExe.name,
		"PK\x03\x04":                       kZip.name,
		"%PDF-1.7":                         kPDF.name,
		"\x00\x00\x00\x18ftypmp42":         kMP4.name,
		"  <!DOCTYPE html><html>":          kHTML.name,
		"Rar!\x1a\x07\x01\x00":             kRar.name,
		"\x1aE\xdf\xa3":                    kMKV.name,
		"ID3\x04":                          kMP3.name,
		"L\x00\x00\x00\x01\x14\x02\x00abc": kShort.name,
	}
	for in, want := range cases {
		if got := sniff([]byte(in)).name; got != want {
			t.Errorf("sniff(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestChecksumFor(t *testing.T) {
	list := "abc123 nothing\n" + strings.Repeat("a", 64) + "  other.iso\n" + strings.Repeat("b", 64) + " *ubuntu.iso\n"
	got := checksumFor([]byte(list), map[string]bool{"ubuntu.iso": true}, false)
	if got != "sha256:"+strings.Repeat("b", 64) {
		t.Fatalf("list: %q", got)
	}
	page := `<tr><td>tool-1.2.exe</td></tr><tr><td>SHA1: ` + strings.Repeat("c", 40) + `</td></tr>`
	if got := checksumFor([]byte(page), map[string]bool{"tool-1.2.exe": true}, true); got != "sha1:"+strings.Repeat("c", 40) {
		t.Fatalf("page: %q", got)
	}
	if got := checksumFor([]byte(strings.Repeat("d", 64)+"\n"), map[string]bool{"x": true}, false); got == "" {
		t.Fatal("lone .sha256 file")
	}
	if _, err := parseChecksum("SHA256: " + strings.Repeat("E", 64)); err != nil {
		t.Fatal(err)
	}
}

func TestDirectLink(t *testing.T) {
	if got := directLink("https://drive.google.com/file/d/1AbC_d-9/view?usp=sharing"); got != "https://drive.usercontent.google.com/download?id=1AbC_d-9&export=download&confirm=t" {
		t.Fatal(got)
	}
	if got := directLink("https://www.dropbox.com/scl/fi/xyz/file.zip?rlkey=k&dl=0"); !strings.Contains(got, "dl=1") {
		t.Fatal(got)
	}
	if got := directLink("https://example.com/a.zip"); got != "https://example.com/a.zip" {
		t.Fatal(got)
	}
}

func TestValidChunk(t *testing.T) {
	ts := make([]byte, 188*3)
	for i := 0; i < 3; i++ {
		ts[i*188] = 0x47
	}
	if !validChunk(ts) {
		t.Error("ts")
	}
	ts[188] = 0
	if validChunk(ts) {
		t.Error("broken ts")
	}
	if validChunk([]byte("<html>error</html>")) {
		t.Error("html")
	}
	box := []byte("\x00\x00\x00\x10moof\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x0cmdatabcd")
	if !validChunk(box) {
		t.Error("boxes")
	}
	if validChunk(box[:len(box)-2]) {
		t.Error("cut-off boxes")
	}
}

func TestFakeTorrentWords(t *testing.T) {
	r := AfterRule{On: true, Match: "ext:.pdf epub"}
	if !r.matches(&Download{FileName: "a.epub"}) || r.matches(&Download{FileName: "a.mp4"}) {
		t.Fatal("ext match")
	}
	if got := expandName("{date} {name}", "lecture.mp4", "https://www.uni.edu/x"); !strings.HasSuffix(got, " lecture.mp4") {
		t.Fatal(got)
	}
}

func TestChecksumWholeName(t *testing.T) {
	list := strings.Repeat("a", 64) + "  debug-setup.exe\n" + strings.Repeat("b", 64) + " *setup.exe\n"
	if got := checksumFor([]byte(list), map[string]bool{"setup.exe": true}, false); got != "sha256:"+strings.Repeat("b", 64) {
		t.Fatalf("got %q", got)
	}
	if !containsName("download setup.exe.", "setup.exe") || containsName("setup.exe.sig", "setup.exe") || containsName("my_setup.exe", "setup.exe") {
		t.Fatal("containsName")
	}
	if !validChunk([]byte("\x00\x00\x00\x08ftyp")) {
		t.Fatal("tiny ftyp")
	}
	if validChunk([]byte("\x00\x00\x00\x01moof\xff\xff\xff\xff\xff\xff\xff\xffxxxx")) {
		t.Fatal("huge largesize must be rejected")
	}
	if e, refs := parseSidx([]byte("\x00\x00\x00\x08ftyp\x00\x00\x00\x01moov\x7f\xff\xff\xff\xff\xff\xff\xf0")); e != 0 || refs != nil {
		t.Fatal("bad sidx input")
	}
}
