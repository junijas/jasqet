package main

// Safety checks after a download: is the file really what its name says,
// does it match the checksum the site published, and what do Windows Defender
// (and VirusTotal, if you add a key) think of it.

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/bodgit/sevenzip"
	"github.com/nwaples/rardecode/v2"
)

type Safety struct {
	Checking  bool     `json:"checking,omitempty"`
	Level     string   `json:"level"`               // "ok", "info", "warn", "danger"
	RealType  string   `json:"realType,omitempty"`  // what the file really is
	Hash      string   `json:"hash,omitempty"`      // "sha256:…"
	HashMatch string   `json:"hashMatch,omitempty"` // "ok" (the checksum you typed), "site" (one from the same website: not damaged, but no proof), "bad" or ""
	Scan      string   `json:"scan,omitempty"`      // Windows Defender result
	VT        string   `json:"vt,omitempty"`        // VirusTotal result
	Notes     []string `json:"notes,omitempty"`
	Renamed   string   `json:"renamed,omitempty"`
	block     bool     // dangerous for a reason besides a checksum mismatch: renamed so it can't be double-clicked
}

func (s *Safety) raise(level, note string) {
	rank := map[string]int{"": 0, "ok": 0, "info": 1, "warn": 2, "danger": 3}
	if rank[level] > rank[s.Level] {
		s.Level = level
	}
	if level == "danger" {
		s.block = true
	}
	if note != "" {
		s.Notes = append(s.Notes, note)
	}
}

// ---------- what is this file really? ----------

type fileKind struct {
	name string   // shown to the user
	exts []string // extensions that are honest for it
	exec bool     // runs code when opened
}

var (
	kWinExe  = &fileKind{"a Windows program", []string{".exe", ".dll", ".scr", ".com", ".sys", ".cpl", ".ocx", ".efi", ".mui", ".node", ".pyd", ".bin", ".drv", ".msstyles", ".ax", ".tmp"}, true}
	kELF     = &fileKind{"a Linux program", []string{"", ".bin", ".so", ".run", ".appimage", ".elf", ".out"}, true}
	kShort   = &fileKind{"a Windows shortcut (.lnk)", []string{".lnk"}, true}
	kOLE     = &fileKind{"an Office/Installer file", []string{".doc", ".xls", ".ppt", ".msi", ".msp", ".msg", ".pub", ".vsd", ".mpp", ".db", ".dot", ".xlt", ".pot", ".wps", ".mst"}, false}
	kZip     = &fileKind{"a ZIP archive", []string{".zip", ".docx", ".xlsx", ".pptx", ".odt", ".ods", ".odp", ".apk", ".jar", ".epub", ".xpi", ".crx", ".nupkg", ".whl", ".aab", ".ipa", ".appx", ".msix", ".vsix", ".kmz", ".3mf", ".cbz", ".docm", ".xlsm", ".pptm", ".dotx", ".xps", ".oxps", ".sketch", ".apkm", ".xapk", ".mcpack", ".mcworld", ".ora", ".usdz", ".pkpass", ".wsz", ".ear", ".war", ".aar", ".zipx"}, false}
	kRar     = &fileKind{"a RAR archive", []string{".rar", ".cbr", ".part1.rar"}, false}
	k7z      = &fileKind{"a 7-Zip archive", []string{".7z", ".cb7"}, false}
	kGz      = &fileKind{"a gzip archive", []string{".gz", ".tgz", ".svgz"}, false}
	kXz      = &fileKind{"an xz archive", []string{".xz", ".txz"}, false}
	kBz      = &fileKind{"a bzip2 archive", []string{".bz2", ".tbz", ".tbz2"}, false}
	kZst     = &fileKind{"a zstd archive", []string{".zst", ".tzst"}, false}
	kPDF     = &fileKind{"a PDF document", []string{".pdf", ".ai"}, false}
	kMP4     = &fileKind{"an MP4/MOV video or audio", []string{".mp4", ".m4a", ".m4v", ".mov", ".3gp", ".3g2", ".heic", ".heif", ".avif", ".m4b", ".m4p", ".f4v", ".mj2", ".qt", ".m4r", ".cr3", ".jxl"}, false}
	kMKV     = &fileKind{"an MKV/WebM video", []string{".mkv", ".webm", ".mka", ".mk3d", ".weba"}, false}
	kAVI     = &fileKind{"an AVI video", []string{".avi", ".divx"}, false}
	kWAV     = &fileKind{"a WAV sound", []string{".wav"}, false}
	kWEBP    = &fileKind{"a WebP image", []string{".webp"}, false}
	kMP3     = &fileKind{"an MP3 sound", []string{".mp3", ".mp2", ".aac"}, false}
	kOGG     = &fileKind{"an Ogg sound/video", []string{".ogg", ".oga", ".ogv", ".opus", ".spx"}, false}
	kFLAC    = &fileKind{"a FLAC sound", []string{".flac"}, false}
	kPNG     = &fileKind{"a PNG image", []string{".png", ".apng"}, false}
	kJPG     = &fileKind{"a JPEG image", []string{".jpg", ".jpeg", ".jfif", ".jpe"}, false}
	kGIF     = &fileKind{"a GIF image", []string{".gif"}, false}
	kHTML    = &fileKind{"a web page", []string{".html", ".htm", ".xhtml", ".php", ".asp", ".aspx", ".svg", ".xml", ".jsp", ".shtml", ".mhtml", ".hta"}, false}
	kTS      = &fileKind{"an MPEG-TS video", []string{".ts", ".mts", ".m2ts", ".tsv", ".tsa"}, false}
	kISO     = &fileKind{"a disc image (ISO)", []string{".iso", ".img"}, false}
	kWMV     = &fileKind{"a Windows Media file", []string{".wmv", ".wma", ".asf"}, false}
	kFLV     = &fileKind{"a Flash video", []string{".flv"}, false}
	kCAB     = &fileKind{"a Windows cabinet", []string{".cab", ".msu"}, false}
	kMach    = &fileKind{"a Mac program", []string{"", ".dylib", ".bundle", ".so"}, true}
	kDMG     = &fileKind{"a Mac disk image", []string{".dmg"}, false}
	kUnknown = &fileKind{}
)

