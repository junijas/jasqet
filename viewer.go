package main

// The built-in viewer: Open shows videos, music, pictures and PDFs in
// Jasqet's own viewer window instead of another program.
//
// Safety comes first: every Open checks what the file really is (its first
// bytes, not just its name). A "video" that's really a program, script or
// archive is blocked. Open never runs programs without a separate, clear
// confirmation. The viewer runs in its own window process, and the player
// and PDF reader run inside the browser engine's sandboxed processes, so a
// broken file can't stop the downloads.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anacrolix/torrent"
)

type ViewerSettings struct {
	OpenWith string `json:"openWith"` // "viewer", "default", "ask"
	Video    string `json:"video"`    // "viewer" or "default"
	Audio    string `json:"audio"`
	Image    string `json:"image"`
	PDF      string `json:"pdf"`
	AutoOpen bool   `json:"autoOpen"` // open in the viewer when a download finishes
	Preview  bool   `json:"preview"`  // allow watching while downloading
	HWDecode string `json:"hwDecode"` // "auto" or "off"
	Subs     bool   `json:"subs"`     // load matching subtitle files
	Remember bool   `json:"remember"` // remember where you stopped
	Window   string `json:"window"`   // "inside" the Jasqet window, or a "window" of its own

	// how version 1.5.0 stored these choices (carried over once, then dropped)
	OldExternal  bool `json:"external,omitempty"`
	OldNoVideo   bool `json:"noVideo,omitempty"`
	OldNoAudio   bool `json:"noAudio,omitempty"`
	OldNoImages  bool `json:"noImages,omitempty"`
	OldNoPDF     bool `json:"noPdf,omitempty"`
	OldNoPreview bool `json:"noPreview,omitempty"`
}

func defaultViewerSettings() ViewerSettings {
	return ViewerSettings{OpenWith: "viewer", Video: "viewer", Audio: "viewer", Image: "viewer", PDF: "viewer",
		Preview: true, HWDecode: "auto", Subs: true, Remember: true, Window: "inside"}
}

func (v *ViewerSettings) normalize() {
	if v.OldExternal {
		v.OpenWith = "default"
	}
	for _, o := range []struct {
		off  bool
		kind *string
	}{{v.OldNoVideo, &v.Video}, {v.OldNoAudio, &v.Audio}, {v.OldNoImages, &v.Image}, {v.OldNoPDF, &v.PDF}} {
		if o.off {
			*o.kind = "default"
		}
	}
	if v.OldNoPreview {
		v.Preview = false
	}
	v.OldExternal, v.OldNoVideo, v.OldNoAudio, v.OldNoImages, v.OldNoPDF, v.OldNoPreview = false, false, false, false, false, false
	pick := func(s *string, def string, ok ...string) {
		for _, o := range ok {
			if *s == o {
				return
			}
		}
		*s = def
	}
	pick(&v.OpenWith, "viewer", "viewer", "default", "ask")
	pick(&v.Video, "viewer", "viewer", "default")
	pick(&v.Audio, "viewer", "viewer", "default")
	pick(&v.Image, "viewer", "viewer", "default")
	pick(&v.PDF, "viewer", "viewer", "default")
	pick(&v.HWDecode, "auto", "auto", "off")
	pick(&v.Window, "inside", "inside", "window")
}

func (v ViewerSettings) forKind(kind string) string {
	switch kind {
	case "video":
		return v.Video
	case "audio":
		return v.Audio
	case "image":
		return v.Image
	case "pdf":
		return v.PDF
	}
	return "default"
}

var viewExts = map[string]string{
	".mp4": "video", ".m4v": "video", ".mov": "video", ".mkv": "video", ".webm": "video", ".avi": "video", ".flv": "video",
	".ts": "video", ".mts": "video", ".m2ts": "video", ".3gp": "video", ".ogv": "video",
	".mp3": "audio", ".m4a": "audio", ".aac": "audio", ".flac": "audio", ".wav": "audio", ".ogg": "audio", ".oga": "audio",
	".opus": "audio", ".mka": "audio", ".weba": "audio",
	".jpg": "image", ".jpeg": "image", ".jfif": "image", ".png": "image", ".gif": "image", ".webp": "image", ".bmp": "image",
	".svg": "image", ".avif": "image", ".ico": "image",
	".pdf": "pdf",
}

// engineFor: how the viewer shows a file: "native" (the player plays it),
// "remux" (re-wrapped as MP4 first, nothing re-encoded) or "" (not here).
func engineFor(name string) string {
	switch extOf(name) {
	case ".ts", ".mts", ".m2ts", ".flv":
		return "remux"
	case ".avi":
		return "" // old DivX/Xvid videos: the computer's own player does these better
	}
	if viewExts[extOf(name)] != "" {
		return "native"
	}
	return ""
}

func viewKind(name string) string { return viewExts[extOf(name)] }

func displayName(p string) string { return filepath.Base(strings.TrimSuffix(p, ".part")) }

// ---------- what is it really? ----------

func readHead(p string, n int) []byte {
	f, err := openShared(p)
	if err != nil {
		return nil
	}
	defer f.Close()
	b := make([]byte, n)
	k, _ := io.ReadFull(f, b)
	return b[:k]
}

var scriptMarks = []string{"#!", "@echo", "powershell", "wscript", "createobject(", "<script", "cmd.exe", "start-process",
	"invoke-expression", "set objshell", "<?php", "eval(", "function(", "function ", "require(", "import os"}

// blockReason says why a file must not be opened: its name says video, music,
// picture or PDF (or another harmless type), but it's really a program,
// script, archive or web page. head is the start of the file.
func blockReason(name string, head []byte) (reason, real string) {
	name = strings.TrimRight(name, " .") // Windows drops these, so "a.mp4.exe ." is "a.mp4.exe"
	k := sniff(head)
	ext := extOf(name)
	extU := strings.ToUpper(strings.TrimPrefix(ext, "."))
	inner := innerExt(name, ext)
	if execExts[ext] && (viewExts[inner] != "" || contentExts[inner]) {
		return fmt.Sprintf("Its name pretends to be a %s file, but it ends in %s, which makes it a program. Jasqet won't open it.",
			strings.ToUpper(strings.TrimPrefix(inner, ".")), ext), "a program"
	}
	kind := viewKind(name)
	if ext == ".ts" && k != kTS {
		kind = "" // a TypeScript file, not a video
	}
	if kind == "" {
		// not one for the viewer, but a program hiding behind another name is still blocked
		if k.exec && !execExts[ext] && !honestFor(k, name) {
			return fmt.Sprintf("The name says %s, but it's really %s. Jasqet won't open it.", extU, k.name), k.name
		}
		return "", k.name
	}
	switch k {
	case kWinExe, kELF, kMach, kShort, kOLE, kZip, kRar, k7z, kGz, kXz, kBz, kZst, kCAB, kISO, kDMG:
		return fmt.Sprintf("The name says %s, but it's really %s. Jasqet won't open it: fake videos and PDFs like this are how viruses spread.", extU, k.name), k.name
	case kHTML:
		if ext != ".svg" {
			return fmt.Sprintf("The name says %s, but it's really a web page (often a login page or an error message from the site). Jasqet won't open it.", extU), k.name
		}
	case kPDF:
		if kind != "pdf" {
			return fmt.Sprintf("The name says %s, but it's really a PDF document. Rename it to .pdf to view it.", extU), k.name
		}
	}
	if kind == "pdf" && k != kPDF && !bytes.Contains(head[:min(len(head), 1024)], []byte("%PDF-")) {
		if looksLikeScript(head) {
			return "The name says PDF, but it's really a script (a small program). Jasqet won't open it.", "a script"
		}
	}
	if k == kUnknown && ext != ".svg" && looksLikeScript(head) {
		return fmt.Sprintf("The name says %s, but it's really a script (a small program). Jasqet won't open it.", extU), "a script"
	}
	return "", k.name
}

func honestFor(k *fileKind, name string) bool {
	low := strings.ToLower(strings.TrimRight(name, " ."))
	for _, e := range k.exts {
		if e != "" && strings.HasSuffix(low, e) {
			return true
		}
	}
	return false
}

