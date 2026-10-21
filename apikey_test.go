package main

// Tests for the key to the local API (apikey.go) and the secrets the window
// no longer sees (secrets.go).

import (
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// isolateConfig: anything that still asks for Jasqet's settings folder gets a
// temporary one, never the real one on this computer.
func isolateConfig(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("AppData", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	if runtime.GOOS == "darwin" {
		t.Setenv("HOME", dir)
	}
}

// apiRoutes: every address the server answers, read from the code, so a new
// one can't be left without the key by mistake.
func apiRoutes(t *testing.T) []string {
	t.Helper()
	files, _ := filepath.Glob("*.go")
	re := regexp.MustCompile(`mux\.HandleFunc\("(/[^"]*)"`)
	seen := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "lan.go" { // lan.go: its own server on the network
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			p := m[1]
			if strings.HasSuffix(p, "/") && p != "/" {
				p += "x"
			}
			seen[p] = true
		}
	}
	var out []string
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	if len(out) < 40 {
		t.Fatalf("found only %d routes: %v", len(out), out)
	}
	return out
}

func TestAPINeedsTheKey(t *testing.T) {
	isolateConfig(t)
	s, h := newTestServer(t)
	_, st := s.m.Snapshot()
	st.Notify.TelegramToken = "123:SECRET-TG"
	s.m.SetSettings(st)
	tried := 0
	for _, p := range apiRoutes(t) {
		if openPaths[p] || p == "/viewer" || strings.HasPrefix(p, "/viewer/") {
			continue
		}
		for _, method := range []string{"GET", "POST"} {
			if p == "/api/tools" && method == "GET" { // only says whether the video helper is ready
				continue
			}
			body := ""
			if method == "POST" {
				body = "{}"
			}
			// a local program (no Origin), or a tab another extension opened on
			// Jasqet's address (its Origin is Jasqet's own), with no key, a
			// made-up one, or the key another copy of Jasqet uses
			for _, origin := range []string{"", "http://127.0.0.1:9614", "http://localhost:9614"} {
				keys := []string{"", "0123456789abcdef", s.key.command}
				if commandPaths[p] {
					keys = keys[:2]
				}
				for _, k := range keys {
					var hdr []string
					if k != "" {
						hdr = []string{keyHeader, k}
					}
					w := call(h, method, p, origin, body, hdr...)
					tried++
					if w.Code != http.StatusUnauthorized {
						t.Errorf("%s %s (Origin %q, key %q): %d %s", method, p, origin, k, w.Code, w.Body)
					}
					if strings.Contains(w.Body.String(), "SECRET") {
						t.Fatalf("%s %s gave away a secret", method, p)
					}
				}
			}
		}
	}
	select {
	case <-s.quit:
		t.Fatal("a request without the key closed Jasqet")
	default:
	}
	t.Logf("%d requests refused", tried)
}

func TestTheWindowStillWorks(t *testing.T) {
	isolateConfig(t)
	s, h := newTestServer(t)
	app := "http://127.0.0.1:9614"
	win := func(method, path, body string) int {
		w := call(h, method, path, app, body, keyHeader, s.key.window)
		if w.Code == http.StatusUnauthorized || w.Code == http.StatusForbidden {
			t.Errorf("the window was refused %s %s: %d %s", method, path, w.Code, w.Body)
		}
		return w.Code
	}
	// every call the window's page makes, as it makes it (GET without a body,
	// POST with one); left out: the ones that open a dialog, close Jasqet or
	// go online (the helper tools, torrents), and media/probe (it retries an
	// empty link for seconds; learn_test.go calls it with the key)
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`api\('([a-z/]+)'(\s*,)?`)
	skip := map[string]bool{"pickfolder": true, "quit": true, "tools/install": true, "tools/update": true, "torrent/add": true, "media/probe": true}
	n, done := 0, map[string]bool{}
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		if skip[m[1]] || done[m[0]] {
			continue
		}
		done[m[0]] = true
		if m[2] != "" {
			win("POST", "/api/"+m[1], "{}")
		} else {
			win("GET", "/api/"+m[1], "")
		}
		n++
	}
	if n < 25 {
		t.Fatalf("found only %d calls in index.html", n)
	}
	// the page's own folder choice still counts
	dir := filepath.Join(t.TempDir(), "mine")
	w := call(h, "POST", "/api/add", app, `{"url":"http://example.invalid/a.zip","later":true,"dir":`+jsonQ(dir)+`}`, keyHeader, s.key.window)
	var res struct{ IDs []string }
	json.Unmarshal(w.Body.Bytes(), &res)
	if len(res.IDs) != 1 {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	if d := s.m.get(res.IDs[0]); d == nil || !d.DirChosen || d.Dir != dir {
		t.Fatal("the window's folder choice was lost")
	}
	// the same request without the key may not choose the folder (it's refused outright)
	if w := call(h, "POST", "/api/add", app, `{"url":"http://example.invalid/b.zip","dir":`+jsonQ(dir)+`}`); w.Code != http.StatusUnauthorized {
		t.Fatalf("add without the key: %d", w.Code)
	}
}