var execExts = map[string]bool{
	".exe": true, ".scr": true, ".com": true, ".bat": true, ".cmd": true, ".pif": true, ".lnk": true,
	".vbs": true, ".vbe": true, ".js": true, ".jse": true, ".wsf": true, ".wsh": true, ".hta": true,
	".msi": true, ".ps1": true, ".cpl": true, ".jar": true, ".reg": true, ".scf": true, ".msc": true,
	".appref-ms": true, ".application": true, ".gadget": true, ".msp": true, ".iso": true, ".img": true, ".vhd": true, ".vhdx": true,
	".url": true, ".settingcontent-ms": true, ".library-ms": true, ".xll": true, ".ws": true, ".wsc": true, ".sct": true, ".psm1": true,
	".chm": true, ".inf": true, ".diagcab": true, ".appx": true, ".msix": true, ".appxbundle": true, ".msixbundle": true, ".py": true, ".pyw": true,
	".vb": true, ".mde": true, ".ade": true, ".hlp": true, ".sh": true, ".command": true, ".pkg": true, ".dmg": true, ".deb": true, ".rpm": true,
	".appimage": true, ".run": true, ".apk": true, ".msu": true, ".appinstaller": true, ".jnlp": true, ".vsix": true, ".xbap": true, ".mshxml": true,
	// these don't look like programs, but opening one can run code or connect out
	".theme": true, ".themepack": true, ".deskthemepack": true, ".rdp": true, ".wsb": true, ".website": true, ".search-ms": true,
	".searchconnector-ms": true, ".msh": true, ".msh1": true, ".msh2": true, ".msh1xml": true, ".msh2xml": true, ".shb": true, ".shs": true,
	".ins": true, ".isp": true, ".pyz": true, ".pyzw": true, ".pyc": true, ".psc1": true, ".mda": true, ".its": true, ".xnk": true,
	".appcontent-ms": true, ".printerexport": true, ".vsmacros": true, ".bgi": true,
}

