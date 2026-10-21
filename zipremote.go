package main

// Take one file out of a ZIP on a server without downloading the whole ZIP:
// Jasqet reads the ZIP's table of contents from its end (a few KB), then
// downloads only the bytes of the file you pick (with many connections and
// resume, like any other download) and unpacks it, checking its CRC.

import (
	"compress/flate"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"archive/zip"
)

type ZipEntry struct {
	Name      string    `json:"name"`
	Size      int64     `json:"size"`      // unpacked
	Packed    int64     `json:"packed"`    // bytes to download
	Method    uint16    `json:"method"`    //
	CRC32     uint32    `json:"crc32"`     //
	Modified  time.Time `json:"modified"`  //
	Encrypted bool      `json:"encrypted"` //
	Supported bool      `json:"supported"` // Jasqet can unpack it
	Dir       bool      `json:"dir"`       //
	headerOff int64
}

type ZipListing struct {
	URL     string     `json:"url"`
	Size    int64      `json:"size"`
	Entries []ZipEntry `json:"entries"`
	Used    int64      `json:"used"` // bytes downloaded to read the contents
}

// httpReaderAt reads parts of a remote file with byte-range requests,
// keeping what it read so the same bytes aren't fetched twice.
type httpReaderAt struct {
	m     *Manager
	ctx   context.Context
	d     *Download
	size  int64
	block int64
	mu    sync.Mutex
	cache map[int64][]byte
	used  int64
}

func (r *httpReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off >= r.size {
		return 0, io.EOF
	}
	n := 0
	for n < len(p) && off < r.size {
		b := off / r.block * r.block
		r.mu.Lock()
		data, ok := r.cache[b]
		r.mu.Unlock()
		if !ok {
			ln := min(r.block, r.size-b)
			var err error
			data, err = r.m.rangeBytes(r.ctx, r.d, b, ln)
			if err != nil {
				return n, err
			}
			if int64(len(data)) != ln {
				return n, io.ErrUnexpectedEOF
			}
			r.mu.Lock()
			if len(r.cache) > 256 {
				r.cache = map[int64][]byte{}
			}
			r.cache[b] = data
			r.used += int64(len(data))
			r.mu.Unlock()
		}
		c := copy(p[n:], data[off-b:])
		n += c
		off += int64(c)
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func looksLikeZip(raw string) bool {
	low := strings.ToLower(raw)
	if i := strings.IndexAny(low, "?#"); i >= 0 {
		low = low[:i]
	}
	return strings.HasSuffix(low, ".zip") || strings.HasSuffix(low, ".apk") || strings.HasSuffix(low, ".jar") || strings.HasSuffix(low, ".epub") || strings.HasSuffix(low, ".cbz")
}

// ListZip shows what's inside a ZIP on a server.
func (m *Manager) ListZip(ctx context.Context, raw string, o reqOpts) (*ZipListing, error) {
	d := &Download{URL: directLink(raw), Headers: o.headers, Cookies: o.cookies, User: o.user, Password: o.pass}
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := m.probe(pctx, d)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("couldn't open the ZIP: %s", friendlyError(err))
	}
	if !d.Resumable || d.Size <= 0 {
		return nil, errors.New("this server doesn't allow taking part of a file, so the whole ZIP has to be downloaded")
	}
	ra := &httpReaderAt{m: m, ctx: ctx, d: d, size: d.Size, block: 64 << 10, cache: map[int64][]byte{}}
	zr, err := zip.NewReader(ra, d.Size)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("that file isn't a ZIP Jasqet can read (or it's damaged)")
	}
	l := &ZipListing{URL: d.URL, Size: d.Size}
	for _, f := range zr.File {
		e := ZipEntry{
			Name: f.Name, Size: int64(f.UncompressedSize64), Packed: int64(f.CompressedSize64),
			Method: f.Method, CRC32: f.CRC32, Modified: f.Modified, Encrypted: f.Flags&1 != 0,
			Dir: strings.HasSuffix(f.Name, "/"),
		}
		e.Supported = !e.Encrypted && (f.Method == zip.Store || f.Method == zip.Deflate)
		l.Entries = append(l.Entries, e)
	}
	sort.SliceStable(l.Entries, func(i, j int) bool { return l.Entries[i].Name < l.Entries[j].Name })
	l.Used = ra.used
	return l, nil
}

type AddZipRequest struct {
	URL     string            `json:"url"`
	Names   []string          `json:"names"`
	Dir     string            `json:"dir"`
	Later   bool              `json:"later"`
	Headers map[string]string `json:"headers"`
	Cookies map[string]string `json:"cookies"`
	User    string            `json:"user"`
	Pass    string            `json:"password"`
}

