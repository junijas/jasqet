package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anacrolix/torrent"
)

const defaultUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36"

type Status string

const (
	StQueued      Status = "queued"
	StConnecting  Status = "connecting"
	StDownloading Status = "downloading"
	StPaused      Status = "paused"
	StDone        Status = "completed"
	StError       Status = "error"
	StMerging     Status = "merging"
	StScheduled   Status = "scheduled"
	StDuplicate   Status = "duplicate"
	StCancelled   Status = "cancelled" // stopped by you; stays in the list (and keeps what it got) until removed
)

type Download struct {
	mu sync.Mutex

	ID          string            `json:"id"`
	URL         string            `json:"url"`
	FileName    string            `json:"fileName"`
	Dir         string            `json:"dir"`
	DirChosen   bool              `json:"dirChosen"`
	Size        int64             `json:"size"`
	Resumable   bool              `json:"resumable"`
	Connections int               `json:"connections"`
	Headers     map[string]string `json:"headers,omitempty"`
	Status      Status            `json:"status"`
	Error       string            `json:"error,omitempty"`
	Segments    []*Segment        `json:"segments,omitempty"`
	AddedAt     time.Time         `json:"addedAt"`
	CompletedAt time.Time         `json:"completedAt,omitempty"`

	// Video streams (HLS / DASH)
	Kind    string            `json:"kind,omitempty"` // "" = normal file, "media" = video stream
	Variant string            `json:"variant,omitempty"`
	Quality string            `json:"quality,omitempty"`
	Cookies map[string]string `json:"cookies,omitempty"` // host -> Cookie header
	EstSize int64             `json:"estSize,omitempty"`
	Note    string            `json:"note,omitempty"`
	Placed  bool              `json:"placed,omitempty"`

	User     string `json:"user,omitempty"` // login for sites / FTP servers that ask for one
	Password string `json:"password,omitempty"`
	Secrets  string `json:"secrets,omitempty"` // cookies and login, encrypted, in the state file (secrets.go)

	secretsPlain, secretsSealed string // the last ones encrypted

	Scheduled    bool   `json:"scheduled,omitempty"`    // belongs to the scheduler's queue
	SpeedLimitKB int64  `json:"speedLimitKB,omitempty"` // this download only (0 = no limit)
	PlanChunks   int    `json:"planChunks,omitempty"`   // chunk count of the saved video plan
	YTFallback   bool   `json:"ytFallback,omitempty"`   // let yt-dlp download the parts
	DupOK        bool   `json:"dupOK,omitempty"`        // download even though a copy exists
	DupPath      string `json:"dupPath,omitempty"`      // where the existing copy is
	// progress remembered while a video isn't running (e.g. paused, then app restarted)
	LastBytes int64 `json:"lastBytes,omitempty"`
	LastSize  int64 `json:"lastSize,omitempty"`

	Page         string   `json:"page,omitempty"`         // the page the link came from (to get a fresh one later)
	ETag         string   `json:"etag,omitempty"`         // the file's version on the server
	LastModified string   `json:"lastModified,omitempty"` //
	Mirrors      []string `json:"mirrors,omitempty"`      // other links to the same file
	Restarts     int      `json:"restarts,omitempty"`     // times the file changed on the server
	FixedConns   bool     `json:"fixedConns,omitempty"`   // the user chose the connection count
	Checksum     string   `json:"checksum,omitempty"`     // "sha256:…" expected (typed in or found on the site)
	ChecksumFrom string   `json:"checksumFrom,omitempty"` // where it was found
	Safety       *Safety  `json:"safety,omitempty"`       // results of the safety checks

	lastAutoRefresh time.Time
	pushback        int // "too many requests" answers since the last check
	okValidators    map[string]bool
	badChunks       int                  // damaged video chunks fetched again
	previewWait     int64                // the viewer is waiting for byte previewWait-1 (0 = not waiting)
	previewChunk    int                  // video-site chunk the viewer needs next (+1; 0 = none)
	previewHeads    map[any]*previewHead // where each player is reading this download (preview.go)
	noLineUp        bool                 // a speed limit or FTP: pieces aren't shared out behind the player
	tPreview        *torrent.Torrent     // this torrent's pieces come in order for the viewer
	tReading        int                  // the viewer's player is reading from it right now…
	tReadAt         time.Time            // …or last read at
	mirrors         *mirrorState

	// torrents
	WatchID  string    `json:"watchId,omitempty"` // added by this watch rule
	Clip     *ClipSpec `json:"clip,omitempty"`    // only part of a video
	Subs     string    `json:"subs,omitempty"`    // subtitles to save (language)
	Offset   int64     `json:"offset,omitempty"`  // where this file starts inside the remote file
	Zip      *ZipPart  `json:"zip,omitempty"`     // a file taken out of a ZIP on the server
	InfoHash string    `json:"infoHash,omitempty"`
	Skip     []int     `json:"skip,omitempty"` // files of the torrent not wanted
	tor      *torrent.Torrent
	tFiles   []TorrentFile
	tDone    int64

	segState    []byte // per chunk: 0 waiting, 1 downloading, 2 done
	segDone     int
	segBytes    int64
	segInflight int64
	sizeSum     int64 // for the size estimate: total bytes of chunks seen so far
	sizeCount   int
	mergePct    float64

	limiter      *Limiter
	mediaWorkers int
	lanSrc       string // copying from another Jasqet on the network

	running    bool
	stopReason string
	cancel     context.CancelFunc
	done       chan struct{}
	speed      float64
	lastBytes  int64

	file      *os.File              // the .part file while downloading (flushed before progress is saved)
	durable   []Segment             // progress as of the last flush of that file: what gets saved
	syncedAt  time.Time             // when the file was last flushed
	resolved  map[int]*resolvedLink // where each link redirected to, this run
	noResolve map[int]bool          // links whose redirect targets can't be reused

	connRate     float64       // how fast one connection typically goes (bytes/s), across all pieces
	waiting      int           // connections waiting for something to do
	previewPos   int64         // where someone is watching / listening (preview while downloading)
	previewUntil time.Time     // …and until when that counts
	changed      chan struct{} // closed when a piece finishes or fails: wakes connections waiting for work

	retryAt     time.Time // waiting to try again by itself after a network problem
	autoRetries int       // tries since the last time it made progress
	startBytes  int64     // bytes it had when this run started
}

func (d *Download) partPath() string { return filepath.Join(d.Dir, d.FileName) + ".part" }

// downloadedLocked counts bytes already on disk. Caller holds d.mu.
func (d *Download) downloadedLocked() int64 {
	if d.Status == StDone {
		return d.Size
	}
	if d.Kind == "torrent" {
		if d.tor != nil {
			return d.tDone
		}
		return d.LastBytes
	}
	if d.isStream() {
		if d.segState == nil {
			return d.LastBytes
		}
		return d.segBytes + d.segInflight
	}
	var n int64
	for _, s := range d.Segments {
		end := s.Pos
		if s.End >= 0 && end > s.End+1 {
			end = s.End + 1
		}
		if end > s.Start {
			n += end - s.Start
		}
	}
	return n
}

