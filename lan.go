package main

// LAN sharing: if another Jasqet on the same network (office, hostel, home)
// already has a file, copy it from there instead of using internet data.
// Off by default. Only files Jasqet itself downloaded are shared, only to
// computers on the local network, and only between computers with the same
// group code: without one nothing is shared or asked for. Files that may be
// only yours (fetched with a login, cookies or a link with ?token=…) are
// never shared. A copy is only used after pieces of it, taken at random
// places, match the internet original, and the whole file its checksum when
// one is known.

import (
	"context"
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

type LANSettings struct {
	Enabled bool     `json:"enabled"`
	Share   bool     `json:"share"` // let others copy my finished downloads
	Use     bool     `json:"use"`   // copy from others when they have it
	Code    string   `json:"code"`  // group code, needed to share or use
	Peers   []string `json:"peers"` // extra addresses, e.g. 192.168.1.20 (if discovery is blocked)
	Port    int      `json:"port"`
}

const (
	lanDefaultPort = 9615
	lanDiscoPort   = 9616
	lanMaxPeers    = 32 // computers asked at most (the ones heard from most lately)
)

// lanAskTime: how long asking the other computers may take, all together.
var lanAskTime = 4 * time.Second

type lanPeer struct {
	Addr string    `json:"addr"` // host:port
	Name string    `json:"name"`
	Seen time.Time `json:"seen"`
}

type lanState struct {
	mu      sync.Mutex
	peers   map[string]*lanPeer
	checks  map[*Download]*lanCheck // copies from the network still to be checked
	running bool
	srv     *http.Server
	conn    *net.UDPConn
	served  int64
	saved   int64
}

// ---------- the group code ----------

// lanCodeMin: letters and digits a group code needs at least. A code made by
// Jasqet has 16 (80 bits); one typed in needs 12, so it can't be guessed.
const lanCodeMin = 12

// lanCodeChars: what codes Jasqet makes are written with. 32 letters and
// digits, without 0, 1, I and O, which are easy to mix up.
const lanCodeChars = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

// newLANCode makes a random group code: 16 characters (80 bits), shown in
// groups of 4, e.g. "KQ7M 4TZP 9XWD 2HRC".
func newLANCode() string {
	b := make([]byte, 16)
	rand.Read(b)
	for i := range b {
		b[i] = lanCodeChars[b[i]&31] // 256 is a multiple of 32: every character is as likely
	}
	return lanCodeShown(string(b))
}

// lanCode: the group code as computers compare it: in capitals, without
// spaces and hyphens. "" if it's too short or has other signs in it: then
// nothing is shared or asked for.
func lanCode(code string) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.ToUpper(code) {
		switch {
		case unicode.IsSpace(r) || r == '-':
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			n++
		default:
			return ""
		}
	}
	if n < lanCodeMin {
		return ""
	}
	return b.String()
}

// lanCodeShown: a code in groups of 4, easier to read out and type.
func lanCodeShown(code string) string {
	r := []rune(code)
	var parts []string
	for len(r) > 4 {
		parts = append(parts, string(r[:4]))
		r = r[4:]
	}
	return strings.Join(append(parts, string(r)), " ")
}

// lanKeys: what a group code turns into.
type lanKeys struct {
	tag  string // the hello broadcast
	sign []byte // signing requests
	find []byte // the hashes of links and names in find requests
}

var lanKeyCache = struct {
	sync.Mutex
	keys map[string]*lanKeys
}{keys: map[string]*lanKeys{}}

// lanKeysFor works out the keys of a group code. It takes many rounds of
// hashing on purpose, so someone who records the network can't try millions
// of codes a second. Done once per code.
func lanKeysFor(code string) *lanKeys {
	c := &lanKeyCache
	c.Lock()
	defer c.Unlock()
	if k := c.keys[code]; k != nil {
		return k
	}
	base, _ := pbkdf2.Key(sha256.New, code, []byte("jasqet-lan-group-code"), 200000, 32)
	sub := func(what string) []byte {
		mac := hmac.New(sha256.New, base)
		mac.Write([]byte(what))
		return mac.Sum(nil)
	}
	k := &lanKeys{tag: hex.EncodeToString(sub("tag")[:8]), sign: sub("sign"), find: sub("find")}
	if len(c.keys) > 16 { // codes hardly ever change
		clear(c.keys)
	}
	c.keys[code] = k
	return k
}