// The page only talks to the API through api(), which sends the key.
func TestPageSendsTheKey(t *testing.T) {
	b, _ := webFS.ReadFile("web/index.html")
	page := string(b)
	if !strings.Contains(page, `'X-Jasqet-Key': await KEY`) {
		t.Fatal("api() doesn't send the key")
	}
	for _, m := range regexp.MustCompile(`fetch\('(/api/[a-z/]*)`).FindAllStringSubmatch(page, -1) {
		if m[1] != "/api/" && m[1] != "/api/session" {
			t.Errorf("index.html fetches %s without api() (no key)", m[1])
		}
	}
	for _, read := range []string{"n.telegramToken", "sf.virusTotalKey", "l.code"} {
		if strings.Contains(page, read) {
			t.Errorf("index.html still reads %s (it's never sent any more)", read)
		}
	}
}

func TestAnotherCopyOfJasqet(t *testing.T) {
	isolateConfig(t)
	s, h := newTestServer(t)
	// the key file: only for your account, and read back the same
	dir := t.TempDir()
	if err := s.key.saveCommandKey(dir, "9614"); err != nil {
		t.Fatal(err)
	}
	if got := readCommandKey(dir, "9614"); got != s.key.command {
		t.Fatalf("read back %q", got)
	}
	if readCommandKey(dir, "9714") != "" {
		t.Fatal("another port's Jasqet got this key")
	}
	if runtime.GOOS != "windows" { // Windows: your profile folder is yours alone
		if st, _ := os.Stat(commandKeyFile(dir, "9614")); st.Mode().Perm() != 0o600 {
			t.Errorf("key file is %v", st.Mode().Perm())
		}
	}
	// it can pass on a jasqet:// link and bring the window forward, nothing else
	cmd := []string{keyHeader, s.key.command}
	if w := call(h, "POST", "/api/protocol", "", `{"url":"jasqet://open/none"}`, cmd...); w.Code != 200 {
		t.Fatalf("protocol: %d %s", w.Code, w.Body)
	}
	for _, c := range []struct{ page, want string }{{"settings", ""}, {"view:abc", ""}, {"connect", "connect"}} {
		s.openPage = ""
		if w := call(h, "POST", "/api/show", "", `{"page":"`+c.page+`"}`, cmd...); w.Code != 200 {
			t.Fatalf("show: %d", w.Code)
		}
		if s.openPage != c.want {
			t.Errorf("show %q without the window's key opened %q", c.page, s.openPage)
		}
	}
	s.openPage = ""
	call(h, "POST", "/api/show", "", `{"page":"settings"}`, keyHeader, s.key.window)
	if s.openPage != "settings" {
		t.Errorf("the window's own Show: %q", s.openPage)
	}
	if w := call(h, "POST", "/api/quit", "", "{}", cmd...); w.Code != 200 {
		t.Fatalf("quit: %d", w.Code)
	}
	select {
	case <-s.quit:
	case <-time.After(3 * time.Second):
		t.Fatal("quit with the command key didn't close Jasqet")
	}
}