type Settings struct {
	DownloadDir   string   `json:"downloadDir"`
	MaxConcurrent int      `json:"maxConcurrent"`
	Connections   int      `json:"connections"`
	SpeedLimitKB  int64    `json:"speedLimitKB"`
	Categorize    bool     `json:"categorize"`
	Tidy          bool     `json:"tidy"` // tidy names of videos from release sites, a folder per series
	Proxy         string   `json:"proxy"`
	Schedule      Schedule `json:"schedule"`

	Budget      Budget   `json:"budget"`
	MeteredMode string   `json:"meteredMode"` // "pause" (default), "slow", "ignore"
	MeteredKB   int64    `json:"meteredKB"`
	Library     []string `json:"library"`  // folders checked for files you already have
	DupCheck    bool     `json:"dupCheck"` // check for duplicates before downloading

	LAN LANSettings `json:"lan"`

	AutoConnections bool `json:"autoConnections"` // tune connections to what's fastest

	Safety  SafetySettings  `json:"safety"`
	Torrent TorrentSettings `json:"torrent"`
	After   []AfterRule     `json:"after"`  // what to do when a download finishes
	Notify  NotifySettings  `json:"notify"` //
	Smooth  bool            `json:"smooth"` // keep browsing and calls smooth while downloading
	Watch   []WatchRule     `json:"watch"`  // channels, podcasts, releases and pages to follow
	Viewer  ViewerSettings  `json:"viewer"` // the built-in viewer
	Theme   string          `json:"theme"`  // "light", "dark" or "" (like the system): the viewer follows it too

	Extensions []string `json:"extensions,omitempty"` // browser extensions you allowed (besides Jasqet's own folder)
}

type TorrentSettings struct {
	UploadKB int64 `json:"uploadKB"` // upload speed limit for torrents (0 = none)
	Seed     bool  `json:"seed"`     // share back after finishing
}

func defaultSchedule() Schedule {
	return Schedule{Start: "04:00", KeepAwake: true}
}

func (d *Download) getLimiter() *Limiter {
	if d.limiter == nil {
		d.limiter = &Limiter{}
		d.limiter.SetRate(d.SpeedLimitKB * 1024)
	}
	return d.limiter
}

// throttle applies the overall speed limit and this download's own limit.
func (m *Manager) throttle(ctx context.Context, d *Download, n int) {
	d.mu.Lock()
	lan := d.lanSrc != ""
	d.mu.Unlock()
	if lan {
		return // local network: no internet data, no limits
	}
	m.countBytes(n)
	m.limiter.Wait(ctx, n)
	d.mu.Lock()
	l := d.getLimiter()
	d.mu.Unlock()
	l.Wait(ctx, n)
}

type Manager struct {
	mu        sync.Mutex
	downloads map[string]*Download
	settings  Settings
	client    *http.Client
	limiter   *Limiter
	stateFile string
	dirty     bool

	tools       *toolsMgr
	ai          *aiState        // automatic subtitles
	cleared     *clearedBatch   // the last "Clear finished", for Undo
	holds       map[string]bool // reasons everything is paused: "quiet", "metered", "budget"
	metered     *meteredState
	lib         *libIndex
	lan         *lanState
	stats       usageStats
	learn       speedLearn // when the internet is usually fastest
	sched       schedState
	power       *powerAction
	refreshWait *refreshWait
	netSig      string
	tor         *torrentState
	autoOpen    func(*Download)  // the viewer's "open when finished"
	rates       map[string]int64 // speed limits from different places; the lowest wins
	smooth      *smoothState
	ytsess      *ytSessionState

	toastMu sync.Mutex
	toasts  []pendingToast // finished downloads waiting a moment to share one notification
	closing atomic.Bool    // Jasqet is quitting: the last save flushes everything

	saveMu sync.Mutex  // one save at a time
	saving atomic.Bool // a background save is running
}

type refreshWait struct {
	ID    string
	Until time.Time
}

func NewManager(stateFile string) *Manager {
	m := &Manager{
		downloads: map[string]*Download{},
		limiter:   &Limiter{},
		tools:     &toolsMgr{},
		ai:        newAIState(),
		metered:   &meteredState{},
		lib:       &libIndex{},
		lan:       &lanState{},
		tor:       &torrentState{},
		rates:     map[string]int64{},
		smooth:    &smoothState{},
		ytsess:    &ytSessionState{},
		stateFile: stateFile,
		settings: Settings{
			DownloadDir:   defaultDownloadDir(),
			MaxConcurrent: 3,
			Connections:   16,
			Categorize:    true,
			Tidy:          true,
			Schedule:      defaultSchedule(),
			MeteredMode:   "pause",
			MeteredKB:     100,
			DupCheck:      true,

			AutoConnections: true,
			Safety:          SafetySettings{Scan: true, FindChecksums: true},
			Torrent:         TorrentSettings{UploadKB: 50},
			Viewer:          defaultViewerSettings(),
		},
	}
	tr := &http.Transport{
		Proxy: func(r *http.Request) (*url.URL, error) {
			m.mu.Lock()
			p := m.settings.Proxy
			m.mu.Unlock()
			if p != "" {
				if !strings.Contains(p, "://") {
					p = "http://" + p
				}
				return url.Parse(p)
			}
			return http.ProxyFromEnvironment(r)
		},
		DialContext:           dialYouTubeIPv4,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   20 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		DisableCompression:    true,
		ReadBufferSize:        256 * 1024,
		// Remember TLS sessions, so the many connections a download opens to the
		// same server skip most of the handshake (a round trip less on TLS 1.2).
		TLSClientConfig: &tls.Config{ClientSessionCache: tls.NewLRUClientSessionCache(512)},
		// HTTP/1.1 only: HTTP/2 would squeeze all our connections into one TCP stream,
		// which defeats the whole point of downloading over many connections.
		ForceAttemptHTTP2: false,
		TLSNextProto:      map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
	m.client = &http.Client{Transport: newAuthTransport(tr)}
	m.load()
	m.setRate("user", m.settings.SpeedLimitKB*1024)
	go m.loop()
	m.schedule()
	return m
}

// ---------- persistence ----------

type stateFile struct {
	Settings  Settings     `json:"settings"`
	Downloads []*Download  `json:"downloads"`
	Sched     schedState   `json:"sched"`
	Stats     usageStats   `json:"stats"`
	Learn     speedLearn   `json:"learn"`
	ExtSeen   extSeenSaved `json:"extSeen"`             // when the browser extension last checked in
	YTSession string       `json:"ytSession,omitempty"` // encrypted
}

func (m *Manager) load() {
	// If the computer lost power while the list was being saved, the newest
	// copy may be cut short: fall back to the copy saved just before it.
	var st stateFile
	ok := false
	for _, p := range []string{m.stateFile, m.stateFile + ".tmp", m.stateFile + ".bak"} {
		b, err := os.ReadFile(p)
		if err != nil || len(b) == 0 {
			continue
		}
		st = stateFile{Settings: m.settings} // settings missing from older versions keep their defaults
		if json.Unmarshal(b, &st) == nil {
			ok = true
			break
		}
	}
	if !ok {
		if _, err := os.Stat(m.stateFile); err == nil {
			// unreadable: keep it aside (rather than replacing it with an empty
			// list on the next save) so nothing is lost for good
			os.Rename(m.stateFile, m.stateFile+".unreadable-"+time.Now().Format("20060102-150405"))
		}
		return
	}
	if st.Settings.DownloadDir != "" {
		m.settings = st.Settings
		if m.settings.Schedule.Start == "" {
			m.settings.Schedule = defaultSchedule()
		}
	}
	m.settings = openSettings(m.settings)
	m.settings.Viewer.normalize()
	m.sched = st.Sched
	m.stats = st.Stats
	m.learn = st.Learn
	restoreSeen(st.ExtSeen)
	m.ytsess.cookie = unprotect(st.YTSession)
	for _, d := range st.Downloads {
		d.revealSecrets()
		if d.Status == StDownloading || d.Status == StConnecting || d.Status == StMerging {
			d.Status = StQueued // pick up where we left off
		}
		m.downloads[d.ID] = d
	}
}

func (m *Manager) saveSoon() {
	m.mu.Lock()
	m.dirty = true
	m.mu.Unlock()
}

// save writes the download list and settings.
//
// Progress is only recorded once the bytes it counts are safely on the disk:
// the positions are noted first, then the file is flushed, then those
// positions are saved. Otherwise a power cut could leave the list saying
// "40% done" while the last few seconds of data never reached the disk, and
// resuming would leave a hole of zeros in the file.
func (m *Manager) save() {
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	sess := protect(m.ytSessionCookie())
	m.mu.Lock()
	st := stateFile{Settings: m.settings, Sched: m.sched, Stats: m.stats, Learn: m.learn, ExtSeen: seenSnapshot(), YTSession: sess}
	list := m.sortedLocked()
	m.dirty = false
	m.mu.Unlock()

	type plain Download
	raw := make([]json.RawMessage, 0, len(list))
	for _, d := range list {
		d.mu.Lock()
		f := d.file
		var snap []Segment
		if f != nil && (d.durable == nil || time.Since(d.syncedAt) >= syncEvery || m.closing.Load()) {
			snap = make([]Segment, len(d.Segments))
			for i, sg := range d.Segments {
				snap[i] = *sg
			}
		}
		d.mu.Unlock()
		if snap != nil && f.Sync() == nil { // what the noted positions count is now on the disk
			d.mu.Lock()
			d.durable, d.syncedAt = snap, time.Now()
			d.mu.Unlock()
		}
		d.mu.Lock()
		live := d.Segments
		if d.file != nil && d.durable != nil {
			// still downloading: save the progress as of the last flush
			d.Segments = make([]*Segment, len(d.durable))
			for i := range d.durable {
				d.Segments[i] = &d.durable[i]
			}
		}
		if d.file == nil {
			d.durable = nil
		}
		restore := d.hideSecretsLocked()
		b, err := json.Marshal((*plain)(d))
		restore()
		d.Segments = live
		d.mu.Unlock()
		if err == nil {
			raw = append(raw, b)
		}
	}
	out, err := json.Marshal(struct {
		Settings  Settings          `json:"settings"`
		Downloads []json.RawMessage `json:"downloads"`
		Sched     schedState        `json:"sched"`
		Stats     usageStats        `json:"stats"`
		Learn     speedLearn        `json:"learn"`
		ExtSeen   extSeenSaved      `json:"extSeen"`
		YTSession string            `json:"ytSession,omitempty"`
	}{sealSettings(st.Settings), raw, st.Sched, st.Stats, st.Learn, st.ExtSeen, st.YTSession})
	if err != nil {
		return
	}
	os.MkdirAll(filepath.Dir(m.stateFile), 0o700)
	tmp := m.stateFile + ".tmp"
	if writeFileSynced(tmp, out) != nil {
		return
	}
	os.Rename(m.stateFile, m.stateFile+".bak") // the previous copy, in case this one is damaged
	os.Rename(tmp, m.stateFile)
}

// syncEvery: how often a downloading file is flushed to the disk so its
// progress can be saved. Flushing more often slows USB sticks and old hard disks;
// at worst this many seconds of data are downloaded again after a power cut.
const syncEvery = 10 * time.Second

// writeFileSynced writes a file and makes sure it reached the disk.
func writeFileSynced(p string, b []byte) error {
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) // yours only (Mac and Linux)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// saveInBackground saves without holding up the caller (flushing big files
// to a slow disk can take a moment); saves never overlap.
func (m *Manager) saveInBackground() {
	if !m.saving.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer m.saving.Store(false)
		m.save()
	}()
}

