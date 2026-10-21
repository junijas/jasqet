package main

// Tests for pairing (extauth.go): the extension checks it's talking to the
// real Jasqet before it sends cookies and links.

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

const (
	pairExt  = "chrome-extension://pairpairpairpairpairpairpairpair"
	chromeUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36"
	edgeUA   = chromeUA + " Edg/140.0.0.0"
	appO     = "http://127.0.0.1:9614"
)

// pairedServer: a test Jasqet where you clicked Allow for pairExt.
func pairedServer(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	isolateConfig(t)
	s, h := newTestServer(t)
	if w := call(h, "POST", "/api/extension", appO, `{"origin":"`+pairExt+`","allow":true}`, keyHeader, s.key.window); w.Code != 200 {
		t.Fatalf("allow: %d %s", w.Code, w.Body)
	}
	return s, h
}

type pairAnswer struct{ ID, Secret, Error string }

// pairUp: the extension asks to pair, as background.js does.
func pairUp(h http.Handler, origin, ua string) (int, pairAnswer) {
	w := call(h, "POST", "/api/pair", origin, "{}", "User-Agent", ua)
	var a pairAnswer
	json.Unmarshal(w.Body.Bytes(), &a)
	return w.Code, a
}

// hello: the extension's ping with a nonce and its pairing id. Returns the
// proof Jasqet gave (if any) and whether it says it can pair.
func hello(t *testing.T, h http.Handler, origin, id, nonce string) (string, bool) {
	t.Helper()
	w := call(h, "POST", "/api/ping", origin, `{"nonce":`+jsonQ(nonce)+`,"id":`+jsonQ(id)+`}`)
	if w.Code != 200 {
		t.Fatalf("ping: %d %s", w.Code, w.Body)
	}
	var r struct {
		Proof   string
		CanPair bool
	}
	json.Unmarshal(w.Body.Bytes(), &r)
	return r.Proof, r.CanPair
}

func TestPairingWorksOnce(t *testing.T) {
	_, h := pairedServer(t)
	code, a := pairUp(h, pairExt, chromeUA)
	if code != 200 || !isPairSecret(a.Secret) || a.ID == "" {
		t.Fatalf("first pairing: %d %+v", code, a)
	}
	nonce := randomHex(32)
	if proof, can := hello(t, h, pairExt, a.ID, nonce); !can || proof != pairProof(a.Secret, nonce) {
		t.Fatalf("proof %q (canPair %v), want %q", proof, can, pairProof(a.Secret, nonce))
	}
	// the same browser asking again (or a program pretending to be it) gets nothing
	code, b := pairUp(h, pairExt, chromeUA)
	if code != http.StatusForbidden || b.Secret != "" || b.Error != pairRefused {
		t.Fatalf("second pairing: %d %+v", code, b)
	}
	if proof, _ := hello(t, h, pairExt, a.ID, nonce); proof != pairProof(a.Secret, nonce) {
		t.Fatal("the refused pairing spoiled the first one")
	}
}

func TestPairAgain(t *testing.T) {
	s, h := pairedServer(t)
	_, first := pairUp(h, pairExt, chromeUA)
	if code, _ := pairUp(h, pairExt, chromeUA); code != http.StatusForbidden {
		t.Fatalf("second pairing before Pair again: %d", code)
	}
	// you click Pair again in the Connect window: two minutes
	if w := call(h, "POST", "/api/extension", appO, `{"pairAgain":true}`, keyHeader, s.key.window); w.Code != 200 {
		t.Fatalf("pair again: %d %s", w.Code, w.Body)
	}
	if left := time.Until(s.ext.pairAgain); left < pairWindow-5*time.Second || left > pairWindow {
		t.Fatalf("the window is %v long, want %v", left, pairWindow)
	}
	code, again := pairUp(h, pairExt, chromeUA)
	if code != 200 || !isPairSecret(again.Secret) || again.Secret == first.Secret || again.ID == first.ID {
		t.Fatalf("pairing after Pair again: %d %+v", code, again)
	}
	nonce := randomHex(32)
	if proof, _ := hello(t, h, pairExt, again.ID, nonce); proof != pairProof(again.Secret, nonce) {
		t.Fatal("the new pairing doesn't prove")
	}
	// one click, one more pairing for this browser
	if code, _ := pairUp(h, pairExt, chromeUA); code != http.StatusForbidden {
		t.Fatalf("a second pairing on the same click: %d", code)
	}
	// after two minutes the click no longer counts
	call(h, "POST", "/api/extension", appO, `{"pairAgain":true}`, keyHeader, s.key.window)
	s.ext.mu.Lock()
	s.ext.pairAgain = time.Now().Add(-time.Second)
	s.ext.mu.Unlock()
	if code, _ := pairUp(h, pairExt, chromeUA); code != http.StatusForbidden {
		t.Fatalf("pairing after the two minutes: %d", code)
	}
}

