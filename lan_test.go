package main

// Tests for sharing on the local network (lan.go): the group code, private
// files, what a find request says, checking a copy, slow peers and replays.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const lanTestCode = "KQ7M 4TZP 9XWD 2HRC"

// setLAN sets a computer's LAN settings directly: SetSettings would open
// Jasqet's real network ports.
func setLAN(m *Manager, s LANSettings) {
	m.mu.Lock()
	m.settings.LAN = s
	m.mu.Unlock()
}

// lanHas: a finished download on this computer.
func lanHas(t *testing.T, m *Manager, d *Download, data []byte) *Download {
	t.Helper()
	if d.Dir == "" {
		d.Dir = t.TempDir()
	}
	if d.ID == "" {
		d.ID = newID()
	}
	if err := os.WriteFile(filepath.Join(d.Dir, d.FileName), data, 0o644); err != nil {
		t.Fatal(err)
	}
	d.Size, d.Status = int64(len(data)), StDone
	m.mu.Lock()
	m.downloads[d.ID] = d
	m.mu.Unlock()
	return d
}

// lanPeerWith: another computer in the group that has finished link.
func lanPeerWith(t *testing.T, code, link string, data []byte) (*Manager, string) {
	t.Helper()
	peer := newTestManager(t)
	setLAN(peer, LANSettings{Enabled: true, Share: true, Code: code})
	lanHas(t, peer, &Download{URL: link, FileName: "shared.bin"}, data)
	srv := httptest.NewServer(peer.lanHandler())
	t.Cleanup(srv.Close)
	return peer, srv.Listener.Addr().String()
}

// lanCall sends a request from a computer on the network, signed with code
// (none if code is "").
func lanCall(h http.Handler, code, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.RemoteAddr = "192.168.1.20:50000"
	if code != "" {
		lanSignReq(r, code)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// lanFindQuery: a find request as lanFind makes it.
func lanFindQuery(code, link, name string, size int64) string {
	return "/lan/find?" + url.Values{"link": {lanHash(code, "link", lanLinkID(link))},
		"name": {lanHash(code, "name", strings.ToLower(name))}, "size": {strconv.FormatInt(size, 10)}}.Encode()
}

func randomData(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// lanInternet: the website the file comes from, with or without ranges. It
// notes the ranges asked for and counts what it sends.
type lanInternet struct {
	data   []byte
	ranges bool
	sent   atomic.Int64
	mu     sync.Mutex
	asked  []string
}

func (s *lanInternet) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.asked = append(s.asked, r.Header.Get("Range"))
	s.mu.Unlock()
	cw := &countWriter{ResponseWriter: w}
	if s.ranges {
		http.ServeContent(cw, r, "file.bin", time.Time{}, bytes.NewReader(s.data))
	} else { // the whole file every time
		w.Header().Set("Content-Length", strconv.Itoa(len(s.data)))
		cw.Write(s.data)
	}
	s.sent.Add(cw.n)
}

func (s *lanInternet) rangesAsked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.asked...)
}

// ---------- the group code ----------

func TestLANWithoutAGroupCodeSharesNothing(t *testing.T) {
	data := []byte("a public file")
	peer, _ := lanPeerWith(t, "", "https://files.example/pub/a.bin", data)
	id := ""
	peer.mu.Lock()
	for k := range peer.downloads {
		id = k
	}
	peer.mu.Unlock()
	h := peer.lanHandler()
	for _, code := range []string{"", lanTestCode} {
		if w := lanCall(h, code, lanFindQuery(code, "https://files.example/pub/a.bin", "shared.bin", int64(len(data)))); w.Code != http.StatusForbidden {
			t.Errorf("find without a group code (signed with %q): %d %s", code, w.Code, w.Body)
		}
		if w := lanCall(h, code, "/lan/file/"+id); w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), "public") {
			t.Errorf("file without a group code (signed with %q): %d", code, w.Code)
		}
	}
	if w := lanCall(h, "", "/lan/hello"); w.Code != http.StatusOK {
		t.Errorf("hello: %d", w.Code)
	}
	if peer.LANStatus().HasCode {
		t.Error("status says there's a group code")
	}

	// and a computer without a code asks no one
	var asked atomic.Int64
	spy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { asked.Add(1) }))
	defer spy.Close()
	m := newTestManager(t)
	setLAN(m, LANSettings{Enabled: true, Use: true, Peers: []string{spy.Listener.Addr().String()}})
	d := &Download{URL: "https://files.example/pub/a.bin", FileName: "a.bin", Size: int64(len(data))}
	if src, _ := m.lanFind(context.Background(), d); src != "" || asked.Load() != 0 {
		t.Fatalf("asked %d computers without a group code (got %q)", asked.Load(), src)
	}
}