func (m *Manager) sortedLocked() []*Download {
	list := make([]*Download, 0, len(m.downloads))
	for _, d := range m.downloads {
		list = append(list, d)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].AddedAt.Before(list[j].AddedAt) })
	return list
}

// loop measures speeds twice a second and saves progress every few seconds.
func (m *Manager) loop() {
	t := time.NewTicker(500 * time.Millisecond)
	ticks := 0
	last := time.Now()
	for now := range t.C {
		ticks++
		elapsed := now.Sub(last).Seconds()
		last = now
		if elapsed <= 0 {
			elapsed = 0.5
		}
		m.mu.Lock()
		list := m.sortedLocked()
		dirty := m.dirty
		m.mu.Unlock()
		active, waiting := false, false
		var internet float64 // from the internet, without a download's own limit
		for _, d := range list {
			d.mu.Lock()
			cur := d.downloadedLocked()
			waiting = waiting || d.Status == StQueued
			if d.running {
				active = true
				inst := float64(cur-d.lastBytes) / elapsed
				if inst < 0 {
					inst = 0
				}
				d.speed = d.speed*0.6 + inst*0.4
				if d.lanSrc == "" && d.SpeedLimitKB == 0 {
					internet += d.speed
				}
			} else {
				d.speed = 0
			}
			d.lastBytes = cur
			d.mu.Unlock()
		}
		m.mu.Lock()
		m.learn.sample(now, internet, internet > 0 && len(m.rates) == 0) // no limit of any kind: the line's own speed
		m.mu.Unlock()
		if ticks%2 == 0 {
			m.checkSchedule(now)
			m.powerTick(now)
			if m.retriesDue(now) {
				m.schedule()
			}
		}
		if ticks%10 == 0 {
			setAwake(m.wantAwake())
		}
		// Watching the network (adapters every 2 s, Windows' metered flag through
		// PowerShell) only matters while something downloads or waits to.
		busy := active || waiting
		if ticks%20 == 1 && busy {
			go m.checkMetered()
		}
		if ticks%4 == 3 {
			if busy {
				m.checkNetChange()
			} else {
				m.mu.Lock()
				m.netSig = "" // after a quiet spell (or sleep), start afresh
				m.mu.Unlock()
			}
		}
		if ticks%120 == 2 {
			m.checkBudget()
		}
		if ticks%120 == 60 {
			go m.closeIdleTorrents()
		}
		// progress is only flushed every syncEvery (10 s), so saving more often while
		// downloading only rewrites the same list
		if dirty || (active && ticks%20 == 0) {
			m.saveInBackground()
		}
	}
}

// ---------- scheduling ----------

func (m *Manager) schedule() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.heldLocked() {
		return
	}
	running := 0
	for _, d := range m.downloads {
		d.mu.Lock()
		if d.running {
			running++
		}
		d.mu.Unlock()
	}
	left := m.budgetLeftLocked()
	for _, d := range m.sortedLocked() {
		if running >= m.settings.MaxConcurrent {
			return
		}
		d.mu.Lock()
		if d.Status == StQueued && !d.running && !time.Now().Before(d.retryAt) {
			if left >= 0 {
				need := d.Size - d.downloadedLocked()
				if d.isStream() {
					need = max(d.EstSize, d.LastSize) - d.downloadedLocked()
				}
				if need > 0 && need > left-budgetMargin {
					d.Note = fmt.Sprintf("Waiting: needs about %s but only %s is left in your data budget", humanSize(need), humanSize(left))
					d.mu.Unlock()
					continue
				}
				if strings.HasPrefix(d.Note, "Waiting: needs") {
					d.Note = ""
				}
			}
			m.startLocked(d)
			running++
		}
		d.mu.Unlock()
	}
}