func TestPairProof(t *testing.T) {
	_, h := pairedServer(t)
	_, a := pairUp(h, pairExt, chromeUA)
	nonce := randomHex(32)
	want := pairProof(a.Secret, nonce)
	if len(want) != 64 {
		t.Fatalf("proof %q", want)
	}
	// the same sum background.js works out (checked with Node's crypto)
	if got := pairProof(strings.Repeat("ab", 32), "0123456789abcdef"); got != "de4d8bd44a8977da900b5674c35399c941b28170ff515d3e58c5f867b8f4eb8c" {
		t.Errorf("known answer: %s", got)
	}
	if pairProof(a.Secret, randomHex(32)) == want {
		t.Error("another nonce gave the same proof")
	}
	if pairProof(randomHex(32), nonce) == want {
		t.Error("another secret gave the same proof")
	}
	// without the right pairing there's no proof to give
	if proof, _ := hello(t, h, pairExt, "not-an-id", nonce); proof != "" {
		t.Errorf("made-up id got a proof: %s", proof)
	}
	if proof, can := hello(t, h, pairExt, "", nonce); proof != "" || !can {
		t.Errorf("no id: proof %q, canPair %v", proof, can)
	}
	if proof, _ := hello(t, h, pairExt, a.ID, "short"); proof != "" {
		t.Error("a short nonce got a proof")
	}
	if w := call(h, "GET", "/api/ping", pairExt, ""); strings.Contains(w.Body.String(), "proof") || !strings.Contains(w.Body.String(), `"canPair":true`) {
		t.Errorf("plain hello: %s", w.Body)
	}
}

func TestUnknownExtensionCantPair(t *testing.T) {
	_, h := pairedServer(t)
	for _, o := range []string{"chrome-extension://unknownunknownunknownunknownunkn", "moz-extension://1b2c3d4e-0000-4000-8000-000000000000"} {
		if code, a := pairUp(h, o, chromeUA); code != http.StatusForbidden || a.Secret != "" {
			t.Errorf("%s paired: %d %+v", o, code, a)
		}
		w := call(h, "POST", "/api/ping", o, `{"nonce":"`+randomHex(32)+`"}`)
		if w.Code != 200 || strings.Contains(w.Body.String(), "proof") || strings.Contains(w.Body.String(), "canPair") || !strings.Contains(w.Body.String(), `"allowed":false`) {
			t.Errorf("%s hello: %d %s", o, w.Code, w.Body)
		}
	}
}

func TestPairingSecretsAreSealed(t *testing.T) {
	s, h := pairedServer(t)
	_, a := pairUp(h, pairExt, chromeUA)
	b, err := os.ReadFile(s.pairFile())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), a.Secret) {
		t.Fatalf("the secret is in plain text: %s", b)
	}
	var saved map[string][]pairing
	if json.Unmarshal(b, &saved) != nil || len(saved[pairExt]) != 1 || !isSealed(saved[pairExt][0].Secret) || saved[pairExt][0].ID != a.ID {
		t.Fatalf("pairing file: %s", b)
	}
	// nor in the state file
	s.m.save()
	st, err := os.ReadFile(s.m.stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(st), a.Secret) {
		t.Fatal("the secret is in state.json")
	}
	// Jasqet started again reads it back and still proves itself
	s2 := &Server{m: s.m, port: "9614", quit: make(chan struct{}), hub: newViewerHub()}
	h2 := s2.routes()
	nonce := randomHex(32)
	if proof, _ := hello(t, h2, pairExt, a.ID, nonce); proof != pairProof(a.Secret, nonce) {
		t.Fatal("after a restart the pairing is gone")
	}
	if code, _ := pairUp(h2, pairExt, chromeUA); code != http.StatusForbidden {
		t.Fatalf("after a restart a second pairing came by itself: %d", code)
	}
}