func TestLANMadeCodeIsStrongAndReadable(t *testing.T) {
	if len(lanCodeChars) != 32 || strings.ContainsAny(lanCodeChars, "01IO") {
		t.Fatalf("code letters %q: want 32 (5 bits each), none of 0 1 I O", lanCodeChars)
	}
	for _, c := range lanCodeChars {
		if strings.Count(lanCodeChars, string(c)) != 1 {
			t.Fatalf("%q twice in the code letters", c)
		}
	}
	shape := regexp.MustCompile(`^[` + lanCodeChars + `]{4}( [` + lanCodeChars + `]{4}){3}$`)
	seen, used := map[string]bool{}, map[rune]bool{}
	for i := 0; i < 500; i++ {
		c := newLANCode()
		if !shape.MatchString(c) {
			t.Fatalf("made code %q isn't 4 groups of 4", c)
		}
		if n := len(lanCode(c)); n != 16 { // 16 × 5 bits = 80 bits
			t.Fatalf("made code %q has %d characters", c, n)
		}
		if seen[c] {
			t.Fatalf("code %q made twice", c)
		}
		seen[c] = true
		for _, r := range lanCode(c) {
			used[r] = true
		}
	}
	if len(used) != 32 {
		t.Fatalf("only %d of the 32 letters ever came up", len(used))
	}

	// Show code: the window gets it from its own route, not from /api/state
	isolateConfig(t)
	s, h := newTestServer(t)
	key := []string{keyHeader, s.key.window}
	app := "http://127.0.0.1:9614"
	show := func() (string, bool, bool) {
		w := call(h, "POST", "/api/lan/code", app, "{}", key...)
		var r struct {
			Code           string
			Made, Replaced bool
		}
		json.Unmarshal(w.Body.Bytes(), &r)
		if w.Code != 200 {
			t.Fatalf("show code: %d %s", w.Code, w.Body)
		}
		return r.Code, r.Made, r.Replaced
	}
	code, made, _ := show()
	if !made || !shape.MatchString(code) {
		t.Fatalf("no code yet: got %q (made %v)", code, made)
	}
	if again, made, _ := show(); again != code || made {
		t.Fatalf("Show code again gave %q (made %v), want %q", again, made, code)
	}
	if _, st := s.m.Snapshot(); lanCode(st.LAN.Code) != lanCode(code) {
		t.Fatalf("the code wasn't kept: %q", st.LAN.Code)
	}
	w := call(h, "GET", "/api/state", app, "", key...)
	if body := w.Body.String(); strings.Contains(body, code) || strings.Contains(body, lanCode(code)) || !strings.Contains(body, `"lanCode":true`) {
		t.Fatalf("/api/state shows the code, or not that one is set: %s", body)
	}
	if w := call(h, "POST", "/api/lan/code", app, "{}"); w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), code) {
		t.Fatalf("Show code without the window's key: %d %s", w.Code, w.Body)
	}
	// an older version's short code is replaced, and the window is told
	_, st := s.m.Snapshot()
	st.LAN.Code = "office"
	s.m.SetSettings(st)
	if code2, made, replaced := show(); !made || !replaced || code2 == code || !shape.MatchString(code2) {
		t.Fatalf("short old code: got %q made %v replaced %v", code2, made, replaced)
	}
}

