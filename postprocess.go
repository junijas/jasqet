package main

// What happens after a download finishes: safety checks, then your
// "after downloading" rules (unzip, extract audio, rename, move, tell you on
// Telegram or with a Windows notification).

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/mp4/codecs"
	"github.com/bluenviron/mediacommon/v2/pkg/formats/pmp4"
	"github.com/bodgit/sevenzip"
	"github.com/nwaples/rardecode/v2"
)

type AfterRule struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	On   bool   `json:"on"`
	// which downloads: "all", a category ("Video", "Music", "Compressed",
	// "Documents", "Programs"), "ext:.pdf .epub", "site:example.com" or "watch:<rule id>"
	Match string `json:"match"`

	Unzip         bool   `json:"unzip"`         // ZIP, RAR, 7z, tar.gz
	DeleteArchive bool   `json:"deleteArchive"` // after unzipping
	ExtractAudio  bool   `json:"extractAudio"`  // video → .m4a sound file
	RenameTo      string `json:"renameTo"`      // e.g. "{date} {name}"
	MoveTo        string `json:"moveTo"`        // a folder
	Telegram      bool   `json:"telegram"`
	Desktop       bool   `json:"desktop"`
}

type NotifySettings struct {
	TelegramToken  string `json:"telegramToken"`
	TelegramChat   string `json:"telegramChat"`
	DesktopAll     bool   `json:"desktopAll"`     // a Windows notification for every finished download
	TelegramAll    bool   `json:"telegramAll"`    // a Telegram message for every finished download
	TelegramErrors bool   `json:"telegramErrors"` // and when one fails
}

func (r *AfterRule) matches(d *Download) bool {
	if !r.On {
		return false
	}
	m := strings.TrimSpace(r.Match)
	switch {
	case m == "" || strings.EqualFold(m, "all"):
		return true
	case strings.HasPrefix(m, "ext:"):
		ext := extOf(d.FileName)
		for _, e := range strings.FieldsFunc(strings.ToLower(m[4:]), func(c rune) bool { return c == ' ' || c == ',' }) {
			if !strings.HasPrefix(e, ".") {
				e = "." + e
			}
			if e == ext {
				return true
			}
		}
		return false
	case strings.HasPrefix(m, "site:"):
		host := strings.ToLower(hostOf(d.URL))
		pageHost := strings.ToLower(hostOf(d.Page))
		site := strings.ToLower(strings.TrimSpace(m[5:]))
		return site != "" && (strings.HasSuffix(host, site) || strings.HasSuffix(pageHost, site))
	case strings.HasPrefix(m, "watch:"):
		return d.WatchID != "" && d.WatchID == strings.TrimSpace(m[6:])
	case strings.EqualFold(m, "Video") && d.Kind == "yt":
		return !strings.HasSuffix(d.FileName, ".m4a")
	case strings.EqualFold(m, "Music") && d.Kind == "yt":
		return strings.HasSuffix(d.FileName, ".m4a")
	}
	return strings.EqualFold(categoryOf(d.FileName), m)
}

// recheckSafety runs the safety checks again (e.g. after you typed a checksum).
func (m *Manager) recheckSafety(d *Download) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	d.mu.Lock()
	p := filepath.Join(d.Dir, d.FileName)
	old := d.Safety
	d.Safety = &Safety{Checking: true}
	d.mu.Unlock()
	s := m.checkSafety(ctx, d, p)
	if s == nil {
		s = old
	}
	d.mu.Lock()
	d.Safety = s
	d.mu.Unlock()
	m.saveSoon()
}