// startLocked launches a download. Caller holds m.mu and d.mu.
func (m *Manager) startLocked(d *Download) {
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel = cancel
	d.running = true
	d.stopReason = ""
	d.Error = ""
	d.Status = StConnecting
	d.done = make(chan struct{})
	d.retryAt = time.Time{}
	d.startBytes = d.downloadedLocked()
	if d.Connections <= 0 {
		d.Connections = m.settings.Connections
	}
	go func() {
		err := m.execute(ctx, d)
		d.mu.Lock()
		d.running = false
		if d.isStream() && d.segState != nil {
			var v DownloadView
			d.mediaView(&v)
			d.LastBytes, d.LastSize = d.segBytes+d.segInflight, v.Size
		}
		if d.Kind == "torrent" {
			d.LastBytes = d.tDone
		}
		switch {
		case d.stopReason == "pause":
			d.Status = StPaused
		case d.stopReason == "cancel":
			d.Status = StCancelled
		case d.stopReason == "schedule":
			d.Status = StScheduled
		case d.stopReason == "hold":
			d.Status = StQueued // carries on when the hold is lifted
		case errors.Is(err, errDuplicate):
			d.Status = StDuplicate
		case errors.Is(err, errTorrentRisky):
			d.Status = StPaused
			d.Error = err.Error()
		case d.stopReason == "delete":
		case err == nil:
			d.Status = StDone
			d.CompletedAt = time.Now()
			delete(d.Cookies, ytSessionKey) // don't keep your YouTube session with finished downloads
			if !d.isStream() {
				d.Segments = compactSegments(d.Segments)
			}
		default:
			d.Error = friendlyError(err)
			if wait, ok := d.autoRetryLocked(err); ok {
				d.Error = withoutResumeHint(d.Error)
				// a network problem: try again by itself in a little while
				d.Status = StQueued
				d.retryAt = time.Now().Add(wait)
			} else {
				d.Status = StError
			}
		}
		finished, failed := d.Status == StDone && d.stopReason == "", d.Status == StError
		close(d.done)
		d.mu.Unlock()
		if finished {
			go m.afterDownload(d)
		} else if failed {
			m.notifyFailed(d)
		}
		m.saveSoon()
		m.schedule()
	}()
}

func compactSegments(s []*Segment) []*Segment {
	if len(s) <= 1 {
		return s
	}
	end := int64(-1) // pieces are added out of order: the last one isn't the end of the file
	for _, sg := range s {
		end = max(end, sg.End)
	}
	return []*Segment{{Start: 0, End: end, Pos: end + 1}}
}

// serverWords: the error without a file or folder name in it, so a status
// code only counts where the server said it ("Movies 2404" isn't a 404).
func serverWords(err error) string {
	var pe *fs.PathError
	var le *os.LinkError
	switch {
	case errors.As(err, &pe):
		return pe.Err.Error()
	case errors.As(err, &le):
		return le.Err.Error()
	}
	return err.Error()
}

func friendlyError(err error) string {
	var ns *noSpaceError
	if errors.As(err, &ns) {
		return ns.Error()
	}
	var ne net.Error
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	msg := err.Error()
	said := serverWords(err)
	switch {
	case strings.Contains(msg, "connection refused"):
		return "The server refused the connection. It may be down, so try again later"
	case strings.Contains(msg, "connection reset") || strings.Contains(msg, "stalled") || strings.Contains(msg, "closed early"):
		return "The connection kept dropping. Press Resume to continue where it stopped"
	case strings.Contains(msg, "certificate"):
		return "The site's security certificate isn't valid"
	case strings.Contains(msg, "no space left") || strings.Contains(msg, "not enough space"):
		return "Your disk is full. Free some space, then press Resume"
	case errors.As(err, &ne) && ne.Timeout():
		return "Connection timed out. Check your internet and press Resume"
	case strings.Contains(msg, "no such host"):
		return "Couldn't reach the server (no internet, or wrong address)"
	case strings.Contains(said, "401"):
		return "This site needs a login. Add your username and password (Add link → Login), or use the browser extension while signed in"
	case strings.Contains(said, "407"):
		return "The proxy needs a login. Put it in Settings → Proxy as http://user:password@host:port"
	case strings.Contains(said, "403"):
		return "Server refused (403). The link may have expired. Get a fresh link from the site"
	case strings.Contains(said, "404"):
		return "File not found on the server (404)"
	case strings.Contains(said, "410"):
		return "Link has expired (410). Get a fresh link from the site"
	case errors.Is(err, io.ErrUnexpectedEOF) || msg == "EOF":
		return "The connection was cut off. Press Resume to continue where it stopped"
	case strings.Contains(msg, "being used by another process"):
		return "The file is open in another program. Close it, then try again"
	case errors.Is(err, fs.ErrPermission):
		return "Windows didn't let Jasqet use that file or folder (access denied). Choose another folder"
	}
	// a file or folder problem: Windows' own words, without Go's "open C:\…:" in front
	var pe *fs.PathError
	var le *os.LinkError
	switch {
	case errors.As(err, &pe):
		return fmt.Sprintf("Couldn't use “%s”: %s", filepath.Base(pe.Path), pe.Err)
	case errors.As(err, &le):
		return fmt.Sprintf("Couldn't use “%s”: %s", filepath.Base(le.Old), le.Err)
	}
	return msg
}

// placeFile picks the folder (by category) and reserves a unique filename.
func (m *Manager) placeFile(d *Download) {
	m.mu.Lock()
	base := m.settings.DownloadDir
	categorize, tidy := m.settings.Categorize, m.settings.Tidy
	others := map[string]bool{}
	for _, o := range m.downloads {
		if o != d {
			o.mu.Lock()
			if o.Status != StDone {
				others[strings.ToLower(filepath.Join(o.Dir, o.FileName))] = true
			}
			o.mu.Unlock()
		}
	}
	m.mu.Unlock()

	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.DirChosen {
		d.Dir = base
		if categorize {
			d.Dir = filepath.Join(base, categoryOf(d.FileName))
		}
	}
	if tidy && d.Kind == "" { // a plain file: video streams and torrents keep their own names
		name, series := tidyVideoName(d.FileName)
		d.FileName = name
		if series != "" && !d.DirChosen {
			d.Dir = filepath.Join(d.Dir, series)
		}
	}
	os.MkdirAll(d.Dir, 0o755)
	p := uniquePath(d.Dir, d.FileName, func(p string) bool { return others[strings.ToLower(p)] })
	d.FileName = filepath.Base(p)
	d.Placed = true
	if d.isStream() {
		os.MkdirAll(p+".parts", 0o755)
	} else if f, err := os.OpenFile(p+".part", os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
		f.Close()
	}
}

// ---------- public actions ----------

type AddRequest struct {
	URL         string            `json:"url"`
	URLs        string            `json:"urls"`
	FileName    string            `json:"fileName"`
	Dir         string            `json:"dir"`
	Connections int               `json:"connections"`
	Headers     map[string]string `json:"headers"`
	Paused      bool              `json:"paused"`
	Later       bool              `json:"later"`   // wait for the scheduler
	Cookies     map[string]string `json:"cookies"` // per site, from the browser extension
	User        string            `json:"user"`
	Password    string            `json:"password"`
	Page        string            `json:"page"`     // the page the link was on
	Mirrors     bool              `json:"mirrors"`  // all the links are copies of one file
	Checksum    string            `json:"checksum"` // expected SHA-256 / SHA-1 / MD5
}

