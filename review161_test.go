package main

// Tests for the fixes from the 1.6.0 final review (1.6.1).

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"golang.org/x/time/rate"
)

// tempDirRetry: a temporary folder removed with a few tries at the end. On
// Windows a file the virus scanner or a last background save still holds
// stops the first try ("directory is not empty").
func tempDirRetry(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tg-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for i := 0; i < 50 && os.RemoveAll(dir) != nil; i++ {
			time.Sleep(100 * time.Millisecond)
		}
	})
	return dir
}

// ---------- file names ----------

func TestSanitizeWindowsNames(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"CON", "_CON"},
		{"con.txt", "_con.txt"},
		{"NUL.tar.gz", "_NUL.tar.gz"},
		{"COM1.mp4", "_COM1.mp4"},
		{"lpt9.zip", "_lpt9.zip"},
		{"COM¹.txt", "_COM¹.txt"},
		{"CONIN$", "_CONIN$"},
		{"console.txt", "console.txt"},
		{"setup.exe ", "setup.exe"},
		{"setup.exe. . .", "setup.exe"},
		{"invoice\u202efdp.exe", "invoicefdp.exe"},
		{"a\u200bb\u2066.pdf", "ab.pdf"},
		{"..", "download"},
	} {
		if got := sanitizeFilename(c.in); got != c.want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	for _, in := range []string{`..\..\evil.exe`, "../../evil.exe", `C:\Windows\x.dll`, `\\server\share\x`, "a:b:c"} {
		if got := sanitizeFilename(in); strings.ContainsAny(got, `\/:`) {
			t.Errorf("sanitizeFilename(%q) = %q still has a path in it", in, got)
		}
	}
	long := "a" + strings.Repeat("é", 150) + ".mp4"
	got := sanitizeFilename(long)
	if !utf8.ValidString(got) || len(got) > 200 || !strings.HasSuffix(got, ".mp4") {
		t.Errorf("long name cut badly: %q (%d bytes)", got, len(got))
	}
}

func TestProgramsWithTrailingDotsAndNewTypes(t *testing.T) {
	for _, c := range []struct{ name, ext string }{
		{"setup.exe ", ".exe"}, {"setup.exe.", ".exe"}, {"run.bat . ", ".bat"}, {"film.mp4", ".mp4"},
	} {
		if got := extOf(c.name); got != c.ext {
			t.Errorf("extOf(%q) = %q, want %q", c.name, got, c.ext)
		}
	}
	missing := filepath.Join(t.TempDir(), "nothing")
	for _, n := range []string{"setup.exe ", "setup.exe.", "x.theme", "x.themepack", "connect.rdp", "box.wsb", "x.website",
		"x.search-ms", "x.searchConnector-ms", "x.psc1", "x.lnk", "x.scr", "x.msi", "x.jar", "x.hta", "x.ps1", "x.bat"} {
		if !isProgram(missing, n) {
			t.Errorf("%q should need the Run it confirmation", n)
		}
	}
	if w := programWarning("connect.rdp"); strings.Contains(w, "is a program") {
		t.Errorf("an .rdp file isn't a program: %q", w)
	}
	if w := programWarning("setup.exe"); !strings.Contains(w, "is a program") {
		t.Errorf("setup.exe: %q", w)
	}
	if r, _ := blockReason("movie.mp4.exe .", nil); r == "" {
		t.Error(`"movie.mp4.exe ." should be blocked as a program pretending to be a video`)
	}
	// an .exe renamed .mp4 is blocked, whatever its name ends with
	if r, _ := blockReason("movie.mp4 ", []byte("MZ\x90\x00 program")); r == "" {
		t.Error("a program named .mp4 should be blocked")
	}
}

// ---------- browser extensions and the local API ----------

func newTestServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	m := newTestManager(t)
	s := &Server{m: m, port: "9614", quit: make(chan struct{}), hub: newViewerHub()}
	return s, s.routes()
}

