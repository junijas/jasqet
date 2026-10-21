package main

import (
	"embed"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

//go:embed web/*
var webFS embed.FS

const version = "1.6.2"

type Server struct {
	m    *Manager
	port string
	quit chan struct{}
	desk *desktop

	pageMu   sync.Mutex
	openPage string // a page the window should switch to (e.g. "connect")

	hub *viewerHub // the built-in viewer
	ext extGate    // browser extensions waiting for your Allow (extauth.go)
	key *apiKey    // the window's key to the API (apikey.go)
}

func (s *Server) routes() http.Handler {
	if s.key == nil {
		s.key = newAPIKey()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.index)
	mux.HandleFunc("/api/ping", s.ping)
	mux.HandleFunc("/api/session", s.apiSession)
	mux.HandleFunc("/api/state", s.state)
	mux.HandleFunc("/api/add", s.add)
	mux.HandleFunc("/api/action", s.action)
	mux.HandleFunc("/api/settings", s.saveSettings)
	mux.HandleFunc("/api/schedule", s.saveSchedule)
	mux.HandleFunc("/api/pickfolder", s.pickFolder)
	mux.HandleFunc("/api/quit", s.quitApp)
	mux.HandleFunc("/api/show", s.showWindow)
	mux.HandleFunc("/api/browsers", s.browsers)
	mux.HandleFunc("/api/budget", s.setBudget)
	mux.HandleFunc("/api/media/probe", s.mediaProbe)
	mux.HandleFunc("/api/media/add", s.mediaAdd)
	mux.HandleFunc("/api/yt/probe", s.ytProbe)
	mux.HandleFunc("/api/yt/add", s.ytAdd)
	mux.HandleFunc("/api/tools", s.toolsStatus)
	mux.HandleFunc("/api/tools/install", s.toolsInstall)
	mux.HandleFunc("/api/tools/update", s.toolsUpdate)
	mux.HandleFunc("/api/extension", s.apiExtension)
	mux.HandleFunc("/api/pair", s.apiPair)
	mux.HandleFunc("/api/lan/code", s.apiLANCode)
	s.moreRoutes(mux)
	s.viewerRoutes(mux)
	return s.guard(mux)
}

// guard keeps random websites from talking to the app: only our own page and
// browser extensions may use the API, and only through a localhost address.
// The page proves it's ours with the key (apikey.go), extensions pass their
// own gate (extauth.go).
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.Host)
		if host != "127.0.0.1" && host != "localhost" {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		origin := r.Header.Get("Origin")
		self := origin == "http://127.0.0.1:"+s.port || origin == "http://localhost:"+s.port
		ext := isExtensionOrigin(origin)
		if origin != "" && !self && !ext {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		// a page on another site that sends no Origin (a link, a picture):
		// nothing for it here except the viewer frame inside Jasqet's window
		if origin == "" && r.Header.Get("Sec-Fetch-Site") == "cross-site" && r.URL.Path != "/viewer" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if ext {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if ext && !s.extensionOK(w, r, origin) {
			return
		}
		if !ext && !s.keyOK(w, r) {
			return
		}
		if r.Method == http.MethodPost && origin == "" && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			http.Error(w, "json only", http.StatusUnsupportedMediaType)
			return
		}
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, 32<<20) // no endless requests
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, msg string) { writeErrCode(w, http.StatusBadRequest, msg) }

func writeErrCode(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	b, _ := webFS.ReadFile("web/index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(b)
}

func (s *Server) ping(w http.ResponseWriter, r *http.Request) {
	o := r.Header.Get("Origin")
	allowed := !isExtensionOrigin(o) || s.extAllowed(o) // false: the extension waits for your Allow
	res := map[string]any{"app": "Jasqet", "version": version, "allowed": allowed}
	if allowed && isExtensionOrigin(o) {
		s.pingProof(r, o, res) // proof that this is the Jasqet it paired with (extauth.go)
	}
	writeJSON(w, res)
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	list, settings := s.m.Snapshot()
	settings.Watch = nil // has its own page; its lists are long
	settings, saved := forWindow(settings)
	var total int64
	for _, d := range list {
		total += d.Speed
	}
	s.pageMu.Lock()
	page := s.openPage
	s.openPage = ""
	s.pageMu.Unlock()
	seen, _ := extensionSeen()
	writeJSON(w, map[string]any{"downloads": list, "settings": settings, "saved": saved, "totalSpeed": total, "version": version,
		"extras": s.m.Extras(), "openPage": page, "extConnected": seen, "extAsk": s.extQuestion(), "pairAsk": s.pairQuestion()})
}

func (s *Server) add(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req AddRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "bad request")
		return
	}
	req.Headers = cleanHeaders(req.Headers)
	if !s.fromApp(r) {
		req.Dir = "" // only you choose folders, in Jasqet's window
	}
	// Waiting for a fresh link for an expired download? If this one matches, use it.
	if d := s.m.pendingRefresh(); d != nil && strings.Count(strings.TrimSpace(req.URL+"\n"+req.URLs), "\n") == 0 {
		link := strings.TrimSpace(req.URL + req.URLs)
		if err := s.m.Refresh(r.Context(), d, link, req.Headers); err == nil {
			writeJSON(w, map[string]any{"ids": []string{d.ID}, "refreshed": true})
			return
		}
	}
	ids, err := s.m.Add(req)
	if err != nil {
		writeErr(w, friendlyError(err))
		return
	}
	writeJSON(w, map[string]any{"ids": ids})
}