func TestLANShortTypedCodeIsRefused(t *testing.T) {
	for in, want := range map[string]string{
		"KQ7M 4TZP 9XWD 2HRC":  "KQ7M4TZP9XWD2HRC",
		"kq7m-4tzp-9xwd-2hrc":  "KQ7M4TZP9XWD2HRC",
		" kq7m4tzp 9xwd-2hrc ": "KQ7M4TZP9XWD2HRC",
		"abcd efgh ijkl":       "ABCDEFGHIJKL",
		"abcd efgh ijk":        "", // 11
		"family":               "",
		"office_wifi_2024":     "", // other signs
		"":                     "",
	} {
		if got := lanCode(in); got != want {
			t.Errorf("lanCode(%q) = %q, want %q", in, got, want)
		}
	}

	isolateConfig(t)
	s, h := newTestServer(t)
	key := []string{keyHeader, s.key.window}
	app := "http://127.0.0.1:9614"
	for _, short := range []string{"abcd-1234-xy", "family", "abcd efgh ij!k"} {
		w := call(h, "POST", "/api/lan/code", app, `{"code":`+jsonQ(short)+`}`, key...)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "at least 12 letters or digits") {
			t.Errorf("typed %q: %d %s", short, w.Code, w.Body)
		}
	}
	if _, st := s.m.Snapshot(); st.LAN.Code != "" {
		t.Fatalf("a refused code was kept: %q", st.LAN.Code)
	}
	w := call(h, "POST", "/api/lan/code", app, `{"code":"kq7m-4tzp-9xwd-2hrc"}`, key...)
	if w.Code != 200 || !strings.Contains(w.Body.String(), lanTestCode) {
		t.Fatalf("a good typed code: %d %s", w.Code, w.Body)
	}

	// typed in small letters with hyphens on one computer, as shown on the other
	data := []byte("same file")
	peer, _ := lanPeerWith(t, lanTestCode, "https://files.example/pub/a.bin", data)
	if w := lanCall(peer.lanHandler(), lanCode("kq7m-4tzp-9xwd-2hrc"), lanFindQuery(lanCode("kq7m-4tzp-9xwd-2hrc"), "https://files.example/pub/a.bin", "a.bin", int64(len(data)))); w.Code != 200 || !strings.Contains(w.Body.String(), `"found":true`) {
		t.Fatalf("the same code written another way: %d %s", w.Code, w.Body)
	}
	// a computer that still has a short code shares nothing, even with it
	weak, _ := lanPeerWith(t, "family", "https://files.example/pub/a.bin", data)
	for _, code := range []string{"family", "FAMILY"} {
		if w := lanCall(weak.lanHandler(), code, lanFindQuery(code, "https://files.example/pub/a.bin", "a.bin", int64(len(data)))); w.Code != http.StatusForbidden {
			t.Fatalf("short code %q was used: %d %s", code, w.Code, w.Body)
		}
	}
}

func TestLANCodeIsSealedInStateFile(t *testing.T) {
	m := newTestManager(t)
	_, s := m.Snapshot()
	s.LAN.Code = lanTestCode
	m.SetSettings(s) // LAN off: no ports opened
	m.save()
	b, err := os.ReadFile(m.stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte("KQ7M")) || bytes.Contains(b, []byte(lanCode(lanTestCode))) {
		t.Fatal("the group code is in state.json in plain text")
	}
	var saved struct {
		Settings Settings `json:"settings"`
	}
	json.Unmarshal(b, &saved)
	if !isSealed(saved.Settings.LAN.Code) {
		t.Fatalf("the group code isn't sealed: %q", saved.Settings.LAN.Code)
	}
	if _, s2 := NewManager(m.stateFile).Snapshot(); s2.LAN.Code != lanTestCode {
		t.Fatalf("the group code didn't come back: %q", s2.LAN.Code)
	}

	// a list saved by an older version, with the code in plain text
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	old := `{"settings":{"downloadDir":` + jsonQ(filepath.Join(dir, "dl")) + `,"lan":{"enabled":false,"code":"OLD PLAIN CODE 1234"}},"downloads":[]}`
	os.WriteFile(p, []byte(old), 0o600)
	if _, s3 := NewManager(p).Snapshot(); s3.LAN.Code != "OLD PLAIN CODE 1234" {
		t.Fatalf("older list's code: %q", s3.LAN.Code)
	}
}

