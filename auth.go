package main

// Site logins: Basic, Digest and NTLM (Windows / company servers).
// Credentials are only sent when a server asks for them.

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/Azure/go-ntlmssp"
)

type authTransport struct {
	base http.RoundTripper // plain connection
	ntlm http.RoundTripper // tries anonymous, then Basic, then NTLM
	mu   sync.Mutex
	dig  map[string]*digestState // per host: last Digest challenge
}

func newAuthTransport(base http.RoundTripper) *authTransport {
	return &authTransport{base: base, ntlm: ntlmssp.Negotiator{RoundTripper: base, AllowBasicAuth: true}, dig: map[string]*digestState{}}
}

func (a *authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	user, pass, ok := req.BasicAuth()
	if !ok {
		return a.base.RoundTrip(req)
	}
	a.mu.Lock()
	st := a.dig[req.URL.Host]
	a.mu.Unlock()
	if st != nil { // we already know this server wants Digest
		r2 := req.Clone(req.Context())
		r2.Header.Set("Authorization", st.header(user, pass, req.Method, req.URL.RequestURI()))
		resp, err := a.base.RoundTrip(r2)
		if err != nil || resp.StatusCode != http.StatusUnauthorized {
			return resp, err
		}
		drain(resp) // nonce went stale: ask again
	}
	resp, err := a.ntlm.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		return resp, err
	}
	ch := parseDigest(resp.Header.Values("WWW-Authenticate"))
	if ch == nil {
		return resp, nil
	}
	drain(resp)
	a.mu.Lock()
	a.dig[req.URL.Host] = ch
	a.mu.Unlock()
	r2 := req.Clone(req.Context())
	r2.Header.Set("Authorization", ch.header(user, pass, req.Method, req.URL.RequestURI()))
	return a.base.RoundTrip(r2)
}

func drain(r *http.Response) {
	io.Copy(io.Discard, io.LimitReader(r.Body, 64<<10))
	r.Body.Close()
}

type digestState struct {
	mu                              sync.Mutex
	realm, nonce, opaque, algo, qop string
	nc                              int
}

func parseDigest(values []string) *digestState {
	for _, v := range values {
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), "digest ") {
			continue
		}
		p := parseAuthParams(strings.TrimSpace(v)[7:])
		st := &digestState{realm: p["realm"], nonce: p["nonce"], opaque: p["opaque"], algo: p["algorithm"]}
		for _, q := range strings.Split(p["qop"], ",") {
			if strings.TrimSpace(q) == "auth" {
				st.qop = "auth"
			}
		}
		if st.nonce != "" {
			return st
		}
	}
	return nil
}

func parseAuthParams(s string) map[string]string {
	out := map[string]string{}
	for len(s) > 0 {
		s = strings.TrimLeft(s, " ,")
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			break
		}
		key := strings.ToLower(strings.TrimSpace(s[:eq]))
		s = s[eq+1:]
		var val string
		if strings.HasPrefix(s, `"`) {
			end := strings.IndexByte(s[1:], '"')
			if end < 0 {
				val, s = s[1:], ""
			} else {
				val, s = s[1:end+1], s[end+2:]
			}
		} else {
			end := strings.IndexByte(s, ',')
			if end < 0 {
				val, s = s, ""
			} else {
				val, s = s[:end], s[end:]
			}
		}
		out[key] = strings.TrimSpace(val)
	}
	return out
}

func (st *digestState) header(user, pass, method, uri string) string {
	st.mu.Lock()
	st.nc++
	nc := fmt.Sprintf("%08x", st.nc)
	st.mu.Unlock()
	var h func() hash.Hash = md5.New
	algo := strings.ToUpper(st.algo)
	if strings.HasPrefix(algo, "SHA-256") {
		h = sha256.New
	}
	H := func(s string) string { x := h(); x.Write([]byte(s)); return hex.EncodeToString(x.Sum(nil)) }
	cb := make([]byte, 8)
	rand.Read(cb)
	cnonce := hex.EncodeToString(cb)
	ha1 := H(user + ":" + st.realm + ":" + pass)
	if strings.HasSuffix(algo, "-SESS") {
		ha1 = H(ha1 + ":" + st.nonce + ":" + cnonce)
	}
	ha2 := H(method + ":" + uri)
	var resp string
	if st.qop == "auth" {
		resp = H(strings.Join([]string{ha1, st.nonce, nc, cnonce, "auth", ha2}, ":"))
	} else {
		resp = H(ha1 + ":" + st.nonce + ":" + ha2)
	}
	b := fmt.Sprintf(`Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s"`, user, st.realm, st.nonce, uri, resp)
	if st.algo != "" {
		b += `, algorithm=` + st.algo
	}
	if st.qop == "auth" {
		b += fmt.Sprintf(`, qop=auth, nc=%s, cnonce="%s"`, nc, cnonce)
	}
	if st.opaque != "" {
		b += fmt.Sprintf(`, opaque="%s"`, st.opaque)
	}
	return b
}