func looksLikeScript(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	b = b[:min(len(b), 2048)]
	text := 0
	for _, c := range b {
		if c == '\n' || c == '\r' || c == '\t' || (c >= 32 && c < 127) {
			text++
		}
	}
	if text < len(b)*95/100 {
		return false // binary data, like real media
	}
	low := strings.ToLower(string(b))
	for _, m := range scriptMarks {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

func blockedFile(p, name string) (string, string) { return blockReason(name, readHead(p, 0x8010)) }

// isProgram: opening this runs code.
func isProgram(p, name string) bool {
	if execExts[extOf(name)] {
		return true
	}
	k := sniff(readHead(p, 1024))
	return k.exec && (extOf(name) == "" || honestFor(k, name))
}

func programWarning(name string) string {
	switch extOf(name) {
	case ".exe", ".msi", ".msix", ".appx", ".scr", ".com", ".bat", ".cmd", ".ps1", ".vbs", ".vbe", ".js", ".jse", ".wsf", ".hta", ".jar", ".msp", ".lnk", ".pif", ".cpl", ".apk":
		return fmt.Sprintf("“%s” is a program. Programs can change your computer, so only run it if you trust the website it came from.", name)
	}
	// .rdp, .theme, .reg, .url…: not programs, but opening one can still run code or change settings
	return fmt.Sprintf("“%s” isn't a picture or a document: opening it can start programs or change your computer's settings. Only open it if you trust the website it came from.", name)
}

// ---------- the viewer hub ----------

type viewEntry struct {
	ID         string
	Path       string // where the file is (or will be) on disk
	DownloadID string
	TorrentIdx int  // a file inside a torrent (-1 = no)
	Stream     bool // a video-site download still in parts
}

type viewerHub struct {
	mu        sync.Mutex
	token     string
	files     map[string]*viewEntry
	byKey     map[string]string
	pending   string
	notify    chan struct{}
	waiting   int // viewer windows waiting for something to show
	lastPoll  time.Time
	spawned   time.Time
	positions map[string]float64
	posLoaded bool
	remux     map[string]*remuxJob
	subs      map[string][]subCue
}

type remuxJob struct {
	done     chan struct{}
	out      string
	err      error
	mu       sync.Mutex
	progress float64
}

func newViewerHub() *viewerHub {
	b := make([]byte, 16)
	rand.Read(b)
	return &viewerHub{token: hex.EncodeToString(b), files: map[string]*viewEntry{}, byKey: map[string]string{},
		notify: make(chan struct{}, 1), remux: map[string]*remuxJob{}, subs: map[string][]subCue{}}
}

func (h *viewerHub) register(path, downloadID string, torrentIdx int, stream bool) *viewEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := fmt.Sprintf("%s|%s|%d|%v", path, downloadID, torrentIdx, stream)
	if id, ok := h.byKey[key]; ok {
		return h.files[id]
	}
	if len(h.files) > 20000 {
		h.files, h.byKey = map[string]*viewEntry{}, map[string]string{}
	}
	b := make([]byte, 8)
	rand.Read(b)
	e := &viewEntry{ID: hex.EncodeToString(b), Path: path, DownloadID: downloadID, TorrentIdx: torrentIdx, Stream: stream}
	h.files[e.ID] = e
	h.byKey[key] = e.ID
	return e
}

func (h *viewerHub) get(id string) *viewEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.files[id]
}

func positionsFile() string {
	return filepath.Join(filepath.Dir(stateFilePath()), "viewer-positions.json")
}

func posKey(p string) string { return strings.ToLower(strings.TrimSuffix(p, ".part")) }

func (h *viewerHub) position(p string) float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.loadPositionsLocked()
	return h.positions[posKey(p)]
}

func (h *viewerHub) loadPositionsLocked() {
	if h.posLoaded {
		return
	}
	h.posLoaded = true
	h.positions = map[string]float64{}
	if b, err := os.ReadFile(positionsFile()); err == nil {
		json.Unmarshal(b, &h.positions)
	}
}

func (h *viewerHub) setPosition(p string, t float64) {
	h.mu.Lock()
	h.loadPositionsLocked()
	k := posKey(p)
	if t <= 0 {
		if _, ok := h.positions[k]; !ok {
			h.mu.Unlock()
			return
		}
		delete(h.positions, k)
	} else {
		h.positions[k] = t
	}
	if len(h.positions) > 500 { // keep it small
		for kk := range h.positions {
			delete(h.positions, kk)
			if len(h.positions) <= 400 {
				break
			}
		}
	}
	b, _ := json.Marshal(h.positions)
	h.mu.Unlock()
	writeFileAtomic(positionsFile(), b)
}

// ---------- opening ----------

type OpenResult struct {
	Result   string `json:"result"` // "viewer", "external", "blocked", "confirm", "ask", "folder"
	Message  string `json:"message,omitempty"`
	RealType string `json:"realType,omitempty"`
	Kind     string `json:"kind,omitempty"`
	Name     string `json:"name,omitempty"`
	File     string `json:"-"`              // the viewer's id for it
	View     string `json:"view,omitempty"` // show the viewer inside the Jasqet window at this address
}

// deliver shows a file Open sent to the viewer: inside the Jasqet window
// (the page that asked gets the address; otherwise the window is told) or in
// a viewer window of its own, as set in Settings → Viewer.
func (s *Server) deliver(res *OpenResult, fromApp bool) {
	if res.Result != "viewer" || res.File == "" {
		return
	}
	if s.viewerSettings().Window == "window" {
		s.showViewer(res.File)
		return
	}
	if fromApp {
		res.View = s.viewPath(res.File)
		return
	}
	s.showPage("view:" + res.File)
}

func (s *Server) viewPath(fileID string) string {
	return "/viewer?t=" + s.hub.token + "&f=" + url.QueryEscape(fileID) + "&embed=1"
}

// apiViewURL: the Jasqet window asks where to show a file (only it may know).
func (s *Server) apiViewURL(w http.ResponseWriter, r *http.Request) {
	var req struct {
		F string `json:"f"`
	}
	if !decodePost(w, r, &req) {
		return
	}
	if !s.fromApp(r) || s.hub.get(req.F) == nil {
		writeErr(w, "not available")
		return
	}
	writeJSON(w, map[string]string{"url": s.viewPath(req.F)})
}

// theme: the window's light or dark mode, or "" to follow the system.
func (s *Server) theme() string {
	s.m.mu.Lock()
	t := s.m.settings.Theme
	s.m.mu.Unlock()
	if t == "light" || t == "dark" {
		return t
	}
	return ""
}

func (s *Server) viewerSettings() ViewerSettings {
	s.m.mu.Lock()
	defer s.m.mu.Unlock()
	return s.m.settings.Viewer
}

// pickInFolder: for a folder (a torrent, an unzipped archive) the biggest
// file the viewer can show.
func pickInFolder(dir string) string {
	best, bestSize := "", int64(-1)
	n := 0
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if n++; n > 20000 {
			return filepath.SkipAll
		}
		if err == nil && !info.IsDir() && viewKind(info.Name()) != "" && engineFor(info.Name()) != "" && info.Size() > bestSize {
			best, bestSize = p, info.Size()
		}
		return nil
	})
	return best
}

// open decides what Open does for a download (how: "auto", "viewer",
// "external", "run", "with", "preview").
func (s *Server) open(d *Download, how string) (OpenResult, error) {
	if how == "preview" {
		return s.openPreview(d)
	}
	d.mu.Lock()
	status := d.Status
	p := filepath.Join(d.Dir, d.FileName)
	name := d.FileName
	id := d.ID
	d.mu.Unlock()
	if status != StDone {
		return OpenResult{}, errors.New("the download hasn't finished yet")
	}
	return s.openFile(p, name, id, how)
}

func (s *Server) openFile(p, name, downloadID, how string) (OpenResult, error) {
	vs := s.viewerSettings()
	st, err := os.Stat(p)
	if err != nil {
		return OpenResult{}, errors.New("the file was moved or deleted")
	}
	if st.IsDir() {
		inner := ""
		if how != "external" && how != "with" {
			inner = pickInFolder(p)
		}
		if inner == "" {
			revealPath(p)
			return OpenResult{Result: "folder", Message: "This download is a folder. It's open in File Explorer."}, nil
		}
		p, name = inner, filepath.Base(inner)
	}
	res := OpenResult{Name: name}
	if strings.HasSuffix(strings.ToLower(name), ".blocked") {
		res.Result, res.Message = "blocked", "Jasqet blocked this file because it isn't what its name says. Delete it, or look at the download's safety details."
		return res, nil
	}
	// 1. what is it really?
	if reason, real := blockedFile(p, name); reason != "" {
		res.Result, res.Message, res.RealType = "blocked", reason, real
		return res, nil
	}
	kind := viewKind(name)
	if extOf(name) == ".ts" && sniff(readHead(p, 400)) != kTS {
		kind = "" // TypeScript code, not a video
	}
	res.Kind = kind
	builtIn := kind != "" && engineFor(name) != "" && vs.forKind(kind) != "default"
	why := ""
	if builtIn && (kind == "video" || kind == "audio") {
		if why = unplayable(p); why != "" {
			builtIn = false // e.g. a DivX video: the computer's own player can do it
		}
	}
	if kind != "" && engineFor(name) == "" {
		why = "The built-in viewer can't show this kind of file, so it opened in your computer's own program."
	}
	switch {
	case how == "with":
		res.Result = "external"
		return res, openWith(p)
	case how == "run": // you confirmed running a program
		res.Result = "external"
		return res, openPath(p)
	case how == "external":
		if isProgram(p, name) {
			res.Result, res.Message = "confirm", programWarning(name)
			return res, nil
		}
		res.Result = "external"
		return res, openPath(p)
	case builtIn && (how == "viewer" || how == "autoview" || (how == "auto" && vs.OpenWith == "viewer")):
		e := s.hub.register(p, downloadID, -1, false)
		res.Result, res.File = "viewer", e.ID
		return res, nil
	case builtIn && how == "auto" && vs.OpenWith == "ask":
		res.Result = "ask"
		return res, nil
	case how == "autoview": // "open automatically when finished": the viewer or nothing
		res.Result = "none"
		return res, nil
	}
	// the computer's own program, but never a program from a plain Open
	if isProgram(p, name) {
		res.Result, res.Message = "confirm", programWarning(name)
		return res, nil
	}
	res.Result = "external"
	if kind != "" && (how == "viewer" || vs.forKind(kind) != "default" && vs.OpenWith != "default") {
		res.Message = why
	}
	return res, openPath(p)
}