func call(h http.Handler, method, path, origin, body string, hdr ...string) *httptest.ResponseRecorder {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, "http://127.0.0.1:9614"+path, rd)
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestOnlyKnownExtensionsAndOnlyDownloads(t *testing.T) {
	s, h := newTestServer(t)
	const ext = "chrome-extension://abcdefghijklmnopabcdefghijklmnop"
	const app = "http://127.0.0.1:9614"

	// an extension Jasqet doesn't know: it can say hello, nothing more
	if w := call(h, "GET", "/api/ping", ext, ""); w.Code != 200 || !strings.Contains(w.Body.String(), `"allowed":false`) {
		t.Fatalf("ping: %d %s", w.Code, w.Body)
	}
	if w := call(h, "POST", "/api/add", ext, `{"url":"http://example.invalid/a.zip","later":true}`); w.Code != http.StatusForbidden {
		t.Fatalf("unknown extension added a download: %d", w.Code)
	}
	if q := s.extQuestion(); q == nil || q.Origin != ext {
		t.Fatalf("the window should be asking about %s, got %+v", ext, q)
	}
	call(h, "POST", "/api/show", ext, `{"page":"settings"}`)
	if s.openPage != "connect" {
		t.Fatalf("an unknown extension's Show should open the Connect page, got %q", s.openPage)
	}

	// you click Allow in the Jasqet window
	if w := call(h, "POST", "/api/extension", app, `{"origin":"`+ext+`","allow":true}`, keyHeader, s.key.window); w.Code != 200 {
		t.Fatalf("allow: %d %s", w.Code, w.Body)
	}
	other := filepath.Join(t.TempDir(), "Startup")
	w := call(h, "POST", "/api/add", ext, `{"url":"http://example.invalid/a.zip","later":true,"dir":`+jsonQ(other)+`}`)
	if w.Code != 200 {
		t.Fatalf("allowed extension can't add: %d %s", w.Code, w.Body)
	}
	var res struct{ IDs []string }
	json.Unmarshal(w.Body.Bytes(), &res)
	if d := s.m.get(res.IDs[0]); d == nil || d.DirChosen || d.Dir == other {
		t.Fatal("an extension chose the folder")
	}

	// even an allowed extension can't read settings or change them, open, or delete
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/api/state", ""}, {"POST", "/api/settings", `{"downloadDir":"C:\\"}`},
		{"POST", "/api/action", `{"id":"*","action":"clear"}`}, {"POST", "/api/open", `{"id":"x","how":"run"}`},
		{"POST", "/api/safety", `{"id":"x","action":"unblock"}`}, {"POST", "/api/quit", `{}`},
		{"POST", "/api/torrent/add", `{"uri":"magnet:?xt=urn:btih:0000000000000000000000000000000000000000"}`},
		{"POST", "/api/extension", `{"origin":"chrome-extension://zzz","allow":true}`},
	} {
		if w := call(h, c.method, c.path, ext, c.body); w.Code != http.StatusForbidden {
			t.Errorf("extension reached %s %s: %d", c.method, c.path, w.Code)
		}
	}

	// Jasqet's own window may choose the folder
	w = call(h, "POST", "/api/add", app, `{"url":"http://example.invalid/b.zip","later":true,"dir":`+jsonQ(other)+`}`, keyHeader, s.key.window)
	json.Unmarshal(w.Body.Bytes(), &res)
	if d := s.m.get(res.IDs[0]); d == nil || !d.DirChosen || d.Dir != other {
		t.Fatal("the window's own folder choice was lost")
	}

	// web pages: another origin, a spoofed Host, or no Origin from another site
	if w := call(h, "POST", "/api/add", "https://evil.example", `{"url":"http://x/a"}`); w.Code != http.StatusForbidden {
		t.Errorf("another site: %d", w.Code)
	}
	r := httptest.NewRequest("GET", "http://evil.example:9614/api/state", nil)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, r)
	if rw.Code != http.StatusForbidden {
		t.Errorf("spoofed Host: %d", rw.Code)
	}
	if w := call(h, "GET", "/api/state", "", "", "Sec-Fetch-Site", "cross-site"); w.Code != http.StatusForbidden {
		t.Errorf("cross-site request without Origin: %d", w.Code)
	}
	// a local program (no Origin) needs the window's key too (apikey_test.go)
	if w := call(h, "GET", "/api/state", "", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("no Origin and no key: %d", w.Code)
	}
	if w := call(h, "GET", "/api/state", "", "", keyHeader, s.key.window); w.Code != 200 {
		t.Errorf("the window's key should still work: %d", w.Code)
	}
}

func jsonQ(s string) string { b, _ := json.Marshal(s); return string(b) }

func TestUnpackedExtensionID(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows paths")
	}
	// the sum was checked against the ID Microsoft Edge 155 gave a real folder
	// (loaded unpacked); this is the same sum for a sample folder
	const want = "pfdjgocfmjjhldkamfoloifnonjlicap"
	for _, p := range []string{`C:\Users\Public\Jasqet\extension`, `c:\Users\Public\Jasqet\extension`} {
		if got := unpackedExtensionID(p); got != want {
			t.Errorf("unpackedExtensionID(%q) = %s, want %s", p, got, want)
		}
	}
}

// ---------- secrets in the state file ----------

func TestSecretsAreNotInTheStateFile(t *testing.T) {
	m := newTestManager(t)
	_, s := m.Snapshot()
	s.Notify.TelegramToken = "123:SECRET-TG"
	s.Safety.VirusTotalKey = "SECRET-VT"
	s.Proxy = "http://me:SECRET-PROXY@proxy.example:8080"
	m.SetSettings(s)
	d := &Download{ID: "x1", URL: "https://site.example/f.zip", FileName: "f.zip", Status: StPaused, AddedAt: time.Now(),
		Headers: map[string]string{"Cookie": "sid=SECRET-C1", "Authorization": "Bearer SECRET-A", "Referer": "https://site.example/"},
		Cookies: map[string]string{"site.example": "sid=SECRET-C2"}, User: "me", Password: "SECRET-PW"}
	m.mu.Lock()
	m.downloads[d.ID] = d
	m.mu.Unlock()
	m.save()
	b, err := os.ReadFile(m.stateFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"SECRET-TG", "SECRET-VT", "SECRET-PROXY", "SECRET-C1", "SECRET-C2", "SECRET-A", "SECRET-PW"} {
		if bytes.Contains(b, []byte(secret)) {
			t.Errorf("%s is in state.json in plain text", secret)
		}
	}
	if d.Password != "SECRET-PW" || d.Headers["Cookie"] != "sid=SECRET-C1" || d.Secrets != "" {
		t.Fatal("saving changed the download in memory")
	}
	m2 := NewManager(m.stateFile)
	d2 := m2.get("x1")
	if d2 == nil || d2.Password != "SECRET-PW" || d2.Cookies["site.example"] != "sid=SECRET-C2" ||
		d2.Headers["Cookie"] != "sid=SECRET-C1" || d2.Headers["Authorization"] != "Bearer SECRET-A" || d2.Headers["Referer"] == "" {
		t.Fatalf("secrets didn't come back: %+v", d2)
	}
	_, s2 := m2.Snapshot()
	if s2.Notify.TelegramToken != "123:SECRET-TG" || s2.Safety.VirusTotalKey != "SECRET-VT" || s2.Proxy != s.Proxy {
		t.Fatal("settings' secrets didn't come back")
	}
}