func (m *Manager) Add(r AddRequest) ([]string, error) {
	var urls []string
	for _, line := range strings.Split(r.URL+"\n"+r.URLs, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		expanded, err := expandPattern(line)
		if err != nil {
			return nil, err
		}
		for _, link := range expanded {
			u, err := url.Parse(link)
			if strings.HasPrefix(strings.ToLower(link), "magnet:") {
				urls = append(urls, link)
				continue
			}
			if err != nil || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "ftp") || u.Host == "" {
				return nil, errors.New("not a valid link: " + link)
			}
			urls = append(urls, directLink(link))
		}
	}
	if len(urls) == 0 {
		return nil, errors.New("please paste a download link")
	}
	m.mu.Lock()
	conns := m.settings.Connections
	m.mu.Unlock()
	if r.Connections > 0 {
		conns = r.Connections
	}
	if conns > 32 {
		conns = 32
	}
	var mirrors []string
	if r.Mirrors && len(urls) > 1 {
		urls, mirrors = urls[:1], urls[1:]
	}
	checksum, err := parseChecksum(r.Checksum)
	if err != nil {
		return nil, err
	}
	var ids []string
	for i, u := range urls {
		if isTorrentLink(u) {
			id, err := m.AddTorrent(AddTorrentRequest{URI: u, Dir: r.Dir, Later: r.Later, Paused: r.Paused, Headers: r.Headers, Cookies: r.Cookies, WebSeeds: mirrors})
			if err != nil {
				return nil, err
			}
			ids = append(ids, id)
			continue
		}
		d := &Download{
			ID:          newID(),
			URL:         u,
			Connections: conns,
			FixedConns:  r.Connections > 0,
			Headers:     r.Headers,
			Status:      StQueued,
			Size:        -1,
			AddedAt:     time.Now().Add(time.Duration(i) * time.Millisecond),
		}
		if len(urls) == 1 && r.FileName != "" {
			d.FileName = sanitizeFilename(r.FileName)
		}
		d.Page = r.Page
		if d.Page == "" && r.Headers["Referer"] != "" {
			d.Page = r.Headers["Referer"]
		}
		// A cookie that came with the link belongs to that link's site only, so
		// it's never sent to the page, mirrors or other sites.
		if c := r.Headers["Cookie"]; c != "" {
			d.Headers = withoutCookie(r.Headers)
			if d.Cookies == nil {
				d.Cookies = map[string]string{}
			}
			if h := strings.ToLower(hostOf(u)); d.Cookies[h] == "" {
				d.Cookies[h] = c
			}
		}
		d.Mirrors = mirrors
		if len(urls) == 1 {
			d.Checksum = checksum
		}
		if r.Dir != "" {
			d.Dir = r.Dir
			d.DirChosen = true
		}
		if r.Paused {
			d.Status = StPaused
		}
		if r.Later {
			d.Status, d.Scheduled = StScheduled, true
		}
		if r.User != "" {
			d.User, d.Password = r.User, r.Password
		}
		if len(r.Cookies) > 0 {
			d.Cookies = map[string]string{}
			for h, c := range r.Cookies {
				d.Cookies[strings.ToLower(h)] = c
			}
		}
		m.mu.Lock()
		m.downloads[d.ID] = d
		m.mu.Unlock()
		ids = append(ids, d.ID)
	}
	m.saveSoon()
	m.schedule()
	return ids, nil
}

func (m *Manager) get(id string) *Download {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.downloads[id]
}

// Cancel stops a download for good, but leaves it in the list (with what it
// already downloaded, in case you change your mind) until you remove it.
func (m *Manager) Cancel(d *Download) {
	d.mu.Lock()
	if d.running {
		d.stopReason = "cancel"
		d.cancel()
	} else if d.Status != StDone {
		d.Status = StCancelled
	}
	d.Scheduled = false
	d.retryAt, d.autoRetries = time.Time{}, 0
	d.mu.Unlock()
	m.saveSoon()
	m.schedule()
}

func (m *Manager) Pause(d *Download) {
	d.mu.Lock()
	if d.running {
		d.stopReason = "pause"
		d.cancel()
	} else if d.Status == StQueued || d.Status == StScheduled {
		d.Status = StPaused
		d.Scheduled = false
	}
	d.retryAt, d.autoRetries = time.Time{}, 0
	d.mu.Unlock()
	m.saveSoon()
}

// Later moves a download into the scheduler's queue.
func (m *Manager) Later(d *Download) {
	d.mu.Lock()
	if d.running {
		d.stopReason = "schedule"
		d.Scheduled = true
		d.cancel()
	} else if d.Status != StDone {
		d.Status, d.Scheduled = StScheduled, true
	}
	d.retryAt, d.autoRetries = time.Time{}, 0
	d.mu.Unlock()
	m.saveSoon()
}

// SetLive changes connections / speed limit, even while downloading.
func (m *Manager) SetLive(d *Download, conns int, limitKB int64) {
	d.mu.Lock()
	if conns > 0 {
		d.Connections = min(conns, 32)
		d.FixedConns = true
	}
	if limitKB >= 0 {
		d.SpeedLimitKB = limitKB
		d.getLimiter().SetRate(limitKB * 1024)
	}
	d.mu.Unlock()
	m.saveSoon()
}

func (m *Manager) Resume(d *Download) {
	d.mu.Lock()
	if !d.running && (d.Status == StPaused || d.Status == StError || d.Status == StScheduled || d.Status == StDuplicate || d.Status == StCancelled) {
		d.Status = StQueued
		d.Scheduled = false
	}
	d.retryAt, d.autoRetries = time.Time{}, 0 // you asked: now, not later
	d.mu.Unlock()
	m.saveSoon()
	m.schedule()
}

func (m *Manager) Restart(d *Download) {
	d.mu.Lock()
	if d.running {
		d.mu.Unlock()
		return
	}
	if d.Kind == "torrent" {
		if d.Placed && d.FileName != "" {
			d.removeTorrentFilesLocked()
		}
		d.LastBytes, d.tDone = 0, 0
	} else if d.Status != StDone && d.FileName != "" {
		os.Remove(d.partPath())
		if d.isStream() {
			os.RemoveAll(d.partsDir())
		}
	}
	if d.isStream() {
		d.YTFallback = false
		d.Placed = false
		d.Note = ""
		d.LastBytes, d.LastSize = 0, 0
		d.segState = nil
		if !strings.HasSuffix(d.FileName, ".mp4") && !strings.HasSuffix(d.FileName, ".m4a") {
			d.FileName = strings.TrimSuffix(d.FileName, filepath.Ext(d.FileName)) + ".mp4"
		}
	}
	d.Segments = nil
	d.Size = -1
	d.Status = StQueued
	d.CompletedAt = time.Time{}
	d.retryAt, d.autoRetries = time.Time{}, 0
	d.mu.Unlock()
	m.saveSoon()
	m.schedule()
}

// clearUndoFor: how long "Clear finished" can be undone. Until then the
// entries are only hidden: a finished torrent keeps its saved .torrent file.
var clearUndoFor = 2 * time.Minute

// clearedBatch: the entries one "Clear finished" took off the list.
type clearedBatch struct {
	list []*Download
	done bool // the time ran out (or another clear came): they're gone for good
}

// ClearDone takes every finished download off the list (the files stay on
// disk). Undo brings them back for a while; after that they're removed for good.
func (m *Manager) ClearDone() int {
	m.mu.Lock()
	b := &clearedBatch{}
	for id, d := range m.downloads {
		d.mu.Lock()
		done := d.Status == StDone && !d.running
		d.mu.Unlock()
		if done {
			b.list = append(b.list, d)
			delete(m.downloads, id)
		}
	}
	old := m.cleared
	if len(b.list) > 0 {
		m.cleared = b
	}
	m.dirty = true
	m.mu.Unlock()
	if len(b.list) > 0 {
		m.finishClear(old) // a newer clear: the older one can't be undone any more
		time.AfterFunc(clearUndoFor, func() { m.finishClear(b) })
	}
	return len(b.list)
}

// UndoClear puts back what the last "Clear finished" took off the list.
func (m *Manager) UndoClear() int {
	m.mu.Lock()
	b := m.cleared
	n := 0
	if b != nil && !b.done {
		for _, d := range b.list {
			if _, taken := m.downloads[d.ID]; !taken {
				m.downloads[d.ID] = d
				n++
			}
		}
		b.done = true
		m.cleared = nil
		m.dirty = true
	}
	m.mu.Unlock()
	return n
}