// ---------- private files ----------

func TestLANNeverServesPrivateFiles(t *testing.T) {
	peer := newTestManager(t)
	setLAN(peer, LANSettings{Enabled: true, Share: true, Code: lanTestCode})
	code := lanCode(lanTestCode)
	h := peer.lanHandler()
	for _, c := range []struct {
		name    string
		d       *Download
		private bool
	}{
		{"public link", &Download{URL: "https://files.example/pub/a.bin"}, false},
		{"?token= in the link", &Download{URL: "https://files.example/pub/b.bin?token=SECRET"}, true},
		{"login in the link", &Download{URL: "https://me:pw@files.example/pub/c.bin"}, true},
		{"empty query", &Download{URL: "https://files.example/pub/d.bin?"}, true},
		{"cookies", &Download{URL: "https://files.example/pub/e.bin", Cookies: map[string]string{"files.example": "sid=1"}}, true},
		{"login header", &Download{URL: "https://files.example/pub/f.bin", Headers: map[string]string{"authorization": "Bearer x"}}, true},
		{"site login", &Download{URL: "https://files.example/pub/g.bin", User: "me", Password: "pw"}, true},
	} {
		d := c.d
		d.FileName = lanBaseName(d.URL)
		data := []byte("contents of " + c.name)
		lanHas(t, peer, d, data)
		file := lanCall(h, code, "/lan/file/"+d.ID)
		find := lanCall(h, code, lanFindQuery(code, d.URL, d.FileName, int64(len(data))))
		served := file.Code == 200 && bytes.Equal(file.Body.Bytes(), data)
		offered := strings.Contains(find.Body.String(), `"found":true`)
		if c.private && (served || offered) {
			t.Errorf("%s: served %v, offered %v", c.name, served, offered)
		}
		if !c.private && (!served || !offered) {
			t.Errorf("%s (public): served %v (%d), offered %v", c.name, served, file.Code, offered)
		}
	}
}

func lanBaseName(link string) string { u, _ := url.Parse(link); return filepath.Base(u.Path) }

// ---------- what a find request says ----------

func TestLANFindSendsNoLinkOrNameInClear(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	spy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sent = append(sent, r.URL.String())
		for k, v := range r.Header {
			sent = append(sent, k+": "+strings.Join(v, ","))
		}
		mu.Unlock()
		writeJSON(w, map[string]any{"found": false})
	}))
	defer spy.Close()
	m := newTestManager(t)
	setLAN(m, LANSettings{Enabled: true, Use: true, Code: lanTestCode, Peers: []string{spy.Listener.Addr().String()}})
	d := &Download{URL: "https://Files.Example/reports/Quarterly-Report.pdf?token=SECRETTOKEN", FileName: "Quarterly-Report.pdf", Size: 5000}
	m.lanFind(context.Background(), d)
	mu.Lock()
	all := strings.ToLower(strings.Join(sent, "\n"))
	first := ""
	if len(sent) > 0 {
		first = sent[0]
	}
	mu.Unlock()
	if first == "" {
		t.Fatal("the find request never came")
	}
	for _, clear := range []string{"files.example", "files", "reports", "quarterly", "report", "pdf", "token", "secret"} {
		if strings.Contains(all, clear) {
			t.Errorf("the find request carries %q in clear:\n%s", clear, all)
		}
	}
	u, _ := url.Parse(first)
	q := u.Query()
	hexRe := regexp.MustCompile(`^[0-9a-f]{64}$`)
	if !hexRe.MatchString(q.Get("link")) || !hexRe.MatchString(q.Get("name")) || q.Get("size") != "5000" || len(q) != 3 {
		t.Fatalf("find request: %s", first)
	}

	// a computer of the group still finds its file from the hashes
	data := bytes.Repeat([]byte("r"), 5000)
	_, addr := lanPeerWith(t, lanTestCode, "https://files.example/reports/Quarterly-Report.pdf", data)
	setLAN(m, LANSettings{Enabled: true, Use: true, Code: lanTestCode, Peers: []string{addr}})
	if src, peer := m.lanFind(context.Background(), d); peer != addr || !strings.HasPrefix(src, "http://"+addr+"/lan/file/") {
		t.Fatalf("the peer didn't match the hashes: %q %q", src, peer)
	}
	// one with another code doesn't
	setLAN(m, LANSettings{Enabled: true, Use: true, Code: "ANOT HERC ODE2 3456", Peers: []string{addr}})
	if src, _ := m.lanFind(context.Background(), d); src != "" {
		t.Fatalf("found with another group code: %q", src)
	}
}

