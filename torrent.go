package main

// Torrents and magnet links, with web servers as extra sources (BEP 19 web
// seeds): pieces come from the swarm and from HTTP mirrors at the same time,
// and every piece is checked against the torrent's fingerprints.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"golang.org/x/time/rate"
)

var errTorrentRisky = errors.New("this torrent looks fake or dangerous. Check the warning before downloading")

type torrentState struct {
	mu       sync.Mutex
	client   *torrent.Client
	storages map[string]storage.ClientImplCloser
	down, up *rate.Limiter
	err      error
	roots    sync.Map // info hash → *torrentRoot
	lastUse  time.Time
	seeding  atomic.Int32
}

// closeIdleTorrents shuts the torrent engine down when nothing has used it
// for two minutes, so Jasqet waiting in the tray isn't answering the
// torrent network (DHT) all day. It starts again with the next torrent.
func (m *Manager) closeIdleTorrents() {
	m.mu.Lock()
	list := m.sortedLocked()
	m.mu.Unlock()
	for _, d := range list {
		d.mu.Lock()
		busy := d.Kind == "torrent" && (d.running || d.tor != nil)
		d.mu.Unlock()
		if busy {
			return
		}
	}
	ts := m.tor
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.client == nil || ts.seeding.Load() > 0 || time.Since(ts.lastUse) < 2*time.Minute {
		return
	}
	ts.client.Close()
	for _, s := range ts.storages {
		s.Close()
	}
	ts.client, ts.storages = nil, nil
}

func (ts *torrentState) used() {
	ts.mu.Lock()
	ts.lastUse = time.Now()
	ts.mu.Unlock()
}

// torrentRoot: the folder a torrent's files go in. A new torrent never lands
// on top of a file or folder that's already there: it goes into "Name (1)".
type torrentRoot struct {
	mu     sync.Mutex
	placed bool
	dir    string
}

type TorrentFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	Done   int64  `json:"done"`
	Wanted bool   `json:"wanted"`
}

type AddTorrentRequest struct {
	URI      string            `json:"uri"`  // magnet: or a link to a .torrent file
	Data     []byte            `json:"data"` // or the .torrent file itself
	Dir      string            `json:"dir"`
	Later    bool              `json:"later"`
	Paused   bool              `json:"paused"`
	Headers  map[string]string `json:"headers"`
	Cookies  map[string]string `json:"cookies"`
	WebSeeds []string          `json:"webSeeds"` // HTTP mirrors of the same files
}

func isTorrentLink(u string) bool {
	if strings.HasPrefix(strings.ToLower(u), "magnet:") {
		return true
	}
	pu, err := url.Parse(u)
	return err == nil && strings.HasSuffix(strings.ToLower(pu.Path), ".torrent")
}

func torrentsDir() string {
	return filepath.Join(filepath.Dir(stateFilePath()), "torrents")
}

func (m *Manager) torrentDataDir() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return filepath.Join(m.settings.DownloadDir, "Torrents")
}

func (m *Manager) torrentClient() (*torrent.Client, error) {
	ts := m.tor
	ts.mu.Lock()
	defer ts.mu.Unlock()
	ts.lastUse = time.Now()
	if ts.client != nil {
		return ts.client, nil
	}
	m.mu.Lock()
	down, up := m.settings.SpeedLimitKB, m.settings.Torrent.UploadKB
	m.mu.Unlock()
	ts.down = rate.NewLimiter(kbLimit(down), 1<<20)
	ts.up = rate.NewLimiter(kbLimit(up), 256<<10)
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = m.torrentDataDir()
	cfg.Seed = false
	cfg.DownloadRateLimiter = ts.down
	cfg.UploadRateLimiter = ts.up
	cfg.DisableWebtorrent = true
	cfg.HTTPProxy = func(r *http.Request) (*url.URL, error) {
		m.mu.Lock()
		p := m.settings.Proxy
		m.mu.Unlock()
		if p != "" && !strings.HasPrefix(p, "socks") {
			if !strings.Contains(p, "://") {
				p = "http://" + p
			}
			return url.Parse(p)
		}
		return http.ProxyFromEnvironment(r)
	}
	cfg.HTTPUserAgent = "Jasqet/" + version
	cfg.ExtendedHandshakeClientVersion = "Jasqet " + version
	cfg.Bep20 = "-TG1300-"
	cfg.Slogger = slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg.ListenPort = 42069
	if os.Getenv("JASQET_TORRENT_PORT") != "" {
		fmt.Sscan(os.Getenv("JASQET_TORRENT_PORT"), &cfg.ListenPort)
	}
	if os.Getenv("JASQET_NO_DHT") != "" { // for tests
		cfg.NoDHT = true
	}
	// Try the normal setup first, then fall back step by step on computers
	// where some kinds of network sockets aren't available.
	tweaks := []func(){
		func() {},
		func() { cfg.DisableIPv6 = true },                 // no IPv6 on this computer
		func() { cfg.ListenPort = 0 },                     // the usual port is taken
		func() { cfg.DisableUTP = true },                  // UDP connections not available
		func() { cfg.NoDHT, cfg.DisableUTP = true, true }, // TCP only
	}
	var cl *torrent.Client
	var err error
	for _, tw := range tweaks {
		tw()
		if cl, err = torrent.NewClient(cfg); err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("couldn't start torrents: %w", err)
	}
	ts.client = cl
	ts.storages = map[string]storage.ClientImplCloser{}
	return cl, nil
}