var contentExts = map[string]bool{
	".mp4": true, ".mkv": true, ".avi": true, ".mov": true, ".webm": true, ".mp3": true, ".m4a": true, ".wav": true,
	".flac": true, ".pdf": true, ".doc": true, ".docx": true, ".xls": true, ".xlsx": true, ".ppt": true, ".pptx": true,
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".txt": true, ".zip": true, ".rar": true, ".epub": true,
	".srt": true, ".m4v": true, ".wmv": true, ".flv": true, ".3gp": true, ".ogg": true, ".aac": true, ".avif": true, ".webp": true,
}

func sniff(b []byte) *fileKind {
	has := func(off int, sig string) bool {
		return len(b) >= off+len(sig) && string(b[off:off+len(sig)]) == sig
	}
	trimmed := bytes.ToLower(bytes.TrimLeft(b[:min(len(b), 1024)], " \r\n\t\uFEFF"))
	switch {
	case has(0, "MZ"):
		return kWinExe
	case has(0, "\x7fELF"):
		return kELF
	case has(0, "L\x00\x00\x00\x01\x14\x02\x00"):
		return kShort
	case has(0, "\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1"):
		return kOLE
	case has(0, "PK\x03\x04"), has(0, "PK\x05\x06"), has(0, "PK\x07\x08"):
		return kZip
	case has(0, "Rar!\x1a\x07"):
		return kRar
	case has(0, "7z\xbc\xaf\x27\x1c"):
		return k7z
	case has(0, "\x1f\x8b"):
		return kGz
	case has(0, "\xfd7zXZ\x00"):
		return kXz
	case has(0, "BZh"):
		return kBz
	case has(0, "\x28\xb5\x2f\xfd"):
		return kZst
	case has(0, "%PDF"):
		return kPDF
	case has(4, "ftyp"):
		return kMP4
	case has(0, "\x1aE\xdf\xa3"):
		return kMKV
	case has(0, "RIFF") && has(8, "AVI "):
		return kAVI
	case has(0, "RIFF") && has(8, "WAVE"):
		return kWAV
	case has(0, "RIFF") && has(8, "WEBP"):
		return kWEBP
	case has(0, "ID3"), len(b) > 2 && b[0] == 0xff && (b[1]&0xe0) == 0xe0:
		return kMP3
	case has(0, "OggS"):
		return kOGG
	case has(0, "fLaC"):
		return kFLAC
	case has(0, "\x89PNG"):
		return kPNG
	case has(0, "\xff\xd8\xff"):
		return kJPG
	case has(0, "GIF8"):
		return kGIF
	case has(0, "\x30\x26\xb2\x75\x8e\x66\xcf\x11"):
		return kWMV
	case has(0, "FLV\x01"):
		return kFLV
	case has(0, "MSCF"):
		return kCAB
	case has(0, "\xcf\xfa\xed\xfe"), has(0, "\xca\xfe\xba\xbe"), has(0, "\xce\xfa\xed\xfe"):
		return kMach
	case len(b) > 376 && b[0] == 0x47 && b[188] == 0x47:
		return kTS
	case has(0x8001, "CD001"):
		return kISO
	case bytes.HasPrefix(trimmed, []byte("<!doctype html")), bytes.HasPrefix(trimmed, []byte("<html")),
		bytes.HasPrefix(trimmed, []byte("<head")), bytes.HasPrefix(trimmed, []byte("<!-- ")) && bytes.Contains(trimmed, []byte("<html")):
		return kHTML
	}
	return kUnknown
}

var copyNumber = regexp.MustCompile(` \(\d+\)$`)