// unplayable: the reason the built-in player can't play this, or "".
func unplayable(p string) string {
	if engineFor(p) == "remux" {
		return ""
	}
	tracks := safeTracks(p)
	video, audioOK, audio := false, false, ""
	for _, t := range tracks {
		switch t.Type {
		case "video":
			video = true
			if !t.Played {
				return fmt.Sprintf("This video uses %s, which the built-in viewer can't play, so it opened in your computer's video player.", t.Name)
			}
		case "audio":
			audioOK = audioOK || t.Played
			if audio == "" {
				audio = t.Name
			}
		}
	}
	if audio != "" && !audioOK && !video {
		return fmt.Sprintf("This uses %s sound, which the built-in viewer can't play, so it opened in your computer's player.", audio)
	}
	return ""
}

// openWith shows Windows' "How do you want to open this file?" list of apps.
func openWith(p string) error {
	if testLog("openwith", p) {
		return nil
	}
	if runtime.GOOS == "windows" {
		return exec.Command("rundll32.exe", "shell32.dll,OpenAs_RunDLL", p).Start()
	}
	return revealPath(p)
}

func (s *Server) openPreview(d *Download) (OpenResult, error) {
	if !s.viewerSettings().Preview {
		return OpenResult{}, errors.New("preview while downloading is off (Settings → Viewer)")
	}
	d.mu.Lock()
	kind, name, id, status := d.Kind, d.FileName, d.ID, d.Status
	final, part := filepath.Join(d.Dir, d.FileName), d.partPath()
	d.mu.Unlock()
	if status == StDone {
		return s.open(d, "viewer")
	}
	switch kind {
	case "yt", "media":
		e := s.hub.register(final, id, -1, true)
		return OpenResult{Result: "viewer", Kind: "video", Name: name, File: e.ID}, nil
	case "torrent":
		idx, p := torrentPreviewFile(d)
		if idx < 0 {
			return OpenResult{}, errors.New("there's no video or music in this torrent the viewer can play")
		}
		e := s.hub.register(p, id, idx, false)
		return OpenResult{Result: "viewer", Kind: viewKind(p), Name: filepath.Base(p), File: e.ID}, nil
	case "", "zipentry":
		d.mu.Lock()
		packed := d.Zip != nil && d.Zip.Method != 0
		d.mu.Unlock()
		if packed {
			return OpenResult{}, errors.New("this file is compressed inside the ZIP, so it can be played once it has finished downloading")
		}
		k := viewKind(name)
		if k == "image" || k == "pdf" {
			return OpenResult{}, errors.New("pictures and PDFs open once they've finished downloading")
		}
		if k == "" || engineFor(name) != "native" {
			return OpenResult{}, errors.New("this kind of file can only be opened once it has finished downloading")
		}
		e := s.hub.register(part, id, -1, false)
		return OpenResult{Result: "viewer", Kind: k, Name: name, File: e.ID}, nil
	}
	return OpenResult{}, errors.New("this download can't be previewed")
}

func torrentPreviewFile(d *Download) (int, string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	files := d.torrentFilesLocked()
	best, bestSize := -1, int64(-1)
	for i, f := range files {
		k := viewKind(f.Path)
		if f.Wanted && (k == "video" || k == "audio") && engineFor(f.Path) == "native" && f.Size > bestSize {
			best, bestSize = i, f.Size
		}
	}
	if best < 0 {
		return -1, ""
	}
	return best, d.torrentDiskPathLocked(best)
}

// showViewer shows a file in the viewer window: the open one if there is
// one, otherwise a new window (its own process).
func (s *Server) showViewer(fileID string) {
	h := s.hub
	h.mu.Lock()
	alive := h.waiting > 0 || time.Since(h.lastPoll) < 3*time.Second || time.Since(h.spawned) < 10*time.Second
	h.pending = fileID
	if !alive {
		h.spawned = time.Now()
		h.pending = ""
	}
	h.mu.Unlock()
	if alive {
		allowSetForeground()
		select {
		case h.notify <- struct{}{}:
		default:
		}
		go s.pendingCheck(fileID)
		return
	}
	s.spawnViewer(fileID)
}

// pendingCheck: if no viewer window took the file (it was closing just as
// you clicked Open), start a new one.
func (s *Server) pendingCheck(fileID string) {
	h := s.hub
	time.Sleep(1500 * time.Millisecond)
	for {
		h.mu.Lock()
		if h.pending != fileID || h.waiting > 0 {
			h.mu.Unlock()
			return
		}
		if wait := 10*time.Second - time.Since(h.spawned); wait > 0 { // a window is still starting up
			h.mu.Unlock()
			time.Sleep(wait)
			continue
		}
		h.pending = ""
		h.spawned = time.Now()
		h.mu.Unlock()
		s.spawnViewer(fileID)
		return
	}
}

func (s *Server) spawnViewer(fileID string) {
	h := s.hub
	u := fmt.Sprintf("http://127.0.0.1:%s/viewer?t=%s&f=%s", s.port, h.token, fileID)
	if runtime.GOOS == "windows" {
		if exe, err := os.Executable(); err == nil {
			cmd := exec.Command(exe, "-viewer", u)
			args := "--enable-blink-features=AudioVideoTracks --autoplay-policy=no-user-gesture-required"
			if s.viewerSettings().HWDecode == "off" {
				args += " --disable-accelerated-video-decode"
			}
			cmd.Env = append(os.Environ(), "WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS="+args)
			if cmd.Start() == nil {
				go cmd.Wait()
				return
			}
		}
	}
	openBrowser(u)
}

// ---------- HTTP ----------

func (s *Server) viewerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/viewer", s.viewerPage)
	mux.HandleFunc("/viewer/pdfjs/", s.viewerStatic)
	mux.HandleFunc("/viewer/api/info", s.viewerInfo)
	mux.HandleFunc("/viewer/api/wait", s.viewerWait)
	mux.HandleFunc("/viewer/api/act", s.viewerAct)
	mux.HandleFunc("/viewer/api/pos", s.viewerPos)
	mux.HandleFunc("/viewer/api/alive", s.viewerAlive)
	mux.HandleFunc("/viewer/api/prepare", s.viewerPrepare)
	mux.HandleFunc("/viewer/file/", s.viewerFile)
	mux.HandleFunc("/viewer/stream/", s.viewerStream)
	mux.HandleFunc("/viewer/subs/", s.viewerSubs)
	mux.HandleFunc("/viewer/cover/", s.viewerCover)
	mux.HandleFunc("/api/open", s.apiOpen)
	mux.HandleFunc("/api/protocol", s.apiProtocol)
	mux.HandleFunc("/api/viewurl", s.apiViewURL)
}

// viewerOK: the request comes from our viewer (it knows the secret token) and
// not from another website.
func (s *Server) viewerOK(w http.ResponseWriter, r *http.Request) bool {
	cross := r.Header.Get("Sec-Fetch-Site") == "cross-site"
	if cross && r.URL.Path == "/viewer" && s.fromAppReferer(r) {
		cross = false // shown inside the Jasqet window (a frame on the app's other address)
	}
	if cross || r.URL.Query().Get("t") != s.hub.token {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	return true
}

func (s *Server) fromAppReferer(r *http.Request) bool {
	u, err := url.Parse(r.Header.Get("Referer"))
	return err == nil && u.Scheme == "http" && (u.Host == "127.0.0.1:"+s.port || u.Host == "localhost:"+s.port)
}

func (s *Server) viewerPage(w http.ResponseWriter, r *http.Request) {
	if !s.viewerOK(w, r) {
		return
	}
	b, _ := webFS.ReadFile("web/viewer.html")
	if t := s.theme(); t != "" { // light or dark like the window, before the page draws
		b = bytes.Replace(b, []byte(`<html lang="en">`), []byte(`<html lang="en" data-theme="`+t+`">`), 1)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	// only our own code runs here; files come from this app only, never from the internet
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; media-src 'self' blob:; font-src 'self' data:; worker-src 'self' blob:; connect-src 'self'; frame-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'; "+
		"frame-ancestors 'self' http://127.0.0.1:"+s.port+" http://localhost:"+s.port) // only Jasqet's own window may show it inside
	w.Write(b)
}

func (s *Server) viewerStatic(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/viewer/pdfjs/")
	if strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	b, err := webFS.ReadFile("web/pdfjs/" + name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(name, ".mjs"), strings.HasSuffix(name, ".js"):
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Cache-Control", "max-age=86400")
	w.Write(b)
}

func (s *Server) apiOpen(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID  string `json:"id"`
		How string `json:"how"`
	}
	if !decodePost(w, r, &req) {
		return
	}
	d := s.m.get(req.ID)
	if d == nil {
		writeErr(w, "download not found")
		return
	}
	switch req.How {
	case "", "auto", "viewer", "external", "run", "with", "preview":
	default:
		writeErr(w, "unknown way to open")
		return
	}
	if req.How == "" {
		req.How = "auto"
	}
	if req.How == "run" && !s.fromApp(r) {
		writeErr(w, "programs can only be run from the Jasqet window")
		return
	}
	res, err := s.open(d, req.How)
	if err != nil {
		writeErr(w, friendlyError(err))
		return
	}
	s.deliver(&res, s.fromApp(r))
	writeJSON(w, res)
}

// appOrigin: the request comes from a page on Jasqet's own address (the
// viewer, which proves itself with its token; the window has the key).
func (s *Server) appOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	return o == "http://127.0.0.1:"+s.port || o == "http://localhost:"+s.port
}