// ---------- checking a copy ----------

func TestLANCheckPiecesAreRandomForEachDownload(t *testing.T) {
	data := randomData(11, 4<<20)
	web := &lanInternet{data: data, ranges: true}
	srv := httptest.NewServer(web)
	defer srv.Close()
	link := srv.URL + "/big.bin"
	_, addr := lanPeerWith(t, lanTestCode, link, data)
	m := newTestManager(t)
	setLAN(m, LANSettings{Enabled: true, Use: true, Code: lanTestCode, Peers: []string{addr}})

	var picks [][]int64
	for i := 0; i < 2; i++ {
		d := &Download{URL: link, FileName: "big.bin", Size: int64(len(data)), Resumable: true}
		before := len(web.rangesAsked())
		if src, _ := m.lanFind(context.Background(), d); src == "" {
			t.Fatal("the peer's copy wasn't found")
		}
		c := m.lanPending(d)
		if c == nil || len(c.offs) != lanPieces || c.n != lanPiece {
			t.Fatalf("check: %+v", c)
		}
		asked := web.rangesAsked()[before:]
		if len(asked) != lanPieces {
			t.Fatalf("asked the website for %d pieces, want %d", len(asked), lanPieces)
		}
		for j, off := range c.offs {
			if off < 0 || off+c.n > int64(len(data)) {
				t.Fatalf("piece at %d is outside the file", off)
			}
			want := "bytes=" + strconv.FormatInt(off, 10) + "-" + strconv.FormatInt(off+c.n-1, 10)
			found := false
			for _, a := range asked {
				found = found || a == want
			}
			if !found {
				t.Fatalf("piece %d (%s) wasn't fetched from the website: %v", j, want, asked)
			}
			if sha256.Sum256(data[off:off+c.n]) != c.sums[j] {
				t.Fatalf("piece %d's sum isn't the website's", j)
			}
		}
		picks = append(picks, c.offs)
	}
	same := 0
	for _, a := range picks[0] {
		for _, b := range picks[1] {
			if a == b {
				same++
			}
		}
	}
	if same == lanPieces {
		t.Fatalf("both downloads checked the same pieces: %v", picks)
	}
	fixed := map[int64]bool{0: true, int64(len(data))/2 - lanPiece/2: true, int64(len(data)) - lanPiece: true}
	for _, off := range append(picks[0], picks[1]...) {
		if !fixed[off] {
			return
		}
	}
	t.Fatal("only the old fixed pieces were checked")
}