// lanTag: what the "hello" broadcast says about the group code. It's enough for
// computers of one group to find each other, and useless for asking for files.
func lanTag(code string) string { return lanKeysFor(code).tag }

// ---------- signed requests ----------

const lanSignFor = 5 * time.Minute // computers' clocks differ a little

// lanSign proves knowing the group code for one request, so the code itself
// never travels over the network. It's good for a few minutes, and only once:
// it carries a random nonce, and lanSigned refuses one it has seen.
func lanSign(code, method, uri string, at time.Time) string {
	ts := strconv.FormatInt(at.Unix(), 10)
	nonce := randomHex(12)
	return ts + ":" + nonce + ":" + lanMAC(code, ts, nonce, method, uri)
}

func lanMAC(code, ts, nonce, method, uri string) string {
	mac := hmac.New(sha256.New, lanKeysFor(code).sign)
	mac.Write([]byte(ts + "\n" + nonce + "\n" + method + "\n" + uri))
	return hex.EncodeToString(mac.Sum(nil))
}

func lanSignReq(req *http.Request, code string) {
	req.Header.Set("X-Jasqet-Auth", lanSign(code, req.Method, req.URL.RequestURI(), time.Now()))
}

func lanSigned(r *http.Request, code string) bool {
	if code == "" { // anyone could sign with no code
		return false
	}
	p := strings.Split(r.Header.Get("X-Jasqet-Auth"), ":")
	if len(p) != 3 || len(p[1]) < 16 || len(p[1]) > 64 {
		return false
	}
	sec, err := strconv.ParseInt(p[0], 10, 64)
	if err != nil {
		return false
	}
	at := time.Unix(sec, 0)
	if age := time.Since(at); age > lanSignFor || age < -lanSignFor {
		return false
	}
	if !hmac.Equal([]byte(p[2]), []byte(lanMAC(code, p[0], p[1], r.Method, r.URL.RequestURI()))) {
		return false
	}
	return lanFirstUse(p[1], at.Add(lanSignFor+time.Second))
}

// lanNonces: the nonces of signed requests whose time isn't up yet, so the
// same request can't be sent again by someone who saw it go by.
var lanNonces = struct {
	sync.Mutex
	seen    map[string]time.Time // nonce, and when its signature runs out
	cleaned time.Time
}{seen: map[string]time.Time{}}

const lanMaxNonces = 100000

func lanFirstUse(nonce string, until time.Time) bool {
	n := &lanNonces
	n.Lock()
	defer n.Unlock()
	if _, ok := n.seen[nonce]; ok {
		return false
	}
	if now := time.Now(); len(n.seen) >= 1000 && now.Sub(n.cleaned) > 10*time.Second {
		n.cleaned = now
		for k, t := range n.seen {
			if now.After(t) {
				delete(n.seen, k)
			}
		}
	}
	if len(n.seen) >= lanMaxNonces { // flooded: refuse rather than forget one too soon
		return false
	}
	n.seen[nonce] = until
	return true
}

// ---------- serving other computers ----------

// lanSettings: the LAN settings, with the group code as computers compare
// it ("" if there's none, or it's too short).
func (m *Manager) lanSettings() LANSettings {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.settings.LAN
	if s.Port == 0 {
		s.Port = lanDefaultPort
	}
	s.Code = lanCode(s.Code)
	return s
}