// afterDownload runs once a download has finished.
func (m *Manager) afterDownload(d *Download) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	d.mu.Lock()
	p := filepath.Join(d.Dir, d.FileName)
	d.Safety = &Safety{Checking: true}
	d.mu.Unlock()
	m.saveSoon()

	s := m.checkSafety(ctx, d, p)
	if s == nil {
		s = &Safety{Level: "ok"}
	}
	if s.block {
		// make sure it can't be opened by a double-click by mistake (a file
		// that's only damaged keeps its name, so downloading it again works)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			blocked := p + ".blocked"
			if os.Rename(p, blocked) == nil {
				s.Renamed = filepath.Base(blocked)
				d.mu.Lock()
				d.FileName = filepath.Base(blocked)
				d.mu.Unlock()
				p = blocked
			}
		}
	}
	d.mu.Lock()
	d.Safety = s
	d.mu.Unlock()
	m.saveSoon()

	m.mu.Lock()
	rules := append([]AfterRule(nil), m.settings.After...)
	notify := m.settings.Notify
	m.mu.Unlock()
	var done []string
	tg, desk := notify.TelegramAll, notify.DesktopAll
	if s.Level != "danger" {
		for i := range rules {
			r := &rules[i]
			if !r.matches(d) {
				continue
			}
			res, err := m.runRule(ctx, d, r, &p)
			if err != nil {
				done = append(done, fmt.Sprintf("%s: %s", r.Name, err))
			} else if res != "" {
				done = append(done, res)
			}
			tg = tg || r.Telegram
			desk = desk || r.Desktop
		}
	}
	if len(done) > 0 {
		d.mu.Lock()
		d.Note = strings.Join(done, " · ")
		d.mu.Unlock()
		m.saveSoon()
	}
	d.mu.Lock()
	name, size := d.FileName, d.Size
	d.mu.Unlock()
	msg := fmt.Sprintf("✅ Downloaded: %s (%s)", name, humanSize(size))
	if s.Level == "danger" {
		msg = fmt.Sprintf("⚠️ Downloaded %s but it looks dangerous: %s", name, strings.Join(s.Notes, " "))
	}
	if len(done) > 0 {
		msg += "\n" + strings.Join(done, "\n")
	}
	if tg {
		go m.sendTelegram(msg)
	}
	if desk {
		d.mu.Lock()
		id := d.ID
		d.mu.Unlock()
		if s.Level == "danger" {
			id = "" // no Open button on a dangerous file
		}
		m.notifyDesktop(msg, id)
	}
	if s.Level != "danger" && m.autoOpen != nil {
		m.autoOpen(d)
	}
}

type pendingToast struct{ msg, id string }

// toastWait: how long a finished download's notification waits for others,
// so a batch that finishes together gives one notification, not a pile.
var toastWait = 3 * time.Second

var showToast = desktopNotifyFile // tests watch it instead

func (m *Manager) notifyDesktop(msg, id string) {
	m.toastMu.Lock()
	m.toasts = append(m.toasts, pendingToast{msg, id})
	first := len(m.toasts) == 1
	m.toastMu.Unlock()
	if !first {
		return
	}
	go func() {
		time.Sleep(toastWait)
		m.toastMu.Lock()
		list := m.toasts
		m.toasts = nil
		m.toastMu.Unlock()
		title, text, id := toastText(list)
		showToast(title, text, id)
	}()
}

// toastText: one download keeps its Open and Show in folder buttons; several
// are listed in one notification that opens the Jasqet window.
func toastText(list []pendingToast) (title, text, id string) {
	if len(list) == 1 {
		return "Jasqet", list[0].msg, list[0].id
	}
	lines := []string{}
	for i, t := range list {
		if i == 4 {
			lines = append(lines, fmt.Sprintf("…and %d more", len(list)-4))
			break
		}
		line, _, _ := strings.Cut(t.msg, "\n")
		lines = append(lines, strings.Replace(line, "Downloaded: ", "", 1))
	}
	return fmt.Sprintf("Jasqet: %d downloads finished", len(list)), strings.Join(lines, "\n"), ""
}

func (m *Manager) notifyFailed(d *Download) {
	m.mu.Lock()
	n := m.settings.Notify
	m.mu.Unlock()
	if !n.TelegramErrors {
		return
	}
	d.mu.Lock()
	msg := fmt.Sprintf("❌ Download stopped: %s\n%s", d.FileName, d.Error)
	d.mu.Unlock()
	go m.sendTelegram(msg)
}