func TestLANCopyThatFailsItsCheckComesFromTheInternet(t *testing.T) {
	good := randomData(21, 4<<20)
	bad := append([]byte(nil), good...)
	bad[len(bad)/3] ^= 0xff // one byte changed: same size, same name
	sum := sha256.Sum256(good)
	for _, c := range []struct {
		name     string
		ranges   bool   // the website can give pieces to compare with
		peerHas  []byte // what the other computer has
		checksum string
		fromLAN  bool // the copy is kept
	}{
		{"checksum doesn't match", false, bad, "sha256:" + hex.EncodeToString(sum[:]), false},
		{"pieces don't match", true, randomData(22, len(good)), "", false},
		{"good copy", true, good, "sha256:" + hex.EncodeToString(sum[:]), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			web := &lanInternet{data: good, ranges: c.ranges}
			srv := httptest.NewServer(web)
			defer srv.Close()
			link := srv.URL + "/file.bin"
			peer, addr := lanPeerWith(t, lanTestCode, link, c.peerHas)
			m := newTestManager(t)
			setLAN(m, LANSettings{Enabled: true, Use: true, Code: lanTestCode, Peers: []string{addr}})
			ids, err := m.Add(AddRequest{URL: link, Checksum: c.checksum})
			if err != nil {
				t.Fatal(err)
			}
			d := waitStatus(t, m, ids[0], StDone, time.Minute)
			if !bytes.Equal(fileOf(d), good) {
				t.Fatal("the finished file isn't the one on the website")
			}
			peer.lan.mu.Lock()
			served := peer.lan.served
			peer.lan.mu.Unlock()
			if served < int64(len(good)) {
				t.Fatalf("the other computer sent only %d bytes: the copy wasn't tried", served)
			}
			d.mu.Lock()
			note := d.Note
			d.mu.Unlock()
			if c.fromLAN {
				if !strings.Contains(note, "Copied from another computer") || web.sent.Load() > lanPieces*lanPiece+1024 {
					t.Fatalf("a good copy wasn't kept: note %q, %d bytes from the website", note, web.sent.Load())
				}
			} else if !strings.Contains(note, "didn't match") || web.sent.Load() < int64(len(good)) {
				t.Fatalf("a bad copy was kept: note %q, %d bytes from the website", note, web.sent.Load())
			}
		})
	}
}

func TestLANWholeVideoCopyChecksItsChecksum(t *testing.T) {
	good, bad := []byte("the real video"), []byte("a changed video")
	sum := sha256.Sum256(good)
	for _, c := range []struct {
		peerHas []byte
		ok      bool
	}{{bad, false}, {good, true}} {
		_, addr := lanPeerWith(t, lanTestCode, "https://videos.example/v/1", c.peerHas)
		m := newTestManager(t)
		setLAN(m, LANSettings{Enabled: true, Use: true, Code: lanTestCode, Peers: []string{addr}})
		d := &Download{URL: "https://videos.example/v/1", FileName: "v.mp4", Size: -1, Checksum: "sha256:" + hex.EncodeToString(sum[:])}
		src, _ := m.lanFind(context.Background(), d)
		if src == "" {
			t.Fatal("not found")
		}
		dest := filepath.Join(t.TempDir(), "v.mp4")
		err := m.lanCopyWhole(context.Background(), d, src, dest)
		got, _ := os.ReadFile(dest)
		if c.ok != (err == nil) || c.ok != bytes.Equal(got, good) {
			t.Fatalf("copy of %q: err %v, file %q", c.peerHas, err, got)
		}
		if _, err := os.Stat(dest + ".lan"); err == nil {
			t.Fatal("the refused copy was left behind")
		}
	}
}

// ---------- slow and fake computers ----------