// startLAN (re)starts sharing according to the settings.
func (m *Manager) startLAN() {
	s := m.lanSettings()
	ls := m.lan
	ls.mu.Lock()
	if ls.srv != nil {
		ls.srv.Close()
		ls.srv = nil
	}
	if ls.conn != nil {
		ls.conn.Close()
		ls.conn = nil
	}
	ls.running = s.Enabled
	ls.mu.Unlock()
	if !s.Enabled {
		return
	}
	srv := &http.Server{Addr: fmt.Sprintf(":%d", s.Port), Handler: m.lanHandler(), ReadHeaderTimeout: 10 * time.Second}
	ls.mu.Lock()
	ls.srv = srv
	ls.mu.Unlock()
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Println("LAN sharing couldn't start:", err)
		}
	}()
	if s.Code != "" { // without a group code there's no one to find
		go m.lanDiscovery(s)
	}
}

// lanHandler: what other computers on the network can ask for.
func (m *Manager) lanHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/lan/find", m.lanFindHandler)
	mux.HandleFunc("/lan/file/", m.lanFileHandler)
	mux.HandleFunc("/lan/hello", func(w http.ResponseWriter, r *http.Request) {
		host, _ := os.Hostname()
		writeJSON(w, map[string]any{"app": "Jasqet", "name": host})
	})
	return m.lanGuard(mux)
}

// lanGuard: only local-network computers with the right code. Without a
// code only hello is answered (anyone could sign with an empty code).
func (m *Manager) lanGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		ip := net.ParseIP(host)
		if ip == nil || !(ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast()) {
			http.Error(w, "local network only", http.StatusForbidden)
			return
		}
		s := m.lanSettings()
		hello := r.URL.Path == "/lan/hello"
		switch {
		case s.Code == "" && !hello:
			http.Error(w, "no group code", http.StatusForbidden)
		case !s.Share && !hello:
			http.Error(w, "sharing is off", http.StatusForbidden)
		case s.Code != "" && !lanSigned(r, s.Code):
			http.Error(w, "wrong group code", http.StatusForbidden)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

type lanMatch struct {
	ID   string `json:"id"`
	Size int64  `json:"size"`
}

// lanPrivate: a download that may be only yours to see, so it's never
// offered or given to another computer: fetched with your login, cookies
// or a login header, or from a link with a login or a query string in it
// (?token=… is often the key to the file). The caller holds d.mu.
func lanPrivate(d *Download) bool {
	if d.User != "" || d.Password != "" || len(d.Cookies) > 0 {
		return true
	}
	for k, v := range d.Headers {
		if v != "" && (strings.EqualFold(k, "Cookie") || strings.EqualFold(k, "Authorization")) {
			return true
		}
	}
	u, err := url.Parse(d.URL)
	return err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery
}

// lanHash: a link or name as find requests carry it, a keyed hash, so
// only computers with your group code can match it with a file they have.
func lanHash(code, what, value string) string {
	if value == "" {
		return ""
	}
	mac := hmac.New(sha256.New, lanKeysFor(code).find)
	mac.Write([]byte(what + "\n" + value))
	return hex.EncodeToString(mac.Sum(nil))
}

// lanLinkID: the link as find requests name it: no login, no ?token=…, and
// the site in small letters.
func lanLinkID(raw string) string {
	u, err := url.Parse(linkWithoutSecrets(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	return u.String()
}

// lanFindHandler: does this computer have the file another Jasqet wants?
func (m *Manager) lanFindHandler(w http.ResponseWriter, r *http.Request) {
	code := m.lanSettings().Code
	q := r.URL.Query()
	wantLink, wantName := q.Get("link"), q.Get("name")
	wantSize, _ := strconv.ParseInt(q.Get("size"), 10, 64)
	m.mu.Lock()
	list := m.sortedLocked()
	m.mu.Unlock()
	for _, d := range list {
		d.mu.Lock()
		ok := d.Status == StDone && !lanPrivate(d)
		path, size, name, u := filepath.Join(d.Dir, d.FileName), d.Size, strings.ToLower(d.FileName), d.URL
		id := d.ID
		d.mu.Unlock()
		if !ok || wantSize > 0 && size != wantSize {
			continue
		}
		link := lanHash(code, "link", lanLinkID(u))
		if !(wantLink != "" && link == wantLink || wantName != "" && lanHash(code, "name", name) == wantName) {
			continue
		}
		st, err := os.Stat(path)
		if err != nil || !st.Mode().IsRegular() || (size > 0 && st.Size() != size) {
			continue
		}
		writeJSON(w, map[string]any{"found": true, "match": lanMatch{ID: id, Size: st.Size()}})
		return
	}
	writeJSON(w, map[string]any{"found": false})
}

// linkWithoutSecrets: just the site and path of a link (no login, no
// ?token=…).
func linkWithoutSecrets(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path}).String()
}

func (m *Manager) lanFileHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/lan/file/")
	d := m.get(id)
	if d == nil {
		http.NotFound(w, r)
		return
	}
	d.mu.Lock()
	ok, path := d.Status == StDone && !lanPrivate(d), filepath.Join(d.Dir, d.FileName)
	d.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		http.NotFound(w, r)
		return
	}
	cw := &countWriter{ResponseWriter: w}
	http.ServeContent(cw, r, filepath.Base(path), st.ModTime(), f)
	m.lan.mu.Lock()
	m.lan.served += cw.n
	m.lan.mu.Unlock()
}

