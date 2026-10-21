package main

// API for the v1.3 features: torrents, ZIP peeking, watch rules, Telegram,
// link sizes for the smart link picker, and safety actions.

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

func (s *Server) moreRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/torrent/add", s.torrentAdd)
	mux.HandleFunc("/api/torrent/files", s.torrentFiles)
	mux.HandleFunc("/api/zip/list", s.zipList)
	mux.HandleFunc("/api/zip/add", s.zipAdd)
	mux.HandleFunc("/api/watch", s.watch)
	mux.HandleFunc("/api/watch/check", s.watchCheck)
	mux.HandleFunc("/api/telegram/find", s.telegramFind)
	mux.HandleFunc("/api/telegram/test", s.telegramTest)
	mux.HandleFunc("/api/sizes", s.sizes)
	mux.HandleFunc("/api/safety", s.safetyAction)
	mux.HandleFunc("/api/lan", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, s.m.LANStatus()) })
	mux.HandleFunc("/api/ytsession", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			s.m.ForgetYTSession()
		}
		saved, when := s.m.YTSessionInfo()
		writeJSON(w, map[string]any{"saved": saved, "when": when})
	})
}

func decodePost(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != http.MethodPost || json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<20)).Decode(v) != nil {
		writeErr(w, "bad request")
		return false
	}
	return true
}

func (s *Server) torrentAdd(w http.ResponseWriter, r *http.Request) {
	var req AddTorrentRequest
	if !decodePost(w, r, &req) {
		return
	}
	req.Headers = cleanHeaders(req.Headers)
	id, err := s.m.AddTorrent(req)
	if err != nil {
		writeErr(w, friendlyError(err))
		return
	}
	writeJSON(w, map[string]any{"id": id})
}

func (s *Server) torrentFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		d := s.m.get(r.URL.Query().Get("id"))
		if d == nil {
			writeErr(w, "download not found")
			return
		}
		d.mu.Lock()
		files := d.torrentFilesLocked()
		d.mu.Unlock()
		writeJSON(w, map[string]any{"files": files})
		return
	}
	var req struct {
		ID   string `json:"id"`
		Skip []int  `json:"skip"`
	}
	if !decodePost(w, r, &req) {
		return
	}
	d := s.m.get(req.ID)
	if d == nil {
		writeErr(w, "download not found")
		return
	}
	s.m.SetTorrentFiles(d, req.Skip)
	writeJSON(w, map[string]bool{"ok": true})
}

type zipReq struct {
	URL      string            `json:"url"`
	Headers  map[string]string `json:"headers"`
	Cookies  map[string]string `json:"cookies"`
	User     string            `json:"user"`
	Password string            `json:"password"`
}

func (s *Server) zipList(w http.ResponseWriter, r *http.Request) {
	var req zipReq
	if !decodePost(w, r, &req) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	l, err := s.m.ListZip(ctx, strings.TrimSpace(req.URL), reqOpts{headers: cleanHeaders(req.Headers), cookies: lowerKeys(req.Cookies), user: req.User, pass: req.Password})
	if err != nil {
		writeErr(w, friendlyError(err))
		return
	}
	writeJSON(w, l)
}

func (s *Server) zipAdd(w http.ResponseWriter, r *http.Request) {
	var req AddZipRequest
	if !decodePost(w, r, &req) {
		return
	}
	req.Headers = cleanHeaders(req.Headers)
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	ids, err := s.m.AddZipEntries(ctx, req)
	if err != nil && len(ids) == 0 {
		writeErr(w, friendlyError(err))
		return
	}
	resp := map[string]any{"ids": ids}
	if err != nil {
		resp["warning"] = friendlyError(err)
	}
	writeJSON(w, resp)
}

func (s *Server) watch(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.m.mu.Lock()
		rules := append([]WatchRule(nil), s.m.settings.Watch...)
		s.m.mu.Unlock()
		for i := range rules {
			rules[i].Seen = nil
		}
		if rules == nil {
			rules = []WatchRule{}
		}
		writeJSON(w, map[string]any{"rules": rules, "ytReady": s.m.tools.ready()})
		return
	}
	var req struct {
		Rules []WatchRule `json:"rules"`
	}
	if !decodePost(w, r, &req) {
		return
	}
	for _, rl := range req.Rules {
		if strings.TrimSpace(rl.URL) == "" {
			writeErr(w, "every rule needs a link")
			return
		}
	}
	s.m.SaveWatch(req.Rules)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) watchCheck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if !decodePost(w, r, &req) {
		return
	}
	n, err := s.m.CheckWatch(r.Context(), req.ID)
	if err != nil {
		writeErr(w, friendlyError(err))
		return
	}
	writeJSON(w, map[string]any{"started": n})
}