// finishClear removes a cleared batch for good (what Delete does besides
// taking it off the list), unless it was undone.
func (m *Manager) finishClear(b *clearedBatch) {
	if b == nil {
		return
	}
	m.mu.Lock()
	if b.done {
		m.mu.Unlock()
		return
	}
	b.done = true
	if m.cleared == b {
		m.cleared = nil
	}
	m.mu.Unlock()
	for _, d := range b.list {
		m.Delete(d, false)
	}
}

func (m *Manager) Delete(d *Download, withFile bool) {
	m.mu.Lock()
	delete(m.downloads, d.ID)
	m.mu.Unlock()
	d.mu.Lock()
	done := d.done
	if d.running {
		d.stopReason = "delete"
		d.cancel()
	}
	d.mu.Unlock()
	go func() {
		if done != nil {
			<-done
		}
		d.mu.Lock()
		if d.Kind == "torrent" {
			if (withFile || d.Status != StDone) && d.Placed && d.FileName != "" {
				d.removeTorrentFilesLocked() // needs the .torrent file for the list
			}
			os.Remove(filepath.Join(torrentsDir(), d.InfoHash+".torrent"))
		} else if d.Status != StDone && d.FileName != "" {
			os.Remove(d.partPath())
			if d.isStream() && d.Placed {
				os.RemoveAll(d.partsDir())
			}
		} else if withFile && d.FileName != "" {
			os.Remove(filepath.Join(d.Dir, d.FileName))
		}
		d.mu.Unlock()
	}()
	m.saveSoon()
	m.schedule()
}

func (m *Manager) SetSettings(s Settings) {
	s.Viewer.normalize()
	if s.MaxConcurrent < 1 {
		s.MaxConcurrent = 1
	}
	if s.MaxConcurrent > 10 {
		s.MaxConcurrent = 10
	}
	if s.Connections < 1 {
		s.Connections = 1
	}
	if s.Connections > 32 {
		s.Connections = 32
	}
	if s.SpeedLimitKB < 0 {
		s.SpeedLimitKB = 0
	}
	if s.DownloadDir == "" {
		s.DownloadDir = defaultDownloadDir()
	}
	m.mu.Lock()
	lanChanged := fmt.Sprint(m.settings.LAN) != fmt.Sprint(s.LAN)
	m.settings = s
	m.dirty = true
	m.mu.Unlock()
	m.setRate("user", s.SpeedLimitKB*1024)
	if lanChanged {
		go m.startLAN()
	}
	m.schedule()
}

// ---------- snapshots for the UI ----------

type DownloadView struct {
	ID          string     `json:"id"`
	URL         string     `json:"url"`
	FileName    string     `json:"fileName"`
	Dir         string     `json:"dir"`
	Size        int64      `json:"size"`
	Downloaded  int64      `json:"downloaded"`
	Speed       int64      `json:"speed"`
	Status      Status     `json:"status"`
	Error       string     `json:"error,omitempty"`
	Resumable   bool       `json:"resumable"`
	Connections int        `json:"connections"`
	Active      int        `json:"active"`
	Category    string     `json:"category"`
	AddedAt     time.Time  `json:"addedAt"`
	CompletedAt time.Time  `json:"completedAt"`
	Segments    [][3]int64 `json:"segments"`
	Stopping    bool       `json:"stopping"`
	Kind        string     `json:"kind,omitempty"`
	Quality     string     `json:"quality,omitempty"`
	Estimated   bool       `json:"estimated,omitempty"`
	BarTotal    int64      `json:"barTotal,omitempty"`
	MergePct    float64    `json:"mergePct,omitempty"`
	Note        string     `json:"note,omitempty"`
	LimitKB     int64      `json:"limitKB,omitempty"`
	Scheduled   bool       `json:"scheduled,omitempty"`
	DupPath     string     `json:"dupPath,omitempty"`
	RetryIn     int64      `json:"retryIn,omitempty"` // seconds until it tries again by itself

	Safety       *Safety `json:"safety,omitempty"`
	Checksum     string  `json:"checksum,omitempty"`
	ChecksumFrom string  `json:"checksumFrom,omitempty"`
	Mirrors      int     `json:"mirrors,omitempty"`   // other links to the same file
	MirrorsOK    int     `json:"mirrorsOK,omitempty"` // of those, how many are being used
	Clip         string  `json:"clip,omitempty"`
	Subs         string  `json:"subs,omitempty"`
	Watch        string  `json:"watch,omitempty"` // name of the watch rule that added it
	Page         string  `json:"page,omitempty"`
	Files        int     `json:"files,omitempty"`   // torrents: files inside
	ZipName      string  `json:"zipName,omitempty"` // taken out of a ZIP: its path there
	AutoConns    bool    `json:"autoConns,omitempty"`
	FixedChunks  int     `json:"fixedChunks,omitempty"` // damaged chunks fetched again
}

func (m *Manager) Snapshot() ([]DownloadView, Settings) {
	m.mu.Lock()
	list := m.sortedLocked()
	s := m.settings
	m.mu.Unlock()
	out := make([]DownloadView, 0, len(list))
	for _, d := range list {
		d.mu.Lock()
		v := DownloadView{
			ID: d.ID, URL: d.URL, FileName: d.FileName, Dir: d.Dir, Size: d.Size,
			Downloaded: d.downloadedLocked(), Speed: int64(d.speed), Status: d.Status,
			Error: d.Error, Resumable: d.Resumable, Connections: d.Connections,
			Category: categoryOf(d.FileName), AddedAt: d.AddedAt, CompletedAt: d.CompletedAt,
			Stopping: d.running && d.stopReason != "",
			Segments: [][3]int64{},
			LimitKB:  d.SpeedLimitKB, Scheduled: d.Scheduled, DupPath: d.DupPath, Note: d.Note,
		}
		if v.FileName == "" {
			v.FileName = filenameFromURL(d.URL)
		}
		if d.Status == StQueued && !d.retryAt.IsZero() {
			if left := time.Until(d.retryAt); left > 0 {
				v.RetryIn = int64(left.Seconds() + 0.999)
			}
		}
		for _, sg := range d.Segments {
			if sg.active && !sg.done() {
				v.Active++
			}
			if len(v.Segments) < 128 {
				v.Segments = append(v.Segments, [3]int64{sg.Start, sg.End, sg.Pos})
			}
		}
		if d.isStream() {
			d.mediaView(&v)
		}
		v.Kind = d.Kind
		v.Safety, v.Checksum, v.ChecksumFrom, v.Page = d.Safety, d.Checksum, d.ChecksumFrom, d.Page
		v.Mirrors = len(d.Mirrors)
		if d.mirrors != nil {
			for _, ok := range d.mirrors.ok[1:] {
				if ok {
					v.MirrorsOK++
				}
			}
		}
		v.Clip, v.Subs = d.Clip.label(), d.Subs
		v.FixedChunks = d.badChunks
		if d.Zip != nil {
			v.ZipName = d.Zip.Name
		}
		if d.Kind == "torrent" {
			v.Files = len(d.torrentFilesLocked())
		}
		v.AutoConns = s.AutoConnections && !d.FixedConns && d.Kind == ""
		if d.WatchID != "" {
			for _, w := range s.Watch {
				if w.ID == d.WatchID {
					v.Watch = w.Name
				}
			}
		}
		d.mu.Unlock()
		out = append(out, v)
	}
	return out, s
}

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