func (m *Manager) runRule(ctx context.Context, d *Download, r *AfterRule, p *string) (string, error) {
	var did []string
	var extras []string // files made from this one (they move along with it)
	if r.Unzip && isArchive(*p) {
		dest, n, err := extractArchive(ctx, *p)
		if err != nil {
			return "", err
		}
		markFromInternet(dest, d) // unpacked files came from the internet too: SmartScreen still checks them
		did = append(did, fmt.Sprintf("unzipped %d files into %s", n, filepath.Base(dest)))
		if warn := checkUnpacked(d, dest); warn != "" {
			did = append(did, warn)
		}
		if r.DeleteArchive {
			os.Remove(*p)
			for _, extra := range otherVolumes(*p) {
				os.Remove(extra)
			}
			*p = dest
			d.mu.Lock()
			d.FileName = filepath.Base(dest)
			d.mu.Unlock()
		}
	}
	if r.ExtractAudio && isVideoExt(extOf(*p)) {
		out, err := extractAudio(ctx, *p)
		if err != nil {
			return "", err
		}
		did = append(did, "sound saved as "+filepath.Base(out))
		extras = append(extras, out)
	}
	if r.RenameTo != "" {
		d.mu.Lock()
		src := d.URL
		d.mu.Unlock()
		newName := sanitizeFilename(expandName(r.RenameTo, filepath.Base(*p), src))
		if newName != "" && newName != filepath.Base(*p) {
			np := uniqueFinal(filepath.Dir(*p), newName)
			if err := os.Rename(*p, np); err != nil {
				return "", err
			}
			*p = np
			d.mu.Lock()
			d.FileName = filepath.Base(np)
			d.mu.Unlock()
			did = append(did, "renamed")
		}
	}
	if dir := strings.TrimSpace(r.MoveTo); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		np := uniqueFinal(dir, filepath.Base(*p))
		if err := moveFile(*p, np); err != nil {
			return "", err
		}
		for _, e := range extras {
			moveFile(e, uniqueFinal(dir, filepath.Base(e)))
		}
		*p = np
		d.mu.Lock()
		d.Dir, d.FileName = dir, filepath.Base(np)
		d.mu.Unlock()
		did = append(did, "moved to "+dir)
	}
	return strings.Join(did, ", "), nil
}

func expandName(pattern, name, src string) string {
	ext := filepath.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	r := strings.NewReplacer(
		"{name}", stem, "{ext}", strings.TrimPrefix(ext, "."),
		"{date}", time.Now().Format("2006-01-02"), "{time}", time.Now().Format("15.04"),
		"{site}", strings.TrimPrefix(hostOf(src), "www."), "{category}", categoryOf(name),
	)
	out := r.Replace(pattern)
	if filepath.Ext(out) == "" && ext != "" && !strings.Contains(pattern, "{ext}") {
		out += ext
	}
	return out
}

func moveFile(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	return copyMove(src, dst)
}

// copyMove moves a file to a different drive: copy, then delete. The
// "downloaded from the internet" tag goes along, so SmartScreen still checks it.
func copyMove(src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if st.IsDir() {
		return errors.New("folders can only be moved on the same drive")
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		in.Close()
		return err
	}
	_, err = io.Copy(out, in)
	in.Close()
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dst)
		return err
	}
	keepInternetTag(src, dst)
	return os.Remove(src)
}

// ---------- archives ----------

func isArchive(p string) bool {
	low := strings.ToLower(p)
	if strings.Contains(low, ".part") && strings.HasSuffix(low, ".rar") && !strings.Contains(low, ".part1.rar") && !strings.Contains(low, ".part01.rar") && !strings.Contains(low, ".part001.rar") {
		return false // later volumes: unpacked together with part 1
	}
	for _, e := range []string{".zip", ".rar", ".7z", ".tar.gz", ".tgz", ".tar"} {
		if strings.HasSuffix(low, e) {
			return true
		}
	}
	return false
}

func otherVolumes(p string) []string {
	low := strings.ToLower(p)
	if !strings.HasSuffix(low, ".rar") || !strings.Contains(low, ".part") {
		return nil
	}
	base := p[:strings.LastIndex(low, ".part")]
	out, _ := filepath.Glob(base + ".part*.rar")
	return out
}

// cleanEntryName makes a path inside an archive safe to create on Windows:
// each part is cleaned like a download's name, so "notes.txt:hidden" can't
// make a hidden stream, "con.txt" isn't a device and ".." goes nowhere.
func cleanEntryName(name string) string {
	var parts []string
	for _, part := range strings.Split(strings.ReplaceAll(name, "\\", "/"), "/") {
		if part != "" && part != "." {
			parts = append(parts, sanitizeFilename(part))
		}
	}
	return strings.Join(parts, "/")
}

func safeJoin(dir, name string) (string, error) {
	name = strings.ReplaceAll(name, "\\", "/")
	p := filepath.Join(dir, filepath.FromSlash(name))
	if p != dir && !strings.HasPrefix(p, dir+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe path in archive: %s", name)
	}
	return p, nil
}