type countWriter struct {
	http.ResponseWriter
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.ResponseWriter.Write(p)
	c.n += int64(n)
	return n, err
}

// ---------- discovery ----------

type lanHello struct {
	App  string `json:"app"`
	Port int    `json:"port"`
	Code string `json:"code"`
	Name string `json:"name"`
}

func (m *Manager) lanDiscovery(s LANSettings) {
	addr := &net.UDPAddr{Port: lanDiscoPort}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		fmt.Println("LAN discovery unavailable:", err)
		return
	}
	m.lan.mu.Lock()
	m.lan.conn = conn
	m.lan.mu.Unlock()
	host, _ := os.Hostname()
	tag := lanTag(s.Code)
	hello, _ := json.Marshal(lanHello{App: "Jasqet", Port: s.Port, Code: tag, Name: host})
	mine := map[string]bool{}
	for _, a := range strings.Split(localAddrs(), ",") {
		if i := strings.IndexByte(a, '='); i >= 0 {
			if ip, _, err := net.ParseCIDR(a[i+1:]); err == nil {
				mine[ip.String()] = true
			}
		}
	}
	// announce ourselves every 30 s
	go func() {
		for {
			for _, b := range broadcastAddrs() {
				conn.WriteToUDP(hello, &net.UDPAddr{IP: b, Port: lanDiscoPort})
			}
			time.Sleep(30 * time.Second)
			m.lan.mu.Lock()
			alive := m.lan.conn == conn
			m.lan.mu.Unlock()
			if !alive {
				return
			}
		}
	}()
	buf := make([]byte, 2048)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		var h lanHello
		if json.Unmarshal(buf[:n], &h) != nil || h.App != "Jasqet" || h.Code != tag {
			continue
		}
		if mine[from.IP.String()] && h.Port == s.Port {
			continue
		}
		m.addPeer(net.JoinHostPort(from.IP.String(), strconv.Itoa(h.Port)), h.Name)
	}
}

// addPeer notes a computer that said hello. Only the lanMaxPeers heard from
// most lately are kept.
func (m *Manager) addPeer(addr, name string) {
	ls := m.lan
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.peers == nil {
		ls.peers = map[string]*lanPeer{}
	}
	ls.peers[addr] = &lanPeer{Addr: addr, Name: name, Seen: time.Now()}
	for len(ls.peers) > lanMaxPeers {
		oldest := ""
		for a, p := range ls.peers {
			if oldest == "" || p.Seen.Before(ls.peers[oldest].Seen) {
				oldest = a
			}
		}
		delete(ls.peers, oldest)
	}
}

func broadcastAddrs() []net.IP {
	out := []net.IP{net.IPv4bcast}
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagBroadcast == 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
				ip := n.IP.To4()
				b := make(net.IP, 4)
				for k := 0; k < 4; k++ {
					b[k] = ip[k] | ^n.Mask[k]
				}
				out = append(out, b)
			}
		}
	}
	return out
}

