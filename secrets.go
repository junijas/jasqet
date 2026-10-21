package main

// Secrets in the state file: site cookies, logins, the Telegram bot token,
// the VirusTotal key and a proxy password are saved encrypted (on Windows
// only your user account can read them back, like the YouTube session), never
// in plain text. Lists saved by older versions still load. The browser
// extensions' pairing secrets are sealed the same way, in their own file.

import (
	"encoding/json"
	"maps"
	"net/url"
	"strings"
)

// sealSecret encrypts s for the state file. If Windows can't encrypt it
// right now, it stays as it is rather than being lost.
func sealSecret(s string) string {
	if s == "" || isSealed(s) {
		return s
	}
	if p := protect(s); p != "" {
		return p
	}
	return s
}

// openSecret reads a sealed secret back (older lists have them in plain text).
func openSecret(s string) string {
	if isSealed(s) {
		return unprotect(s)
	}
	return s
}

func isSealed(s string) bool { return strings.HasPrefix(s, "dpapi:") || strings.HasPrefix(s, "b64:") }

type downloadSecrets struct {
	Cookies  map[string]string `json:"c,omitempty"`
	Password string            `json:"p,omitempty"`
	Cookie   string            `json:"hc,omitempty"` // the Cookie header
	Auth     string            `json:"ha,omitempty"` // the Authorization header
}

// hideSecretsLocked swaps the download's cookies and login for an encrypted
// copy while it's written to the state file; restore puts them back.
func (d *Download) hideSecretsLocked() (restore func()) {
	cookie, auth := d.Headers["Cookie"], d.Headers["Authorization"]
	if len(d.Cookies) == 0 && d.Password == "" && cookie == "" && auth == "" {
		return func() {}
	}
	b, _ := json.Marshal(downloadSecrets{d.Cookies, d.Password, cookie, auth})
	if string(b) != d.secretsPlain { // encrypting costs a little: only when they changed
		d.secretsPlain, d.secretsSealed = string(b), sealSecret(string(b))
	}
	if !isSealed(d.secretsSealed) {
		return func() {}
	}
	cookies, pass, headers := d.Cookies, d.Password, d.Headers
	h := maps.Clone(d.Headers)
	delete(h, "Cookie")
	delete(h, "Authorization")
	d.Cookies, d.Password, d.Headers, d.Secrets = nil, "", h, d.secretsSealed
	return func() { d.Cookies, d.Password, d.Headers, d.Secrets = cookies, pass, headers, "" }
}

// revealSecrets: after loading, put the decrypted cookies and login back.
func (d *Download) revealSecrets() {
	if d.Secrets == "" {
		return
	}
	var s downloadSecrets
	if json.Unmarshal([]byte(openSecret(d.Secrets)), &s) == nil {
		if len(s.Cookies) > 0 {
			d.Cookies = s.Cookies
		}
		if s.Password != "" {
			d.Password = s.Password
		}
		if s.Cookie != "" || s.Auth != "" {
			if d.Headers == nil {
				d.Headers = map[string]string{}
			}
			if s.Cookie != "" {
				d.Headers["Cookie"] = s.Cookie
			}
			if s.Auth != "" {
				d.Headers["Authorization"] = s.Auth
			}
		}
	}
	d.Secrets = ""
}

// sealSettings / openSettings: the keys and passwords among the settings,
// and the LAN group code.
func sealSettings(s Settings) Settings {
	s.Notify.TelegramToken = sealSecret(s.Notify.TelegramToken)
	s.Safety.VirusTotalKey = sealSecret(s.Safety.VirusTotalKey)
	if u, err := url.Parse(s.Proxy); err == nil && u.User != nil {
		s.Proxy = sealSecret(s.Proxy)
	}
	s.LAN.Code = sealSecret(s.LAN.Code)
	return s
}

func openSettings(s Settings) Settings {
	s.Notify.TelegramToken = openSecret(s.Notify.TelegramToken)
	s.Safety.VirusTotalKey = openSecret(s.Safety.VirusTotalKey)
	s.Proxy = openSecret(s.Proxy)
	s.LAN.Code = openSecret(s.LAN.Code)
	return s
}