// Limits for unpacking, so a "zip bomb" (a small archive that unpacks to
// terabytes, or to millions of files) can't fill the drive. Tests lower them.
var (
	unpackRatio      int64 = 100     // all files together: at most 100 times the archive's own size
	unpackMaxEntries       = 100_000 // files and folders
	unpackKeepFree   int64 = 1 << 30 // and at least 1 GB stays free on the drive
	unpackAtLeast    int64 = 1 << 30 // but up to 1 GB is always fine (a small archive of logs can shrink 1000 times)
)

var (
	errUnpackBomb    = errors.New("this archive unpacks to far more than its own size, so Jasqet stopped (it may be a zip bomb)")
	errUnpackTooMany = errors.New("this archive holds more than 100,000 files, so Jasqet stopped (it may be a zip bomb)")
	errUnpackNoSpace = errors.New("unpacking this archive would leave less than 1 GB free on the drive, so Jasqet stopped")
)

// unpackLimit: how many bytes an archive may unpack to (all files together),
// and the error that says why when it's more.
func unpackLimit(p, dest string) (int64, error) {
	vols := otherVolumes(p) // a RAR in parts: all of them count
	if len(vols) == 0 {
		vols = []string{p}
	}
	if strings.HasSuffix(strings.ToLower(p), ".rar") {
		more, _ := filepath.Glob(p[:len(p)-4] + ".r[0-9][0-9]") // old style parts: .r00, .r01…
		vols = append(vols, more...)
	}
	var size int64
	for _, v := range vols {
		if st, err := os.Stat(v); err == nil {
			size += st.Size()
		}
	}
	limit, why := max(size*unpackRatio, unpackAtLeast), errUnpackBomb
	if free := diskFree(dest); free >= 0 && free-unpackKeepFree < limit {
		limit, why = max(0, free-unpackKeepFree), errUnpackNoSpace
	}
	return limit, why
}

// listedLimits: ZIP and 7z list every file and its size up front, so most
// bombs are refused before anything is written.
func listedLimits(sizes []uint64, limit int64, tooBig error) error {
	if len(sizes) > unpackMaxEntries {
		return errUnpackTooMany
	}
	var sum uint64
	for _, s := range sizes {
		sum += min(s, 1<<62) // a made-up size can't wrap the sum around
		if sum > uint64(limit) {
			return tooBig
		}
	}
	return nil
}