// lanPeers: the computers to ask, lanMaxPeers at most: the ones typed in
// Settings first, then the ones heard from most lately.
func (m *Manager) lanPeers() []string {
	s := m.lanSettings()
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if len(out) < lanMaxPeers && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range s.Peers {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !strings.Contains(p, ":") {
			p = net.JoinHostPort(p, strconv.Itoa(lanDefaultPort))
		}
		add(p)
	}
	ls := m.lan
	ls.mu.Lock()
	var heard []*lanPeer
	for _, p := range ls.peers {
		if time.Since(p.Seen) < 3*time.Minute {
			heard = append(heard, p)
		}
	}
	ls.mu.Unlock()
	slices.SortFunc(heard, func(a, b *lanPeer) int { return b.Seen.Compare(a.Seen) })
	for _, p := range heard {
		add(p.Addr)
	}
	return out
}

// ---------- using a copy from the network ----------

var lanClient = &http.Client{Timeout: 0, Transport: &http.Transport{
	Proxy:                 nil,
	DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
	MaxIdleConnsPerHost:   64,
	IdleConnTimeout:       90 * time.Second, // don't keep connections to other computers forever
	ResponseHeaderTimeout: 15 * time.Second,
}}

const (
	lanPiece  = 64 << 10 // bytes in each sample piece
	lanPieces = 8        // sample pieces checked for each copy
)

// lanCheck: how a copy from another computer is checked once it's all here
// (lanVerify). The pieces are chosen at random for each download and never
// sent to the other computer, so it can't know which parts are checked.
type lanCheck struct {
	at   time.Time
	n    int64      // bytes in each piece
	offs []int64    // where the pieces start
	sums [][32]byte // sha-256 of each piece of the internet copy
}

var errLANCopy = errors.New("the copy from the network doesn't match the file on the internet")

// lanFind asks the other Jasqets whether one of them has this file. It sends
// only keyed hashes of the link and the name, never the link or the name
// themselves.
func (m *Manager) lanFind(ctx context.Context, d *Download) (string, string) {
	m.lanForget(d)
	s := m.lanSettings()
	if !s.Enabled || !s.Use || s.Code == "" {
		return "", ""
	}
	peers := m.lanPeers()
	if len(peers) == 0 {
		return "", ""
	}
	d.mu.Lock()
	u, name, size, resumable := d.URL, d.FileName, d.Size, d.Resumable
	d.mu.Unlock()
	q := url.Values{
		"link": {lanHash(s.Code, "link", lanLinkID(u))},
		"name": {lanHash(s.Code, "name", strings.ToLower(name))},
		"size": {strconv.FormatInt(size, 10)},
	}
	peer, id := lanAsk(ctx, peers, q.Encode(), s.Code, size)
	if peer == "" {
		return "", ""
	}
	c := &lanCheck{at: time.Now()}
	if size > 0 && resumable {
		var err error
		if c, err = m.lanSample(ctx, d, size); err != nil {
			return "", ""
		}
	}
	// Otherwise the internet server can't give pieces to compare with (size
	// unknown, or no ranges). As before, such a copy is used only because it
	// comes from a computer with your group code, which is now always needed.
	// Its checksum, when known, is still checked once it's copied.
	m.lanExpect(d, c)
	return "http://" + peer + "/lan/file/" + id, peer
}

// lanAsk asks all the peers at once, and takes the first that has the file.
// It waits lanAskTime at most, so computers that are slow (or only pretend
// to be Jasqet) can't hold up the download.
func lanAsk(ctx context.Context, peers []string, query, code string, size int64) (peer, id string) {
	ctx, cancel := context.WithTimeout(ctx, lanAskTime)
	defer cancel()
	type answer struct{ peer, id string }
	got := make(chan answer, len(peers))
	for _, p := range peers {
		go func() { got <- answer{p, lanAskOne(ctx, p, query, code, size)} }()
	}
	for range peers {
		select {
		case a := <-got:
			if a.id != "" {
				return a.peer, a.id
			}
		case <-ctx.Done():
			return "", ""
		}
	}
	return "", ""
}