// innerExt: the extension before the last one ("movie.mp4.exe" → ".mp4"),
// also when Jasqet numbered the copy ("movie.mp4 (1).exe").
func innerExt(name, ext string) string {
	base := strings.TrimSuffix(strings.ToLower(name), ext)
	return extOf(copyNumber.ReplaceAllString(base, ""))
}

// checksumTyped: where a checksum you typed in came from (the others say
// where on the website they were found).
const checksumTyped = "typed in by you"

// quarantineTag is macOS's "downloaded from the internet" mark (the
// com.apple.quarantine attribute browsers set), so Gatekeeper checks the file
// before it's opened. 0081 is what browsers write: it doesn't mean "approved".
func quarantineTag(at time.Time) string {
	return fmt.Sprintf("0081;%08x;Jasqet;", at.Unix())
}

// withoutLogin: a link without any user:password@ in it.
func withoutLogin(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.User == nil {
		return raw
	}
	u.User = nil
	return u.String()
}

// extOf is the extension Windows goes by: it ignores dots and spaces at the
// end of a name, so "setup.exe ." is still a program.
func extOf(name string) string {
	return strings.ToLower(filepath.Ext(strings.TrimRight(name, " .")))
}

// checkType compares a file's name with what it really is.
func checkType(p, name string, s *Safety) {
	name = strings.TrimRight(name, " .") // the name Windows goes by
	f, err := os.Open(p)
	if err != nil {
		return
	}
	head := make([]byte, 0x8010)
	n, _ := io.ReadFull(f, head)
	f.Close()
	head = head[:n]
	k := sniff(head)
	ext := extOf(name)
	inner := innerExt(name, ext)

	// "movie.mp4.exe": Windows hides the last part, so it looks like a video.
	if execExts[ext] && contentExts[inner] {
		s.raise("danger", fmt.Sprintf("The name pretends to be a %s file, but it ends in %s, which makes it a program. This is a common trick: don't open it.", strings.TrimPrefix(inner, "."), ext))
	}
	// programs and scripts never get "✓ Checked" (scripts like .bat or .ps1 are
	// plain text, so nothing below would notice them)
	if execExts[ext] {
		s.raise("warn", fmt.Sprintf("This is a program or something that runs like one (%s). Only open it if you trust the website it came from.", strings.ToUpper(strings.TrimPrefix(ext, "."))))
	}
	if k.name == "" {
		return
	}
	s.RealType = k.name
	honest := false
	for _, e := range k.exts {
		if e == ext || (e != "" && strings.HasSuffix(strings.ToLower(name), e)) {
			honest = true
		}
	}
	switch {
	case honest:
		if k.exec && s.Level != "danger" && !execExts[ext] { // (programs in execExts were warned about above)
			s.raise("info", "This is a program. Only run it if you trust the website it came from.")
		}
	case k == kHTML:
		s.raise("warn", "This isn't the file. It's a web page (often a login page, an error, or a \"link expired\" message). Get a fresh link from the site.")
	case k.exec:
		s.raise("danger", fmt.Sprintf("The name says %s, but it's really %s. Don't open it: this is how fake downloads spread viruses.", strings.ToUpper(strings.TrimPrefix(ext, ".")), k.name))
	case k == kOLE && (ext == ".exe" || ext == ".msi"):
		// installers inside OLE containers are fine
	case ext == "" || ext == ".bin" || ext == ".dat" || ext == ".tmp" || ext == ".download":
		s.raise("info", fmt.Sprintf("This file has no proper type in its name; it's %s.", k.name))
	case (k == kMP4 || k == kMKV || k == kTS || k == kAVI || k == kWMV || k == kFLV) && contentExts[ext] && isVideoExt(ext):
		// a video in a different video container: harmless
	case k == kZip && (ext == ".zip" || ext == ".cbz"):
	default:
		s.raise("warn", fmt.Sprintf("The name says %s, but it's really %s.", strings.ToUpper(strings.TrimPrefix(ext, ".")), k.name))
	}
	if k == kZip || k == kRar || k == k7z {
		if enc := archiveEncrypted(p, k); enc {
			s.raise("warn", "This archive is password-protected. If a site wants you to visit a page, fill a survey or pay to get the password, it's a scam.")
		}
	}
}

