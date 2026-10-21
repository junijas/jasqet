package main

// Helper tools for YouTube (and 1,000+ other video sites): yt-dlp, plus Deno,
// the small JavaScript engine yt-dlp now needs to read YouTube pages.
// Jasqet downloads both on first use and keeps yt-dlp up to date.

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type ToolsStatus struct {
	State    string  `json:"state"` // "missing", "installing", "ready", "error"
	Progress float64 `json:"progress"`
	Message  string  `json:"message,omitempty"`
	YtDlp    string  `json:"ytdlp,omitempty"`
	Deno     string  `json:"deno,omitempty"`
	SizeMB   int     `json:"sizeMB"`
}

type toolsMgr struct {
	mu         sync.Mutex
	status     ToolsStatus
	lastUpdate time.Time
	installing bool
	versionsAt time.Time
}

func toolsDir() string { return filepath.Join(filepath.Dir(stateFilePath()), "tools") }

// Where the helper tools come from (tests use their own server).
var (
	ytdlpRelease = "https://github.com/yt-dlp/yt-dlp/releases/latest/download/"
	denoRelease  = "https://github.com/denoland/deno/releases/latest/download/"
)

func exeName(n string) string {
	if runtime.GOOS == "windows" {
		return n + ".exe"
	}
	return n
}

func ytdlpPath() string { return filepath.Join(toolsDir(), exeName("yt-dlp")) }

func ytdlpAsset() string {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "windows/arm64":
		return "yt-dlp_arm64.exe"
	case "windows/amd64", "windows/386":
		return "yt-dlp.exe"
	case "darwin/amd64", "darwin/arm64":
		return "yt-dlp_macos"
	case "linux/arm64":
		return "yt-dlp_linux_aarch64"
	}
	return "yt-dlp_linux"
}

func denoAsset() string {
	arch := "x86_64"
	if runtime.GOARCH == "arm64" {
		arch = "aarch64"
	}
	switch runtime.GOOS {
	case "windows":
		return "deno-" + arch + "-pc-windows-msvc.zip"
	case "darwin":
		return "deno-" + arch + "-apple-darwin.zip"
	}
	return "deno-" + arch + "-unknown-linux-gnu.zip"
}

// denoPath finds Deno: our own copy, or one already installed on the computer.
func denoPath() string {
	own := filepath.Join(toolsDir(), exeName("deno"))
	if fileExists(own) {
		return own
	}
	if p, err := exec.LookPath("deno"); err == nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".deno", "bin", exeName("deno"))
		if fileExists(p) {
			return p
		}
	}
	return ""
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Size() > 0
}

func (t *toolsMgr) ready() bool { return fileExists(ytdlpPath()) && denoPath() != "" }

func (m *Manager) ToolsStatus() ToolsStatus {
	t := m.tools
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.status
	switch {
	case t.installing:
		st.State = "installing"
	case t.ready():
		st.State = "ready"
		if t.versionsAt.IsZero() || time.Since(t.versionsAt) > 10*time.Minute {
			t.versionsAt = time.Now()
			go readToolVersions(m)
		}
	case st.State != "error":
		st.State = "missing"
	}
	st.SizeMB = 0
	if !fileExists(ytdlpPath()) {
		st.SizeMB += 18
	}
	if denoPath() == "" {
		st.SizeMB += 45
	}
	return st
}

// readToolVersions runs the tools to ask their versions (tests, whose tools
// are pretend files, leave that out).
var readToolVersions = (*Manager).readVersions

func (m *Manager) readVersions() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	y := runQuiet(ctx, ytdlpPath(), "--version")
	d := runQuiet(ctx, denoPath(), "--version")
	if i := strings.IndexByte(d, '\n'); i > 0 {
		d = d[:i]
	}
	m.tools.mu.Lock()
	m.tools.status.YtDlp = strings.TrimSpace(y)
	m.tools.status.Deno = strings.TrimSpace(strings.TrimPrefix(d, "deno "))
	m.tools.mu.Unlock()
}