func TestOldStateFileStillLoads(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "state.json")
	old := `{"settings":{"downloadDir":` + jsonQ(filepath.Join(dir, "dl")) + `,"notify":{"telegramToken":"OLD-TG"},"viewer":{"external":true}},
		"downloads":[{"id":"o1","url":"https://a.example/f","fileName":"f","status":"paused","cookies":{"a.example":"sid=OLD"},"password":"OLD-PW","user":"u"}]}`
	os.WriteFile(p, []byte(old), 0o600)
	m := NewManager(p)
	d := m.get("o1")
	if d == nil || d.Cookies["a.example"] != "sid=OLD" || d.Password != "OLD-PW" {
		t.Fatalf("1.6.0 download lost its login: %+v", d)
	}
	_, s := m.Snapshot()
	if s.Notify.TelegramToken != "OLD-TG" || s.Viewer.OpenWith != "default" {
		t.Fatalf("1.6.0 settings not carried over: %+v %+v", s.Notify, s.Viewer)
	}
}

// ---------- a login only goes to its own site ----------

func TestLoginOnlyGoesToItsOwnSite(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("WWW-Authenticate", `Basic realm="x"`)
		http.Error(w, "who are you", http.StatusUnauthorized)
	}))
	defer other.Close()
	otherURL := strings.Replace(other.URL, "127.0.0.1", "localhost", 1) // another site
	m := newTestManager(t)
	d := &Download{URL: "http://127.0.0.1:1/f.iso", User: "me", Password: "pw", FileName: "f.iso", Size: 10}
	req, _ := m.newReq(context.Background(), otherURL+"/f.iso", d.reqOpts())
	if _, _, ok := req.BasicAuth(); ok {
		t.Fatal("the login went into a request for another site")
	}
	req, _ = m.newReq(context.Background(), "http://127.0.0.1:1/g", d.reqOpts())
	if _, _, ok := req.BasicAuth(); !ok {
		t.Fatal("the login is missing on its own site")
	}
	d2 := &Download{URL: "http://127.0.0.1:1/f.iso", Headers: map[string]string{"Authorization": "Bearer T"}, FileName: "f.iso"}
	req, _ = m.newReq(context.Background(), otherURL+"/f.iso", d2.reqOpts())
	if req.Header.Get("Authorization") != "" {
		t.Fatal("an Authorization header went to another site")
	}
	// a mirror on another site is checked without the login
	m.checkLink(context.Background(), d, otherURL+"/f.iso", nil, true)
	m.checkLink(context.Background(), d2, otherURL+"/f.iso", nil, true)
	mu.Lock()
	defer mu.Unlock()
	for _, a := range seen {
		if a != "" {
			t.Fatalf("the other site saw %q", a)
		}
	}
	if len(seen) == 0 {
		t.Fatal("the other site was never asked")
	}
}

// ---------- paths from sites, archives and torrents ----------

func TestSubtitleLanguageIsNeverAPath(t *testing.T) {
	m := newTestManager(t)
	for _, bad := range []string{`..\..\x`, "../x", "en:evil", "en/../../x", ""} {
		if _, err := m.AddYT(AddYTRequest{URL: "https://www.youtube.com/watch?v=abc", Variant: "subs", Subs: bad, Later: true}); err == nil {
			t.Errorf("subtitle language %q was accepted", bad)
		}
	}
	for _, ok := range []string{"en", "pt-BR", "zh-Hans", "en-orig", "live_chat"} {
		if !subLangRe.MatchString(ok) {
			t.Errorf("%q should be a valid language", ok)
		}
	}
}

func TestArchiveEntryNames(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"notes.txt:hidden", "notes.txt-hidden"},
		{"con.txt", "_con.txt"},
		{"../../evil.exe", "download/download/evil.exe"},
		{`dir\sub\a.txt.`, "dir/sub/a.txt"},
		{"/abs/x", "abs/x"},
		{"./a/./b", "a/b"},
		{`C:\Windows\x.dll`, "C-/Windows/x.dll"},
	} {
		if got := cleanEntryName(c.in); got != c.want {
			t.Errorf("cleanEntryName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// and a real archive with them stays inside its folder
	dir := t.TempDir()
	zp := filepath.Join(dir, "pack.zip")
	f, _ := os.Create(zp)
	zw := zip.NewWriter(f)
	for _, n := range []string{"ok.txt", "notes.txt:hidden", "con.txt", "sub/a.txt."} {
		w, _ := zw.Create(n)
		w.Write([]byte("hello"))
	}
	zw.Close()
	f.Close()
	dest, n, err := extractArchive(context.Background(), zp)
	if err != nil && !errors.Is(err, zip.ErrInsecurePath) {
		t.Fatal(err)
	}
	if err == nil && n != 4 {
		t.Fatalf("unpacked %d files, want 4", n)
	}
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && p != zp && !strings.HasPrefix(p, dest+string(filepath.Separator)) {
			t.Errorf("%s was written outside %s", p, dest)
		}
		return nil
	})
}