var categories = map[string][]string{
	"Video":      {"mp4", "mkv", "avi", "mov", "wmv", "flv", "webm", "m4v", "3gp", "ts", "mpg", "mpeg", "srt", "vtt", "ass"},
	"Music":      {"mp3", "wav", "flac", "aac", "m4a", "ogg", "wma", "opus"},
	"Compressed": {"zip", "rar", "7z", "tar", "gz", "bz2", "xz", "tgz", "iso", "img"},
	"Programs":   {"exe", "msi", "apk", "dmg", "deb", "rpm", "appimage", "msix", "pkg", "bat", "xapk"},
	"Documents":  {"pdf", "doc", "docx", "xls", "xlsx", "ppt", "pptx", "txt", "epub", "csv", "odt", "rtf"},
	"Images":     {"jpg", "jpeg", "png", "gif", "webp", "svg", "bmp", "heic", "tiff"},
}

func categoryOf(name string) string {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	for cat, exts := range categories {
		for _, e := range exts {
			if e == ext {
				return cat
			}
		}
	}
	return "General"
}

// mediaView fills in progress for a video stream: size estimate and a chunk map.
func (d *Download) mediaView(v *DownloadView) {
	v.Kind, v.Quality, v.Note = d.Kind, d.Quality, d.Note
	v.Resumable = true
	v.MergePct = d.mergePct
	if d.Status == StDone {
		return
	}
	total := len(d.segState)
	if total == 0 && d.LastSize > 0 {
		v.Size, v.Estimated = d.LastSize, true
		return
	}
	v.Size = d.EstSize
	if d.sizeCount >= 3 && total > 0 {
		v.Size = int64(float64(d.sizeSum) / float64(d.sizeCount) * float64(total))
	}
	if v.Size <= 0 {
		v.Size = -1
	}
	v.Estimated = true
	if total == 0 {
		return
	}
	v.BarTotal = int64(total)
	for i := 0; i < total; {
		st := d.segState[i]
		j := i + 1
		for j < total && d.segState[j] == st && st != 1 {
			j++
		}
		switch st {
		case 2:
			v.Segments = append(v.Segments, [3]int64{int64(i), int64(j - 1), int64(j)})
		case 1:
			v.Active++
			v.Segments = append(v.Segments, [3]int64{int64(i), int64(i), int64(i)})
		}
		i = j
	}
	if len(v.Segments) > 400 {
		v.Segments = v.Segments[:400]
	}
}

type AddMediaRequest struct {
	URL         string            `json:"url"`
	Variant     string            `json:"variant"`
	Quality     string            `json:"quality"`
	Title       string            `json:"title"`
	AudioOnly   bool              `json:"audioOnly"`
	EstSize     int64             `json:"estSize"`
	Headers     map[string]string `json:"headers"`
	Cookies     map[string]string `json:"cookies"`
	Connections int               `json:"connections"`
	Dir         string            `json:"dir"`
	Later       bool              `json:"later"`
	Clip        *ClipSpec         `json:"clip"`
	Duration    float64           `json:"duration"`
}

func (m *Manager) AddMedia(r AddMediaRequest) (string, error) {
	u, err := url.Parse(r.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", errors.New("not a valid video link")
	}
	m.mu.Lock()
	conns := m.settings.Connections
	m.mu.Unlock()
	if r.Connections > 0 {
		conns = min(r.Connections, 32)
	}
	title := strings.TrimSpace(r.Title)
	if title == "" {
		title = strings.TrimSuffix(filenameFromURL(r.URL), filepath.Ext(filenameFromURL(r.URL)))
	}
	if title == "" || title == "index" || title == "master" || title == "playlist" || title == "manifest" {
		title = "Video " + time.Now().Format("2006-01-02 15.04")
	}
	ext := ".mp4"
	if r.AudioOnly || r.Variant == "audio" {
		ext = ".m4a"
	}
	cookies := map[string]string{}
	for h, c := range r.Cookies {
		cookies[strings.ToLower(h)] = c
	}
	d := &Download{
		ID:          newID(),
		Kind:        "media",
		URL:         r.URL,
		Variant:     r.Variant,
		Quality:     r.Quality,
		FileName:    sanitizeFilename(title) + ext,
		Connections: conns,
		Headers:     r.Headers,
		Cookies:     cookies,
		EstSize:     r.EstSize,
		Size:        -1,
		Status:      StQueued,
		AddedAt:     time.Now(),
	}
	if r.Dir != "" {
		d.Dir, d.DirChosen = r.Dir, true
	}
	if r.Later {
		d.Status, d.Scheduled = StScheduled, true
	}
	if r.Clip != nil && (r.Clip.Start > 0 || r.Clip.End > 0) {
		c := *r.Clip
		d.Clip = &c
		ext := filepath.Ext(d.FileName)
		d.FileName = strings.TrimSuffix(d.FileName, ext) + " (" + c.label() + ")" + ext
		d.EstSize = clipEstimateDur(d.EstSize, r.Duration, c)
	}
	m.mu.Lock()
	m.downloads[d.ID] = d
	m.mu.Unlock()
	m.saveSoon()
	m.schedule()
	return d.ID, nil
}

// Refresh gives a paused or failed download a new link (e.g. after the old one
// expired) and keeps everything already downloaded.
func (m *Manager) Refresh(ctx context.Context, d *Download, newURL string, headers map[string]string) error {
	u, err := url.Parse(strings.TrimSpace(newURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "ftp") || u.Host == "" {
		return errors.New("that isn't a valid link")
	}
	newURL = u.String()
	d.mu.Lock()
	kind, size, running, done := d.Kind, d.Size, d.running, d.done
	if running {
		d.stopReason = "pause"
		d.cancel()
	}
	d.mu.Unlock()
	if running && done != nil {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	}
	switch kind {
	case "yt":
		return errors.New("YouTube downloads get a fresh link by themselves each time they start")
	case "media":
		d.mu.Lock()
		d.URL = newURL
		if len(headers) > 0 {
			d.Headers = headers
		}
		if d.Placed {
			os.Remove(filepath.Join(d.partsDir(), "plan.json"))
		}
	default:
		tmp, err := m.checkLink(ctx, d, newURL, headers, false)
		if err != nil {
			return err
		}
		d.mu.Lock()
		d.URL = newURL
		d.Headers = withoutCookie(tmp.Headers)
		if c := tmp.Headers["Cookie"]; c != "" {
			if d.Cookies == nil {
				d.Cookies = map[string]string{}
			}
			d.Cookies[strings.ToLower(hostOf(newURL))] = c
		}
		if tmp.Size > 0 && size > 0 {
			d.Resumable = tmp.Resumable
		}
	}
	d.Error = ""
	d.retryAt, d.autoRetries = time.Time{}, 0 // a fresh link: try it now
	if d.Status != StDone {
		d.Status = StQueued
	}
	d.mu.Unlock()
	m.mu.Lock()
	if m.refreshWait != nil && m.refreshWait.ID == d.ID {
		m.refreshWait = nil
	}
	m.mu.Unlock()
	m.saveSoon()
	m.schedule()
	return nil
}

func withoutCookie(h map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		if !strings.EqualFold(k, "Cookie") {
			out[k] = v
		}
	}
	return out
}

