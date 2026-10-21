package main

import (
	"net/http"
	"strings"
	"testing"
)

// The viewer opens light or dark like Jasqet's window: the window saves its
// choice, and the viewer's page comes with it before it draws.
func TestViewerFollowsTheWindowsMode(t *testing.T) {
	isolateConfig(t)
	s, h := newTestServer(t)
	app := "http://127.0.0.1:9614"
	set := func(body string) {
		if w := call(h, "POST", "/api/settings", app, body, keyHeader, s.key.window); w.Code != http.StatusOK {
			t.Fatalf("saving %s: %d %s", body, w.Code, w.Body)
		}
	}
	page := func() string {
		w := call(h, "GET", "/viewer?t="+s.hub.token, "", "")
		if w.Code != http.StatusOK {
			t.Fatalf("viewer page: %d %s", w.Code, w.Body)
		}
		return w.Body.String()
	}
	if !strings.Contains(page(), `<html lang="en">`) {
		t.Fatal("nothing chosen yet: the viewer should follow the system")
	}
	for _, mode := range []string{"light", "dark"} {
		set(`{"theme":"` + mode + `"}`)
		if !strings.Contains(page(), `<html lang="en" data-theme="`+mode+`">`) {
			t.Errorf("%s mode: the viewer's page doesn't say so", mode)
		}
	}
	// saving the other settings (they don't send the theme) keeps the mode
	set(`{"connections":8}`)
	if !strings.Contains(page(), `<html lang="en" data-theme="dark">`) {
		t.Error("saving other settings lost the mode")
	}
	// anything else means "like the system", and never reaches the page
	set(`{"theme":"\"><script>alert(1)</script>"}`)
	if b := page(); !strings.Contains(b, `<html lang="en">`) || strings.Contains(b, "alert(1)") {
		t.Error("an unknown theme reached the viewer's page")
	}
}
