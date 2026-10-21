package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// pairAskOf: the "wants to pair again" question the window shows, from /api/state.
func pairAskOf(t *testing.T, s *Server, h http.Handler) *extAsk {
	t.Helper()
	w := call(h, "GET", "/api/state", appO, "", keyHeader, s.key.window)
	var st struct {
		PairAsk *extAsk `json:"pairAsk"`
	}
	if json.Unmarshal(w.Body.Bytes(), &st) != nil {
		t.Fatalf("state: %d %s", w.Code, w.Body)
	}
	return st.PairAsk
}

// The extension was removed and added back: it has no secret, and Jasqet
// already knows that browser, so it says no. Then Jasqet asks you in its
// window, and one click on Pair lets the browser pair by itself.
func TestJasqetAsksWhenABrowserWantsToPairAgain(t *testing.T) {
	s, h := pairedServer(t)
	if code, _ := pairUp(h, pairExt, edgeUA); code != http.StatusOK {
		t.Fatalf("first pairing: %d", code)
	}
	if a := pairAskOf(t, s, h); a != nil {
		t.Fatal("nothing to ask yet")
	}
	if code, _ := pairUp(h, pairExt, edgeUA); code != http.StatusForbidden {
		t.Fatalf("pairing again without your OK: %d", code)
	}
	a := pairAskOf(t, s, h)
	if a == nil || a.Browser != "Microsoft Edge" || a.Origin != pairExt {
		t.Fatalf("the window should ask about Edge: %+v", a)
	}
	// you click Pair: the extension's next try works and the question goes
	if w := call(h, "POST", "/api/extension", appO, `{"pairAgain":true}`, keyHeader, s.key.window); w.Code != 200 {
		t.Fatalf("pair: %d %s", w.Code, w.Body)
	}
	if a := pairAskOf(t, s, h); a != nil {
		t.Fatal("still asking after you clicked Pair")
	}
	code, p := pairUp(h, pairExt, edgeUA)
	if code != http.StatusOK || !isPairSecret(p.Secret) {
		t.Fatalf("pairing after you clicked Pair: %d", code)
	}
	nonce := randomHex(32)
	if proof, _ := hello(t, h, pairExt, p.ID, nonce); proof != pairProof(p.Secret, nonce) {
		t.Fatal("the new pairing doesn't prove")
	}
}

// Not now: no pairing, and no more asking about that browser until Jasqet
// restarts. Other programs can't answer the question for you.
func TestPairAgainNotNowAndOnlyTheWindowAnswers(t *testing.T) {
	s, h := pairedServer(t)
	pairUp(h, pairExt, edgeUA)
	pairUp(h, pairExt, edgeUA) // refused: Jasqet asks
	if pairAskOf(t, s, h) == nil {
		t.Fatal("Jasqet should ask")
	}
	// an extension or a program without the window's key can't click Pair
	if w := call(h, "POST", "/api/extension", pairExt, `{"pairAgain":true}`); w.Code == http.StatusOK {
		t.Fatal("an extension clicked Pair for you")
	}
	if w := call(h, "POST", "/api/extension", "", `{"pairAgain":true}`); w.Code == http.StatusOK {
		t.Fatal("a program without the key clicked Pair for you")
	}
	if code, _ := pairUp(h, pairExt, edgeUA); code != http.StatusForbidden {
		t.Fatal("pairing worked without your OK")
	}
	if w := call(h, "POST", "/api/extension", appO, `{"pairNo":true}`, keyHeader, s.key.window); w.Code != 200 {
		t.Fatalf("not now: %d %s", w.Code, w.Body)
	}
	if pairAskOf(t, s, h) != nil {
		t.Fatal("still asking after Not now")
	}
	pairUp(h, pairExt, edgeUA)
	if pairAskOf(t, s, h) != nil {
		t.Fatal("asked again after Not now")
	}
}

// The question goes after two minutes (the extension stops trying then).
func TestPairAgainQuestionGoesAfterTwoMinutes(t *testing.T) {
	s, h := pairedServer(t)
	pairUp(h, pairExt, edgeUA)
	pairUp(h, pairExt, edgeUA)
	s.ext.mu.Lock()
	s.ext.pairAsk.at = time.Now().Add(-pairWindow - time.Second)
	s.ext.mu.Unlock()
	if pairAskOf(t, s, h) != nil {
		t.Fatal("an old question is still shown")
	}
}