func isVideoExt(e string) bool {
	switch e {
	case ".mp4", ".mkv", ".avi", ".mov", ".webm", ".m4v", ".wmv", ".flv", ".3gp", ".ts", ".mts":
		return true
	}
	return false
}

// archiveEncrypted: does the archive need a password?
func archiveEncrypted(p string, k *fileKind) bool {
	switch k {
	case kZip:
		zr, err := zip.OpenReader(p)
		if err != nil {
			return false
		}
		defer zr.Close()
		for _, f := range zr.File {
			if f.Flags&1 != 0 {
				return true
			}
		}
	case kRar:
		rr, err := rardecode.OpenReader(p)
		if err != nil {
			return errors.Is(err, rardecode.ErrArchiveEncrypted)
		}
		defer rr.Close()
		for i := 0; i < 20; i++ {
			h, err := rr.Next()
			if err != nil {
				return errors.Is(err, rardecode.ErrArchiveEncrypted) || errors.Is(err, rardecode.ErrArchivedFileEncrypted)
			}
			if h.Encrypted || h.HeaderEncrypted {
				return true
			}
		}
	case k7z:
		zr, err := sevenzip.OpenReader(p)
		if err != nil {
			var re *sevenzip.ReadError
			return errors.As(err, &re) && re.Encrypted
		}
		defer zr.Close()
		for _, f := range zr.File {
			if f.FileInfo().IsDir() || f.UncompressedSize == 0 {
				continue
			}
			rc, err := f.Open()
			if err == nil {
				_, err = rc.Read(make([]byte, 1))
				rc.Close()
			}
			var re *sevenzip.ReadError
			return err != nil && errors.As(err, &re) && re.Encrypted
		}
	}
	return false
}

// ---------- checksums ----------

var hexRe = regexp.MustCompile(`(?i)\b([a-f0-9]{128}|[a-f0-9]{64}|[a-f0-9]{40}|[a-f0-9]{32})\b`)

func algoForLen(n int) string {
	switch n {
	case 32:
		return "md5"
	case 40:
		return "sha1"
	case 64:
		return "sha256"
	case 128:
		return "sha512"
	}
	return ""
}

// parseChecksum accepts "sha256:abc…", "SHA256 abc…" or just the hex.
func parseChecksum(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	m := hexRe.FindString(s)
	if m == "" {
		return "", errors.New("that checksum doesn't look right. Paste the long code of letters and numbers (SHA-256, SHA-1 or MD5)")
	}
	return algoForLen(len(m)) + ":" + strings.ToLower(m), nil
}

// findChecksum looks for a published checksum: files next to the download
// (file.sha256, SHA256SUMS, checksums.txt) and the page it came from.
func (m *Manager) findChecksum(ctx context.Context, d *Download) {
	d.mu.Lock()
	if d.Checksum != "" || d.Kind != "" || d.Size < 1<<20 {
		d.mu.Unlock()
		return
	}
	raw, page, name := d.URL, d.Page, d.FileName
	if page == "" {
		page = d.Headers["Referer"]
	}
	o := d.reqOpts()
	d.mu.Unlock()
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return
	}
	remoteName := path.Base(u.Path)
	names := map[string]bool{strings.ToLower(remoteName): true, strings.ToLower(name): true}
	try := func(link string, from string) bool {
		cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		body, _, ctype, err := m.fetchSmall(cctx, link, o, 512<<10)
		m.countBytes(len(body))
		if err != nil || strings.Contains(ctype, "html") && !strings.HasSuffix(from, "page") {
			return false
		}
		if sum := checksumFor(body, names, strings.Contains(ctype, "html")); sum != "" {
			d.mu.Lock()
			if d.Checksum == "" {
				d.Checksum, d.ChecksumFrom = sum, from
			}
			d.mu.Unlock()
			return true
		}
		return false
	}
	base := *u
	base.RawQuery, base.Fragment = "", ""
	full := base.String()
	if u.RawQuery == "" { // signed links (with ?token=…) won't have a .sha256 next to them
		for _, ext := range []string{".sha256", ".sha256sum", ".sha256.txt", ".sha512", ".sha1", ".md5"} {
			if try(full+ext, "the "+ext+" file next to the download") {
				return
			}
		}
		dir := strings.TrimSuffix(full, path.Base(u.Path))
		for _, f := range []string{"SHA256SUMS", "sha256sums.txt", "SHA256SUMS.txt", "checksums.txt", "CHECKSUMS", "sha256sum.txt", "MD5SUMS"} {
			if try(dir+f, "the "+f+" list next to the download") {
				return
			}
		}
	}
	if page != "" && page != raw {
		try(page, "the download page")
	}
}