func (s *Server) telegramFind(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
	}
	if !decodePost(w, r, &req) {
		return
	}
	if req.Token == savedMask { // the token you saved before
		s.m.mu.Lock()
		req.Token = s.m.settings.Notify.TelegramToken
		s.m.mu.Unlock()
	}
	id, name, err := s.m.TelegramFindChat(req.Token)
	if err != nil {
		writeErr(w, friendlyError(err))
		return
	}
	writeJSON(w, map[string]string{"chat": id, "name": name})
}

func (s *Server) telegramTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeErr(w, "POST only")
		return
	}
	if err := s.m.sendTelegram("👋 Jasqet is connected. You'll get a message here when downloads finish."); err != nil {
		writeErr(w, friendlyError(err))
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// sizes looks up how big each link is (for "all PDFs over 1 MB" in the link picker).
func (s *Server) sizes(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URLs    []string          `json:"urls"`
		Headers map[string]string `json:"headers"`
		Cookies map[string]string `json:"cookies"`
	}
	if !decodePost(w, r, &req) {
		return
	}
	if len(req.URLs) > 400 {
		req.URLs = req.URLs[:400]
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	out := make(map[string]int64, len(req.URLs))
	var mu sync.Mutex
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	h := cleanHeaders(req.Headers)
	ck := lowerKeys(req.Cookies)
	for _, u := range req.URLs {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			d := &Download{URL: u, Headers: h, Cookies: ck, FileName: "x"}
			pctx, c := context.WithTimeout(ctx, 15*time.Second)
			err := s.m.probe(pctx, d)
			c()
			size := int64(-1)
			if err == nil {
				size = d.Size
			}
			mu.Lock()
			out[u] = size
			mu.Unlock()
		}(u)
	}
	wg.Wait()
	writeJSON(w, map[string]any{"sizes": out})
}

func (s *Server) safetyAction(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     string `json:"id"`
		Action string `json:"action"` // "unblock", "allow", "recheck", "checksum"
		Value  string `json:"value"`
	}
	if !decodePost(w, r, &req) {
		return
	}
	d := s.m.get(req.ID)
	if d == nil {
		writeErr(w, "download not found")
		return
	}
	switch req.Action {
	case "unblock": // the user knows what this file is
		d.mu.Lock()
		p := filepath.Join(d.Dir, d.FileName)
		if strings.HasSuffix(p, ".blocked") {
			np := strings.TrimSuffix(p, ".blocked")
			if _, err := os.Stat(np); err == nil {
				d.mu.Unlock()
				writeErr(w, "a file with the original name already exists")
				return
			}
			if err := os.Rename(p, np); err != nil {
				d.mu.Unlock()
				writeErr(w, friendlyError(err))
				return
			}
			d.FileName = filepath.Base(np)
			if d.Safety != nil {
				d.Safety.Renamed = ""
			}
		}
		d.mu.Unlock()
	case "allow": // download a torrent even though it looked fake
		d.mu.Lock()
		d.DupOK = true
		d.Error = ""
		d.mu.Unlock()
		s.m.Resume(d)
	case "checksum": // the user typed the checksum from the site
		sum, err := parseChecksum(req.Value)
		if err != nil {
			writeErr(w, friendlyError(err))
			return
		}
		d.mu.Lock()
		d.Checksum, d.ChecksumFrom = sum, checksumTyped
		done := d.Status == StDone
		d.mu.Unlock()
		if done {
			go s.m.recheckSafety(d)
		}
	case "recheck":
		d.mu.Lock()
		done := d.Status == StDone
		d.mu.Unlock()
		if done {
			go s.m.recheckSafety(d)
		}
	default:
		writeErr(w, "unknown action")
		return
	}
	s.m.saveSoon()
	writeJSON(w, map[string]bool{"ok": true})
}