func (s *Server) action(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID          string `json:"id"`
		Action      string `json:"action"`
		URL         string `json:"url"`
		Connections int    `json:"connections"`
		LimitKB     *int64 `json:"limitKB"`
	}
	if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&req) != nil {
		writeErr(w, "bad request")
		return
	}
	if req.Action == "cancelpower" {
		s.m.CancelPower()
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	if req.Action == "meteredok" {
		s.m.MeteredOverride()
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	if req.Action == "besthours" || req.Action == "besthoursno" { // the fastest-hours suggestion
		s.m.UseBestHours(req.Action == "besthours")
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	if req.Action == "quiet" || req.Action == "unquiet" {
		s.m.SetQuiet(req.Action == "quiet")
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	if req.Action == "cancelrefresh" {
		s.m.mu.Lock()
		s.m.refreshWait = nil
		s.m.mu.Unlock()
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	if req.ID == "*" {
		switch req.Action {
		case "clear": // "Clear finished": can be undone for a while
			writeJSON(w, map[string]any{"ok": true, "count": s.m.ClearDone()})
			return
		case "unclear":
			writeJSON(w, map[string]any{"ok": true, "count": s.m.UndoClear()})
			return
		}
		list, _ := s.m.Snapshot()
		for _, v := range list {
			if d := s.m.get(v.ID); d != nil {
				switch req.Action {
				case "pause":
					s.m.Pause(d)
				case "resume":
					if v.Status != StCancelled {
						s.m.Resume(d)
					}
				}
			}
		}
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	d := s.m.get(req.ID)
	if d == nil {
		writeErr(w, "download not found")
		return
	}
	d.mu.Lock()
	full := filepath.Join(d.Dir, d.FileName)
	status := d.Status
	d.mu.Unlock()
	switch req.Action {
	case "pause":
		s.m.Pause(d)
	case "cancel":
		s.m.Cancel(d)
	case "resume":
		s.m.Resume(d)
	case "restart":
		s.m.Restart(d)
	case "later":
		s.m.Later(d)
	case "set":
		lim := int64(-1)
		if req.LimitKB != nil {
			lim = *req.LimitKB
		}
		s.m.SetLive(d, req.Connections, lim)
	case "refresh":
		if err := s.m.Refresh(r.Context(), d, req.URL, nil); err != nil {
			writeErr(w, friendlyError(err))
			return
		}
	case "refreshwait":
		s.m.WaitForRefresh(d)
	case "dupok": // download it even though a copy exists
		d.mu.Lock()
		d.DupOK = true
		d.mu.Unlock()
		s.m.Resume(d)
	case "openexisting":
		d.mu.Lock()
		p := d.DupPath
		d.mu.Unlock()
		if p != "" {
			revealPath(p)
		}
	case "remove":
		s.m.Delete(d, false)
	case "deletefile":
		s.m.Delete(d, true)
	case "subs", "subsen": // automatic subtitles (subsen: translated into English)
		if err := s.m.MakeSubtitles(d, req.Action == "subsen"); err != nil {
			writeErr(w, friendlyError(err))
			return
		}
	case "open", "openext", "preview": // never runs a program without asking (see viewer.go)
		how := map[string]string{"open": "auto", "openext": "external", "preview": "preview"}[req.Action]
		res, err := s.open(d, how)
		if err != nil {
			writeErr(w, friendlyError(err))
			return
		}
		s.deliver(&res, s.fromApp(r))
		writeJSON(w, res)
		return
	case "folder":
		if status != StDone {
			full += ".part"
		}
		revealPath(full)
	default:
		writeErr(w, "unknown action")
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	s.m.mu.Lock()
	st := s.m.settings // anything the page doesn't send stays as it is
	old := st
	s.m.mu.Unlock()
	if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&st) != nil {
		writeErr(w, "bad request")
		return
	}
	st = keepSaved(st, old) // the dots in place of a saved key mean "keep it"
	if st.Theme != "light" && st.Theme != "dark" {
		st.Theme = ""
	}
	if err := os.MkdirAll(st.DownloadDir, 0o755); st.DownloadDir != "" && err != nil {
		writeErr(w, "Can't use that folder: "+friendlyError(err))
		return
	}
	s.m.mu.Lock()
	st.Schedule = s.m.settings.Schedule // the schedule and budget have their own pages
	st.Budget = s.m.settings.Budget
	st.Watch = s.m.settings.Watch // watch rules have their own page too
	st.Extensions = s.m.settings.Extensions
	if st.MeteredMode == "" {
		st.MeteredMode = s.m.settings.MeteredMode
	}
	s.m.mu.Unlock()
	s.m.SetSettings(st)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) saveSchedule(w http.ResponseWriter, r *http.Request) {
	var sc Schedule
	if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&sc) != nil {
		writeErr(w, "bad request")
		return
	}
	if _, ok := parseHM(sc.Start); !ok {
		writeErr(w, "please choose a start time")
		return
	}
	if sc.Stop != "" {
		if _, ok := parseHM(sc.Stop); !ok {
			writeErr(w, "the stop time isn't valid")
			return
		}
		if sc.Stop == sc.Start {
			writeErr(w, "start and stop can't be the same time")
			return
		}
	}
	if sc.WhenDone != "sleep" && sc.WhenDone != "shutdown" {
		sc.WhenDone = ""
	}
	days := []int{}
	for _, d := range sc.Days {
		if d >= 0 && d <= 6 {
			days = append(days, d)
		}
	}
	sc.Days = days
	var extra []SchedWindow
	for _, win := range sc.Extra {
		if _, ok := parseHM(win.Start); !ok {
			continue
		}
		if win.Stop != "" {
			if _, ok := parseHM(win.Stop); !ok || win.Stop == win.Start {
				writeErr(w, "the stop time of \""+win.Name+"\" isn't valid")
				return
			}
		}
		extra = append(extra, win)
	}
	sc.Extra = extra
	s.m.mu.Lock()
	s.m.settings.Schedule = sc
	s.m.dirty = true
	s.m.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true, "nextRun": sc.NextRun(time.Now())})
}

func (s *Server) pickFolder(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, "POST only")
		return
	}
	_, st := s.m.Snapshot()
	writeJSON(w, map[string]string{"path": pickFolder(st.DownloadDir)})
}