// checkLink makes sure a new link works and is for the same file.
func (m *Manager) checkLink(ctx context.Context, d *Download, newURL string, headers map[string]string, strict bool) (*Download, error) {
	d.mu.Lock()
	size := d.Size
	tmp := &Download{URL: newURL, Headers: d.Headers, Cookies: d.Cookies, FileName: "x"}
	if strings.EqualFold(hostOf(newURL), hostOf(d.URL)) {
		tmp.User, tmp.Password = d.User, d.Password // a login only goes to the site it's for
	} else if d.Headers["Authorization"] != "" {
		tmp.Headers = maps.Clone(d.Headers)
		delete(tmp.Headers, "Authorization")
	}
	if len(headers) > 0 {
		tmp.Headers = headers
	}
	if c := tmp.Headers["Cookie"]; c != "" && len(tmp.Cookies) > 0 {
		cp := map[string]string{}
		for k, v := range tmp.Cookies {
			cp[k] = v
		}
		cp[strings.ToLower(hostOf(newURL))] = c
		tmp.Cookies = cp
	}
	d.mu.Unlock()
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	err := m.probe(pctx, tmp)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("the new link doesn't work: %s", friendlyError(err))
	}
	if size > 0 && tmp.Size > 0 && tmp.Size != size {
		return nil, fmt.Errorf("that link is for a different file (%s, but this download is %s)", humanSize(tmp.Size), humanSize(size))
	}
	if strict && size > 0 && tmp.Size <= 0 {
		return nil, errors.New("that link doesn't say how big the file is, so it can't be checked")
	}
	return tmp, nil
}

// WaitForRefresh: the next link the browser sends that matches this download's
// size becomes its new link.
func (m *Manager) WaitForRefresh(d *Download) {
	m.mu.Lock()
	m.refreshWait = &refreshWait{ID: d.ID, Until: time.Now().Add(3 * time.Minute)}
	m.mu.Unlock()
}

func (m *Manager) pendingRefresh() *Download {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.refreshWait == nil || time.Now().After(m.refreshWait.Until) {
		m.refreshWait = nil
		return nil
	}
	return m.downloads[m.refreshWait.ID]
}

func humanSize(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

// Status extras for the UI: scheduler, pending power action, refresh wait.
type Extras struct {
	NextRun     string             `json:"nextRun,omitempty"`
	SchedActive bool               `json:"schedActive,omitempty"`
	StopAt      string             `json:"stopAt,omitempty"`
	Power       *powerAction       `json:"power,omitempty"`
	RefreshID   string             `json:"refreshId,omitempty"`
	Holds       []string           `json:"holds,omitempty"`
	Metered     bool               `json:"metered,omitempty"`
	Network     string             `json:"network,omitempty"`
	BudgetLeft  int64              `json:"budgetLeft"`
	BudgetLimit int64              `json:"budgetLimit,omitempty"`
	UsedToday   int64              `json:"usedToday"`
	UsedMonth   int64              `json:"usedMonth"`
	Smooth      *SmoothInfo        `json:"smooth,omitempty"`
	YTSession   bool               `json:"ytSession,omitempty"` // a YouTube session from the browser is saved
	Speeds      *SpeedInsight      `json:"speeds,omitempty"`    // when the internet is usually fastest
	Subs        map[string]SubsJob `json:"subs,omitempty"`      // automatic subtitles being made, by download
}

func (m *Manager) Extras() Extras {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	e := Extras{NextRun: m.settings.Schedule.NextRun(now), SchedActive: m.sched.Active, Power: m.power}
	e.Speeds = m.learn.insight(now, m.settings.Schedule)
	e.Subs = m.ai.jobsView()
	if m.sched.Active && !m.sched.StopAt.IsZero() {
		e.StopAt = m.sched.StopAt.Format("15:04")
	}
	if m.refreshWait != nil && now.Before(m.refreshWait.Until) {
		e.RefreshID = m.refreshWait.ID
	}
	for r, on := range m.holds {
		if on {
			e.Holds = append(e.Holds, r)
		}
	}
	sort.Strings(e.Holds)
	e.BudgetLeft = m.budgetLeftLocked()
	if m.settings.Budget.Enabled {
		e.BudgetLimit = m.settings.Budget.Limit
	}
	st := m.stats
	if st.Day == now.Format("2006-01-02") {
		e.UsedToday = st.Today
	}
	if st.Month == now.Format("2006-01") {
		e.UsedMonth = st.Total
	}
	ms := m.metered
	ms.mu.Lock()
	e.Metered, e.Network = ms.isMetered, ms.network
	ms.mu.Unlock()
	e.YTSession = m.ytSessionCookie() != ""
	if m.settings.Smooth {
		ss := m.smooth
		ss.mu.Lock()
		e.Smooth = &SmoothInfo{On: true, Cap: ss.cap, RTT: ss.rtt.Milliseconds(), Base: ss.base.Milliseconds(), Note: ss.note}
		ss.mu.Unlock()
	}
	return e
}

// isStream: a video download made of many chunks (HLS / DASH / YouTube).
func (d *Download) isStream() bool { return d.Kind == "media" || d.Kind == "yt" }

var baseDialer = &net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second}

// dialYouTubeIPv4: YouTube ties each video link to the address that asked for it.
// yt-dlp runs with --force-ipv4, so YouTube's video servers are reached over IPv4
// too. Otherwise computers with both IPv4 and IPv6 can get "403 Forbidden".
func dialYouTubeIPv4(ctx context.Context, network, addr string) (net.Conn, error) {
	if host, _, err := net.SplitHostPort(addr); err == nil && strings.HasSuffix(strings.ToLower(host), ".googlevideo.com") && network == "tcp" {
		network = "tcp4"
	}
	return baseDialer.DialContext(ctx, network, addr)
}

// ---------- holds: pause everything for a reason, carry on afterwards ----------

func (m *Manager) heldLocked() bool {
	for _, on := range m.holds {
		if on {
			return true
		}
	}
	return false
}

// SetHold pauses (on) or releases (off) all downloads for one reason.
func (m *Manager) SetHold(reason string, on bool) {
	m.mu.Lock()
	if m.holds == nil {
		m.holds = map[string]bool{}
	}
	if m.holds[reason] == on {
		m.mu.Unlock()
		return
	}
	m.holds[reason] = on
	held := m.heldLocked()
	if on {
		for _, d := range m.downloads {
			d.mu.Lock()
			// torrents reconnect to people by themselves after a network change
			if d.running && d.stopReason == "" && !(reason == "network" && d.Kind == "torrent") {
				d.stopReason = "hold"
				d.cancel()
			}
			d.mu.Unlock()
		}
	}
	m.mu.Unlock()
	if !held {
		m.schedule()
	}
}

func (m *Manager) Holds() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for r, on := range m.holds {
		if on {
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}

func (m *Manager) Quiet() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.holds["quiet"]
}

func (m *Manager) SetQuiet(on bool) { m.SetHold("quiet", on) }

func (m *Manager) PauseAll() {
	m.mu.Lock()
	list := m.sortedLocked()
	m.mu.Unlock()
	for _, d := range list {
		m.Pause(d)
	}
}

func (m *Manager) ResumeAll() {
	m.mu.Lock()
	list := m.sortedLocked()
	m.mu.Unlock()
	for _, d := range list {
		d.mu.Lock()
		st := d.Status
		d.mu.Unlock()
		if st == StPaused || st == StError {
			m.Resume(d)
		}
	}
}

func (m *Manager) ToggleSchedule() {
	m.mu.Lock()
	m.settings.Schedule.Enabled = !m.settings.Schedule.Enabled
	m.dirty = true
	m.mu.Unlock()
}

type Summary struct {
	Active, Scheduled int
	Speed             int64
	NextRun           string
	SchedOn, Quiet    bool
	BudgetLeft        int64 // -1 = no budget set
}

func (m *Manager) Summary() Summary {
	list, _ := m.Snapshot()
	var s Summary
	for _, d := range list {
		switch d.Status {
		case StDownloading, StConnecting, StMerging:
			s.Active++
			s.Speed += d.Speed
		case StScheduled:
			s.Scheduled++
		}
	}
	e := m.Extras()
	s.NextRun = e.NextRun
	m.mu.Lock()
	s.SchedOn = m.settings.Schedule.Enabled
	s.Quiet = m.holds["quiet"]
	m.mu.Unlock()
	s.BudgetLeft = m.BudgetLeft()
	return s
}