// checksumFor finds the checksum for one of names in a checksum list or page.
func checksumFor(body []byte, names map[string]bool, isPage bool) string {
	text := string(body)
	if isPage {
		text = regexp.MustCompile(`(?s)<[^>]+>`).ReplaceAllString(text, " ")
	}
	lines := strings.Split(text, "\n")
	var lone []string
	for i, line := range lines {
		hexes := hexRe.FindAllString(line, -1)
		low := strings.ToLower(line)
		for _, h := range hexes {
			lone = append(lone, h)
		}
		for n := range names {
			if n == "" || !containsName(low, n) {
				continue
			}
			// the checksum is on the same line, or the lines right after the name
			for j := i; j < min(len(lines), i+4); j++ {
				if h := hexRe.FindString(lines[j]); h != "" {
					return algoForLen(len(h)) + ":" + strings.ToLower(h)
				}
			}
		}
	}
	// a ".sha256" file has just one checksum and maybe no name
	if !isPage && len(lone) == 1 {
		return algoForLen(len(lone[0])) + ":" + strings.ToLower(lone[0])
	}
	return ""
}

// containsName: the line mentions exactly this file name (not "debug-setup.exe"
// when looking for "setup.exe").
func containsName(line, name string) bool {
	part := func(c byte) bool {
		return c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == '_' || c == '+' || c >= 0x80
	}
	for i := 0; ; {
		j := strings.Index(line[i:], name)
		if j < 0 {
			return false
		}
		j += i
		end := j + len(name)
		if (j == 0 || !part(line[j-1])) && (end == len(line) || !part(line[end]) || line[end] == '.' && (end+1 == len(line) || !part(line[end+1]))) {
			return true
		}
		i = j + 1
	}
}