// unpackCopy copies one file out of an archive in pieces. Between pieces it
// checks that it wasn't stopped and that all files together are still under
// the limit, so one huge file stops early too.
func unpackCopy(ctx context.Context, w io.Writer, r io.Reader, buf []byte, total *int64, limit int64, tooBig error) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		k, rerr := r.Read(buf)
		if k > 0 {
			*total += int64(k)
			if *total > limit {
				return tooBig
			}
			if _, err := w.Write(buf[:k]); err != nil {
				return err
			}
		}
		if rerr == io.EOF {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

// checkUnpacked runs the safety checks on every file an archive unpacked to.
// What they find goes on the download's own safety badge too, so the
// archive's "Checked" can't hide a dangerous file inside it.
func checkUnpacked(d *Download, dir string) string {
	inside := &Safety{}
	checkFolder(dir, inside)
	if inside.Level != "warn" && inside.Level != "danger" {
		return ""
	}
	d.mu.Lock()
	if d.Safety == nil {
		d.Safety = &Safety{Level: "ok"}
	}
	d.Safety.raise(inside.Level, "")
	d.Safety.Notes = append(d.Safety.Notes, inside.Notes...)
	d.mu.Unlock()
	if len(inside.Notes) == 0 {
		return ""
	}
	msg := "careful with what's inside: " + inside.Notes[0]
	switch more := len(inside.Notes) - 1; {
	case more == 1:
		msg += " (and 1 more warning)"
	case more > 1:
		msg += fmt.Sprintf(" (and %d more warnings)", more)
	}
	return msg
}

// extractArchive unpacks next to the archive, into a folder with its name.
func extractArchive(ctx context.Context, p string) (string, int, error) {
	low := strings.ToLower(p)
	stem := filepath.Base(p)
	for _, e := range []string{".tar.gz", ".tgz", ".tar", ".zip", ".7z", ".rar"} {
		if strings.HasSuffix(low, e) {
			stem = stem[:len(stem)-len(e)]
			break
		}
	}
	if i := strings.LastIndex(strings.ToLower(stem), ".part"); i > 0 && strings.HasSuffix(low, ".rar") {
		stem = stem[:i]
	}
	dest := uniqueFinal(filepath.Dir(p), stem)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", 0, err
	}
	limit, tooBig := unpackLimit(p, dest)
	var total int64 // bytes written so far, all files together
	entries := 0
	buf := make([]byte, 256<<10)
	// next counts one more entry of the archive, before it's unpacked
	next := func() error {
		entries++
		if entries > unpackMaxEntries {
			return errUnpackTooMany
		}
		return ctx.Err()
	}
	n := 0
	write := func(name string, mode os.FileMode, isDir bool, open func() (io.ReadCloser, error)) error {
		if err := next(); err != nil {
			return err
		}
		name = cleanEntryName(name)
		if name == "" {
			return nil
		}
		out, err := safeJoin(dest, name)
		if err != nil {
			return err
		}
		if isDir {
			return os.MkdirAll(out, 0o755)
		}
		os.MkdirAll(filepath.Dir(out), 0o755)
		rc, err := open()
		if err != nil {
			return err
		}
		defer rc.Close()
		f, err := os.Create(out)
		if err != nil {
			return err
		}
		err = unpackCopy(ctx, f, rc, buf, &total, limit, tooBig)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		n++
		return err
	}
	var err error
	switch {
	case strings.HasSuffix(low, ".zip"):
		var zr *zip.ReadCloser
		zr, err = zip.OpenReader(p)
		if err == nil {
			defer zr.Close()
			var sizes []uint64
			for _, f := range zr.File {
				sizes = append(sizes, f.UncompressedSize64)
			}
			err = listedLimits(sizes, limit, tooBig)
		}
		if err == nil {
			for _, f := range zr.File {
				if f.Flags&1 != 0 {
					err = errors.New("the archive needs a password")
					break
				}
				f := f
				if err = write(f.Name, f.Mode(), f.FileInfo().IsDir(), f.Open); err != nil {
					break
				}
			}
		}
	case strings.HasSuffix(low, ".7z"):
		var zr *sevenzip.ReadCloser
		zr, err = sevenzip.OpenReader(p)
		if err == nil {
			defer zr.Close()
			var sizes []uint64
			for _, f := range zr.File {
				sizes = append(sizes, f.UncompressedSize)
			}
			err = listedLimits(sizes, limit, tooBig)
		}
		if err == nil {
			for _, f := range zr.File {
				f := f
				if err = write(f.Name, f.Mode(), f.FileInfo().IsDir(), f.Open); err != nil {
					break
				}
			}
		}
		var re *sevenzip.ReadError
		if err != nil && (strings.Contains(strings.ToLower(err.Error()), "password") || errors.As(err, &re) && re.Encrypted) {
			err = errors.New("the archive needs a password")
		}
	case strings.HasSuffix(low, ".rar"):
		var rr *rardecode.ReadCloser
		rr, err = rardecode.OpenReader(p)
		if err == nil {
			defer rr.Close()
			for {
				h, herr := rr.Next()
				if herr == io.EOF {
					break
				}
				if herr != nil {
					err = herr
					break
				}
				if err = write(h.Name, h.Mode(), h.IsDir, func() (io.ReadCloser, error) { return io.NopCloser(rr), nil }); err != nil {
					break
				}
			}
		}
		if err != nil && strings.Contains(strings.ToLower(err.Error()), "password") {
			err = errors.New("the archive needs a password")
		}
	default: // tar, tar.gz
		var f *os.File
		f, err = os.Open(p)
		if err == nil {
			defer f.Close()
			var r io.Reader = f
			if !strings.HasSuffix(low, ".tar") {
				gz, gerr := gzip.NewReader(f)
				if gerr != nil {
					return "", 0, gerr
				}
				r = gz
			}
			tr := tar.NewReader(r)
			for {
				h, herr := tr.Next()
				if herr == io.EOF {
					break
				}
				if herr != nil {
					err = herr
					break
				}
				if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeDir {
					if err = next(); err != nil {
						break // skipped ones count too
					}
					continue // links and devices are skipped on purpose
				}
				if err = write(h.Name, h.FileInfo().Mode(), h.Typeflag == tar.TypeDir, func() (io.ReadCloser, error) { return io.NopCloser(tr), nil }); err != nil {
					break
				}
			}
		}
	}
	if err != nil {
		if n == 0 || errors.Is(err, tooBig) || errors.Is(err, errUnpackTooMany) {
			os.RemoveAll(dest) // nothing came out, or the half of a bomb: gone
		}
		return "", n, err
	}
	return dest, n, nil
}