// AddZipEntries adds one download per chosen file inside the ZIP.
func (m *Manager) AddZipEntries(ctx context.Context, r AddZipRequest) ([]string, error) {
	o := reqOpts{headers: r.Headers, cookies: lowerKeys(r.Cookies), user: r.User, pass: r.Pass}
	src := &Download{URL: directLink(r.URL), Headers: r.Headers, Cookies: o.cookies, User: r.User, Password: r.Pass}
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	err := m.probe(pctx, src)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("couldn't open the ZIP: %s", friendlyError(err))
	}
	ra := &httpReaderAt{m: m, ctx: ctx, d: src, size: src.Size, block: 64 << 10, cache: map[int64][]byte{}}
	zr, err := zip.NewReader(ra, src.Size)
	if err != nil {
		return nil, errors.New("couldn't read the ZIP's contents")
	}
	want := map[string]bool{}
	for _, n := range r.Names {
		want[n] = true
	}
	// local headers are small: read them with small requests
	ra.block = 4 << 10
	var ids []string
	for _, f := range zr.File {
		if !want[f.Name] || strings.HasSuffix(f.Name, "/") {
			continue
		}
		if f.Flags&1 != 0 {
			return ids, fmt.Errorf("%s needs a password", f.Name)
		}
		if f.Method != zip.Store && f.Method != zip.Deflate {
			return ids, fmt.Errorf("%s is packed in a way Jasqet can't unpack on its own. Download the whole ZIP", f.Name)
		}
		off, err := f.DataOffset()
		if err != nil {
			return ids, fmt.Errorf("couldn't find %s in the ZIP", f.Name)
		}
		d := &Download{
			ID: newID(), Kind: "zipentry", URL: src.URL, Headers: r.Headers, Cookies: o.cookies,
			User: r.User, Password: r.Pass, Status: StQueued, AddedAt: time.Now(),
			FileName: sanitizeFilename(path.Base(f.Name)), Size: int64(f.CompressedSize64), Resumable: true,
			Offset: off, Zip: &ZipPart{Name: f.Name, Method: f.Method, CRC32: f.CRC32, Size: int64(f.UncompressedSize64), Packed: int64(f.CompressedSize64)},
			Page: r.URL,
		}
		m.mu.Lock()
		d.Connections = m.settings.Connections
		m.mu.Unlock()
		if d.Size < 4<<20 {
			d.Connections = 1
		}
		if r.Dir != "" {
			d.Dir, d.DirChosen = r.Dir, true
		}
		if r.Later {
			d.Status, d.Scheduled = StScheduled, true
		}
		m.mu.Lock()
		m.downloads[d.ID] = d
		m.mu.Unlock()
		ids = append(ids, d.ID)
	}
	if len(ids) == 0 {
		return nil, errors.New("pick at least one file")
	}
	m.saveSoon()
	m.schedule()
	return ids, nil
}

// ZipPart describes a file taken out of a ZIP on a server.
type ZipPart struct {
	Packed int64  `json:"packed"` // bytes downloaded (the file packed inside the ZIP)
	Name   string `json:"name"`
	Method uint16 `json:"method"`
	CRC32  uint32 `json:"crc32"`
	Size   int64  `json:"size"`
}

// unpackZipPart turns the downloaded bytes into the real file and checks it.
func unpackZipPart(part, final string, z *ZipPart) error {
	in, err := os.Open(part)
	if err != nil {
		return err
	}
	defer in.Close()
	var r io.Reader = in
	if z.Method == zip.Deflate {
		fr := flate.NewReader(in)
		defer fr.Close()
		r = fr
	}
	out, err := os.Create(final + ".unpack")
	if err != nil {
		return err
	}
	h := crc32.NewIEEE()
	// never more than the ZIP says (one byte more shows it lied): a damaged or
	// hostile ZIP could otherwise inflate until the drive is full
	n, err := io.Copy(io.MultiWriter(out, h), io.LimitReader(r, z.Size+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > z.Size {
		err = errors.New("the file taken out of the ZIP came out bigger than the ZIP says it is, so Jasqet stopped (the ZIP is damaged or made to fill your drive)")
	}
	if err == nil && (n != z.Size || h.Sum32() != z.CRC32) {
		err = errors.New("the file taken out of the ZIP didn't pass its check (CRC), so it's damaged. Press Restart")
	}
	if err != nil {
		os.Remove(final + ".unpack")
		return err
	}
	in.Close()
	os.Remove(part)
	return os.Rename(final+".unpack", final)
}

var _ = http.MethodGet
var _ = filepath.Join