// sealPairs / openPairs: the browser extensions' pairing secrets (extauth.go),
// kept in a file of their own next to the state file. A secret that can't be
// read back is dropped: that browser pairs again.
func sealPairs(all map[string][]pairing) map[string][]pairing {
	out := map[string][]pairing{}
	for origin, list := range all {
		for _, p := range list {
			p.Secret = sealSecret(p.Secret)
			out[origin] = append(out[origin], p)
		}
	}
	return out
}

func openPairs(all map[string][]pairing) map[string][]pairing {
	out := map[string][]pairing{}
	for origin, list := range all {
		for _, p := range list {
			if p.Secret = openSecret(p.Secret); isPairSecret(p.Secret) && p.ID != "" {
				out[origin] = append(out[origin], p)
			}
		}
	}
	return out
}

// ---------- what the window sees ----------
// The window never gets the keys and passwords back, only whether each one
// is set; it shows a saved one as dots (savedMask), and saving the dots
// keeps it as it is.

const savedMask = "••••••••"

type savedSecrets struct {
	TelegramToken bool `json:"telegramToken"`
	VirusTotalKey bool `json:"virusTotalKey"`
	ProxyPassword bool `json:"proxyPassword"`
	LANCode       bool `json:"lanCode"`
}

// forWindow: the settings without their secrets, and which ones are set.
// The proxy keeps its address, with dots for the password.
func forWindow(s Settings) (Settings, savedSecrets) {
	v := savedSecrets{TelegramToken: s.Notify.TelegramToken != "", VirusTotalKey: s.Safety.VirusTotalKey != "", LANCode: s.LAN.Code != ""}
	s.Notify.TelegramToken, s.Safety.VirusTotalKey, s.LAN.Code = "", "", ""
	s.Proxy, v.ProxyPassword = maskProxy(s.Proxy)
	return s, v
}

// keepSaved: settings from the window, with the dots put back to what they
// stand for in old.
func keepSaved(s, old Settings) Settings {
	if s.Notify.TelegramToken == savedMask {
		s.Notify.TelegramToken = old.Notify.TelegramToken
	}
	if s.Safety.VirusTotalKey == savedMask {
		s.Safety.VirusTotalKey = old.Safety.VirusTotalKey
	}
	if s.LAN.Code == savedMask {
		s.LAN.Code = old.LAN.Code
	}
	s.Proxy = unmaskProxy(s.Proxy, old.Proxy)
	return s
}

func proxyURL(p string) (*url.URL, error) {
	if !strings.Contains(p, "://") {
		p = "http://" + p // as the downloads read it (manager.go)
	}
	return url.Parse(p)
}

// maskProxy: the proxy with dots for its password, and whether it has one.
func maskProxy(p string) (string, bool) {
	if !strings.Contains(p, "@") {
		return p, false
	}
	u, err := proxyURL(p)
	if err != nil { // can't be read: hide everything before the host
		at := strings.LastIndex(p, "@")
		start := strings.Index(p, "://") + 3
		if start < 3 || start > at {
			start = 0
		}
		return p[:start] + savedMask + p[at:], true
	}
	if u.User == nil {
		return p, false
	}
	if _, ok := u.User.Password(); !ok {
		return p, false
	}
	name := u.User.Username()
	u.User = nil
	rest := strings.TrimPrefix(u.String(), u.Scheme+"://")
	return u.Scheme + "://" + url.User(name).String() + ":" + savedMask + "@" + rest, true
}

// unmaskProxy: the proxy the window sent, with the saved password in place
// of the dots (the address around them may have changed).
func unmaskProxy(p, old string) string {
	if !strings.Contains(p, savedMask) {
		return p
	}
	if m, _ := maskProxy(old); p == m {
		return old
	}
	pass := ""
	if u, err := proxyURL(old); err == nil && u.User != nil {
		pass, _ = u.User.Password()
	}
	if pass == "" { // nothing saved to put back
		return strings.ReplaceAll(strings.ReplaceAll(p, ":"+savedMask, ""), savedMask, "")
	}
	return strings.Replace(p, ":"+savedMask+"@", url.UserPassword("", pass).String()+"@", 1)
}