func lanAskOne(ctx context.Context, peer, query, code string, size int64) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+peer+"/lan/find?"+query, nil)
	if err != nil {
		return ""
	}
	lanSignReq(req, code)
	resp, err := lanClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	var res struct {
		Found bool     `json:"found"`
		Match lanMatch `json:"match"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&res) != nil ||
		!res.Found || !lanGoodID(res.Match.ID) || (size > 0 && res.Match.Size != size) {
		return ""
	}
	return res.Match.ID
}

// lanGoodID: a download ID as Jasqet makes them, safe to put in a link.
func lanGoodID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// lanPieceLen: 64 KB pieces, smaller ones for a small file, so the check
// takes at most about a quarter of it from the internet.
func lanPieceLen(size int64) int64 { return max(1, min(lanPiece, size/(4*lanPieces))) }

// lanSample picks lanPieces pieces at random places of the file and fetches
// them from the internet (range requests), to check the copy against.
func (m *Manager) lanSample(ctx context.Context, d *Download, size int64) (*lanCheck, error) {
	n := lanPieceLen(size)
	c := &lanCheck{at: time.Now(), n: n, offs: make([]int64, lanPieces), sums: make([][32]byte, lanPieces)}
	for i := range c.offs {
		off, err := rand.Int(rand.Reader, big.NewInt(size-n+1))
		if err != nil {
			return nil, err
		}
		c.offs[i] = off.Int64()
	}
	errs := make(chan error, len(c.offs))
	for i, off := range c.offs {
		go func() {
			b, err := m.rangeBytes(ctx, d, off, n)
			if err == nil && int64(len(b)) != n {
				err = errors.New("short piece")
			}
			if err == nil {
				c.sums[i] = sha256.Sum256(b)
			}
			errs <- err
		}()
	}
	var first error
	for range c.offs {
		if err := <-errs; err != nil && first == nil {
			first = err
		}
	}
	if first != nil {
		return nil, first
	}
	return c, nil
}

// lanExpect / lanPending / lanForget: the check a download's copy from the
// network must pass, kept until it's done (also across a pause).
func (m *Manager) lanExpect(d *Download, c *lanCheck) {
	ls := m.lan
	ls.mu.Lock()
	defer ls.mu.Unlock()
	if ls.checks == nil {
		ls.checks = map[*Download]*lanCheck{}
	}
	for k, old := range ls.checks {
		if time.Since(old.at) > 24*time.Hour { // removed from the list meanwhile
			delete(ls.checks, k)
		}
	}
	ls.checks[d] = c
}

func (m *Manager) lanPending(d *Download) *lanCheck {
	m.lan.mu.Lock()
	defer m.lan.mu.Unlock()
	return m.lan.checks[d]
}

func (m *Manager) lanForget(d *Download) {
	m.lan.mu.Lock()
	defer m.lan.mu.Unlock()
	delete(m.lan.checks, d)
}

// lanVerify checks a file copied from the network: the sample pieces must
// match the internet copy, and the whole file its checksum when one is known.
func (m *Manager) lanVerify(ctx context.Context, d *Download, path string, c *lanCheck) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	for i, off := range c.offs {
		buf := make([]byte, c.n)
		if got, _ := f.ReadAt(buf, off); int64(got) != c.n || sha256.Sum256(buf) != c.sums[i] {
			f.Close()
			return errLANCopy
		}
	}
	f.Close()
	d.mu.Lock()
	want, inZip := d.Checksum, d.Zip != nil
	d.mu.Unlock()
	if want == "" || inZip { // a file inside a ZIP: its checksum is of the file once taken out
		return nil
	}
	algo, _, _ := strings.Cut(want, ":")
	sum, err := hashFile(ctx, path, algo)
	if err != nil {
		return err
	}
	if algo+":"+sum != want {
		return errLANCopy
	}
	return nil
}

// lanCopyOK is called when a download has all its bytes. If some came from
// another computer, they must pass the check (lanVerify). If they don't,
// the copy is thrown away and the download starts again from the internet;
// then it returns false.
func (m *Manager) lanCopyOK(ctx context.Context, d *Download, f *os.File) bool {
	c := m.lanPending(d)
	if c == nil {
		return true
	}
	err := m.lanVerify(ctx, d, f.Name(), c)
	if ctx.Err() != nil {
		return true // stopped meanwhile: checked when it carries on
	}
	m.lanForget(d)
	if err == nil {
		return true
	}
	d.mu.Lock()
	d.lanSrc = ""
	d.Segments = initialSegments(d.Size, d.Resumable, d.Connections)
	d.Note = "The copy from another computer on your network didn't match, so this one came from the internet"
	d.mu.Unlock()
	f.Truncate(0)
	m.saveSoon()
	return false
}

// lanCopyWhole copies a finished video from a peer straight into place.
func (m *Manager) lanCopyWhole(ctx context.Context, d *Download, src, dest string) error {
	defer m.lanForget(d)
	s := m.lanSettings()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
	if err != nil {
		return err
	}
	lanSignReq(req, s.Code)
	resp, err := lanClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("network copy failed: %s", resp.Status)
	}
	tmp := dest + ".lan"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	buf := make([]byte, 256<<10)
	var got int64
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				os.Remove(tmp)
				return werr
			}
			got += int64(n)
			d.mu.Lock()
			d.segInflight = got
			d.mu.Unlock()
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			os.Remove(tmp)
			return rerr
		}
	}
	f.Close()
	if resp.ContentLength > 0 && got != resp.ContentLength {
		os.Remove(tmp)
		return errors.New("network copy was cut short")
	}
	if c := m.lanPending(d); c != nil {
		if err := m.lanVerify(ctx, d, tmp, c); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	m.lan.mu.Lock()
	m.lan.saved += got
	m.lan.mu.Unlock()
	return os.Rename(tmp, dest)
}

type LANStatus struct {
	Enabled bool      `json:"enabled"`
	HasCode bool      `json:"hasCode"` // a group code that's long enough is set
	Peers   []lanPeer `json:"peers"`
	Served  int64     `json:"served"`
	Saved   int64     `json:"saved"`
}

func (m *Manager) LANStatus() LANStatus {
	s := m.lanSettings()
	st := LANStatus{Enabled: s.Enabled, HasCode: s.Code != ""}
	ls := m.lan
	ls.mu.Lock()
	for _, p := range ls.peers {
		if time.Since(p.Seen) < 3*time.Minute {
			st.Peers = append(st.Peers, *p)
		}
	}
	st.Served, st.Saved = ls.served, ls.saved
	ls.mu.Unlock()
	return st
}

// ---------- the window ----------

// apiLANCode: the window's "Show code", and a code typed in from another
// computer. Only the window can ask (the code is what keeps others out);
// /api/state only says whether one is set.
//
//	{}             the code; a new one if there's none yet (or it's too
//	               short, from an older version)
//	{"code": "…"}  use this code, typed in from another computer
func (s *Server) apiLANCode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code string `json:"code"`
	}
	if !decodePost(w, r, &req) {
		return
	}
	code, made := lanCode(req.Code), false
	if strings.TrimSpace(req.Code) != "" && code == "" {
		writeErr(w, "The group code needs at least 12 letters or digits. Spaces and hyphens don't count, and other signs can't be used.")
		return
	}
	m := s.m
	m.mu.Lock()
	old := m.settings.LAN.Code
	if code == "" {
		if code = lanCode(old); code == "" {
			code, made = lanCode(newLANCode()), true
		}
	}
	changed := code != lanCode(old)
	if changed {
		m.settings.LAN.Code = lanCodeShown(code)
		m.dirty = true
	}
	on := m.settings.LAN.Enabled
	m.mu.Unlock()
	if changed && on {
		go m.startLAN()
	}
	writeJSON(w, map[string]any{"code": lanCodeShown(code), "made": made, "replaced": made && strings.TrimSpace(old) != ""})
}
