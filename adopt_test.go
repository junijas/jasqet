package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// adoptSetup: a manager, and a folder standing in for the browser's Downloads.
func adoptSetup(t *testing.T) (*Manager, string) {
	t.Helper()
	m := newTestManager(t)
	browser := tempDirRetry(t)
	old := browserDownloadDirs
	browserDownloadDirs = func() []string { return []string{browser} }
	t.Cleanup(func() { browserDownloadDirs = old })
	return m, browser
}

// settled: waits for the checks after a finished download, so nothing is
// still using its files when the test ends.
func settled(t *testing.T, d *Download) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		d.mu.Lock()
		s := d.Safety
		d.mu.Unlock()
		if s != nil && !s.Checking {
			return
		}
	}
	t.Fatal("the checks after the download didn't finish")
}

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAdoptMovesBrowserFileIntoJasqet(t *testing.T) {
	m, browser := adoptSetup(t)
	src := filepath.Join(browser, "Q3 report.pptx")
	writeFile(t, src, "slides")
	d, err := m.Adopt(AdoptRequest{Path: src, Page: "https://outlook.cloud.microsoft/mail/"})
	if err != nil {
		t.Fatal(err)
	}
	settled(t, d)
	_, s := m.Snapshot()
	dst := filepath.Join(s.DownloadDir, "Q3 report.pptx")
	if b, err := os.ReadFile(dst); err != nil || string(b) != "slides" {
		t.Fatalf("file not moved into Jasqet's folder: %v %q", err, b)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("the browser's copy is still there")
	}
	list, _ := m.Snapshot()
	if len(list) != 1 || list[0].Status != StDone || list[0].Size != 6 || list[0].Downloaded != 6 || !list[0].FromBrowser || list[0].FileName != "Q3 report.pptx" {
		t.Fatalf("not listed as a finished download: %+v", list)
	}
	if list[0].Page != "https://outlook.cloud.microsoft/mail/" {
		t.Fatalf("page = %q", list[0].Page)
	}
}

func TestAdoptKeepsBothWhenNameTaken(t *testing.T) {
	m, browser := adoptSetup(t)
	_, s := m.Snapshot()
	os.MkdirAll(s.DownloadDir, 0o755)
	writeFile(t, filepath.Join(s.DownloadDir, "a.pdf"), "older")
	writeFile(t, filepath.Join(browser, "a.pdf"), "newer")
	d, err := m.Adopt(AdoptRequest{Path: filepath.Join(browser, "a.pdf")})
	if err != nil {
		t.Fatal(err)
	}
	settled(t, d)
	if b, _ := os.ReadFile(filepath.Join(s.DownloadDir, "a.pdf")); string(b) != "older" {
		t.Fatal("the file already there was overwritten")
	}
	d.mu.Lock()
	name := d.FileName
	d.mu.Unlock()
	if b, _ := os.ReadFile(filepath.Join(s.DownloadDir, name)); name == "a.pdf" || string(b) != "newer" {
		t.Fatalf("new file not saved beside it: %q %q", name, b)
	}
}

func TestAdoptInJasqetsOwnFolderStays(t *testing.T) {
	m, _ := adoptSetup(t)
	_, s := m.Snapshot()
	os.MkdirAll(s.DownloadDir, 0o755)
	src := filepath.Join(s.DownloadDir, "export.csv")
	writeFile(t, src, "a,b")
	d, err := m.Adopt(AdoptRequest{Path: src})
	if err != nil {
		t.Fatal(err)
	}
	settled(t, d)
	d.mu.Lock()
	got := filepath.Join(d.Dir, d.FileName)
	d.mu.Unlock()
	if got != src {
		t.Fatalf("moved within its own folder: %s", got)
	}
	if _, err := m.Adopt(AdoptRequest{Path: src}); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("the same file was taken twice: %v", err)
	}
}

func TestAdoptRefuses(t *testing.T) {
	m, browser := adoptSetup(t)
	elsewhere := tempDirRetry(t)
	old := filepath.Join(browser, "old.zip")
	writeFile(t, old, "x")
	past := time.Now().Add(-time.Hour)
	os.Chtimes(old, past, past)
	outside := filepath.Join(elsewhere, "secret.txt")
	writeFile(t, outside, "x")
	unfinished := filepath.Join(browser, "big.iso.crdownload")
	writeFile(t, unfinished, "x")
	folder := filepath.Join(browser, "folder")
	os.Mkdir(folder, 0o755)
	for name, p := range map[string]string{
		"an old file":         old,
		"a file elsewhere":    outside,
		"an unfinished file":  unfinished,
		"a folder":            folder,
		"a missing file":      filepath.Join(browser, "gone.pdf"),
		"a relative path":     "report.pdf",
		"a path out of a dir": filepath.Join(browser, "..", filepath.Base(elsewhere), "secret.txt"),
	} {
		if _, err := m.Adopt(AdoptRequest{Path: p}); err == nil {
			t.Errorf("%s was taken", name)
		}
	}
	for _, p := range []string{old, outside, unfinished} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was moved or removed", p)
		}
	}
	if list, _ := m.Snapshot(); len(list) != 0 {
		t.Fatalf("something was listed: %+v", list)
	}
}

func TestAdoptedDownloadIsntDownloadedAgain(t *testing.T) {
	m, browser := adoptSetup(t)
	src := filepath.Join(browser, "mail.eml")
	writeFile(t, src, "hello")
	d, err := m.Adopt(AdoptRequest{Path: src, Page: "javascript:alert(1)"})
	if err != nil {
		t.Fatal(err)
	}
	settled(t, d)
	m.Restart(d)
	d.mu.Lock()
	st, url := d.Status, d.URL
	d.mu.Unlock()
	if st != StDone {
		t.Fatalf("Download again restarted a file the browser saved: %s", st)
	}
	if url != "" {
		t.Fatalf("a page that isn't a web address was kept: %q", url)
	}
}

func TestAdoptThroughTheAPI(t *testing.T) {
	s, h := newTestServer(t)
	browser := tempDirRetry(t)
	old := browserDownloadDirs
	browserDownloadDirs = func() []string { return []string{browser} }
	defer func() { browserDownloadDirs = old }()
	const ext = "chrome-extension://abcdefghijklmnopabcdefghijklmnop"
	src := filepath.Join(browser, "invoice.pdf")
	writeFile(t, src, "pdf")
	body, _ := json.Marshal(AdoptRequest{Path: src, Page: "https://mail.example/"})

	// an extension you haven't allowed can't hand over files
	if w := call(h, "POST", "/api/adopt", ext, string(body)); w.Code != http.StatusForbidden {
		t.Fatalf("unknown extension: %d %s", w.Code, w.Body)
	}
	if w := call(h, "POST", "/api/extension", "http://127.0.0.1:9614", `{"origin":"`+ext+`","allow":true}`, keyHeader, s.key.window); w.Code != 200 {
		t.Fatalf("allow: %d %s", w.Code, w.Body)
	}
	w := call(h, "POST", "/api/adopt", ext, string(body))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"fileName":"invoice.pdf"`) {
		t.Fatalf("allowed extension: %d %s", w.Code, w.Body)
	}
	var res struct{ IDs []string }
	json.Unmarshal(w.Body.Bytes(), &res)
	settled(t, s.m.get(res.IDs[0]))
	// the same file again: a clear answer, not a second entry
	if w := call(h, "POST", "/api/adopt", ext, string(body)); w.Code != http.StatusBadRequest {
		t.Fatalf("again: %d %s", w.Code, w.Body)
	}
}