func runQuiet(ctx context.Context, exe string, args ...string) string {
	if exe == "" {
		return ""
	}
	cmd := exec.CommandContext(ctx, exe, args...)
	hideWindow(cmd)
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

// InstallTools fetches yt-dlp and Deno in the background.
func (m *Manager) InstallTools() {
	t := m.tools
	t.mu.Lock()
	if t.installing {
		t.mu.Unlock()
		return
	}
	t.installing = true
	t.status = ToolsStatus{State: "installing", Message: "Starting…"}
	t.mu.Unlock()

	go func() {
		err := m.installTools(context.Background())
		t.mu.Lock()
		t.installing = false
		if err != nil {
			t.status.State = "error"
			t.status.Message = "Setup failed: " + friendlyError(err) + ". Check your internet and try again."
		} else {
			t.status.State = "ready"
			t.status.Message = ""
			t.status.Progress = 1
			t.versionsAt = time.Time{}
		}
		t.mu.Unlock()
	}()
}

// toolsWait: the helper tools set themselves up the first time something
// needs them (a video or subtitle download, a watched channel). This starts
// that if needed and waits for it. Only looking a video up never starts it.
func (m *Manager) toolsWait(ctx context.Context) error {
	if m.tools.ready() {
		return nil
	}
	m.InstallTools()
	for {
		switch st := m.ToolsStatus(); st.State {
		case "ready":
			return nil
		case "error":
			return errors.New(st.Message)
		}
		select {
		case <-ctx.Done():
			return ctx.Err() // the setup carries on by itself
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// toolsFor: the same for a download, which says meanwhile what it waits for.
func (m *Manager) toolsFor(ctx context.Context, d *Download) error {
	if m.tools.ready() {
		return nil
	}
	note := fmt.Sprintf("Setting up YouTube support, one time only (two free helper tools, about %d MB)", m.ToolsStatus().SizeMB)
	d.mu.Lock()
	d.Note = note
	d.mu.Unlock()
	err := m.toolsWait(ctx)
	d.mu.Lock()
	if d.Note == note {
		d.Note = ""
	}
	d.mu.Unlock()
	return err
}

func (m *Manager) installTools(ctx context.Context) error {
	dir := toolsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	type job struct {
		label, url, dest string
		unzip            string // file to take out of the zip
		sums, asset      string // the checksum the project publishes for it
	}
	var jobs []job
	if !fileExists(ytdlpPath()) {
		rel := ytdlpRelease
		jobs = append(jobs, job{"yt-dlp", rel + ytdlpAsset(), ytdlpPath(), "", rel + "SHA2-256SUMS", ytdlpAsset()})
	}
	if denoPath() == "" {
		rel := denoRelease
		jobs = append(jobs, job{"Deno", rel + denoAsset(), filepath.Join(dir, "deno-download.zip"), exeName("deno"),
			rel + denoAsset() + ".sha256sum", denoAsset()})
	}
	for i, j := range jobs {
		base, span := float64(i)/float64(len(jobs)), 1/float64(len(jobs))
		m.setToolMsg(fmt.Sprintf("Downloading %s…", j.label), base)
		tmp := j.dest + ".part"
		err := m.fetchToFile(ctx, j.url, tmp, func(done, total int64) {
			if total > 0 {
				m.setToolMsg(fmt.Sprintf("Downloading %s… %s of %s", j.label, humanSize(done), humanSize(total)), base+span*float64(done)/float64(total))
			}
		})
		if err != nil {
			return err
		}
		if err := m.checkPublished(ctx, tmp, j.sums, j.asset); err != nil {
			os.Remove(tmp) // damaged, or left from an older version: never run it
			return fmt.Errorf("%s: %w", j.label, err)
		}
		if j.unzip != "" {
			m.setToolMsg("Unpacking "+j.label+"…", base+span)
			if err := unzipOne(tmp, j.unzip, filepath.Join(dir, j.unzip)); err != nil {
				return err
			}
			os.Remove(tmp)
			continue
		}
		os.Remove(j.dest)
		if err := os.Rename(tmp, j.dest); err != nil {
			return err
		}
		os.Chmod(j.dest, 0o755)
	}
	m.tools.mu.Lock()
	m.tools.lastUpdate = time.Now()
	m.tools.mu.Unlock()
	return nil
}

// checkPublished compares a downloaded helper with the SHA-256 its project
// publishes next to it.
func (m *Manager) checkPublished(ctx context.Context, file, sumsURL, asset string) error {
	body, _, _, err := m.fetchSmall(ctx, sumsURL, reqOpts{}, 256<<10)
	if err != nil {
		return fmt.Errorf("couldn't get its published checksum (%s)", friendlyError(err))
	}
	want := checksumFor(body, map[string]bool{strings.ToLower(asset): true}, false)
	if !strings.HasPrefix(want, "sha256:") {
		return errors.New("its published checksum couldn't be read")
	}
	got, err := hashFile(ctx, file, "sha256")
	if err != nil {
		return err
	}
	if "sha256:"+got != want {
		return errors.New("the download doesn't match its published checksum. Try again")
	}
	return nil
}

func (m *Manager) setToolMsg(msg string, p float64) {
	m.tools.mu.Lock()
	m.tools.status.Message = msg
	m.tools.status.Progress = p
	m.tools.mu.Unlock()
}

// fetchToFile downloads a file, continuing after dropped connections.
func (m *Manager) fetchToFile(ctx context.Context, rawURL, dest string, progress func(done, total int64)) error {
	var lastErr error
	for attempt := 0; attempt < 8; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		var have int64
		if st, err := os.Stat(dest); err == nil {
			have = st.Size()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "Jasqet/"+version)
		if have > 0 {
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
		}
		resp, err := m.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.StatusCode >= 400 {
			resp.Body.Close()
			lastErr = fmt.Errorf("server replied %s", resp.Status)
			if resp.StatusCode == 416 {
				return nil
			}
			continue
		}
		flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
		total := resp.ContentLength
		if resp.StatusCode == http.StatusPartialContent {
			total = parseContentRangeTotal(resp.Header.Get("Content-Range"))
		} else {
			flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
			have = 0
		}
		f, err := os.OpenFile(dest, flags, 0o644)
		if err != nil {
			resp.Body.Close()
			return err
		}
		buf := make([]byte, 128*1024)
		done := have
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := f.Write(buf[:n]); werr != nil {
					rerr = werr
				}
				done += int64(n)
				progress(done, total)
			}
			if rerr == io.EOF {
				rerr = nil
				break
			}
			if rerr != nil {
				lastErr = rerr
				break
			}
		}
		f.Close()
		resp.Body.Close()
		if lastErr == nil || (total > 0 && done >= total) {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		lastErr = nil
		if total > 0 && done < total {
			lastErr = errors.New("connection closed early")
		}
	}
	return lastErr
}

func unzipOne(zipPath, name, dest string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if filepath.Base(f.Name) != name {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		tmp := dest + ".tmp"
		out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
		if err != nil {
			rc.Close()
			return err
		}
		_, err = io.Copy(out, rc)
		rc.Close()
		out.Close()
		if err != nil {
			return err
		}
		os.Remove(dest)
		return os.Rename(tmp, dest)
	}
	return fmt.Errorf("%s not found in the download", name)
}

// UpdateYtDlp asks yt-dlp to update itself (YouTube changes often).
func (m *Manager) UpdateYtDlp(ctx context.Context) error {
	if !fileExists(ytdlpPath()) {
		return errors.New("yt-dlp isn't installed yet")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	// the proxy goes in yt-dlp's environment (see ytdlpCmd), and no config
	// file of yours can put another one in its place
	cmd := m.ytdlpCmd(ctx, "--ignore-config", "-U")
	out, err := cmd.CombinedOutput()
	m.tools.mu.Lock()
	m.tools.lastUpdate = time.Now()
	m.tools.versionsAt = time.Time{}
	m.tools.mu.Unlock()
	if err != nil {
		return fmt.Errorf("update failed: %s", lastLine(string(out)))
	}
	return nil
}

// maybeUpdate runs a quiet yt-dlp update at most every few days.
func (m *Manager) maybeUpdate() {
	m.tools.mu.Lock()
	due := !m.tools.installing && time.Since(m.tools.lastUpdate) > 72*time.Hour
	if due {
		m.tools.lastUpdate = time.Now()
	}
	m.tools.mu.Unlock()
	if due && fileExists(ytdlpPath()) {
		go m.UpdateYtDlp(context.Background())
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}