func TestTorrentRiskCheckSeesTheNamesOnDisk(t *testing.T) {
	info := metainfo.Info{Name: "Big Movie 2024 1080p", PieceLength: 1 << 20, Files: []metainfo.FileInfo{
		{Path: []string{"Big.Movie.2024.1080p.mp4"}, Length: 50 << 20},
		{Path: []string{"Codec Player.ex*e"}, Length: 1 << 20}, // saved as "Codec Player.exe"
		{Path: []string{"install.bat."}, Length: 1 << 10},      // saved as "install.bat"
	}}
	tor := addTestTorrent(t, info)
	s := checkTorrentFiles(tor.Name(), tor.Files())
	if s.Level != "danger" {
		t.Fatalf("level %q (%v): a program and a script hide behind odd names", s.Level, s.Notes)
	}
}

func addTestTorrent(t *testing.T, info metainfo.Info) *torrent.Torrent {
	t.Helper()
	var total int64
	for _, f := range info.Files {
		total += f.Length
	}
	info.Pieces = make([]byte, 20*int((total+info.PieceLength-1)/info.PieceLength))
	mi := metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(info)}
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = t.TempDir()
	// no network at all (no firewall question): only the file list is needed
	cfg.NoDHT, cfg.DisableIPv6, cfg.NoDefaultPortForwarding, cfg.DisableTCP, cfg.DisableUTP = true, true, true, true, true
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })
	tor, err := cl.AddTorrent(&mi)
	if err != nil {
		t.Fatal(err)
	}
	return tor
}

func TestTorrentNeverLandsOnAnExistingFolder(t *testing.T) {
	ts := &torrentState{}
	base := t.TempDir()
	os.MkdirAll(filepath.Join(base, "Holiday Photos"), 0o755) // yours, already there
	info := &metainfo.Info{Name: "Holiday Photos"}
	ih := metainfo.Hash{1}
	ts.roots.Store(ih.HexString(), &torrentRoot{dir: base})
	want := filepath.Join(base, "Holiday Photos (1)")
	if got := ts.rootDir(base, info, ih); got != want {
		t.Fatalf("new torrent goes to %s, want %s", got, want)
	}
	if got := ts.rootDir(base, info, ih); got != want {
		t.Fatalf("second open moved it again: %s", got)
	}
	// a torrent that was already downloading there carries on in place
	ih2 := metainfo.Hash{2}
	ts.roots.Store(ih2.HexString(), &torrentRoot{dir: base, placed: true})
	if got := ts.rootDir(base, info, ih2); got != base {
		t.Fatalf("resumed torrent moved to %s", got)
	}
}

func TestRemovingATorrentKeepsYourOtherFiles(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("AppData", cfg) // torrentsDir() lives next to the state file
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("HOME", cfg)
	info := metainfo.Info{Name: "Pack", PieceLength: 1 << 14, Files: []metainfo.FileInfo{
		{Path: []string{"a", "b.txt"}, Length: 5}, {Path: []string{"c.txt"}, Length: 5},
	}}
	info.Pieces = make([]byte, 20)
	mi := metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(info)}
	hash := mi.HashInfoBytes().HexString()
	os.MkdirAll(torrentsDir(), 0o755)
	tf, _ := os.Create(filepath.Join(torrentsDir(), hash+".torrent"))
	mi.Write(tf)
	tf.Close()
	dir := t.TempDir()
	for _, p := range []string{"Pack/a/b.txt", "Pack/c.txt", "Pack/mine.txt", "Pack/a/mine too.txt"} {
		os.MkdirAll(filepath.Join(dir, filepath.Dir(p)), 0o755)
		os.WriteFile(filepath.Join(dir, p), []byte("hello"), 0o644)
	}
	d := &Download{Kind: "torrent", InfoHash: hash, Dir: dir, FileName: "Pack", Placed: true}
	d.removeTorrentFilesLocked()
	for _, gone := range []string{"Pack/a/b.txt", "Pack/c.txt"} {
		if pathExists(filepath.Join(dir, gone)) {
			t.Errorf("%s is still there", gone)
		}
	}
	for _, kept := range []string{"Pack/mine.txt", "Pack/a/mine too.txt"} {
		if !pathExists(filepath.Join(dir, kept)) {
			t.Errorf("%s (not the torrent's) was deleted", kept)
		}
	}
}

// loopbackTorrentClient: a torrent engine on 127.0.0.1 only (no DHT, no
// firewall question), for tests.
func loopbackTorrentClient(t *testing.T, dataDir string, seed bool) *torrent.Client {
	t.Helper()
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = dataDir
	cfg.Seed = seed
	cfg.NoDHT, cfg.DisableIPv6, cfg.DisableUTP, cfg.NoDefaultPortForwarding = true, true, true, true
	cfg.ListenHost = func(string) string { return "127.0.0.1" }
	cfg.ListenPort = 0
	st := storage.NewFile(dataDir)
	cfg.DefaultStorage = st
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close(); st.Close() }) // before the temporary folders go
	return cl
}