func (s *Server) quitApp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, "POST only")
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
	go func() {
		time.Sleep(200 * time.Millisecond)
		select {
		case <-s.quit:
		default:
			close(s.quit)
		}
	}()
}

func (s *Server) showWindow(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, "POST only")
		return
	}
	var req struct {
		Page string `json:"page"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if o := r.Header.Get("Origin"); isExtensionOrigin(o) {
		switch {
		case !s.extAllowed(o):
			req.Page = "connect" // where you can Allow it
		case req.Page != "" && req.Page != "connect" && !strings.HasPrefix(req.Page, "zip:http") && !strings.HasPrefix(req.Page, "add:http"):
			req.Page = "" // an extension can't open files or settings
		}
	} else if !s.fromApp(r) && req.Page != "connect" {
		req.Page = "" // without the key (another copy of Jasqet): only the window, or Connect
	}
	if req.Page != "" {
		s.pageMu.Lock()
		s.openPage = req.Page
		s.pageMu.Unlock()
	}
	if s.desk != nil {
		s.desk.Show()
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) browsers(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var req struct {
			Open   string `json:"open"`
			Folder string `json:"folder"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.Open != "" && !openBrowserExtensions(req.Open) {
			writeErr(w, "couldn't open that browser")
			return
		}
		if req.Folder != "" {
			dir := extensionDir(req.Folder == "firefox")
			if dir == "" {
				writeErr(w, "the extension folder isn't next to Jasqet.exe. Use the one from the zip")
				return
			}
			target := filepath.Join(dir, "manifest.json")
			revealPath(target)
		}
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	seen, which := extensionSeen()
	s.m.mu.Lock()
	allowed := slices.Clone(s.m.settings.Extensions)
	s.m.mu.Unlock()
	writeJSON(w, map[string]any{"browsers": knownBrowsers(), "connected": seen, "connectedBrowser": which, "lastSeen": seenSnapshot().At,
		"folder": extensionDir(false), "firefoxFolder": extensionDir(true), "ask": s.extQuestion(), "allowed": allowed})
}