// ---------- extract audio ----------

// extractAudio saves the sound of an MP4 video as an .m4a file, without
// converting anything (so it's instant and loses no quality).
func extractAudio(ctx context.Context, p string) (string, error) {
	out := uniqueFinal(filepath.Dir(p), strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))+".m4a")
	ext := extOf(p)
	if ext == ".mp4" || ext == ".m4v" || ext == ".mov" {
		if err := extractAudioMP4(p, out); err == nil {
			return out, nil
		}
	}
	ff := ffmpegPath()
	if ff == "" {
		return "", errors.New("this video's sound can only be taken out with ffmpeg.exe (put it next to Jasqet.exe)")
	}
	return out, runFFmpeg(ctx, ff, "-y", "-i", p, "-vn", "-c:a", "copy", out)
}

func extractAudioMP4(p, out string) error {
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	var pres pmp4.Presentation
	if err := pres.Unmarshal(f); err != nil {
		return err
	}
	var keep []*pmp4.Track
	for _, t := range pres.Tracks {
		switch t.Codec.(type) {
		case *codecs.MPEG4Audio, *codecs.Opus, *codecs.AC3, *codecs.MPEG1Audio, *codecs.LPCM:
			keep = append(keep, t)
		}
	}
	if len(keep) == 0 {
		return errors.New("this video has no sound track")
	}
	pres.Tracks = keep[:1]
	pres.Tracks[0].ID = 1
	w, err := os.Create(out)
	if err != nil {
		return err
	}
	err = pres.Marshal(w)
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(out)
	}
	return err
}

var _ = mpeg4audio.Config{}

// ---------- telling you ----------

func (m *Manager) sendTelegram(text string) error {
	m.mu.Lock()
	tok, chat := strings.TrimSpace(m.settings.Notify.TelegramToken), strings.TrimSpace(m.settings.Notify.TelegramChat)
	m.mu.Unlock()
	if tok == "" || chat == "" {
		return errors.New("set up Telegram first (Settings → Notifications)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	form := url.Values{"chat_id": {chat}, "text": {text}, "disable_web_page_preview": {"true"}}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.telegram.org/bot"+tok+"/sendMessage", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("couldn't reach Telegram: %s", friendlyError(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		var r struct {
			Description string `json:"description"`
		}
		json.Unmarshal(body, &r)
		return fmt.Errorf("Telegram said: %s", r.Description)
	}
	return nil
}

// TelegramFindChat: after you send any message to your bot, this finds your chat.
func (m *Manager) TelegramFindChat(token string) (string, string, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return "", "", errors.New("paste the bot token from @BotFather first")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.telegram.org/bot"+token+"/getUpdates", nil)
	resp, err := m.client.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("couldn't reach Telegram: %s", friendlyError(err))
	}
	defer resp.Body.Close()
	var r struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Result      []struct {
			Message *struct {
				Chat struct {
					ID        int64  `json:"id"`
					FirstName string `json:"first_name"`
					Title     string `json:"title"`
				} `json:"chat"`
			} `json:"message"`
		} `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&r); err != nil {
		return "", "", errors.New("Telegram sent an answer Jasqet didn't understand")
	}
	if !r.OK {
		return "", "", errors.New("that bot token isn't right: " + r.Description)
	}
	for i := len(r.Result) - 1; i >= 0; i-- {
		if msg := r.Result[i].Message; msg != nil {
			name := msg.Chat.FirstName
			if name == "" {
				name = msg.Chat.Title
			}
			return fmt.Sprint(msg.Chat.ID), name, nil
		}
	}
	return "", "", errors.New("send any message (like \"hi\") to your bot in Telegram first, then try again")
}

var _ = bytes.MinRead

func runFFmpeg(ctx context.Context, ff string, args ...string) error {
	cmd := exec.CommandContext(ctx, ff, args...)
	hideWindow(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ffmpeg: %s", lastLine(string(out)))
	}
	return nil
}