func kbLimit(kb int64) rate.Limit {
	if kb <= 0 {
		return rate.Inf
	}
	return rate.Limit(kb * 1024)
}

func (m *Manager) torrentLimits() {
	ts := m.tor
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.client == nil {
		return
	}
	m.mu.Lock()
	down, up := m.settings.SpeedLimitKB, m.settings.Torrent.UploadKB
	m.mu.Unlock()
	if r := m.limiter.Rate(); r > 0 {
		down = r / 1024
	}
	ts.down.SetLimit(kbLimit(down))
	ts.up.SetLimit(kbLimit(up))
}

func (m *Manager) storageFor(dir string) storage.ClientImpl {
	ts := m.tor
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if s, ok := ts.storages[dir]; ok {
		return s
	}
	// Pieces already on disk are found again by checking them when a torrent
	// starts, so nothing is lost even after a power cut (it costs a little
	// computer time, not data).
	opts := storage.NewFileClientOpts{ClientBaseDir: dir}
	// Same cleaned-up names Jasqet shows (Windows can't use : ? * | in names).
	opts.FilePathMaker = func(o storage.FilePathMakerOpts) string { return torrentFilePath(o.Info, o.File) }
	opts.TorrentDirMaker = ts.rootDir
	s := storage.NewFileOpts(opts)
	ts.storages[dir] = s
	return s
}

// rootDir: the folder a torrent's files go in, decided as it opens (it makes
// its empty files at once).
func (ts *torrentState) rootDir(base string, info *metainfo.Info, ih metainfo.Hash) string {
	v, ok := ts.roots.Load(ih.HexString())
	if !ok {
		return base
	}
	r := v.(*torrentRoot)
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.placed && info.BestName() != metainfo.NoName {
		if name := sanitizeFilename(info.BestName()); pathExists(filepath.Join(r.dir, name)) {
			r.dir = uniquePath(r.dir, name, nil)
		}
	}
	r.placed = true
	return r.dir
}

func pathExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// AddTorrent adds a magnet link or .torrent file.
func (m *Manager) AddTorrent(r AddTorrentRequest) (string, error) {
	var mi *metainfo.MetaInfo
	var spec *torrent.TorrentSpec
	uri := strings.TrimSpace(r.URI)
	switch {
	case len(r.Data) > 0:
		x, err := metainfo.Load(bytes.NewReader(r.Data))
		if err != nil {
			return "", errors.New("that isn't a valid .torrent file")
		}
		mi = x
	case strings.HasPrefix(strings.ToLower(uri), "magnet:"):
		s, err := torrent.TorrentSpecFromMagnetUri(uri)
		if err != nil {
			return "", errors.New("that magnet link isn't valid")
		}
		spec = s
	case uri != "":
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		body, _, _, err := m.fetchSmall(ctx, uri, reqOpts{headers: r.Headers, cookies: lowerKeys(r.Cookies)}, 20<<20)
		if err != nil {
			return "", fmt.Errorf("couldn't get the .torrent file: %s", friendlyError(err))
		}
		x, err := metainfo.Load(bytes.NewReader(body))
		if err != nil {
			return "", errors.New("that link didn't give a .torrent file")
		}
		mi = x
	default:
		return "", errors.New("please paste a magnet link or a .torrent link")
	}
	var hash, name string
	if mi != nil {
		info, err := mi.UnmarshalInfo()
		if err != nil {
			return "", errors.New("that .torrent file is damaged")
		}
		hash = mi.HashInfoBytes().HexString()
		name = info.BestName()
		os.MkdirAll(torrentsDir(), 0o755)
		var buf bytes.Buffer
		mi.Write(&buf)
		os.WriteFile(filepath.Join(torrentsDir(), hash+".torrent"), buf.Bytes(), 0o644)
		if uri == "" || !strings.HasPrefix(uri, "magnet:") {
			uri = "magnet:?xt=urn:btih:" + hash + "&dn=" + url.QueryEscape(name)
		}
	} else {
		hash = spec.InfoHash.HexString()
		name = spec.DisplayName
	}
	m.mu.Lock()
	for _, o := range m.downloads {
		if o.Kind == "torrent" && o.InfoHash == hash {
			m.mu.Unlock()
			return o.ID, nil // already in the list
		}
	}
	m.mu.Unlock()
	if name == "" {
		name = "Torrent " + hash[:8]
	}
	d := &Download{
		ID: newID(), Kind: "torrent", URL: uri, InfoHash: hash,
		FileName: sanitizeFilename(name), Size: -1, Status: StQueued, AddedAt: time.Now(),
		Mirrors: r.WebSeeds, Connections: 1,
	}
	d.Dir = m.torrentDataDir()
	if r.Dir != "" {
		d.Dir, d.DirChosen = r.Dir, true
	}
	if r.Later {
		d.Status, d.Scheduled = StScheduled, true
	}
	if r.Paused {
		d.Status = StPaused
	}
	m.mu.Lock()
	m.downloads[d.ID] = d
	m.mu.Unlock()
	m.saveSoon()
	m.schedule()
	return d.ID, nil
}

func lowerKeys(c map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range c {
		out[strings.ToLower(k)] = v
	}
	return out
}