func TestBrowserWindowCode(t *testing.T) {
	isolateConfig(t)
	s, h := newTestServer(t)
	u := s.key.browserURL("http://127.0.0.1:9614/#connect")
	p, err := url.Parse(u)
	if err != nil || p.Path != "/" || p.Fragment != "connect" || len(p.Query().Get("code")) != 32 {
		t.Fatalf("browser address %q", u)
	}
	code := p.Query().Get("code")
	if strings.Contains(u, s.key.window) {
		t.Fatal("the key itself is in the address")
	}
	swap := func(c string) (int, string) {
		w := call(h, "POST", "/api/session", "http://127.0.0.1:9614", `{"code":`+jsonQ(c)+`}`)
		var r struct{ Key string }
		json.Unmarshal(w.Body.Bytes(), &r)
		return w.Code, r.Key
	}
	if code, key := swap(code); code != 200 || key != s.key.window {
		t.Fatalf("swap: %d %q", code, key)
	}
	if code, key := swap(code); code != http.StatusForbidden || key != "" {
		t.Fatalf("a code worked twice: %d", code)
	}
	if code, _ := swap("not-a-code"); code != http.StatusForbidden {
		t.Fatalf("made-up code: %d", code)
	}
	if code, _ := swap(""); code != http.StatusForbidden {
		t.Fatalf("no code: %d", code)
	}
	// two minutes later it's no good
	old := s.key.browserURL("http://127.0.0.1:9614/")
	oc := old[strings.Index(old, "code=")+5:]
	s.key.mu.Lock()
	s.key.codes[oc] = time.Now().Add(-time.Second)
	s.key.mu.Unlock()
	if code, _ := swap(oc); code != http.StatusForbidden {
		t.Fatalf("an old code worked: %d", code)
	}
	// extensions can't swap codes
	ext := "chrome-extension://abcdefghijklmnopabcdefghijklmnop"
	c2 := s.key.browserURL("http://127.0.0.1:9614/")
	if w := call(h, "POST", "/api/session", ext, `{"code":`+jsonQ(c2[strings.Index(c2, "code=")+5:])+`}`); w.Code != http.StatusForbidden || strings.Contains(w.Body.String(), s.key.window) {
		t.Fatalf("extension: %d %s", w.Code, w.Body)
	}
	if u := s.key.browserURL("http://127.0.0.1:9614/"); !strings.HasPrefix(u, "http://127.0.0.1:9614/?code=") || strings.Contains(u, "#") {
		t.Fatalf("plain address: %q", u)
	}
}

func TestWindowScript(t *testing.T) {
	k := newAPIKey()
	js := k.windowScript("http://127.0.0.1:9614/#connect")
	for _, want := range []string{`location.origin === "http://127.0.0.1:9614"`, "window.top === window", `window.jasqetKey = "` + k.window + `"`} {
		if !strings.Contains(js, want) {
			t.Errorf("window script lacks %s: %s", want, js)
		}
	}
	if strings.Contains(js, k.command) {
		t.Error("the window got the command key")
	}
	if a, b := newAPIKey(), newAPIKey(); a.window == b.window || len(a.window) != 64 || a.window == a.command {
		t.Error("keys aren't fresh and random each start")
	}
}

// ---------- secrets the window no longer sees ----------