func hashFile(ctx context.Context, p, algo string) (string, error) {
	var h hash.Hash
	switch algo {
	case "md5":
		h = md5.New()
	case "sha1":
		h = sha1.New()
	case "sha512":
		h = sha512.New()
	default:
		h = sha256.New()
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	buf := make([]byte, 1<<20)
	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		n, err := f.Read(buf)
		h.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ---------- reputation ----------

// virusTotal asks VirusTotal what it knows about a file. Only the file's
// fingerprint (its SHA-256) is sent, never the file.
func (m *Manager) virusTotal(ctx context.Context, sha string) string {
	m.mu.Lock()
	key := strings.TrimSpace(m.settings.Safety.VirusTotalKey)
	m.mu.Unlock()
	if key == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://www.virustotal.com/api/v3/files/"+sha, nil)
	req.Header.Set("x-apikey", key)
	resp, err := m.client.Do(req)
	if err != nil {
		return "couldn't reach VirusTotal"
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case 404:
		return "unknown to VirusTotal (a new or rare file)"
	case 401, 403:
		return "the VirusTotal key isn't valid"
	case 429:
		return "VirusTotal limit reached for now"
	}
	var r struct {
		Data struct {
			Attributes struct {
				Stats struct {
					Malicious, Suspicious, Undetected, Harmless int
				} `json:"last_analysis_stats"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if json.NewDecoder(resp.Body).Decode(&r) != nil {
		return ""
	}
	st := r.Data.Attributes.Stats
	total := st.Malicious + st.Suspicious + st.Undetected + st.Harmless
	if st.Malicious+st.Suspicious == 0 {
		return fmt.Sprintf("clean (0 of %d scanners flag it)", total)
	}
	return fmt.Sprintf("%d of %d scanners flag it", st.Malicious+st.Suspicious, total)
}

type SafetySettings struct {
	Scan          bool   `json:"scan"`          // scan finished files with Windows Defender
	VirusTotalKey string `json:"virusTotalKey"` // optional: look files up on VirusTotal
	FindChecksums bool   `json:"findChecksums"` // look for a published checksum
}

// ---------- after a download ----------

// checkSafety runs the checks on a finished download.
func (m *Manager) checkSafety(ctx context.Context, d *Download, p string) *Safety {
	s := &Safety{Level: "ok"}
	d.mu.Lock()
	name, want, kind, from := d.FileName, d.Checksum, d.Kind, d.ChecksumFrom
	d.mu.Unlock()
	m.mu.Lock()
	ss := m.settings.Safety
	m.mu.Unlock()

	st, err := os.Stat(p)
	if err != nil {
		return nil
	}
	// tag it first: hashing and VirusTotal below can take a while, and the
	// file may be opened meanwhile (SmartScreen only checks tagged files)
	markFromInternet(p, d)
	if !st.IsDir() {
		checkType(p, name, s)
	} else {
		checkFolder(p, s)
	}
	risky := s.RealType == kWinExe.name || s.RealType == kShort.name || s.RealType == kOLE.name || execExts[extOf(name)] ||
		s.RealType == kZip.name || s.RealType == kRar.name || s.RealType == k7z.name || st.IsDir()

	if !st.IsDir() {
		algo := "sha256"
		if want != "" {
			algo = strings.SplitN(want, ":", 2)[0]
		}
		if want != "" || risky || ss.VirusTotalKey != "" {
			if sum, err := hashFile(ctx, p, algo); err == nil {
				s.Hash = algo + ":" + sum
				if want != "" {
					if s.Hash == want {
						// a checksum found on the same website only shows it wasn't damaged:
						// whoever changed the file could change that too
						s.HashMatch = "site"
						if from == "" || from == checksumTyped { // "": typed into the Add box
							s.HashMatch = "ok"
						}
					} else {
						s.HashMatch = "bad"
						block := s.block
						s.raise("danger", "The file doesn't match the checksum published for it. It's damaged or has been changed, so don't use it. Download it again.")
						s.block = block // only damaged: it keeps its name, so downloading it again works
					}
				}
				if algo == "sha256" && risky {
					if vt := m.virusTotal(ctx, sum); vt != "" {
						s.VT = vt
						if strings.Contains(vt, "flag it") && !strings.HasPrefix(vt, "clean") {
							s.raise("danger", "VirusTotal: "+vt+".")
						}
					}
				}
			}
		}
		_ = kind
	}
	if ss.Scan && (risky || st.Size() < 300<<20) {
		if res, threat := defenderScan(ctx, p); res != "" {
			s.Scan = res
			if threat {
				s.raise("danger", "Windows Defender found a threat: "+res)
			}
		}
	}
	return s
}

// checkFolder: for torrents and unzipped folders, look at every file inside.
func checkFolder(dir string, s *Safety) {
	count := 0
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || count > 2000 {
			return nil
		}
		count++
		sub := &Safety{}
		checkType(p, info.Name(), sub)
		if sub.Level == "warn" || sub.Level == "danger" {
			rel, _ := filepath.Rel(dir, p)
			for _, n := range sub.Notes {
				s.raise(sub.Level, rel+": "+n)
			}
		}
		return nil
	})
}