func TestTorrentDownloadsThenTheEngineCloses(t *testing.T) {
	cfg := t.TempDir()
	t.Setenv("AppData", cfg)
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("HOME", cfg)
	// what the other side shares: a folder with two files
	src := filepath.Join(tempDirRetry(t), "Holiday Pack")
	os.MkdirAll(src, 0o755)
	film := bytes.Repeat([]byte("film data "), 300<<10)
	os.WriteFile(filepath.Join(src, "film.mp4"), film, 0o644)
	os.WriteFile(filepath.Join(src, "notes.txt"), []byte("hello"), 0o644)
	info := metainfo.Info{PieceLength: 256 << 10}
	if err := info.BuildFromFilePath(src); err != nil {
		t.Fatal(err)
	}
	mi := metainfo.MetaInfo{InfoBytes: bencode.MustMarshal(info)}
	seeder := loopbackTorrentClient(t, filepath.Dir(src), true)
	st, err := seeder.AddTorrent(&mi)
	if err != nil {
		t.Fatal(err)
	}
	<-st.GotInfo()
	st.VerifyData()

	m := newTestManager(t)
	base := runtime.NumGoroutine()
	m.tor.client = loopbackTorrentClient(t, t.TempDir(), false)
	m.tor.storages = map[string]storage.ClientImplCloser{}
	m.tor.down, m.tor.up = rate.NewLimiter(rate.Inf, 1<<20), rate.NewLimiter(rate.Inf, 1<<20)
	// a folder called "Holiday Pack" is already there: the torrent must not mix into it
	_, s := m.Snapshot()
	mine := filepath.Join(s.DownloadDir, "Torrents", "Holiday Pack")
	os.MkdirAll(mine, 0o755)
	os.WriteFile(filepath.Join(mine, "my notes.txt"), []byte("mine"), 0o644)

	magnet := "magnet:?xt=urn:btih:" + mi.HashInfoBytes().HexString() + "&x.pe=" + seeder.ListenAddrs()[0].String()
	id, err := m.AddTorrent(AddTorrentRequest{URI: magnet})
	if err != nil {
		t.Fatal(err)
	}
	d := waitStatus(t, m, id, StDone, time.Minute)
	got, _ := os.ReadFile(d.torrentDiskPathLocked(0))
	if d.Dir == filepath.Dir(mine) || !bytes.Equal(got, film) {
		t.Fatalf("torrent placed in %s (%d bytes): it should be its own folder next to yours", d.Dir, len(got))
	}
	m.Delete(d, true)
	time.Sleep(300 * time.Millisecond)
	if !pathExists(filepath.Join(mine, "my notes.txt")) {
		t.Fatal("removing the torrent deleted your file")
	}
	// two minutes after the last torrent, the engine closes
	m.tor.mu.Lock()
	m.tor.lastUse = time.Now().Add(-3 * time.Minute)
	m.tor.mu.Unlock()
	m.closeIdleTorrents()
	if m.tor.client != nil {
		t.Fatal("the torrent engine is still running with nothing to do")
	}
	m.client.CloseIdleConnections()
	waitGoroutines(t, base)
}

// ---------- keeping the computer awake ----------

func TestKeepAwakeOnlyWhenThereIsWork(t *testing.T) {
	d := &Download{Status: StQueued}
	m := &Manager{downloads: map[string]*Download{"a": d}, holds: map[string]bool{}}
	m.settings.Schedule.KeepAwake = true
	if !m.wantAwake() {
		t.Fatal("a queued download should keep the computer awake")
	}
	m.holds["metered"] = true
	if m.wantAwake() {
		t.Fatal("paused for a metered network: it may sleep")
	}
	m.holds["metered"] = false
	d.retryAt = time.Now().Add(5 * time.Minute)
	if m.wantAwake() {
		t.Fatal("trying again in 5 minutes: it may sleep meanwhile")
	}
}

// ---------- preview ----------

func TestPreviewReaderKeepsItsFileAndStopsWhenRemoved(t *testing.T) {
	m := newTestManager(t)
	dir := tempDirRetry(t)
	data := bytes.Repeat([]byte("v"), 1<<20)
	os.WriteFile(filepath.Join(dir, "a.mp4.part"), data, 0o644)
	size := int64(len(data))
	done := &Download{ID: "p1", Dir: dir, FileName: "a.mp4", Size: size, Resumable: true, Status: StPaused,
		Segments: []*Segment{{Start: 0, End: size - 1, Pos: size}}}
	m.mu.Lock()
	m.downloads[done.ID] = done
	m.mu.Unlock()
	rd := &previewReader{ctx: context.Background(), m: m, d: done, size: size}
	buf := make([]byte, 32<<10)
	rd.Read(buf)
	first := rd.f
	for i := 0; i < 20; i++ {
		rd.Read(buf)
	}
	if first == nil || rd.f != first {
		t.Fatal("the file was opened again for every read")
	}
	rd.Close()

	// nothing downloaded yet, then the download is removed: the player's request ends
	empty := &Download{ID: "p2", Dir: dir, FileName: "b.mp4", Size: size, Resumable: true, Status: StPaused,
		Segments: []*Segment{{Start: 0, End: size - 1, Pos: 0}}}
	os.WriteFile(filepath.Join(dir, "b.mp4.part"), nil, 0o644)
	m.mu.Lock()
	m.downloads[empty.ID] = empty
	m.mu.Unlock()
	base := runtime.NumGoroutine()
	errc := make(chan error, 1)
	go func() {
		r := &previewReader{ctx: context.Background(), m: m, d: empty, size: size}
		defer r.Close()
		_, err := r.Read(buf)
		errc <- err
	}()
	time.Sleep(300 * time.Millisecond)
	m.Delete(empty, false)
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("no error after the download was removed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the preview kept waiting for a removed download")
	}
	waitGoroutines(t, base)
}