func TestStateHasNoSecrets(t *testing.T) {
	isolateConfig(t)
	s, h := newTestServer(t)
	_, st := s.m.Snapshot()
	st.Notify.TelegramToken = "123:SECRET-TG"
	st.Safety.VirusTotalKey = "SECRET-VT"
	st.Proxy = "http://me:SECRET%40PROXY@proxy.example:8080"
	st.LAN.Code = "SECRET-LAN"
	s.m.SetSettings(st)
	key := []string{keyHeader, s.key.window}
	w := call(h, "GET", "/api/state", "http://127.0.0.1:9614", "", key...)
	if w.Code != 200 {
		t.Fatalf("state: %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "SECRET") {
		t.Fatalf("state gave away a secret: %s", w.Body)
	}
	var got struct {
		Settings Settings     `json:"settings"`
		Saved    savedSecrets `json:"saved"`
	}
	json.Unmarshal(w.Body.Bytes(), &got)
	if got.Saved != (savedSecrets{TelegramToken: true, VirusTotalKey: true, ProxyPassword: true, LANCode: true}) {
		t.Fatalf("is-set flags: %+v", got.Saved)
	}
	if got.Settings.Proxy != "http://me:"+savedMask+"@proxy.example:8080" {
		t.Fatalf("proxy shown as %q", got.Settings.Proxy)
	}

	// Save as the page does: the dots go back, nothing changes
	sv := got.Settings
	sv.Notify.TelegramToken, sv.Safety.VirusTotalKey, sv.LAN.Code = savedMask, savedMask, savedMask
	b, _ := json.Marshal(sv)
	if w := call(h, "POST", "/api/settings", "http://127.0.0.1:9614", string(b), key...); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	_, now := s.m.Snapshot()
	if now.Notify.TelegramToken != st.Notify.TelegramToken || now.Safety.VirusTotalKey != st.Safety.VirusTotalKey ||
		now.LAN.Code != st.LAN.Code || now.Proxy != st.Proxy {
		t.Fatalf("saving the dots changed a secret: %+v %+v %q %q", now.Notify, now.Safety, now.LAN.Code, now.Proxy)
	}

	// typing a new one replaces it, clearing the field removes it, and a new
	// proxy address keeps the saved password
	body := `{"notify":{"telegramToken":"456:NEW"},"safety":{"virusTotalKey":""},"proxy":"http://me:` + savedMask + `@other.example:3128"}`
	if w := call(h, "POST", "/api/settings", "http://127.0.0.1:9614", body, key...); w.Code != 200 {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	_, now = s.m.Snapshot()
	if now.Notify.TelegramToken != "456:NEW" || now.Safety.VirusTotalKey != "" || now.LAN.Code != "SECRET-LAN" ||
		now.Proxy != "http://me:SECRET%40PROXY@other.example:3128" {
		t.Fatalf("after editing: %q %q %q %q", now.Notify.TelegramToken, now.Safety.VirusTotalKey, now.LAN.Code, now.Proxy)
	}
}

func TestMaskProxy(t *testing.T) {
	for _, c := range []struct{ in, shown string }{
		{"", ""},
		{"127.0.0.1:8080", "127.0.0.1:8080"},
		{"socks5://127.0.0.1:1080", "socks5://127.0.0.1:1080"},
		{"http://me@proxy:8080", "http://me@proxy:8080"},
		{"http://me:pw@proxy:8080", "http://me:" + savedMask + "@proxy:8080"},
		{"me:pw@proxy:8080", "http://me:" + savedMask + "@proxy:8080"},
		{"socks5://a%40b:p%3Aw@proxy:1080", "socks5://a%40b:" + savedMask + "@proxy:1080"},
		{"http://me:p w@proxy", "http://" + savedMask + "@proxy"}, // can't be read: all of the login hidden
	} {
		shown, has := maskProxy(c.in)
		if shown != c.shown || has != strings.Contains(c.shown, savedMask) {
			t.Errorf("maskProxy(%q) = %q %v, want %q", c.in, shown, has, c.shown)
		}
		if back := unmaskProxy(shown, c.in); back != c.in {
			t.Errorf("unmaskProxy(%q) = %q, want %q back", shown, back, c.in)
		}
	}
	// a password that wasn't there to keep: the dots just go
	if got := unmaskProxy("http://me:"+savedMask+"@proxy:8080", ""); got != "http://me@proxy:8080" {
		t.Errorf("nothing saved: %q", got)
	}
	if got := unmaskProxy("http://me:"+savedMask+"@new:1", "socks5://me:p%40ss@old:2"); got != "http://me:p%40ss@new:1" {
		t.Errorf("moved proxy: %q", got)
	}
}

// ---------- Linux: which account a connection comes from ----------

func TestSocketOwner(t *testing.T) {
	if binary.NativeEndian.Uint16([]byte{1, 0}) != 1 {
		t.Skip("the sample table is from a little-endian computer")
	}
	// /proc/net/tcp with a browser (uid 1000) and someone else (uid 1001)
	// talking to Jasqet on 127.0.0.1:9614 (258E in hex)
	tcp := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:258E 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 41001 1 0000000000000000 100 0 0 10 0
   1: 0100007F:D431 0100007F:258E 01 00000000:00000000 00:00000000 00000000  1000        0 41002 1 0000000000000000 20 4 30 10 -1
   2: 0100007F:258E 0100007F:D431 01 00000000:00000000 00:00000000 00000000  1000        0 41003 1 0000000000000000 20 4 30 10 -1
   3: 0100007F:E001 0100007F:258E 01 00000000:00000000 00:00000000 00000000  1001        0 41004 1 0000000000000000 20 4 30 10 -1
`
	tcp6 := `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0000000000000000FFFF00000100007F:E002 0000000000000000FFFF00000100007F:258E 01 00000000:00000000 00:00000000 00000000  1002        0 41005 1 0000000000000000 20 4 30 10 -1
`
	jasqet := netip.MustParseAddrPort("127.0.0.1:9614")
	for _, c := range []struct {
		table string
		v6    bool
		peer  string
		uid   int
		ok    bool
	}{
		{tcp, false, "127.0.0.1:54321", 1000, true},
		{tcp, false, "127.0.0.1:57345", 1001, true},
		{tcp, false, "127.0.0.1:1234", 0, false},
		{tcp6, true, "127.0.0.1:57346", 1002, true},
	} {
		uid, ok := socketOwner([]byte(c.table), netip.MustParseAddrPort(c.peer), jasqet, c.v6)
		if uid != c.uid || ok != c.ok {
			t.Errorf("%s: uid %d %v, want %d %v", c.peer, uid, ok, c.uid, c.ok)
		}
	}
}