// Extension 1.3.5 never pairs: an allowed one keeps working as before.
func TestOlderExtensionStillWorks(t *testing.T) {
	_, h := pairedServer(t)
	w := call(h, "GET", "/api/ping", pairExt, "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"allowed":true`) {
		t.Fatalf("ping: %d %s", w.Code, w.Body)
	}
	if w := call(h, "POST", "/api/add", pairExt, `{"url":"http://example.invalid/old.zip","later":true,"headers":{"Cookie":"a=b"}}`); w.Code != 200 {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	if w := call(h, "POST", "/api/show", pairExt, `{"page":"connect"}`); w.Code != 200 {
		t.Fatalf("show: %d %s", w.Code, w.Body)
	}
	if w := call(h, "GET", "/api/tools", pairExt, ""); w.Code != 200 {
		t.Fatalf("tools: %d %s", w.Code, w.Body)
	}
}

// Nothing but an allowed extension can pair, and nothing but Jasqet's window
// can click Pair again.
func TestPairingCantBeBypassed(t *testing.T) {
	s, h := pairedServer(t)
	if code, _ := pairUp(h, pairExt, chromeUA); code != 200 {
		t.Fatalf("first pairing: %d", code)
	}
	for _, c := range []struct {
		origin string
		key    string
		want   int
	}{
		{"", "", http.StatusUnauthorized},
		{appO, "", http.StatusUnauthorized},
		{"http://localhost:9614", "", http.StatusUnauthorized},
		{"", s.key.command, http.StatusUnauthorized},
		{"", s.key.window, http.StatusForbidden},
		{appO, s.key.window, http.StatusForbidden},
	} {
		var hdr []string
		if c.key != "" {
			hdr = []string{keyHeader, c.key}
		}
		w := call(h, "POST", "/api/pair", c.origin, "{}", hdr...)
		if w.Code != c.want || strings.Contains(w.Body.String(), "secret") {
			t.Errorf("pair from Origin %q with key %q: %d %s", c.origin, c.key, w.Code, w.Body)
		}
	}
	// Pair again from anything but the window
	for _, c := range []struct {
		origin string
		key    string
	}{{pairExt, ""}, {"", ""}, {appO, ""}, {"", s.key.command}} {
		var hdr []string
		if c.key != "" {
			hdr = []string{keyHeader, c.key}
		}
		if w := call(h, "POST", "/api/extension", c.origin, `{"pairAgain":true}`, hdr...); w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
			t.Errorf("Pair again from Origin %q with key %q: %d", c.origin, c.key, w.Code)
		}
	}
	if !s.ext.pairAgain.IsZero() {
		t.Fatal("Pair again was clicked without the window")
	}
	if code, _ := pairUp(h, pairExt, chromeUA); code != http.StatusForbidden {
		t.Fatalf("a second pairing got through: %d", code)
	}
}

// Chrome and Edge give the extension in Jasqet's folder the same ID: each
// pairs by itself, and neither spoils the other's pairing.
func TestTwoBrowsersOneOrigin(t *testing.T) {
	_, h := pairedServer(t)
	c1, chrome := pairUp(h, pairExt, chromeUA)
	c2, edge := pairUp(h, pairExt, edgeUA)
	if c1 != 200 || c2 != 200 || chrome.Secret == edge.Secret || chrome.ID == edge.ID {
		t.Fatalf("chrome %d %+v, edge %d %+v", c1, chrome, c2, edge)
	}
	nonce := randomHex(32)
	if p, _ := hello(t, h, pairExt, chrome.ID, nonce); p != pairProof(chrome.Secret, nonce) {
		t.Error("Chrome's pairing doesn't prove")
	}
	if p, _ := hello(t, h, pairExt, edge.ID, nonce); p != pairProof(edge.Secret, nonce) {
		t.Error("Edge's pairing doesn't prove")
	}
	if code, _ := pairUp(h, pairExt, edgeUA); code != http.StatusForbidden {
		t.Errorf("Edge paired twice: %d", code)
	}
}

func TestRemovedExtensionForgetsItsPairing(t *testing.T) {
	s, h := pairedServer(t)
	_, a := pairUp(h, pairExt, chromeUA)
	if w := call(h, "POST", "/api/extension", appO, `{"origin":"`+pairExt+`","allow":false}`, keyHeader, s.key.window); w.Code != 200 {
		t.Fatalf("remove: %d", w.Code)
	}
	if b, _ := os.ReadFile(s.pairFile()); strings.Contains(string(b), pairExt) {
		t.Fatalf("still in the pairing file: %s", b)
	}
	call(h, "POST", "/api/extension", appO, `{"origin":"`+pairExt+`","allow":true}`, keyHeader, s.key.window)
	if p, _ := hello(t, h, pairExt, a.ID, randomHex(32)); p != "" {
		t.Error("the old pairing still proves")
	}
	if code, _ := pairUp(h, pairExt, chromeUA); code != 200 {
		t.Errorf("allowed again, it can't pair: %d", code)
	}
}