// apiProtocol handles jasqet://open/<id> and jasqet://folder/<id> (the
// buttons on the "download finished" notification).
func (s *Server) apiProtocol(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if !decodePost(w, r, &req) {
		return
	}
	s.handleProtocol(req.URL)
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) handleProtocol(raw string) {
	u, err := url.Parse(raw)
	if err != nil || !strings.EqualFold(u.Scheme, "jasqet") {
		return
	}
	id := strings.Trim(u.Path, "/")
	d := s.m.get(id)
	if d == nil {
		s.showPage("")
		return
	}
	switch strings.ToLower(u.Host) {
	case "open":
		res, err := s.open(d, "auto")
		s.deliver(&res, false)
		if err != nil || res.Result == "confirm" || res.Result == "blocked" || res.Result == "ask" || res.Result == "folder" && res.Message == "" {
			s.showPage("open:" + id) // the Jasqet window shows what to do
		}
	case "folder":
		d.mu.Lock()
		p := filepath.Join(d.Dir, d.FileName)
		d.mu.Unlock()
		revealPath(p)
	default:
		s.showPage("")
	}
}

// autoOpen: "Open automatically when a download finishes".
func (s *Server) autoOpen(d *Download) {
	if !s.viewerSettings().AutoOpen {
		return
	}
	d.mu.Lock()
	name, kind := d.FileName, d.Kind
	d.mu.Unlock()
	if kind == "subs" || viewKind(name) == "" || engineFor(name) == "" {
		return
	}
	if res, err := s.open(d, "autoview"); err == nil {
		s.deliver(&res, false)
	}
}

func (s *Server) showPage(page string) {
	s.pageMu.Lock()
	s.openPage = page
	s.pageMu.Unlock()
	if s.desk != nil {
		allowSetForeground()
		s.desk.Show()
	}
}

type viewInfo struct {
	ID       string         `json:"id"`
	Name     string         `json:"name"`
	Size     int64          `json:"size"`
	Site     string         `json:"site,omitempty"`
	Kind     string         `json:"kind"`
	Engine   string         `json:"engine"`
	URL      string         `json:"url"`
	Streams  []viewStream   `json:"streams,omitempty"` // a video-site download still in parts
	Blocked  string         `json:"blocked,omitempty"`
	RealType string         `json:"realType,omitempty"`
	Tracks   []MediaTrack   `json:"tracks,omitempty"`
	Warnings []string       `json:"warnings,omitempty"`
	Subs     []viewSub      `json:"subs,omitempty"`
	Title    string         `json:"title,omitempty"`
	Artist   string         `json:"artist,omitempty"`
	Album    string         `json:"album,omitempty"`
	Cover    string         `json:"cover,omitempty"`
	Prev     *viewLink      `json:"prev,omitempty"`
	Next     *viewLink      `json:"next,omitempty"`
	Position float64        `json:"position,omitempty"`
	Settings ViewerSettings `json:"settings"`
	Download *viewDL        `json:"download,omitempty"`
	Remux    *viewRemux     `json:"remux,omitempty"`
	Program  bool           `json:"program,omitempty"`
	Later    string         `json:"later,omitempty"` // can't be watched until it has finished: why
}

type viewStream struct {
	URL    string `json:"url"`
	Type   string `json:"type"`   // "video" or "audio"
	Ranged bool   `json:"ranged"` // can jump around in it
}

type viewSub struct {
	Label string `json:"label"`
	URL   string `json:"url"`
	Lang  string `json:"lang,omitempty"`
}

type viewLink struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type viewDL struct {
	ID      string  `json:"id"`
	Status  Status  `json:"status"`
	Percent float64 `json:"percent"`
	Ready   float64 `json:"ready"` // how much from the start is downloaded without gaps (0 to 1)
	Waiting bool    `json:"waiting"`
	Running bool    `json:"running"`
	Done    bool    `json:"done"`
	Error   string  `json:"error,omitempty"`
	RetryIn int64   `json:"retryIn,omitempty"` // seconds until it tries again by itself after a network problem
}

type viewRemux struct {
	Ready    bool    `json:"ready"`
	Progress float64 `json:"progress"`
	Error    string  `json:"error,omitempty"`
}

func (s *Server) entryFrom(r *http.Request) *viewEntry {
	id := r.URL.Query().Get("f")
	if id == "" {
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) >= 3 {
			id = strings.TrimSuffix(parts[2], filepath.Ext(parts[2]))
		}
	}
	return s.hub.get(id)
}

func (s *Server) viewerInfo(w http.ResponseWriter, r *http.Request) {
	if !s.viewerOK(w, r) {
		return
	}
	e := s.entryFrom(r)
	if e == nil {
		writeErr(w, "This file isn't available any more.")
		return
	}
	vs := s.viewerSettings()
	t := s.hub.token
	inf := viewInfo{ID: e.ID, Settings: vs}
	var d *Download
	if e.DownloadID != "" {
		d = s.m.get(e.DownloadID)
	}
	inProgress := false
	var size int64 = -1
	var tor *torrentHandle
	if d != nil {
		d.mu.Lock()
		done := d.Status == StDone
		inf.Site = strings.TrimPrefix(hostOf(firstNonEmpty(d.Page, d.URL)), "www.")
		if d.Kind == "torrent" {
			inf.Site = "a torrent"
		}
		dl := &viewDL{ID: d.ID, Status: d.Status, Done: done, Running: d.running, Error: d.Error}
		if d.Status == StQueued && !d.retryAt.IsZero() {
			if left := time.Until(d.retryAt); left > 0 {
				dl.RetryIn = int64(left.Seconds() + 0.999)
			}
		}
		if d.Kind == "torrent" && e.TorrentIdx >= 0 {
			files := d.torrentFilesLocked()
			if e.TorrentIdx < len(files) && files[e.TorrentIdx].Size > 0 {
				f := files[e.TorrentIdx]
				size = f.Size
				dl.Percent = float64(f.Done) / float64(f.Size) * 100
			}
			tor = &torrentHandle{d.tor, e.TorrentIdx}
			if d.tor != nil && d.tor.Info() != nil && e.TorrentIdx < len(d.tor.Files()) {
				ps := d.tor.Files()[e.TorrentIdx].State()
				k := 0
				for k < len(ps) && ps[k].Complete {
					k++
				}
				if len(ps) > 0 {
					dl.Ready = float64(k) / float64(len(ps))
				}
			}
		} else if d.isStream() {
			var v DownloadView
			d.mediaView(&v)
			if v.Size > 0 {
				dl.Percent = float64(v.Downloaded) / float64(v.Size) * 100
			}
			dl.Ready = dl.Percent / 100
		} else if d.Size > 0 {
			size = d.Size
			dl.Percent = float64(d.downloadedLocked()) / float64(d.Size) * 100
			dl.Ready = float64(d.frontierLocked()) / float64(d.Size)
		}
		if done {
			dl.Percent = 100
		}
		dl.Waiting = !done && d.previewWait > 0
		inf.Download = dl
		final := filepath.Join(d.Dir, d.FileName)
		d.mu.Unlock()
		inProgress = !done
		if done && (e.Stream || strings.HasSuffix(e.Path, ".part") || e.TorrentIdx >= 0) {
			// it finished since the viewer opened: show the finished file instead
			p := final
			if e.TorrentIdx >= 0 {
				p = e.Path
			}
			e = s.hub.register(p, d.ID, -1, false)
			inf.ID = e.ID
			tor = nil
		}
	} else if strings.HasSuffix(e.Path, ".part") || e.Stream || e.TorrentIdx >= 0 {
		writeErr(w, "This download was removed.")
		return
	}
	path := e.Path
	inf.Name = displayName(path)
	inf.Kind = viewKind(inf.Name)
	inf.Engine = engineFor(inf.Name)
	if !inProgress {
		st, err := os.Stat(path)
		if err != nil {
			writeErr(w, "The file was moved or deleted.")
			return
		}
		size = st.Size()
	}
	inf.Size = size
	inf.URL = "/viewer/file/" + e.ID + "?t=" + t

	// safety first, every time: what is it really?
	var head []byte
	switch {
	case !inProgress:
		head = readHead(path, 0x8010)
	case tor != nil:
		head = tor.head(r.Context())
	case !e.Stream:
		d.mu.Lock()
		ready := d.rangeDoneLocked(0, min(4096, max(size, 1)))
		d.mu.Unlock()
		if ready {
			head = readHead(path, 4096)
		}
	}
	if head != nil {
		if inf.Kind == "video" && extOf(inf.Name) == ".ts" && sniff(head) != kTS {
			inf.Kind, inf.Engine = "", ""
		}
		if reason, real := blockReason(inf.Name, head); reason != "" {
			inf.Blocked, inf.RealType = reason, real
			writeJSON(w, inf)
			return
		}
	}
	if inf.Kind == "" || inf.Engine == "" {
		inf.Program = !inProgress && isProgram(path, inf.Name)
		writeJSON(w, inf) // the page offers Open externally
		return
	}
	if e.Stream && inProgress {
		streams, why := streamTracks(d)
		if len(streams) == 0 {
			inf.Later = firstNonEmpty(why, "It can be watched as soon as it has finished downloading.")
		}
		for i, st := range streams {
			typ := "video"
			if st.audio {
				typ = "audio"
			}
			inf.Streams = append(inf.Streams, viewStream{URL: fmt.Sprintf("/viewer/stream/%s/%d?t=%s", e.ID, i, t), Type: typ, Ranged: st.size > 0})
		}
	}
	if !inProgress && inf.Engine == "remux" {
		inf.Remux = s.remuxState(path)
		if inf.Remux.Ready {
			inf.URL += "&remux=1"
		}
	}
	if inf.Kind == "video" || inf.Kind == "audio" {
		if !inProgress {
			if inf.Engine == "native" {
				inf.Tracks = safeTracks(path)
			}
			for _, tr := range inf.Tracks {
				if !tr.Played && tr.Type == "audio" {
					inf.Warnings = append(inf.Warnings, fmt.Sprintf("A sound track in this file uses %s, which the built-in viewer can't play. If you hear nothing, use Open externally.", tr.Name))
				}
				if !tr.Played && tr.Type == "video" {
					inf.Warnings = append(inf.Warnings, fmt.Sprintf("This video uses %s, which the built-in viewer can't play. Use Open externally.", tr.Name))
				}
				if !tr.Played && tr.Type == "subtitle" && tr.text == "" && strings.Contains(tr.Name, "picture") {
					inf.Warnings = append(inf.Warnings, fmt.Sprintf("This file has %s, which only your computer's video player can show.", tr.Name))
				}
			}
		}
		if vs.Subs && inf.Kind == "video" {
			for i, sp := range subtitleFilesFor(strings.TrimSuffix(path, ".part")) {
				inf.Subs = append(inf.Subs, viewSub{Label: subtitleLabel(strings.TrimSuffix(path, ".part"), sp), URL: fmt.Sprintf("/viewer/subs/%s/%d?t=%s", e.ID, i, t)})
			}
		}
		n, k := len(inf.Subs), 0
		for _, tr := range inf.Tracks {
			if tr.Type == "subtitle" && tr.text != "" {
				label := firstNonEmpty(tr.Title, langName(tr.Lang), "Subtitles")
				inf.Subs = append(inf.Subs, viewSub{Label: label + " (in the file)", Lang: tr.Lang, URL: fmt.Sprintf("/viewer/subs/%s/%d?t=%s", e.ID, n+k, t)})
				k++
			}
		}
		if vs.Remember && !inProgress {
			inf.Position = s.hub.position(path)
		}
	}
	if inf.Kind == "audio" && !inProgress {
		if tg := safeTags(path); tg != nil {
			inf.Title, inf.Artist, inf.Album = tg.Title, tg.Artist, tg.Album
			if len(tg.Cover) > 0 {
				inf.Cover = "/viewer/cover/" + e.ID + "?t=" + t
			}
		}
	}
	if !inProgress {
		inf.Prev, inf.Next = s.neighbours(path)
	}
	writeJSON(w, inf)
}