func (m *Manager) executeTorrent(ctx context.Context, d *Download) error {
	cl, err := m.torrentClient()
	if err != nil {
		return err
	}
	defer m.tor.used() // the engine closes a while after the last torrent stops
	m.torrentLimits()
	d.mu.Lock()
	uri, hash, dir, webSeeds, placed := d.URL, d.InfoHash, d.Dir, d.Mirrors, d.Placed
	d.mu.Unlock()
	root := &torrentRoot{placed: placed, dir: dir}
	m.tor.roots.Store(strings.ToLower(hash), root)
	defer m.tor.roots.CompareAndDelete(strings.ToLower(hash), root)
	var spec *torrent.TorrentSpec
	if mi, err := metainfo.LoadFromFile(filepath.Join(torrentsDir(), hash+".torrent")); err == nil {
		spec, err = torrent.TorrentSpecFromMetaInfoErr(mi)
		if err != nil {
			return err
		}
		if strings.HasPrefix(uri, "magnet:") { // keep extra peers / web seeds from the magnet
			if ms, err := torrent.TorrentSpecFromMagnetUri(uri); err == nil {
				spec.PeerAddrs = append(spec.PeerAddrs, ms.PeerAddrs...)
				spec.Webseeds = append(spec.Webseeds, ms.Webseeds...)
				spec.Trackers = append(spec.Trackers, ms.Trackers...)
			}
		}
	} else if spec, err = torrent.TorrentSpecFromMagnetUri(uri); err != nil {
		return err
	}
	os.MkdirAll(dir, 0o755)
	spec.Storage = m.storageFor(dir)
	t, _, err := cl.AddTorrentSpec(spec)
	if err != nil {
		return err
	}
	keep := false
	defer func() {
		if !keep {
			t.Drop()
		}
	}()
	if len(webSeeds) > 0 {
		t.AddWebSeeds(webSeeds)
	}
	for _, pa := range spec.PeerAddrs {
		_ = pa // already added from the spec
	}
	d.mu.Lock()
	d.Status = StConnecting
	d.Note = "Getting the file list from other people…"
	d.tor = t
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.torrentFilesLocked()
		d.tor = nil
		if strings.HasPrefix(d.Note, "Getting the file list") || strings.HasPrefix(d.Note, "Looking for people") || strings.Contains(d.Note, "people") && !strings.HasPrefix(d.Note, "Every piece") {
			d.Note = ""
		}
		d.mu.Unlock()
	}()

	select {
	case <-t.GotInfo():
	case <-ctx.Done():
		return ctx.Err()
	}
	mi := t.Metainfo()
	os.MkdirAll(torrentsDir(), 0o755)
	if f, err := os.Create(filepath.Join(torrentsDir(), hash+".torrent")); err == nil {
		mi.Write(f)
		f.Close()
	}
	files := t.Files()
	root.mu.Lock()
	rootDir := root.dir
	root.mu.Unlock()
	d.mu.Lock()
	d.Dir = rootDir // "Name (1)" if a file or folder called Name was already there
	d.FileName = sanitizeFilename(t.Info().BestName())
	d.Placed = true
	skip := map[int]bool{}
	for _, i := range d.Skip {
		skip[i] = true
	}
	riskOK := d.DupOK
	d.torrentFilesLocked() // remember the list for when it's not running
	d.mu.Unlock()

	var wanted []*torrent.File
	for i, f := range files {
		if !skip[i] {
			wanted = append(wanted, f)
		}
	}
	d.mu.Lock()
	d.Safety = nil
	d.mu.Unlock()
	if s := checkTorrentFiles(t.Name(), wanted); s.Level == "danger" || s.Level == "warn" {
		d.mu.Lock()
		d.Safety = s
		d.mu.Unlock()
		if s.Level == "danger" && !riskOK {
			return errTorrentRisky
		}
	}
	var want int64
	for i, f := range files {
		if skip[i] {
			f.SetPriority(torrent.PiecePriorityNone)
		} else {
			f.Download()
			want += f.Length()
		}
	}
	d.mu.Lock()
	d.Size = want
	d.Status = StDownloading
	d.Note = ""
	d.mu.Unlock()

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var counted int64
	count := func() {
		st := t.Stats()
		used := st.ConnStats.BytesReadData.Int64() + st.ConnStats.BytesWrittenData.Int64()
		if used > counted {
			m.countBytes(int(used - counted))
			counted = used
		}
	}
	for {
		select {
		case <-ctx.Done():
			count()
			return ctx.Err()
		case <-tick.C:
		}
		count()
		var done int64
		for i, f := range files {
			if !skip[i] {
				done += f.BytesCompleted()
			}
		}
		st := t.Stats()
		d.mu.Lock()
		d.tDone = done
		fromWeb := st.WebSeeds.BytesReadData.Int64() > 0
		switch {
		case fromWeb && st.ActivePeers == 0:
			d.Note = "Downloading from web servers (no people sharing right now) · every piece is checked"
		case fromWeb:
			d.Note = fmt.Sprintf("%d people + web servers · every piece is checked", st.ActivePeers)
		case st.ActivePeers == 0 && len(webSeeds) == 0:
			d.Note = fmt.Sprintf("Looking for people sharing this… (%d found)", st.TotalPeers)
		case len(webSeeds) > 0:
			d.Note = fmt.Sprintf("%d people + %d web servers · every piece is checked", st.ActivePeers, len(webSeeds))
		default:
			d.Note = fmt.Sprintf("%d people (%d with the whole file) · every piece is checked", st.ActivePeers, st.ConnectedSeeders)
		}
		d.mu.Unlock()
		if done >= want {
			break
		}
	}
	d.mu.Lock()
	d.Note = "Every piece was checked against the torrent's fingerprints"
	d.mu.Unlock()
	m.mu.Lock()
	seed := m.settings.Torrent.Seed
	m.mu.Unlock()
	if !seed && len(skip) > 0 {
		// Files you skipped can still appear when they share a piece with a
		// file you wanted: remove them.
		t.Drop()
		keep = true // already dropped
		var fis []metainfo.FileInfo
		for _, f := range files {
			fis = append(fis, f.FileInfo())
		}
		removeSkipped(d, t.Info(), fis, skip)
	}
	if seed {
		keep = true
		m.tor.seeding.Add(1)
		go m.seedFor(d, t, counted)
	}
	return nil
}

// seedFor shares a finished torrent back until about as much has been sent as
// was received (or for 12 hours at most), then stops (or as soon as you
// remove the download).
func (m *Manager) seedFor(d *Download, t *torrent.Torrent, got int64) {
	defer m.tor.seeding.Add(-1)
	defer m.tor.used()
	deadline := time.Now().Add(12 * time.Hour)
	for time.Now().Before(deadline) {
		time.Sleep(10 * time.Second)
		st := t.Stats()
		if st.ConnStats.BytesWrittenData.Int64() >= got {
			break
		}
		if m.heldAny() || m.get(d.ID) == nil {
			break
		}
	}
	func() {
		defer func() { recover() }() // the engine was already closed
		t.Drop()
	}()
}