func waitGoroutines(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > base+1 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > base+1 {
		buf := make([]byte, 1<<20)
		t.Fatalf("%d goroutines left (was %d):\n%s", n, base, buf[:runtime.Stack(buf, true)])
	}
}

type fakeTorrentReader struct{ torrent.Reader }

func (fakeTorrentReader) Read(p []byte) (int, error) { return len(p), nil }

func TestTorrentPreviewFollowsThePlayer(t *testing.T) {
	d := &Download{}
	again := 0
	r := &torrentPlayReader{Reader: fakeTorrentReader{}, d: d, again: func() { again++ }}
	r.Read(make([]byte, 10))
	if d.tReading != 0 || time.Since(d.tReadAt) > time.Second || again != 1 {
		t.Fatalf("reading: %d, last read %v ago, restarted %d times", d.tReading, time.Since(d.tReadAt), again)
	}
	d.tPreview = &torrent.Torrent{} // in order already: nothing to restart
	r.Read(make([]byte, 10))
	if again != 1 {
		t.Fatal("restarted while already in order")
	}
}

// ---------- damaged or made-up video chunks ----------

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestRemuxDoesNotSwallowHugeBoxes(t *testing.T) {
	old := maxBox
	maxBox = 1 << 20
	defer func() { maxBox = old }()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	// a box that "runs to the end" of 64 MB
	in := io.MultiReader(bytes.NewReader([]byte{0, 0, 0, 0, 'f', 'r', 'e', 'e'}), io.LimitReader(zeros{}, 64<<20))
	if _, err := collectFMP4(context.Background(), in, nil, true, true); err == nil {
		t.Fatal("a box bigger than the limit was accepted")
	}
	// a box that says it's 2 GB
	in = io.MultiReader(bytes.NewReader([]byte{0x7f, 0xff, 0xff, 0xff, 'm', 'o', 'o', 'f'}), zeros{})
	if _, err := collectFMP4(context.Background(), in, nil, true, true); err == nil {
		t.Fatal("a 2 GB box was accepted")
	}
	// the film's data before any index: not fragmented, and not read into memory
	in = io.MultiReader(bytes.NewReader([]byte{0x7f, 0xff, 0xff, 0xff, 'm', 'd', 'a', 't'}), zeros{})
	if _, err := collectFMP4(context.Background(), in, nil, true, true); !errors.Is(err, errNotFragmented) {
		t.Fatalf("got %v, want errNotFragmented", err)
	}
	runtime.ReadMemStats(&after)
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 32<<20 {
		t.Fatalf("used %d MB for chunks it refused", grew>>20)
	}
}

// ---------- sharing on the local network ----------

func TestLANRequestsAreSigned(t *testing.T) {
	r := httptest.NewRequest("GET", "/lan/file/abc", nil)
	lanSignReq(r, "family")
	if !lanSigned(r, "family") {
		t.Fatal("a correctly signed request was refused")
	}
	if lanSigned(r, "other") {
		t.Fatal("the wrong group code was accepted")
	}
	r2 := httptest.NewRequest("GET", "/lan/file/xyz", nil)
	r2.Header.Set("X-Jasqet-Auth", r.Header.Get("X-Jasqet-Auth"))
	if lanSigned(r2, "family") {
		t.Fatal("a signature was reused for another file")
	}
	r3 := httptest.NewRequest("GET", "/lan/file/abc", nil)
	r3.Header.Set("X-Jasqet-Auth", lanSign("family", "GET", "/lan/file/abc", time.Now().Add(-time.Hour)))
	if lanSigned(r3, "family") {
		t.Fatal("an hour-old signature was accepted")
	}
	if strings.Contains(r.Header.Get("X-Jasqet-Auth"), lanTag("family")) {
		t.Fatal("the broadcast tag can be used to ask for files")
	}
}

func TestLANDoesNotShareYourPrivateFilesByName(t *testing.T) {
	m := newTestManager(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "payslip.pdf"), []byte("private"), 0o644)
	d := &Download{ID: "l1", URL: "https://bank.example/docs/payslip.pdf?sig=1", Dir: dir, FileName: "payslip.pdf", Size: 7,
		Status: StDone, Cookies: map[string]string{"bank.example": "sid=1"}}
	m.mu.Lock()
	m.downloads[d.ID] = d
	m.mu.Unlock()
	find := func(q string) bool {
		w := httptest.NewRecorder()
		m.lanFindHandler(w, httptest.NewRequest("GET", "/lan/find?"+q, nil))
		return strings.Contains(w.Body.String(), `"found":true`)
	}
	if find("name=payslip.pdf&size=7") {
		t.Fatal("a file fetched with your cookies was found by its name alone")
	}
	// 1.6.2: a file fetched with a login or cookies is never shared, not even
	// with a computer that has the same link (lan_test.go checks more cases)
	if find("url=https%3A%2F%2Fbank.example%2Fdocs%2Fpayslip.pdf&size=7") {
		t.Fatal("a file fetched with your cookies was offered to another computer")
	}
	if got := linkWithoutSecrets("https://me:pw@bank.example/docs/f.pdf?sig=abc#x"); got != "https://bank.example/docs/f.pdf" {
		t.Fatalf("linkWithoutSecrets = %q", got)
	}
}