// neighbours: the files before and after this one in its folder that the
// viewer can show (for the ‹ › arrows).
func (s *Server) neighbours(path string) (prev, next *viewLink) {
	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) > 5000 {
		return nil, nil
	}
	var names []string
	for _, de := range entries {
		n := de.Name()
		if !de.IsDir() && !strings.HasPrefix(n, ".") && viewKind(n) != "" && engineFor(n) != "" {
			names = append(names, n)
		}
	}
	sort.Slice(names, func(i, j int) bool { return naturalLess(names[i], names[j]) })
	base := filepath.Base(path)
	for i, n := range names {
		if n != base {
			continue
		}
		link := func(n string) *viewLink {
			p := filepath.Join(dir, n)
			e := s.hub.register(p, s.downloadFor(p), -1, false)
			return &viewLink{e.ID, n}
		}
		if i > 0 {
			prev = link(names[i-1])
		}
		if i+1 < len(names) {
			next = link(names[i+1])
		}
	}
	return
}

// naturalLess sorts "Episode 2" before "Episode 10".
func naturalLess(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	for a != "" && b != "" {
		ca, cb := a[0], b[0]
		if ca >= '0' && ca <= '9' && cb >= '0' && cb <= '9' {
			i, j := 0, 0
			for i < len(a) && a[i] >= '0' && a[i] <= '9' {
				i++
			}
			for j < len(b) && b[j] >= '0' && b[j] <= '9' {
				j++
			}
			na, nb := strings.TrimLeft(a[:i], "0"), strings.TrimLeft(b[:j], "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			a, b = a[i:], b[j:]
			continue
		}
		if ca != cb {
			return ca < cb
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

// downloadFor finds the download a file belongs to (for Delete, Verify…).
func (s *Server) downloadFor(p string) string {
	s.m.mu.Lock()
	list := make([]*Download, 0, len(s.m.downloads))
	for _, d := range s.m.downloads {
		list = append(list, d)
	}
	s.m.mu.Unlock()
	for _, d := range list {
		d.mu.Lock()
		same := d.Kind != "torrent" && strings.EqualFold(filepath.Join(d.Dir, d.FileName), p)
		d.mu.Unlock()
		if same {
			return d.ID
		}
	}
	return ""
}

var langNames = map[string]string{"eng": "English", "en": "English", "fre": "French", "fra": "French", "fr": "French", "spa": "Spanish", "es": "Spanish",
	"por": "Portuguese", "pt": "Portuguese", "ger": "German", "deu": "German", "de": "German", "ara": "Arabic", "ar": "Arabic", "chi": "Chinese", "zho": "Chinese",
	"zh": "Chinese", "hin": "Hindi", "hi": "Hindi", "jpn": "Japanese", "ja": "Japanese", "kor": "Korean", "ko": "Korean", "rus": "Russian", "ru": "Russian",
	"ita": "Italian", "it": "Italian", "swa": "Swahili", "sw": "Swahili", "hau": "Hausa", "ha": "Hausa", "yor": "Yoruba", "yo": "Yoruba", "twi": "Twi", "tw": "Twi",
	"aka": "Akan", "ak": "Akan", "ewe": "Ewe", "ee": "Ewe", "dut": "Dutch", "nld": "Dutch", "nl": "Dutch", "tur": "Turkish", "tr": "Turkish"}

func langName(l string) string {
	if l == "" || l == "und" {
		return ""
	}
	if n, ok := langNames[strings.ToLower(l)]; ok {
		return n
	}
	return strings.ToUpper(l)
}

// viewerWait: the open viewer window asks "anything new to show?".
func (s *Server) viewerWait(w http.ResponseWriter, r *http.Request) {
	if !s.viewerOK(w, r) {
		return
	}
	h := s.hub
	h.mu.Lock()
	h.waiting++
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.waiting--
		h.lastPoll = time.Now()
		h.mu.Unlock()
	}()
	deadline := time.After(25 * time.Second)
	for {
		h.mu.Lock()
		if id := h.pending; id != "" {
			h.pending = ""
			h.mu.Unlock()
			writeJSON(w, map[string]string{"f": id})
			return
		}
		h.mu.Unlock()
		select {
		case <-h.notify:
		case <-deadline:
			writeJSON(w, map[string]string{})
			return
		case <-r.Context().Done():
			return
		}
	}
}

// viewerAlive: the viewer is showing a download that's still going, so the
// download keeps working on what the player needs (preview.go).
func (s *Server) viewerAlive(w http.ResponseWriter, r *http.Request) {
	if !s.viewerOK(w, r) {
		return
	}
	e := s.entryFrom(r)
	if e != nil && e.DownloadID != "" && s.viewerSettings().Preview {
		if d := s.m.get(e.DownloadID); d != nil {
			d.mu.Lock()
			d.previewUntil = time.Now().Add(20 * time.Second)
			d.mu.Unlock()
		}
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) viewerPos(w http.ResponseWriter, r *http.Request) {
	if !s.viewerOK(w, r) {
		return
	}
	var req struct {
		F   string  `json:"f"`
		Pos float64 `json:"pos"`
	}
	if !decodePost(w, r, &req) {
		return
	}
	if e := s.hub.get(req.F); e != nil && s.viewerSettings().Remember && !e.Stream && e.TorrentIdx < 0 {
		s.hub.setPosition(e.Path, req.Pos)
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) viewerAct(w http.ResponseWriter, r *http.Request) {
	if !s.viewerOK(w, r) {
		return
	}
	var req struct {
		F      string `json:"f"`
		Action string `json:"action"`
		URL    string `json:"url"`
	}
	if !decodePost(w, r, &req) {
		return
	}
	e := s.hub.get(req.F)
	if e == nil {
		writeErr(w, "This file isn't available any more.")
		return
	}
	var d *Download
	if e.DownloadID != "" {
		d = s.m.get(e.DownloadID)
	}
	p := e.Path
	whole := false // this file is the whole download (not one file inside a torrent or folder)
	if d != nil {
		d.mu.Lock()
		final := filepath.Join(d.Dir, d.FileName)
		if d.Status == StDone && (e.Stream || strings.HasSuffix(p, ".part")) {
			p = final
		}
		whole = d.Kind != "torrent" && p == final
		d.mu.Unlock()
	}
	switch req.Action {
	case "external", "run":
		name := displayName(p)
		if !fileExists(p) {
			writeErr(w, "It can be opened in another program once it has finished downloading.")
			return
		}
		if strings.HasSuffix(strings.ToLower(name), ".blocked") {
			writeErr(w, "Jasqet blocked this file because it isn't what its name says.")
			return
		}
		if reason, _ := blockedFile(p, name); reason != "" {
			writeErr(w, reason)
			return
		}
		if req.Action == "run" && !s.appOrigin(r) {
			writeErr(w, "Programs can only be run from Jasqet's own windows.")
			return
		}
		if req.Action == "external" && isProgram(p, name) {
			writeJSON(w, map[string]string{"confirm": programWarning(name)})
			return
		}
		if err := openPath(p); err != nil {
			writeErr(w, friendlyError(err))
			return
		}
	case "with":
		if reason, _ := blockedFile(p, displayName(p)); reason != "" {
			writeErr(w, reason)
			return
		}
		openWith(p)
	case "folder":
		revealPath(p)
	case "delete":
		switch {
		case strings.HasSuffix(p, ".part") || e.Stream || (d != nil && !fileExists(p)):
			writeErr(w, "Delete this download from the Jasqet window.")
			return
		case whole:
			s.m.Delete(d, true)
		default: // one file: next to the downloads, or inside a torrent or folder
			if st, err := os.Stat(p); err != nil || st.IsDir() {
				writeErr(w, "Couldn't delete it.")
				return
			}
			if err := os.Remove(p); err != nil {
				writeErr(w, "Couldn't delete it: "+friendlyError(err))
				return
			}
		}
	case "verify":
		writeJSON(w, s.verify(r.Context(), d, p, whole))
		return
	case "redownload":
		if d == nil {
			writeErr(w, "This file isn't one of your downloads, so Jasqet doesn't know where it came from.")
			return
		}
		if !whole {
			writeErr(w, "This file is part of a bigger download. Re-download that from the Jasqet window.")
			return
		}
		d.mu.Lock()
		running, done := d.running, d.Status == StDone
		d.mu.Unlock()
		if running {
			writeErr(w, "It's downloading right now.")
			return
		}
		if done {
			os.Remove(p) // the damaged copy
		}
		s.m.Restart(d)
	case "resume":
		if d != nil {
			s.m.Resume(d)
		}
	case "openurl": // a link in a PDF, after you confirmed it
		u, err := url.Parse(req.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "mailto") {
			writeErr(w, "Jasqet only opens web and email links.")
			return
		}
		openPath(u.String()) // your normal browser / email app
	default:
		writeErr(w, "unknown action")
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// verify checks a file the viewer couldn't show: is it what it says, does it
// match its checksum, and do pieces of it match the website's copy?
func (s *Server) verify(ctx context.Context, d *Download, p string, whole bool) map[string]any {
	name := displayName(p)
	var notes []string
	damaged := false
	if reason, _ := blockedFile(p, name); reason != "" {
		return map[string]any{"message": reason, "damaged": true}
	}
	if d != nil && !whole {
		d.mu.Lock()
		tor := d.Kind == "torrent"
		d.mu.Unlock()
		if tor {
			return map[string]any{"message": "Every piece of a torrent is checked against its fingerprint as it arrives, so this is exactly the file the torrent contains. It may be damaged at the source, or in a format the built-in viewer can't play. Try Open externally.", "damaged": false}
		}
		d = nil
	}
	if d == nil {
		return map[string]any{"message": "It looks like a real " + strings.ToUpper(strings.TrimPrefix(extOf(name), ".")) + " file, but it may be damaged. Try Open externally: other players can often play damaged files.", "damaged": false}
	}
	d.mu.Lock()
	hasSum := d.Checksum != ""
	kind, size, resumable := d.Kind, d.Size, d.Resumable
	d.mu.Unlock()
	if hasSum {
		s.m.recheckSafety(d)
		d.mu.Lock()
		sf := d.Safety
		d.mu.Unlock()
		if sf != nil && sf.HashMatch == "bad" {
			damaged = true
			notes = append(notes, "Its checksum doesn't match, so the file is damaged or was changed.")
		} else if sf != nil && (sf.HashMatch == "ok" || sf.HashMatch == "site") {
			notes = append(notes, "Its checksum matches, so it's exactly the file the website published.")
		}
	}
	if kind == "" && resumable && size > 0 && !damaged {
		bad, checked := s.compareWithSite(ctx, d, p, size)
		switch {
		case checked == 0:
		case bad > 0:
			damaged = true
			notes = append(notes, fmt.Sprintf("%d of %d pieces Jasqet checked don't match the website's copy, so this download is damaged. Re-download it.", bad, checked))
		default:
			notes = append(notes, fmt.Sprintf("The %d pieces Jasqet checked match the website's copy, so the website's own file may be damaged or in a format the viewer can't play.", checked))
		}
	}
	if len(notes) == 0 {
		notes = append(notes, "Jasqet couldn't check it against the website. It may be damaged, or in a format the built-in viewer can't play. Try Open externally.")
	}
	return map[string]any{"message": strings.Join(notes, " "), "damaged": damaged}
}

// compareWithSite downloads 8 small pieces (256 KB in all) and compares them.
func (s *Server) compareWithSite(ctx context.Context, d *Download, p string, size int64) (bad, checked int) {
	f, err := os.Open(p)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.Size() != size {
		return 1, 1
	}
	d.mu.Lock()
	probe := &Download{URL: d.URL, Headers: d.Headers, Cookies: d.Cookies, User: d.User, Password: d.Password, Offset: d.Offset}
	d.mu.Unlock()
	const piece = 32 << 10
	for i := 0; i < 8; i++ {
		off := (size - piece) * int64(i) / 7
		if off < 0 {
			off = 0
		}
		n := min(piece, size-off)
		theirs, err := s.m.rangeBytes(ctx, probe, off, n)
		if err != nil || int64(len(theirs)) != n {
			continue
		}
		ours := make([]byte, n)
		if _, err := f.ReadAt(ours, off); err != nil {
			continue
		}
		checked++
		if !bytes.Equal(ours, theirs) {
			bad++
		}
	}
	return bad, checked
}

// ---------- serving files ----------

func mimeFor(name string) string {
	switch extOf(name) {
	case ".mp4", ".m4v", ".mov", ".3gp":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".webm":
		return "video/webm"
	case ".ogv":
		return "video/ogg"
	case ".mp3":
		return "audio/mpeg"
	case ".m4a":
		return "audio/mp4"
	case ".aac":
		return "audio/aac"
	case ".flac":
		return "audio/flac"
	case ".wav":
		return "audio/wav"
	case ".ogg", ".oga", ".opus":
		return "audio/ogg"
	case ".mka", ".weba":
		return "audio/webm"
	case ".jpg", ".jpeg", ".jfif":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".bmp":
		return "image/bmp"
	case ".svg":
		return "image/svg+xml"
	case ".avif":
		return "image/avif"
	case ".ico":
		return "image/x-icon"
	case ".pdf":
		return "application/pdf"
	}
	return "application/octet-stream"
}

// lockDown: if anything ever loads a file as a page, nothing in it can run
// and it can't fetch anything (this is also what locks SVG pictures down).
func lockDown(w http.ResponseWriter) {
	w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'; img-src data:; style-src 'unsafe-inline'; font-src data:")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
}

func (s *Server) viewerFile(w http.ResponseWriter, r *http.Request) {
	if !s.viewerOK(w, r) {
		return
	}
	e := s.entryFrom(r)
	if e == nil {
		http.NotFound(w, r)
		return
	}
	lockDown(w)
	name := displayName(e.Path)
	w.Header().Set("Content-Type", mimeFor(name))
	p := e.Path
	var d *Download
	if e.DownloadID != "" {
		d = s.m.get(e.DownloadID)
	}
	if d != nil {
		d.mu.Lock()
		done := d.Status == StDone
		size := d.Size
		tor := d.tor
		final := filepath.Join(d.Dir, d.FileName)
		d.mu.Unlock()
		switch {
		case !done && e.TorrentIdx >= 0:
			if tor == nil || tor.Info() == nil || e.TorrentIdx >= len(tor.Files()) {
				http.Error(w, "start the download to preview it", http.StatusServiceUnavailable)
				return
			}
			d.mu.Lock()
			d.previewUntil = time.Now().Add(20 * time.Second)
			d.tReadAt = time.Now()
			d.mu.Unlock()
			idx := e.TorrentIdx
			s.torrentPreview(d, idx)
			// pieces under the player come first
			rd := tor.Files()[idx].NewReader()
			defer rd.Close()
			rd.SetContext(r.Context())
			rd.SetResponsive()
			rd.SetReadahead(8 << 20)
			http.ServeContent(w, r, name, time.Time{}, &torrentPlayReader{Reader: rd, d: d, again: func() { s.torrentPreview(d, idx) }})
			return
		case !done && strings.HasSuffix(p, ".part"):
			if size <= 0 {
				http.Error(w, "the size isn't known yet", http.StatusServiceUnavailable)
				return
			}
			rd := &previewReader{ctx: r.Context(), m: s.m, d: d, size: size}
			defer rd.Close()
			defer s.m.previewDone(d, rd)
			http.ServeContent(w, r, name, time.Time{}, rd)
			return
		case done && strings.HasSuffix(p, ".part"):
			p = final
		}
	}
	if r.URL.Query().Get("remux") == "1" {
		if !s.remuxState(p).Ready {
			http.Error(w, "still preparing", http.StatusServiceUnavailable)
			return
		}
		p = s.remuxOut(p)
		now := time.Now()
		os.Chtimes(p, now, now) // in use: not one to tidy away
		w.Header().Set("Content-Type", "video/mp4")
	}
	f, err := openShared(p) // others may rename, move or delete it while it's shown
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	http.ServeContent(w, r, name, st.ModTime(), f)
}

// ---------- video-site downloads still in parts ----------

type streamTrack struct {
	files []string
	sizes []int64 // each part's size (when the whole track's size is known)
	size  int64   // whole track (0 = unknown: played as it arrives, no jumping ahead)
	first int     // the download's job number of files[0]
	audio bool
}

// streamTracks: the parts of a video-site download (video, and sound if it
// comes separately), so the viewer can play them while they arrive.
func streamTracks(d *Download) ([]streamTrack, string) {
	d.mu.Lock()
	dir := d.partsDir()
	audioOnly := strings.HasSuffix(strings.ToLower(d.FileName), ".m4a")
	d.mu.Unlock()
	plan := loadPlan(dir)
	if plan == nil {
		return nil, "Getting the video ready…"
	}
	if plan.Fallback {
		return nil, "This video is being fetched by the site helper, so it can be watched when it's finished."
	}
	var out []streamTrack
	job := 0
	for ti, t := range plan.Tracks {
		st := streamTrack{first: job, audio: t.Role == "audio" || (audioOnly && len(plan.Tracks) == 1)}
		ranged := t.Init == nil && t.SegStart == 0
		var want int64
		if t.Init != nil {
			st.files = append(st.files, filepath.Join(dir, fmt.Sprintf("t%d_init", ti)))
			st.sizes = append(st.sizes, t.Init.Len)
			job++
		}
		for si, sg := range t.Segs {
			st.files = append(st.files, filepath.Join(dir, fmt.Sprintf("t%d_%06d", ti, si)))
			st.sizes = append(st.sizes, sg.Len)
			if sg.Len <= 0 || sg.Off != want {
				ranged = false
			}
			want = sg.Off + sg.Len
			job++
		}
		if ranged {
			st.size = want
		} else {
			st.sizes = nil
		}
		if strings.Contains(strings.ToLower(t.Ext), "ts") || (len(t.Segs) > 0 && strings.Contains(strings.ToLower(t.Segs[0].URL), ".ts")) {
			return nil, "This video comes in pieces that can only be watched once they're joined. It plays as soon as the download finishes."
		}
		if len(st.files) > 0 && fileExists(st.files[0]) {
			if f := sniffFormat(st.files[0]); f != "fmp4" && f != "webm" && f != "adts" && f != "" {
				return nil, "This video comes in pieces that can only be watched once they're joined. It plays as soon as the download finishes."
			}
		}
		out = append(out, st)
	}
	return out, ""
}

// chunkReader reads a video-site download's parts as one file, waiting for
// parts that haven't arrived yet (and asking for them first).
type chunkReader struct {
	ctx context.Context
	m   *Manager
	d   *Download
	st  streamTrack
	pos int64
}

func (c *chunkReader) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		c.pos = off
	case io.SeekCurrent:
		c.pos += off
	case io.SeekEnd:
		c.pos = c.st.size + off
	}
	c.pos = max(c.pos, 0)
	return c.pos, nil
}

func (c *chunkReader) Read(p []byte) (int, error) {
	if c.pos >= c.st.size {
		return 0, io.EOF
	}
	var start int64
	k := 0
	for ; k < len(c.st.sizes); k++ {
		if c.pos < start+c.st.sizes[k] {
			break
		}
		start += c.st.sizes[k]
	}
	if k == len(c.st.sizes) {
		return 0, io.EOF
	}
	for waited := time.Duration(0); ; waited += 300 * time.Millisecond {
		f, err := os.Open(c.st.files[k])
		if err == nil {
			n, err := f.ReadAt(p[:min(int64(len(p)), start+c.st.sizes[k]-c.pos)], c.pos-start)
			f.Close()
			c.pos += int64(n)
			if err == io.EOF && n > 0 {
				err = nil
			}
			c.d.mu.Lock()
			c.d.previewWait = 0
			c.d.mu.Unlock()
			return n, err
		}
		c.d.mu.Lock()
		c.d.previewWait = c.pos + 1
		c.d.previewChunk = c.st.first + k + 1
		c.d.previewUntil = time.Now().Add(20 * time.Second)
		stopped := !c.d.running
		c.d.mu.Unlock()
		if stopped && waited > 5*time.Minute {
			return 0, errors.New("the download isn't running")
		}
		if c.m != nil && c.m.get(c.d.ID) == nil {
			return 0, errors.New("the download was removed")
		}
		select {
		case <-c.ctx.Done():
			c.d.mu.Lock()
			c.d.previewWait = 0
			c.d.mu.Unlock()
			return 0, c.ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// viewerStream plays a video-site download while its parts arrive.
func (s *Server) viewerStream(w http.ResponseWriter, r *http.Request) {
	if !s.viewerOK(w, r) {
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 4 {
		http.NotFound(w, r)
		return
	}
	e := s.hub.get(parts[2])
	ti, _ := strconv.Atoi(parts[3])
	if e == nil || e.DownloadID == "" {
		http.NotFound(w, r)
		return
	}
	d := s.m.get(e.DownloadID)
	if d == nil {
		http.NotFound(w, r)
		return
	}
	tracks, why := streamTracks(d)
	if why != "" || ti < 0 || ti >= len(tracks) {
		http.Error(w, why, http.StatusServiceUnavailable)
		return
	}
	st := tracks[ti]
	lockDown(w)
	ct := "video/mp4"
	if len(st.files) > 0 && fileExists(st.files[0]) && sniffFormat(st.files[0]) == "webm" {
		ct = "video/webm"
	}
	if st.audio {
		ct = strings.Replace(ct, "video/", "audio/", 1)
	}
	w.Header().Set("Content-Type", ct)
	if st.size > 0 {
		http.ServeContent(w, r, "stream", time.Time{}, &chunkReader{ctx: r.Context(), m: s.m, d: d, st: st})
		return
	}
	// sizes unknown: send the parts one after another as they arrive
	if rg := r.Header.Get("Range"); rg != "" && !strings.HasPrefix(rg, "bytes=0-") {
		http.Error(w, "can't jump ahead while downloading", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 256<<10)
	for k, p := range st.files {
		for !fileExists(p) {
			if s.m.get(d.ID) == nil {
				return // the download was removed
			}
			d.mu.Lock()
			d.previewWait = 1
			d.previewChunk = st.first + k + 1
			d.previewUntil = time.Now().Add(20 * time.Second)
			d.mu.Unlock()
			select {
			case <-r.Context().Done():
				d.mu.Lock()
				d.previewWait = 0
				d.mu.Unlock()
				return
			case <-time.After(300 * time.Millisecond):
			}
		}
		d.mu.Lock()
		d.previewWait = 0
		d.mu.Unlock()
		f, err := os.Open(p)
		if err != nil {
			return
		}
		_, err = io.CopyBuffer(w, f, buf)
		f.Close()
		if err != nil {
			return
		}
		if fl != nil {
			fl.Flush()
		}
	}
}

// ---------- subtitles and covers ----------

func (s *Server) viewerSubs(w http.ResponseWriter, r *http.Request) {
	if !s.viewerOK(w, r) {
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 4 {
		http.NotFound(w, r)
		return
	}
	e := s.hub.get(parts[2])
	n, _ := strconv.Atoi(parts[3])
	if e == nil || n < 0 {
		http.NotFound(w, r)
		return
	}
	video := strings.TrimSuffix(e.Path, ".part")
	var files []string
	if s.viewerSettings().Subs {
		files = subtitleFilesFor(video)
	}
	var cues []subCue
	var err error
	if n < len(files) {
		cues, err = safeSubFile(files[n])
	} else {
		k := n - len(files)
		var tr *MediaTrack
		for _, t := range safeTracks(e.Path) {
			if t.Type == "subtitle" && t.text != "" {
				if k == 0 {
					tt := t
					tr = &tt
					break
				}
				k--
			}
		}
		if tr == nil {
			http.NotFound(w, r)
			return
		}
		key := fmt.Sprintf("%s#%d", e.Path, tr.num)
		s.hub.mu.Lock()
		c, ok := s.hub.subs[key]
		s.hub.mu.Unlock()
		if !ok {
			if c, err = safeMKVSubs(e.Path, tr.num, tr.text); err == nil {
				s.hub.mu.Lock()
				if len(s.hub.subs) > 50 {
					s.hub.subs = map[string][]subCue{}
				}
				s.hub.subs[key] = c
				s.hub.mu.Unlock()
			}
		}
		cues = c
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(toVTT(cues)))
}

func (s *Server) viewerCover(w http.ResponseWriter, r *http.Request) {
	if !s.viewerOK(w, r) {
		return
	}
	e := s.entryFrom(r)
	if e == nil {
		http.NotFound(w, r)
		return
	}
	tg := safeTags(e.Path)
	if tg == nil || len(tg.Cover) == 0 {
		http.NotFound(w, r)
		return
	}
	lockDown(w)
	switch sniff(tg.Cover) {
	case kJPG:
		w.Header().Set("Content-Type", "image/jpeg")
	case kPNG:
		w.Header().Set("Content-Type", "image/png")
	case kGIF:
		w.Header().Set("Content-Type", "image/gif")
	case kWEBP:
		w.Header().Set("Content-Type", "image/webp")
	default:
		http.NotFound(w, r)
		return
	}
	w.Write(tg.Cover)
}

// ---------- TS / FLV: re-wrapped as MP4 for the viewer ----------

func viewCacheDir() string { return filepath.Join(os.TempDir(), "Jasqet-viewer") }

func (s *Server) remuxOut(p string) string {
	key := p
	if st, err := os.Stat(p); err == nil {
		key = fmt.Sprintf("%s|%d|%d", p, st.Size(), st.ModTime().UnixNano())
	}
	sum := sha1.Sum([]byte(key))
	return filepath.Join(viewCacheDir(), hex.EncodeToString(sum[:10])+".mp4")
}

func (s *Server) remuxState(p string) *viewRemux {
	out := s.remuxOut(p)
	if fileExists(out) {
		return &viewRemux{Ready: true, Progress: 1}
	}
	s.hub.mu.Lock()
	job := s.hub.remux[out]
	s.hub.mu.Unlock()
	if job == nil {
		return &viewRemux{}
	}
	select {
	case <-job.done:
		if job.err != nil {
			return &viewRemux{Error: job.err.Error()}
		}
		// it was ready but the copy has been tidied away: make it again
		s.hub.mu.Lock()
		if s.hub.remux[out] == job {
			delete(s.hub.remux, out)
		}
		s.hub.mu.Unlock()
		return &viewRemux{}
	default:
		job.mu.Lock()
		defer job.mu.Unlock()
		return &viewRemux{Progress: job.progress}
	}
}

func (s *Server) viewerPrepare(w http.ResponseWriter, r *http.Request) {
	if !s.viewerOK(w, r) {
		return
	}
	var req struct {
		F string `json:"f"`
	}
	if !decodePost(w, r, &req) {
		return
	}
	e := s.hub.get(req.F)
	if e == nil || engineFor(e.Path) != "remux" || !fileExists(e.Path) {
		writeErr(w, "This file isn't available any more.")
		return
	}
	out := s.remuxOut(e.Path)
	s.hub.mu.Lock()
	job := s.hub.remux[out]
	if job == nil && !fileExists(out) {
		job = &remuxJob{done: make(chan struct{}), out: out}
		s.hub.remux[out] = job
		go s.runRemux(e.Path, job)
	}
	s.hub.mu.Unlock()
	writeJSON(w, s.remuxState(e.Path))
}

func (s *Server) runRemux(p string, job *remuxJob) {
	defer close(job.done)
	defer func() {
		if r := recover(); r != nil { // a broken file must never stop the downloads
			job.err = errors.New("this video is damaged or in a format the viewer can't read")
		}
	}()
	dir := viewCacheDir()
	os.MkdirAll(dir, 0o755)
	if entries, err := os.ReadDir(dir); err == nil { // tidy up copies older than a day
		for _, en := range entries {
			if info, err := en.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
				os.RemoveAll(filepath.Join(dir, en.Name()))
			}
		}
	}
	work, err := os.MkdirTemp(dir, "work-")
	if err != nil {
		job.err = err
		return
	}
	defer os.RemoveAll(work)
	st, err := os.Stat(p)
	if err != nil {
		job.err = err
		return
	}
	format := sniffFormat(p)
	if format != "ts" && format != "flv" {
		job.err = errors.New("this isn't a TS or FLV video the viewer can prepare")
		return
	}
	groups := []trackFiles{{role: "main", files: []string{p}, format: format, size: st.Size()}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	outBase := filepath.Join(work, "view")
	ext, _, err := remuxPure(ctx, groups, false, nil, work, outBase, st.Size(), func(f float64) {
		job.mu.Lock()
		job.progress = f
		job.mu.Unlock()
	})
	if err != nil {
		job.err = fmt.Errorf("couldn't prepare this video for the viewer (%s)", friendlyError(err))
		return
	}
	job.err = os.Rename(outBase+ext, job.out)
}

// ---------- torrents ----------

type torrentHandle struct {
	t   *torrent.Torrent
	idx int
}

// head reads the first bytes of a torrent file if they've arrived.
func (h *torrentHandle) head(ctx context.Context) []byte {
	if h.t == nil || h.t.Info() == nil || h.idx >= len(h.t.Files()) {
		return nil
	}
	f := h.t.Files()[h.idx]
	if f.Length() == 0 || f.BytesCompleted() == 0 {
		return nil
	}
	ps := f.State()
	if len(ps) == 0 || !ps[0].Complete {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	rd := f.NewReader()
	defer rd.Close()
	rd.SetContext(ctx)
	b := make([]byte, min(4096, f.Length()))
	n, _ := io.ReadFull(rd, b)
	if n == 0 {
		return nil
	}
	return b[:n]
}

// torrentPreview: while the viewer shows a file from a torrent that's still
// downloading, its pieces come in order from the start (and its last pieces
// first, for an MP4's index) instead of rarest-first, so it can be watched.
// Everything else in the torrent waits until the viewer closes.
func (s *Server) torrentPreview(d *Download, idx int) {
	d.mu.Lock()
	t := d.tor
	if t == nil || t.Info() == nil || d.tPreview == t || idx < 0 || idx >= len(t.Files()) {
		d.mu.Unlock()
		return
	}
	d.tPreview = t
	d.mu.Unlock()
	go func() {
		marked := map[int]bool{}
		defer func() {
			recover() // the torrent was closed under us
			d.mu.Lock()
			if d.tPreview == t {
				d.tPreview = nil
			}
			skip := map[int]bool{}
			for _, k := range d.Skip {
				skip[k] = true
			}
			current := d.tor == t
			d.mu.Unlock()
			if !current {
				return // paused or restarted: that torrent is gone
			}
			func() {
				defer func() { recover() }()
				for i := range marked {
					t.Piece(i).SetPriority(torrent.PiecePriorityNone)
				}
				for i, f := range t.Files() { // back to normal: the files you chose
					if skip[i] {
						f.SetPriority(torrent.PiecePriorityNone)
					} else {
						f.SetPriority(torrent.PiecePriorityNormal)
					}
				}
			}()
		}()
		files := t.Files()
		for _, f := range files {
			f.SetPriority(torrent.PiecePriorityNone)
		}
		f := files[idx]
		begin, end := f.BeginPieceIndex(), f.EndPieceIndex()
		const ahead = 48 // pieces fetched in order ahead of what's complete
		for {
			d.mu.Lock()
			// only while the player reads: paused or closed, the torrent goes back to normal
			reading := d.tReading > 0 || time.Since(d.tReadAt) < 20*time.Second
			active := reading && d.tor == t && d.Status != StDone
			d.mu.Unlock()
			if !active {
				return
			}
			k := begin
			for k < end && t.Piece(k).State().Complete {
				k++
			}
			if k >= end {
				return
			}
			want := map[int]bool{}
			for i := k; i < min(end, k+ahead); i++ {
				want[i] = true
			}
			for i := max(begin, end-2); i < end; i++ {
				want[i] = true
			}
			for i := range want {
				if !marked[i] {
					t.Piece(i).SetPriority(torrent.PiecePriorityHigh)
					marked[i] = true
				}
			}
			time.Sleep(time.Second)
		}
	}()
}

// torrentPlayReader notes when the player reads, so a torrent's pieces stay
// in order only while it plays (and come in order again when it plays on).
type torrentPlayReader struct {
	torrent.Reader
	d     *Download
	again func()
}

func (t *torrentPlayReader) Read(p []byte) (int, error) {
	t.mark(1)
	n, err := t.Reader.Read(p)
	t.mark(-1)
	return n, err
}

func (t *torrentPlayReader) mark(k int) {
	t.d.mu.Lock()
	t.d.tReading += k
	t.d.tReadAt = time.Now()
	off := t.d.tPreview == nil
	t.d.mu.Unlock()
	if off && k > 0 {
		t.again()
	}
}

// The file readers below look inside downloaded files, which may be damaged
// or made to trip them up; a problem there only means "can't read that part".
func safeTracks(p string) (t []MediaTrack) {
	defer func() {
		if recover() != nil {
			t = nil
		}
	}()
	return probeTracks(p)
}

func safeTags(p string) (t *MediaTags) {
	defer func() {
		if recover() != nil {
			t = nil
		}
	}()
	return readTags(p)
}

func safeSubFile(p string) (c []subCue, err error) {
	defer func() {
		if recover() != nil {
			c, err = nil, errors.New("couldn't read these subtitles")
		}
	}()
	return parseSubtitleFile(p)
}

func safeMKVSubs(p string, track int, kind string) (c []subCue, err error) {
	defer func() {
		if recover() != nil {
			c, err = nil, errors.New("couldn't read these subtitles")
		}
	}()
	return mkvSubtitles(p, track, kind)
}
