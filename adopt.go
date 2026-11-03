package main

// Files the browser saved itself. A download a web page makes inside itself
// (an Outlook or Gmail attachment, an export: a blob: link) has no address
// Jasqet could fetch, so the browser saves it, and then the extension hands
// Jasqet the saved file. Jasqet moves it into its own folder and lists it as
// finished, so it gets the same checks, rules and notifications as the rest.
//
// An extension can only hand over a file the browser has just saved, straight
// into a downloads folder: not any file on the computer.

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type AdoptRequest struct {
	Path string `json:"path"` // where the browser saved it
	Page string `json:"page"` // the page it came from
}

// adoptMaxAge: the browser saved it just now; an older file isn't a download
// it just finished.
const adoptMaxAge = 10 * time.Minute

// names a browser gives a file while it's still downloading
var unfinishedExts = map[string]bool{".crdownload": true, ".part": true, ".partial": true, ".download": true, ".tmp": true}

// samePath: the same file or folder (Windows and macOS don't mind capitals).
func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if runtime.GOOS == "linux" {
		return a == b
	}
	return strings.EqualFold(a, b)
}

// browserDownloadDirs: where browsers save downloads unless you choose.
var browserDownloadDirs = func() []string { // tests point it elsewhere

	var dirs []string
	if d := knownDownloadsDir(); d != "" {
		dirs = append(dirs, d)
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, "Downloads"))
	}
	return dirs
}

// Adopt takes a file the browser has just saved: moves it into Jasqet's
// folder and lists it as a finished download. The file stays where it is if
// anything is wrong.
func (m *Manager) Adopt(r AdoptRequest) (*Download, error) {
	src := filepath.Clean(r.Path)
	if !filepath.IsAbs(src) {
		return nil, errors.New("Jasqet needs where the browser saved the file")
	}
	st, err := os.Lstat(src)
	if err != nil || !st.Mode().IsRegular() { // not a folder, link or device
		return nil, errors.New("The browser's copy isn't there any more")
	}
	if time.Since(st.ModTime()) > adoptMaxAge || unfinishedExts[strings.ToLower(filepath.Ext(src))] {
		return nil, errors.New("Only a file the browser has just finished saving can go to Jasqet")
	}

	m.mu.Lock()
	dirs := append(browserDownloadDirs(), m.settings.DownloadDir)
	taken := false
	for _, o := range m.downloads {
		o.mu.Lock()
		taken = taken || (o.FileName != "" && samePath(filepath.Join(o.Dir, o.FileName), src))
		o.mu.Unlock()
	}
	m.mu.Unlock()
	if taken {
		return nil, errors.New("Jasqet already has this file")
	}
	inDownloads := false
	for _, d := range dirs {
		inDownloads = inDownloads || (d != "" && samePath(filepath.Dir(src), d))
	}
	if !inDownloads {
		return nil, errors.New("Jasqet only takes files saved in your Downloads folder")
	}

	page := r.Page
	if u, err := url.Parse(page); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		page = ""
	}
	d := &Download{
		ID: newID(), URL: page, Page: page, FileName: sanitizeFilename(filepath.Base(src)),
		Size: st.Size(), Resumable: true, FromBrowser: true, AddedAt: time.Now(),
	}
	m.placeFile(d) // the folder (by category) and a free name
	d.mu.Lock()
	dst := filepath.Join(d.Dir, d.FileName)
	d.mu.Unlock()
	os.Remove(dst + ".part") // placeFile's placeholder: nothing downloads into it
	if samePath(filepath.Dir(src), filepath.Dir(dst)) {
		dst = src // already in Jasqet's folder: it stays as it is
	} else if err := moveFile(src, dst); err != nil {
		return nil, err
	}
	d.mu.Lock()
	d.FileName = filepath.Base(dst)
	d.Status, d.CompletedAt = StDone, time.Now()
	d.mu.Unlock()

	m.mu.Lock()
	m.downloads[d.ID] = d
	m.mu.Unlock()
	m.saveSoon()
	go m.afterDownload(d) // the safety check, your rules, notifications
	return d, nil
}
