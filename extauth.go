package main

// Which browser extensions may talk to Jasqet.
//
// Every extension in every browser can reach 127.0.0.1, so Jasqet only
// listens to its own: the extension folder next to Jasqet.exe is known by
// the ID browsers give it, and any other (Firefox, or a copy loaded from
// another folder) is let in once you click Allow in the Jasqet window. Even
// then an extension can only hand over downloads and videos. Settings,
// opening files, deleting and the rest belong to Jasqet's own window.
//
// The other way round, the extension checks it's talking to the real Jasqet
// before it sends cookies or links (pairing, at the end of this file).

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

// extensionPaths: everything Jasqet's extension uses.
var extensionPaths = map[string]bool{
	"/api/ping": true, "/api/show": true, "/api/pair": true, "/api/add": true, "/api/sizes": true,
	"/api/media/probe": true, "/api/media/add": true, "/api/yt/probe": true, "/api/yt/add": true,
	"/api/tools": true, "/api/tools/install": true,
}

// storeExtensionIDs: Jasqet's extension in the Chrome Web Store and Edge
// Add-ons, once it's published there (the installer's CHROME_STORE_ID and
// EDGE_STORE_ID).
var storeExtensionIDs = []string{}

func isExtensionOrigin(origin string) bool {
	return strings.HasPrefix(origin, "chrome-extension://") || strings.HasPrefix(origin, "moz-extension://") || strings.HasPrefix(origin, "extension://")
}

// unpackedExtensionID is the ID Chrome, Edge, Brave, Opera and Vivaldi give an
// extension loaded with "Load unpacked" from dir: a hash of the folder's path.
func unpackedExtensionID(dir string) string {
	b := []byte(dir)
	if runtime.GOOS == "windows" {
		if len(dir) >= 2 && dir[1] == ':' && dir[0] >= 'a' && dir[0] <= 'z' {
			dir = strings.ToUpper(dir[:1]) + dir[1:] // browsers write the drive letter in capitals
		}
		u := utf16.Encode([]rune(dir))
		b = make([]byte, 2*len(u))
		for i, c := range u {
			binary.LittleEndian.PutUint16(b[2*i:], c)
		}
	}
	sum := sha256.Sum256(b)
	id := make([]byte, 32)
	for i, c := range sum[:16] {
		id[2*i], id[2*i+1] = 'a'+c>>4, 'a'+c&15
	}
	return string(id)
}

// ownExtensionID: the extension folder next to Jasqet.exe, loaded unpacked.
var ownExtensionID = sync.OnceValue(func() string {
	if dir := extensionDir(false); dir != "" {
		return unpackedExtensionID(dir)
	}
	return ""
})

// extAsk: an extension Jasqet doesn't know yet asked to send downloads.
type extAsk struct {
	Origin  string `json:"origin"`
	Browser string `json:"browser"`
	at      time.Time
}

type extGate struct {
	mu     sync.Mutex
	ask    *extAsk
	notNow map[string]bool // "Not now", until Jasqet restarts

	pairs     map[string][]pairing // origin -> its pairings, read from pairFile once
	pairsRead bool
	pairAgain time.Time       // "Pair again" was clicked: until then each browser may pair once more
	repaired  map[string]bool // origin and browser that already did, since that click
	pairAsk   *extAsk         // a browser was refused a pairing: the window asks you
	pairNo    map[string]bool // "Not now" to that (origin and browser), until Jasqet restarts
}

// extAllowed: this extension may hand over downloads.
func (s *Server) extAllowed(origin string) bool {
	if id, ok := strings.CutPrefix(origin, "chrome-extension://"); ok && id != "" {
		if id == ownExtensionID() || slices.Contains(storeExtensionIDs, id) {
			return true
		}
	}
	s.m.mu.Lock()
	defer s.m.mu.Unlock()
	return slices.Contains(s.m.settings.Extensions, origin)
}

// extensionOK: what an extension may do. Unknown extensions only get to say
// hello (so Jasqet can ask you about them) and bring the window forward.
func (s *Server) extensionOK(w http.ResponseWriter, r *http.Request, origin string) bool {
	if !extensionPaths[r.URL.Path] {
		writeErrCode(w, http.StatusForbidden, "Only the Jasqet window can do this.")
		return false
	}
	ua := r.Header.Get("User-Agent")
	if s.extAllowed(origin) {
		noteExtension(origin, ua)
		return true
	}
	s.extAsks(origin, browserName(origin, ua))
	if r.URL.Path == "/api/ping" || r.URL.Path == "/api/show" {
		return true
	}
	writeErrCode(w, http.StatusForbidden, "Jasqet hasn't allowed this browser yet. Click Allow in the Jasqet window.")
	return false
}