func TestLANPeerListIsCappedAndSlowPeersDontHoldUp(t *testing.T) {
	m := newTestManager(t)
	base := time.Now().Add(-2 * time.Minute)
	for i := 0; i < 100; i++ {
		a := "192.168.1." + strconv.Itoa(i) + ":9615"
		m.addPeer(a, "pc")
		m.lan.mu.Lock()
		if p := m.lan.peers[a]; p != nil {
			p.Seen = base.Add(time.Duration(i) * time.Second)
		}
		m.lan.mu.Unlock()
	}
	m.lan.mu.Lock()
	n := len(m.lan.peers)
	_, newest := m.lan.peers["192.168.1.99:9615"]
	_, oldest := m.lan.peers["192.168.1.67:9615"]
	m.lan.mu.Unlock()
	if n != lanMaxPeers || !newest || oldest {
		t.Fatalf("kept %d peers (newest %v, an old one %v), want the %d heard from last", n, newest, oldest, lanMaxPeers)
	}
	var typed []string
	for i := 0; i < 40; i++ {
		typed = append(typed, "10.0.0."+strconv.Itoa(i))
	}
	setLAN(m, LANSettings{Enabled: true, Use: true, Code: lanTestCode, Peers: typed})
	if got := m.lanPeers(); len(got) != lanMaxPeers || got[0] != "10.0.0.0:9615" {
		t.Fatalf("asking %d computers: %v", len(got), got)
	}

	old := lanAskTime
	lanAskTime = 800 * time.Millisecond
	defer func() { lanAskTime = old }()
	m.lan.mu.Lock()
	m.lan.peers = nil // only the test servers below are asked
	m.lan.mu.Unlock()
	var slow []string
	for i := 0; i < 6; i++ {
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(10 * time.Second):
			}
		}))
		defer s.Close()
		slow = append(slow, s.Listener.Addr().String())
	}
	data := []byte("the file")
	_, good := lanPeerWith(t, lanTestCode, "https://files.example/pub/a.bin", data)
	d := &Download{URL: "https://files.example/pub/a.bin", FileName: "a.bin", Size: int64(len(data))}
	lanKeysFor(lanCode(lanTestCode)) // worked out once, ahead of the timing

	setLAN(m, LANSettings{Enabled: true, Use: true, Code: lanTestCode, Peers: append(slow, good)})
	start := time.Now()
	if _, peer := m.lanFind(context.Background(), d); peer != good {
		t.Fatalf("got %q, want the one that has it (%s)", peer, good)
	}
	if took := time.Since(start); took >= lanAskTime {
		t.Fatalf("took %v: the slow ones were waited for", took)
	}
	setLAN(m, LANSettings{Enabled: true, Use: true, Code: lanTestCode, Peers: slow})
	start = time.Now()
	if src, _ := m.lanFind(context.Background(), d); src != "" {
		t.Fatalf("found %q on computers that never answer", src)
	}
	if took := time.Since(start); took > lanAskTime+time.Second {
		t.Fatalf("took %v, the limit is %v", took, lanAskTime)
	}
}

// ---------- replays ----------

func TestLANReplayedRequestIsRefused(t *testing.T) {
	code := lanCode(lanTestCode)
	r := httptest.NewRequest("GET", "/lan/file/abc", nil)
	lanSignReq(r, code)
	if !lanSigned(r, code) {
		t.Fatal("a fresh signed request was refused")
	}
	if lanSigned(r, code) {
		t.Fatal("the same signed request was taken twice")
	}
	// a request seen going by, sent again a little later
	data := []byte("shared file")
	peer, _ := lanPeerWith(t, lanTestCode, "https://files.example/pub/a.bin", data)
	h := peer.lanHandler()
	q := lanFindQuery(code, "https://files.example/pub/a.bin", "a.bin", int64(len(data)))
	first := httptest.NewRequest("GET", q, nil)
	first.RemoteAddr = "192.168.1.20:50000"
	first.Header.Set("X-Jasqet-Auth", lanSign(code, "GET", q, time.Now().Add(-4*time.Minute)))
	again := first.Clone(context.Background())
	w := httptest.NewRecorder()
	h.ServeHTTP(w, first)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"found":true`) {
		t.Fatalf("first: %d %s", w.Code, w.Body)
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, again)
	if w.Code != http.StatusForbidden {
		t.Fatalf("replay: %d %s", w.Code, w.Body)
	}
	// the old kind of signature, without a nonce
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	old := httptest.NewRequest("GET", "/lan/file/abc", nil)
	old.Header.Set("X-Jasqet-Auth", ts+":"+lanMAC(code, ts, "", "GET", "/lan/file/abc"))
	if lanSigned(old, code) {
		t.Fatal("a signature without a nonce was taken")
	}
}

// ---------- the window ----------

func TestLANPageExplainsTheCode(t *testing.T) {
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	page := string(b)
	for _, want := range []string{`id="lShow"`, `>Show code<`, `id="lCopy"`, "Only computers with the same code share files", "at least 12 letters or digits", "api('lan/code'"} {
		if !strings.Contains(page, want) {
			t.Errorf("the page lacks %s", want)
		}
	}
	start, end := strings.Index(page, `id="lOn"`), strings.Index(page, `id="lStatus"`)
	if start < 0 || end < start {
		t.Fatal("the LAN part of the page is missing")
	}
	if strings.ContainsAny(page[start:end], string([]rune{0x2014, 0x2013})) { // long dashes
		t.Error("the LAN help text has a long dash")
	}
}