// ---------- the YouTube helper tools ----------

func TestHelperToolsMustMatchTheirPublishedChecksum(t *testing.T) {
	m := newTestManager(t)
	file := filepath.Join(t.TempDir(), "yt-dlp.exe.part")
	os.WriteFile(file, []byte("the real program"), 0o644)
	sum := sha256.Sum256([]byte("the real program"))
	good := hex.EncodeToString(sum[:])
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/SHA2-256SUMS":
			io.WriteString(w, strings.Repeat("0", 64)+"  yt-dlp_linux\n"+good+"  yt-dlp.exe\n")
		case "/deno.zip.sha256sum": // Deno's Windows files: PowerShell's Get-FileHash output
			io.WriteString(w, "\r\nAlgorithm       Hash                                                                   Path\r\n---------       ----                                                                   ----\r\nSHA256          "+strings.ToUpper(good)+"       D:\\a\\deno\\deno\\target\\release\\deno.zip\r\n")
		case "/bad":
			io.WriteString(w, strings.Repeat("1", 64)+"  yt-dlp.exe\n")
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	if err := m.checkPublished(ctx, file, srv.URL+"/SHA2-256SUMS", "yt-dlp.exe"); err != nil {
		t.Fatalf("matching file refused: %v", err)
	}
	if err := m.checkPublished(ctx, file, srv.URL+"/deno.zip.sha256sum", "deno.zip"); err != nil {
		t.Fatalf("Deno's checksum format not read: %v", err)
	}
	if err := m.checkPublished(ctx, file, srv.URL+"/bad", "yt-dlp.exe"); err == nil {
		t.Fatal("a file that doesn't match was accepted")
	}
}

