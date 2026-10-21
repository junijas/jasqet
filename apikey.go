package main

// The key to Jasqet's local API.
//
// Anything on this computer can reach 127.0.0.1:9614: other programs, other
// Windows accounts, any browser extension (it can open a tab there, and that
// tab's Origin is Jasqet's own). So a header saying where a request comes
// from proves nothing. Instead Jasqet's window gets a random key each time
// Jasqet starts, kept only in the page, and every request must carry it (the
// X-Jasqet-Key header). Browser extensions have their own gate (extauth.go)
// and the viewer its own key (viewer.go).
//
// How the window gets the key:
//   - Jasqet's own window (WebView2 on Windows) runs a script before the
//     page, and only on Jasqet's own address.
//   - A browser window (Mac, Linux, Windows without WebView2) is opened with
//     a code in its address that works once, for two minutes. The page swaps
//     it for the key and keeps the key for reloads, in that tab only. Other
//     accounts can't read the code from the browser's command line on
//     Windows or Mac; on Linux they can, so there the swap also checks the
//     request comes from your own account (peer_linux.go).
//
// Another copy of Jasqet (the installer closing it, the buttons on a
// notification) has no window, so it uses a second key from a file only
// your account can read. That key can only close Jasqet or pass on a
// jasqet:// link.

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const keyHeader = "X-Jasqet-Key"

type apiKey struct {
	window  string // the window's key
	command string // another copy of Jasqet: quit and jasqet:// links only
	mu      sync.Mutex
	codes   map[string]time.Time // one-time codes for browser windows, and when they stop working
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func newAPIKey() *apiKey {
	return &apiKey{window: randomHex(32), command: randomHex(32), codes: map[string]time.Time{}}
}

func sameKey(got, want string) bool {
	return got != "" && want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// openPaths: what needs no key. The page itself, hello (the extension and
// another copy of Jasqet check that Jasqet is there), swapping a code for
// the key, and bringing the window to the front (showWindow decides which
// page it may open).
var openPaths = map[string]bool{"/": true, "/api/ping": true, "/api/session": true, "/api/show": true}

// commandPaths: what another copy of Jasqet may do with the command key.
var commandPaths = map[string]bool{"/api/quit": true, "/api/protocol": true}

// keyOK: everything but a browser extension must carry the key.
func (s *Server) keyOK(w http.ResponseWriter, r *http.Request) bool {
	k, p := r.Header.Get(keyHeader), r.URL.Path
	switch {
	case sameKey(k, s.key.window),
		sameKey(k, s.key.command) && commandPaths[p],
		openPaths[p],
		p == "/api/tools" && r.Method == http.MethodGet,    // is the video helper ready?
		p == "/viewer" || strings.HasPrefix(p, "/viewer/"): // the viewer checks its own key
		return true
	}
	writeErrCode(w, http.StatusUnauthorized, "Only Jasqet's own window can do this.")
	return false
}

// fromApp: the request comes from Jasqet's own window (it has the key).
func (s *Server) fromApp(r *http.Request) bool {
	return s.key != nil && sameKey(r.Header.Get(keyHeader), s.key.window)
}

// windowScript: run by Jasqet's own window before the page. It hands over
// the key only on the app's own address and not inside frames (the viewer
// is a frame on the app's other address and has a key of its own).
func (k *apiKey) windowScript(appURL string) string {
	origin := appURL
	if i := strings.Index(appURL, "://"); i >= 0 {
		if j := strings.IndexByte(appURL[i+3:], '/'); j >= 0 {
			origin = appURL[:i+3+j]
		}
	}
	o, _ := json.Marshal(origin)
	key, _ := json.Marshal(k.window)
	return "if (location.origin === " + string(o) + " && window.top === window) window.jasqetKey = " + string(key) + ";"
}

// browserURL: page, with a code one browser window can swap for the key.
func (k *apiKey) browserURL(page string) string {
	code := randomHex(16)
	now := time.Now()
	k.mu.Lock()
	for c, until := range k.codes {
		if now.After(until) {
			delete(k.codes, c)
		}
	}
	k.codes[code] = now.Add(2 * time.Minute)
	k.mu.Unlock()
	base, frag, hasFrag := strings.Cut(page, "#")
	u := base + "?code=" + code
	if hasFrag {
		u += "#" + frag
	}
	return u
}

// redeem: a code works once, and only for two minutes.
func (k *apiKey) redeem(code string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	until, ok := k.codes[code]
	delete(k.codes, code)
	return ok && time.Now().Before(until)
}

// apiSession: a browser window swaps its code for the key.
func (s *Server) apiSession(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if !decodePost(w, r, &req) {
		return
	}
	if otherUser(r) || !s.key.redeem(req.Code) {
		writeErrCode(w, http.StatusForbidden, "This window's link has expired. Close it and open Jasqet again.")
		return
	}
	writeJSON(w, map[string]string{"key": s.key.window})
}

// ---------- another copy of Jasqet ----------

func commandKeyFile(dir, port string) string { return filepath.Join(dir, "command-"+port+".key") }

// saveCommandKey leaves the command key where another copy of Jasqet started
// by you can find it (your own settings folder; only you can read it).
func (k *apiKey) saveCommandKey(dir, port string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	p := commandKeyFile(dir, port)
	os.Remove(p + ".tmp")
	if err := os.WriteFile(p+".tmp", []byte(k.command), 0o600); err != nil {
		return err
	}
	return os.Rename(p+".tmp", p)
}

func readCommandKey(dir, port string) string {
	b, _ := os.ReadFile(commandKeyFile(dir, port))
	return strings.TrimSpace(string(b))
}

// postLocal: another copy of Jasqet asks the running one to do something.
func postLocal(c *http.Client, u, key, body string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, u, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set(keyHeader, key)
	}
	return c.Do(req)
}