// torrentFilePath is where a torrent's file goes, relative to its folder.
func torrentFilePath(info *metainfo.Info, fi *metainfo.FileInfo) string {
	var parts []string
	if bn := info.BestName(); bn != metainfo.NoName {
		parts = append(parts, sanitizeFilename(bn))
	}
	for _, p := range fi.BestPath() {
		parts = append(parts, sanitizeFilename(p))
	}
	return filepath.Join(parts...)
}

// removeSkipped deletes the files you unticked. They're in the download's own
// folder (d.Dir: "Name (1)" when a Name was already there), never the older one.
func removeSkipped(d *Download, info *metainfo.Info, files []metainfo.FileInfo, skip map[int]bool) {
	d.mu.Lock()
	root := d.Dir
	d.mu.Unlock()
	for i := range files {
		if skip[i] {
			p := filepath.Join(root, torrentFilePath(info, &files[i]))
			os.Remove(p)
			os.Remove(p + ".part")
		}
	}
}

func (m *Manager) heldAny() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.heldLocked()
}

// torrentDiskPathLocked: where file i of a torrent is saved.
func (d *Download) torrentDiskPathLocked(i int) string {
	info := d.torrentInfoLocked()
	if info == nil {
		return ""
	}
	files := info.UpvertedFiles()
	if i < 0 || i >= len(files) {
		return ""
	}
	return filepath.Join(d.Dir, torrentFilePath(info, &files[i]))
}

// torrentInfoLocked: the torrent's file list, from the running torrent or the saved .torrent file.
func (d *Download) torrentInfoLocked() *metainfo.Info {
	if d.tor != nil && d.tor.Info() != nil {
		return d.tor.Info()
	}
	if mi, err := metainfo.LoadFromFile(filepath.Join(torrentsDir(), d.InfoHash+".torrent")); err == nil {
		if in, err := mi.UnmarshalInfo(); err == nil {
			return &in
		}
	}
	return nil
}

// removeTorrentFilesLocked deletes this torrent's own files and the folders
// they leave empty, never anything else that's in the same folder.
func (d *Download) removeTorrentFilesLocked() {
	info := d.torrentInfoLocked()
	if info == nil || d.Dir == "" {
		return
	}
	var dirs []string
	for _, f := range info.UpvertedFiles() {
		p := filepath.Join(d.Dir, torrentFilePath(info, &f))
		os.Remove(p)
		os.Remove(p + ".part")
		for dir := filepath.Dir(p); len(dir) > len(d.Dir) && !slices.Contains(dirs, dir); dir = filepath.Dir(dir) {
			dirs = append(dirs, dir)
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) }) // deepest first
	for _, dir := range dirs {
		os.Remove(dir) // only if it's empty now
	}
}

func (d *Download) torrentFilesLocked() []TorrentFile {
	if d.tor == nil || d.tor.Info() == nil {
		if d.tFiles == nil && d.InfoHash != "" {
			// not running: read the list from the saved .torrent file
			if mi, err := metainfo.LoadFromFile(filepath.Join(torrentsDir(), d.InfoHash+".torrent")); err == nil {
				if info, err := mi.UnmarshalInfo(); err == nil {
					skip := map[int]bool{}
					for _, i := range d.Skip {
						skip[i] = true
					}
					for i, f := range info.UpvertedFiles() {
						p := strings.Join(f.BestPath(), "/")
						if p == "" {
							p = info.BestName()
						}
						d.tFiles = append(d.tFiles, TorrentFile{Path: p, Size: f.Length, Wanted: !skip[i]})
					}
				}
			}
		}
		return d.tFiles
	}
	skip := map[int]bool{}
	for _, i := range d.Skip {
		skip[i] = true
	}
	var out []TorrentFile
	for i, f := range d.tor.Files() {
		out = append(out, TorrentFile{Path: f.DisplayPath(), Size: f.Length(), Done: f.BytesCompleted(), Wanted: !skip[i]})
	}
	d.tFiles = out
	return out
}