func (s *Server) extAsks(origin, browser string) {
	g := &s.ext
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.notNow[origin] {
		g.ask = &extAsk{Origin: origin, Browser: browser, at: time.Now()}
	}
}

// extQuestion: the extension waiting for your Allow, if any.
func (s *Server) extQuestion() *extAsk {
	g := &s.ext
	g.mu.Lock()
	a := g.ask
	g.mu.Unlock()
	if a == nil || time.Since(a.at) > 10*time.Minute || s.extAllowed(a.Origin) {
		return nil
	}
	return a
}

// apiExtension: you clicked Allow (or Not now) for an extension, removed one,
// or clicked Pair again.
func (s *Server) apiExtension(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Origin    string `json:"origin"`
		Allow     bool   `json:"allow"`
		PairAgain bool   `json:"pairAgain"`
		PairNo    bool   `json:"pairNo"` // "Not now" to a browser that wants to pair again
	}
	if !decodePost(w, r, &req) {
		return
	}
	if req.PairAgain {
		s.openPairAgain()
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	if req.PairNo {
		g := &s.ext
		g.mu.Lock()
		if a := g.pairAsk; a != nil {
			if g.pairNo == nil {
				g.pairNo = map[string]bool{}
			}
			g.pairNo[a.Origin+" "+a.Browser] = true
			g.pairAsk = nil
		}
		g.mu.Unlock()
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	if !isExtensionOrigin(req.Origin) {
		writeErr(w, "that isn't a browser extension")
		return
	}
	s.m.mu.Lock()
	list := slices.DeleteFunc(slices.Clone(s.m.settings.Extensions), func(o string) bool { return o == req.Origin })
	if req.Allow {
		list = append(list, req.Origin)
	}
	s.m.settings.Extensions = list
	s.m.dirty = true
	s.m.mu.Unlock()
	g := &s.ext
	g.mu.Lock()
	if g.ask != nil && g.ask.Origin == req.Origin {
		g.ask = nil
	}
	if !req.Allow {
		if g.notNow == nil {
			g.notNow = map[string]bool{}
		}
		g.notNow[req.Origin] = true
	} else {
		delete(g.notNow, req.Origin)
	}
	g.mu.Unlock()
	if !req.Allow {
		s.forgetPairings(req.Origin) // a removed extension keeps no pairing
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// ---------- pairing: the extension checks it's talking to Jasqet ----------
//
// Any program can listen on 127.0.0.1:9614 while Jasqet isn't running, and
// the extension would hand it your cookies and links. So the first time the
// extension says hello, Jasqet gives it a secret. After that, before it sends
// anything, the extension sends a random word (a nonce) with its hello, and
// Jasqet answers with a proof made from the word and the secret (pairProof).
// A program without the secret can't make it, so it gets nothing.
//
// Each pairing has a secret of its own, so no one can get the secret of an
// extension that's already paired. Chrome, Edge and Brave give the extension
// in Jasqet's folder the same ID, so one origin can have several pairings.
// Each browser gets its first one by itself; another needs "Pair again" in
// the Connect window (pairWindow), so a program can't pile up pairings.

// pairing: one browser's pairing with Jasqet.
type pairing struct {
	ID      string `json:"id"`      // the extension sends it with its nonce
	Secret  string `json:"secret"`  // 32 random bytes in hex; sealed in the file
	Browser string `json:"browser"` // browserName when it paired
}

const (
	pairWindow  = 2 * time.Minute
	pairsKept   = 8 // per origin, the newest
	pairRefused = "Jasqet needs your OK to pair this browser. Click Pair in the Jasqet window, and the browser pairs by itself."
)

// pairProof: Jasqet's answer to a nonce, hex HMAC-SHA256(secret, "jasqet-pair:" + nonce).
// The extension works out the same and compares (background.js proofOK).
func pairProof(secret, nonce string) string {
	key, _ := hex.DecodeString(secret)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("jasqet-pair:" + nonce))
	return hex.EncodeToString(mac.Sum(nil))
}

func isPairSecret(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32
}

// pairFile: next to the state file. The window never sees it, and saving
// settings can't change it.
func (s *Server) pairFile() string { return filepath.Join(filepath.Dir(s.m.stateFile), "pairing.json") }

// pairingsLocked: every origin's pairings, read from the file the first time.
func (s *Server) pairingsLocked() map[string][]pairing {
	g := &s.ext
	if !g.pairsRead {
		g.pairsRead = true
		var p map[string][]pairing
		if b, err := os.ReadFile(s.pairFile()); err == nil && json.Unmarshal(b, &p) == nil {
			g.pairs = openPairs(p)
		}
	}
	return g.pairs
}

// savePairingsLocked writes them, sealed, and only then keeps them.
func (s *Server) savePairingsLocked(p map[string][]pairing) error {
	b, err := json.Marshal(sealPairs(p))
	if err != nil {
		return err
	}
	f := s.pairFile()
	if err := os.MkdirAll(filepath.Dir(f), 0o700); err != nil {
		return err
	}
	if err := writeFileSynced(f+".tmp", b); err != nil {
		return err
	}
	if err := os.Rename(f+".tmp", f); err != nil {
		return err
	}
	s.ext.pairs = p
	return nil
}

// pairSecret: the secret of the extension's pairing id, or "".
func (s *Server) pairSecret(origin, id string) string {
	g := &s.ext
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range s.pairingsLocked()[origin] {
		if id != "" && p.ID == id {
			return p.Secret
		}
	}
	return ""
}

// pingProof: the pairing part of an extension's hello. canPair tells a new
// extension this Jasqet can prove itself; proof is that proof.
func (s *Server) pingProof(r *http.Request, origin string, res map[string]any) {
	res["canPair"] = true
	var req struct {
		Nonce string `json:"nonce"`
		ID    string `json:"id"`
	}
	if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&req) != nil || len(req.Nonce) < 16 || len(req.Nonce) > 256 {
		return
	}
	if secret := s.pairSecret(origin, req.ID); secret != "" {
		res["proof"] = pairProof(secret, req.Nonce)
	}
}

// apiPair: an extension asks for a pairing. The first one for each browser
// comes by itself; after that only in the two minutes after Pair again.
func (s *Server) apiPair(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if !isExtensionOrigin(origin) || !s.extAllowed(origin) {
		writeErrCode(w, http.StatusForbidden, "Only Jasqet's browser extension can pair with Jasqet.")
		return
	}
	if r.Method != http.MethodPost {
		writeErr(w, "POST only")
		return
	}
	browser := browserName(origin, r.Header.Get("User-Agent"))
	g := &s.ext
	g.mu.Lock()
	defer g.mu.Unlock()
	all := s.pairingsLocked()
	mine := all[origin]
	first := !slices.ContainsFunc(mine, func(p pairing) bool { return p.Browser == browser })
	again := time.Now().Before(g.pairAgain) && !g.repaired[origin+" "+browser]
	if !first && !again {
		// most often the extension was removed and added back: ask in the window
		if !g.pairNo[origin+" "+browser] {
			g.pairAsk = &extAsk{Origin: origin, Browser: browser, at: time.Now()}
		}
		writeErrCode(w, http.StatusForbidden, pairRefused)
		return
	}
	p := pairing{ID: randomHex(8), Secret: randomHex(32), Browser: browser}
	mine = append(slices.Clone(mine), p)
	if len(mine) > pairsKept {
		mine = mine[len(mine)-pairsKept:]
	}
	next := maps.Clone(all)
	if next == nil {
		next = map[string][]pairing{}
	}
	next[origin] = mine
	if err := s.savePairingsLocked(next); err != nil {
		writeErrCode(w, http.StatusInternalServerError, "Jasqet couldn't save the pairing: "+friendlyError(err))
		return
	}
	if g.repaired == nil {
		g.repaired = map[string]bool{}
	}
	g.repaired[origin+" "+browser] = true
	if a := g.pairAsk; a != nil && a.Origin == origin && a.Browser == browser {
		g.pairAsk = nil
	}
	writeJSON(w, map[string]string{"id": p.ID, "secret": p.Secret})
}

// openPairAgain: you clicked Pair again. For two minutes each browser's
// extension may pair once more.
func (s *Server) openPairAgain() {
	g := &s.ext
	g.mu.Lock()
	g.pairAgain = time.Now().Add(pairWindow)
	g.repaired = nil
	g.pairAsk = nil
	g.mu.Unlock()
}

// pairQuestion: the browser waiting for your OK to pair again, if any. The
// extension keeps trying for as long as Pair again would count.
func (s *Server) pairQuestion() *extAsk {
	g := &s.ext
	g.mu.Lock()
	defer g.mu.Unlock()
	if a := g.pairAsk; a != nil && time.Since(a.at) < pairWindow {
		return a
	}
	return nil
}

// forgetPairings: an extension you removed.
func (s *Server) forgetPairings(origin string) {
	g := &s.ext
	g.mu.Lock()
	defer g.mu.Unlock()
	all := s.pairingsLocked()
	if _, ok := all[origin]; !ok {
		return
	}
	next := maps.Clone(all)
	delete(next, origin)
	s.savePairingsLocked(next)
}