func TestHelperToolsSetThemselvesUpWhenAVideoNeedsThem(t *testing.T) {
	// the tools folder sits next to the settings: keep this test's apart
	home := t.TempDir()
	for _, k := range []string{"APPDATA", "XDG_CONFIG_HOME", "HOME", "USERPROFILE", "PATH"} {
		t.Setenv(k, home) // PATH too, so a Deno installed on this computer isn't used instead
	}
	if !strings.HasPrefix(toolsDir(), home) {
		t.Fatalf("the tools would land in %s", toolsDir())
	}
	// a pretend GitHub with both tools and their checksums
	yt := []byte("pretend yt-dlp")
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	f, _ := zw.Create(exeName("deno"))
	f.Write([]byte("pretend deno"))
	zw.Close()
	hash := func(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
	var asked atomic.Int32
	var badSum atomic.Bool
	var holdMu sync.Mutex
	hold := make(chan struct{}) // keeps the yt-dlp download going until closed
	holdNew := func() { holdMu.Lock(); hold = make(chan struct{}); holdMu.Unlock() }
	holdOff := func() { holdMu.Lock(); close(hold); holdMu.Unlock() }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		switch r.URL.Path {
		case "/yt/" + ytdlpAsset():
			holdMu.Lock()
			h := hold
			holdMu.Unlock()
			<-h
			w.Write(yt)
		case "/yt/SHA2-256SUMS":
			sum := hash(yt)
			if badSum.Load() {
				sum = strings.Repeat("0", 64)
			}
			io.WriteString(w, sum+"  "+ytdlpAsset()+"\n")
		case "/deno/" + denoAsset():
			w.Write(zb.Bytes())
		case "/deno/" + denoAsset() + ".sha256sum":
			io.WriteString(w, hash(zb.Bytes())+"  "+denoAsset()+"\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	oldYT, oldDeno, oldVer := ytdlpRelease, denoRelease, readToolVersions
	ytdlpRelease, denoRelease = srv.URL+"/yt/", srv.URL+"/deno/"
	readToolVersions = func(*Manager) {} // the pretend tools can't run
	defer func() { ytdlpRelease, denoRelease, readToolVersions = oldYT, oldDeno, oldVer }()
	m := newTestManager(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Looking a video up (the extension does that before you click) sets nothing up.
	p, err := m.ProbeYT(ctx, "https://www.youtube.com/watch?v=abc", nil)
	if err != nil || p.Setup == nil || p.Setup.State != "missing" || asked.Load() != 0 {
		t.Fatalf("probe: %+v %v, %d requests", p, err, asked.Load())
	}

	// A setup that fails says why, and the next video tries again by itself.
	badSum.Store(true)
	holdOff()
	if err := m.toolsFor(ctx, &Download{}); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("failed setup: %v", err)
	}
	if st := m.ToolsStatus(); st.State != "error" || fileExists(ytdlpPath()) {
		t.Fatalf("after a failed setup: %+v", st)
	}
	holdNew()
	badSum.Store(false)

	// A download that needs the tools sets them up, and says so meanwhile.
	d := &Download{}
	done := make(chan error, 1)
	go func() { done <- m.toolsFor(ctx, d) }()
	var note, state string
	for i := 0; i < 300 && (!strings.Contains(note, "one time only") || state != "installing"); i++ {
		time.Sleep(10 * time.Millisecond)
		d.mu.Lock()
		note = d.Note
		d.mu.Unlock()
		state = m.ToolsStatus().State
	}
	if !strings.Contains(note, "one time only") || state != "installing" {
		t.Fatalf("while setting up: note %q, state %q", note, state)
	}
	holdOff()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(ytdlpPath())
	deno, _ := os.ReadFile(filepath.Join(toolsDir(), exeName("deno")))
	if string(got) != "pretend yt-dlp" || string(deno) != "pretend deno" || !m.tools.ready() || d.Note != "" {
		t.Fatalf("after the setup: yt-dlp %q, deno %q, note %q", got, deno, d.Note)
	}
	// and once it's there nothing is asked again
	n := asked.Load()
	if err := m.toolsWait(ctx); err != nil || asked.Load() != n {
		t.Fatalf("ready tools set up again: %v", err)
	}
}

// ---------- notifications and messages ----------

func TestDownloadsFinishingTogetherShareOneNotification(t *testing.T) {
	title, text, id := toastText([]pendingToast{{"✅ Downloaded: a.mp4 (2 MB)", "id1"}})
	if title != "Jasqet" || id != "id1" || !strings.Contains(text, "a.mp4") {
		t.Fatalf("one download: %q %q %q", title, text, id)
	}
	var list []pendingToast
	for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
		list = append(list, pendingToast{"✅ Downloaded: " + n + ".zip (1 MB)\nunzipped 3 files", "id-" + n})
	}
	title, text, id = toastText(list)
	if id != "" || !strings.Contains(title, "6 downloads") || !strings.Contains(text, "…and 2 more") || strings.Contains(text, "unzipped") {
		t.Fatalf("six downloads: %q / %q / %q", title, text, id)
	}
	// the real thing: several finishing within the wait give one notification
	m := newTestManager(t)
	oldWait, oldShow := toastWait, showToast
	shown := make(chan string, 10)
	toastWait, showToast = 50*time.Millisecond, func(title, text, id string) { shown <- title }
	defer func() { toastWait, showToast = oldWait, oldShow }()
	for i := 0; i < 3; i++ {
		m.notifyDesktop("✅ Downloaded: x", "id")
	}
	select {
	case got := <-shown:
		if !strings.Contains(got, "3 downloads") {
			t.Fatalf("got %q", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no notification")
	}
	select {
	case got := <-shown:
		t.Fatalf("a second notification: %q", got)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestErrorsInPlainWords(t *testing.T) {
	_, err := os.Open(filepath.Join(t.TempDir(), "missing", "file.zip"))
	got := friendlyError(err)
	if strings.Contains(got, `\`) || strings.Contains(got, "open ") || !strings.Contains(got, "file.zip") {
		t.Errorf("file error: %q", got)
	}
	if got := friendlyError(io.ErrUnexpectedEOF); !strings.Contains(got, "cut off") {
		t.Errorf("cut-off connection: %q", got)
	}
	if got := friendlyError(errors.New("the download hasn't finished yet")); got != "the download hasn't finished yet" {
		t.Errorf("Jasqet's own messages must stay as they are: %q", got)
	}
}

func TestNumberedCopyStillPretends(t *testing.T) {
	s := &Safety{}
	p := filepath.Join(t.TempDir(), "x")
	os.WriteFile(p, []byte("MZ\x90\x00program"), 0o644)
	checkType(p, "movie.mp4 (1).exe", s)
	if s.Level != "danger" {
		t.Fatalf("level %q: a numbered copy of movie.mp4.exe is still a fake video", s.Level)
	}
}

// ---------- goroutines after pausing, cancelling and previews ----------

func TestNoGoroutinesLeftBehind(t *testing.T) {
	ts := &testServer{data: make([]byte, 4<<20), rate: 1 << 20}
	srv := httptest.NewServer(ts)
	defer srv.Close()
	m := newTestManager(t)
	base := runtime.NumGoroutine()
	ids, err := m.Add(AddRequest{URL: srv.URL + "/file.bin", Connections: 4})
	if err != nil {
		t.Fatal(err)
	}
	d := m.get(ids[0])
	running := func() bool { d.mu.Lock(); defer d.mu.Unlock(); return d.running }
	for i := 0; i < 3; i++ {
		time.Sleep(300 * time.Millisecond)
		m.Pause(d)
		for running() {
			time.Sleep(10 * time.Millisecond)
		}
		m.Resume(d)
	}
	time.Sleep(300 * time.Millisecond)
	// a preview opened and closed while it downloads
	d.mu.Lock()
	size := d.Size
	d.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	rd := &previewReader{ctx: ctx, m: m, d: d, size: size}
	rd.Seek(size-1000, io.SeekStart)
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	rd.Read(make([]byte, 1000))
	rd.Close()
	m.previewDone(d, rd)
	m.Cancel(d)
	for running() {
		time.Sleep(10 * time.Millisecond)
	}
	m.Delete(d, true)
	srv.CloseClientConnections()
	m.client.CloseIdleConnections()
	waitGoroutines(t, base)
}