// SetTorrentFiles chooses which files of a torrent to download.
func (m *Manager) SetTorrentFiles(d *Download, skip []int) {
	d.mu.Lock()
	d.Skip = skip
	for i := range d.tFiles {
		d.tFiles[i].Wanted = true
		for _, k := range skip {
			if k == i {
				d.tFiles[i].Wanted = false
			}
		}
	}
	running := d.running
	d.mu.Unlock()
	if running { // apply at once by restarting it (nothing downloaded is lost)
		m.Pause(d)
		go func() {
			d.mu.Lock()
			done := d.done
			d.mu.Unlock()
			if done != nil {
				<-done
			}
			m.Resume(d)
		}()
	}
	m.saveSoon()
}

// ---------- fake torrent detection ----------

var videoWords = []string{"1080p", "720p", "2160p", "480p", "4k", "x264", "x265", "h264", "h265", "hevc", "bluray", "blu-ray", "brrip", "bdrip", "web-dl", "webrip", "hdrip", "dvdrip", "hdtv", "camrip", "hdcam", "yify", "yts", "s01", "s02", "e01", "season", "episode", "movie", "film", "dual audio"}

func looksLikeVideo(name string, files []*torrent.File) bool {
	low := strings.ToLower(name)
	for _, w := range videoWords {
		if strings.Contains(low, w) {
			return true
		}
	}
	return false
}

// checkTorrentFiles looks at what's inside a torrent before anything is
// downloaded: fake movie torrents carry programs, shortcuts or locked archives.
func checkTorrentFiles(name string, files []*torrent.File) *Safety {
	s := &Safety{Level: "ok"}
	video := looksLikeVideo(name, files)
	var total, videoBytes, archiveBytes int64
	type big struct {
		p string
		n int64
	}
	var all []big
	for _, f := range files {
		p := f.DisplayPath()
		// judge the name it gets on disk ("setup.bat." and "player.ex*e" are saved as .bat and .exe)
		base := sanitizeFilename(filepath.Base(p))
		ext := extOf(base)
		total += f.Length()
		all = append(all, big{p, f.Length()})
		switch {
		case isVideoExt(ext) || ext == ".m4v":
			videoBytes += f.Length()
		case ext == ".rar" || ext == ".zip" || ext == ".7z":
			archiveBytes += f.Length()
		}
		low := strings.ToLower(base)
		switch {
		case ext == ".lnk" || ext == ".scr" || ext == ".pif" || ext == ".vbs" || ext == ".js" || ext == ".hta" || ext == ".wsf" || ext == ".bat" || ext == ".cmd" || ext == ".ps1":
			s.raise("danger", fmt.Sprintf("It contains %s, a file that runs code when opened. Real movies, music and documents never need one.", filepath.Base(p)))
		case (ext == ".exe" || ext == ".msi") && video:
			s.raise("danger", fmt.Sprintf("It looks like a video but contains a program (%s). Fake video torrents ask you to run a \"player\" or \"codec\", and that's the virus.", filepath.Base(p)))
		case ext == ".exe" && (strings.Contains(low, "codec") || strings.Contains(low, "player") || strings.Contains(low, "password") || strings.Contains(low, "unlock")):
			s.raise("danger", fmt.Sprintf("It contains %s. Fake torrents use \"codec\", \"player\" or \"password\" programs to spread viruses.", filepath.Base(p)))
		case strings.Contains(low, "password") && (ext == ".txt" || ext == ".url" || ext == ".html" || ext == ".htm"):
			s.raise("warn", fmt.Sprintf("It contains %s. Torrents that make you get a password from a website are almost always scams.", filepath.Base(p)))
		case (ext == ".wmv" || ext == ".asf") && video:
			s.raise("warn", "The video is a .wmv/.asf file. New films shared as .wmv are a classic trick: playing them asks you to download a \"licence\" that's really malware.")
		case ext == ".url" && video:
			s.raise("info", fmt.Sprintf("It contains a web shortcut (%s). It's just an ad, safe to skip.", filepath.Base(p)))
		}
	}
	if video && total > 0 && archiveBytes > total/2 && videoBytes < total/10 {
		s.raise("warn", "It's named like a video, but the files are packed in an archive. Real video torrents don't pack the video, so a locked archive here usually means a password scam.")
	}
	if video && total > 0 && total < 40<<20 && videoBytes == 0 {
		s.raise("warn", "It's named like a film but it's tiny and has no video inside.")
	}
	sort.Slice(all, func(i, j int) bool { return all[i].n > all[j].n })
	return s
}