func (s *Server) setBudget(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LeftMB   float64 `json:"leftMB"`
		ResetDay int     `json:"resetDay"`
	}
	if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&req) != nil {
		writeErr(w, "bad request")
		return
	}
	if req.ResetDay < 0 || req.ResetDay > 28 {
		req.ResetDay = 0
	}
	s.m.SetBudget(int64(req.LeftMB*1024*1024), req.ResetDay)
	writeJSON(w, map[string]bool{"ok": true})
}

// cleanHeaders only passes through headers that help us act like the browser.
func cleanHeaders(in map[string]string) map[string]string {
	clean := map[string]string{}
	for k, v := range in {
		switch strings.ToLower(k) {
		case "cookie", "referer", "user-agent", "authorization", "origin":
			clean[http.CanonicalHeaderKey(k)] = v
		}
	}
	return clean
}

func (s *Server) mediaProbe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Cookies map[string]string `json:"cookies"`
	}
	if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&req) != nil {
		writeErr(w, "bad request")
		return
	}
	cookies := map[string]string{}
	for h, c := range req.Cookies {
		cookies[strings.ToLower(h)] = c
	}
	info, err := s.m.ProbeMedia(r.Context(), req.URL, reqOpts{headers: cleanHeaders(req.Headers), cookies: cookies})
	if err != nil {
		writeErr(w, friendlyError(err))
		return
	}
	s.writeQualities(w, info)
}

// writeQualities sends a list of qualities with the data you have left
// ("budget", -1 = no budget), so the window and the extension can show which
// ones fit.
func (s *Server) writeQualities(w http.ResponseWriter, v any) {
	var out map[string]any
	if b, err := json.Marshal(v); err != nil || json.Unmarshal(b, &out) != nil || out == nil {
		writeJSON(w, v)
		return
	}
	out["budget"] = s.m.BudgetLeft()
	writeJSON(w, out)
}

func (s *Server) mediaAdd(w http.ResponseWriter, r *http.Request) {
	var req AddMediaRequest
	if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&req) != nil {
		writeErr(w, "bad request")
		return
	}
	req.Headers = cleanHeaders(req.Headers)
	delete(req.Headers, "Cookie") // cookies travel per site in req.Cookies
	if !s.fromApp(r) {
		req.Dir = ""
	}
	id, err := s.m.AddMedia(req)
	if err != nil {
		writeErr(w, friendlyError(err))
		return
	}
	writeJSON(w, map[string]any{"id": id})
}

func (s *Server) ytProbe(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL     string            `json:"url"`
		Cookies map[string]string `json:"cookies"`
	}
	if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&req) != nil || req.URL == "" {
		writeErr(w, "bad request")
		return
	}
	p, err := s.m.ProbeYT(r.Context(), req.URL, req.Cookies)
	if err != nil {
		writeErr(w, friendlyError(err))
		return
	}
	s.writeQualities(w, p)
}

func (s *Server) ytAdd(w http.ResponseWriter, r *http.Request) {
	var req AddYTRequest
	if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&req) != nil {
		writeErr(w, "bad request")
		return
	}
	req.Headers = cleanHeaders(req.Headers)
	delete(req.Headers, "Cookie")
	if !s.fromApp(r) {
		req.Dir = ""
	}
	ids, err := s.m.AddYT(req)
	if err != nil {
		writeErr(w, friendlyError(err))
		return
	}
	writeJSON(w, map[string]any{"ids": ids})
}

func (s *Server) toolsStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.m.ToolsStatus())
}

func (s *Server) toolsInstall(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, "POST only")
		return
	}
	s.m.InstallTools()
	writeJSON(w, s.m.ToolsStatus())
}

func (s *Server) toolsUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, "POST only")
		return
	}
	if err := s.m.UpdateYtDlp(r.Context()); err != nil {
		writeErr(w, friendlyError(err))
		return
	}
	writeJSON(w, s.m.ToolsStatus())
}
