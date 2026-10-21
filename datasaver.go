package main

// Saving data and money: a data budget Jasqet never goes over, going quiet on
// metered connections (phone hotspots), and spotting files you already have.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------- data budget ----------

type Budget struct {
	Enabled  bool  `json:"enabled"`
	Limit    int64 `json:"limit"` // bytes Jasqet may use
	Used     int64 `json:"used"`  // bytes used since the budget was set
	ResetDay int   `json:"resetDay"`
	// ResetDay > 0: on that day of each month the budget starts again
	LastReset time.Time `json:"lastReset"`
}

// budgetMargin: stop this far before the limit, so connections still in
// flight can't push it over.
const budgetMargin = 12 << 20

// countBytes records data Jasqet downloaded (for the budget and stats).
func (m *Manager) countBytes(n int) {
	m.mu.Lock()
	b := &m.settings.Budget
	m.stats.add(n)
	if !b.Enabled {
		m.mu.Unlock()
		return
	}
	b.Used += int64(n)
	over := b.Limit-b.Used <= budgetMargin
	m.mu.Unlock()
	if over {
		m.SetHold("budget", true)
	}
}

// BudgetLeft is the data still allowed, or -1 when no budget is set.
func (m *Manager) BudgetLeft() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.settings.Budget
	if !b.Enabled {
		return -1
	}
	return max(0, b.Limit-b.Used)
}

// SetBudget: "I have this much data left" (0 turns the budget off).
func (m *Manager) SetBudget(left int64, resetDay int) {
	m.mu.Lock()
	m.settings.Budget = Budget{Enabled: left > 0, Limit: left, ResetDay: resetDay, LastReset: time.Now()}
	m.dirty = true
	m.mu.Unlock()
	m.SetHold("budget", false)
	m.checkBudget()
}

func (m *Manager) checkBudget() {
	m.mu.Lock()
	b := &m.settings.Budget
	if b.Enabled && b.ResetDay > 0 {
		now := time.Now()
		if now.Day() == b.ResetDay && (b.LastReset.Year() != now.Year() || b.LastReset.Month() != now.Month() || b.LastReset.Day() != now.Day()) {
			b.Used, b.LastReset = 0, now
			m.dirty = true
		}
	}
	over := b.Enabled && b.Limit-b.Used <= budgetMargin
	m.mu.Unlock()
	m.SetHold("budget", over)
}

// fitsBudget: will this download fit in the data that's left?
func (m *Manager) fitsBudget(d *Download) (bool, int64, int64) {
	left := m.BudgetLeft()
	if left < 0 {
		return true, 0, -1
	}
	d.mu.Lock()
	need := d.Size - d.downloadedLocked()
	if d.isStream() {
		need = max(d.EstSize, d.LastSize) - d.downloadedLocked()
	}
	d.mu.Unlock()
	if need <= 0 {
		return true, 0, left
	}
	return need <= left-budgetMargin, need, left
}

// ---------- metered connections (phone hotspots, capped Wi-Fi) ----------

type meteredState struct {
	mu        sync.Mutex
	isMetered bool
	network   string // name of the current network
	override  string // the user said "download anyway" on this network
	checked   time.Time
	addrs     string
	asking    atomic.Bool // a check is still waiting for Windows' answer
}

// checkMetered looks at the connection every few seconds; Windows itself
// knows when a network is metered (Android hotspots announce it automatically).
func (m *Manager) checkMetered() {
	ms := m.metered
	if !ms.asking.CompareAndSwap(false, true) {
		return
	}
	defer ms.asking.Store(false)
	addrs := localAddrs()
	ms.mu.Lock()
	changed := addrs != ms.addrs
	stale := time.Since(ms.checked) > 5*time.Minute
	ms.addrs = addrs
	ms.mu.Unlock()
	if !changed && !stale {
		return
	}
	metered, name, ok := isMeteredNetwork()
	ms.mu.Lock()
	ms.checked = time.Now()
	if ok {
		if name != ms.network {
			ms.override = ""
		}
		ms.isMetered, ms.network = metered, name
	}
	hold := ms.isMetered && ms.override != ms.network
	ms.mu.Unlock()

	m.mu.Lock()
	mode := m.settings.MeteredMode
	limitKB := m.settings.MeteredKB
	normal := m.settings.SpeedLimitKB
	m.mu.Unlock()
	_ = normal
	switch mode {
	case "ignore":
		m.SetHold("metered", false)
		m.setRate("metered", 0)
	case "slow":
		m.SetHold("metered", false)
		if hold && limitKB > 0 {
			m.setRate("metered", limitKB*1024)
		} else {
			m.setRate("metered", 0)
		}
	default: // "pause"
		m.SetHold("metered", hold)
		m.setRate("metered", 0)
	}
}

// MeteredOverride: "download anyway" on the current metered network.
func (m *Manager) MeteredOverride() {
	ms := m.metered
	ms.mu.Lock()
	ms.override = ms.network
	ms.mu.Unlock()
	m.SetHold("metered", false)
}

func (m *Manager) MeteredInfo() (bool, string) {
	ms := m.metered
	ms.mu.Lock()
	defer ms.mu.Unlock()
	return ms.isMetered, ms.network
}

func localAddrs() string {
	ifs, _ := net.Interfaces()
	var parts []string
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			parts = append(parts, i.Name+"="+a.String())
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// ---------- duplicate check ----------

var errDuplicate = errors.New("you already have this file")

type libIndex struct {
	mu     sync.Mutex
	bySize map[int64][]string
	byName map[string][]string // lower-case base name
	built  time.Time
	files  int
}

func (m *Manager) libraryDirs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	dirs := append([]string{m.settings.DownloadDir}, m.settings.Library...)
	var out []string
	seen := map[string]bool{}
	for _, d := range dirs {
		d = strings.TrimSpace(d)
		if d != "" && !seen[strings.ToLower(d)] {
			seen[strings.ToLower(d)] = true
			out = append(out, d)
		}
	}
	return out
}

// rebuildLibrary indexes the files in your download and library folders
// (only their size and name: nothing is read).
func (m *Manager) rebuildLibrary() {
	bySize := map[int64][]string{}
	byName := map[string][]string{}
	count := 0
	for _, dir := range m.libraryDirs() {
		filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if e.IsDir() {
				if strings.HasSuffix(e.Name(), ".parts") || strings.HasPrefix(e.Name(), ".") || e.Name() == "$RECYCLE.BIN" {
					return filepath.SkipDir
				}
				return nil
			}
			if count > 300000 {
				return filepath.SkipAll
			}
			if strings.HasSuffix(e.Name(), ".part") || strings.HasSuffix(e.Name(), ".tmp") {
				return nil
			}
			info, err := e.Info()
			if err != nil || info.Size() < 256*1024 {
				return nil
			}
			bySize[info.Size()] = append(bySize[info.Size()], p)
			byName[strings.ToLower(e.Name())] = append(byName[strings.ToLower(e.Name())], p)
			count++
			return nil
		})
	}
	li := m.lib
	li.mu.Lock()
	li.bySize, li.byName, li.built, li.files = bySize, byName, time.Now(), count
	li.mu.Unlock()
}

// findDuplicate compares a few small pieces of the remote file with local
// files of the same size: about 200 KB of data instead of the whole file.
func (m *Manager) findDuplicate(ctx context.Context, d *Download) string {
	m.mu.Lock()
	on := m.settings.DupCheck
	m.mu.Unlock()
	d.mu.Lock()
	size, ok, rawURL, resumable := d.Size, d.DupOK, d.URL, d.Resumable
	d.mu.Unlock()
	if !on || ok || size < 1<<20 || !resumable || isFTP(rawURL) {
		return ""
	}
	li := m.lib
	li.mu.Lock()
	if time.Since(li.built) > 10*time.Minute {
		li.mu.Unlock()
		m.rebuildLibrary()
		li.mu.Lock()
	}
	cands := append([]string(nil), li.bySize[size]...)
	li.mu.Unlock()
	if len(cands) == 0 {
		return ""
	}
	if len(cands) > 6 {
		cands = cands[:6]
	}
	const piece = 64 << 10
	offsets := []int64{0, size/2 - piece/2, size - piece}
	var remote [][]byte
	for _, off := range offsets {
		b, err := m.rangeBytes(ctx, d, off, piece)
		if err != nil {
			return ""
		}
		remote = append(remote, b)
	}
	for _, c := range cands {
		f, err := os.Open(c)
		if err != nil {
			continue
		}
		same := true
		for i, off := range offsets {
			buf := make([]byte, piece)
			n, _ := f.ReadAt(buf, off)
			if !bytes.Equal(buf[:n], remote[i]) {
				same = false
				break
			}
		}
		f.Close()
		if same {
			return c
		}
	}
	return ""
}

func (m *Manager) rangeBytes(ctx context.Context, d *Download, off, n int64) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := m.newRequest(ctx, d)
	if err != nil {
		return nil, err
	}
	off += d.Offset // a file inside a ZIP: positions count from its start
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+n-1))
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return nil, errors.New("no ranges")
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, n))
	m.countBytes(len(b))
	return b, err
}

// findDuplicateName: for videos (size unknown in advance) look for the same name.
func (m *Manager) findDuplicateName(name string) string {
	m.mu.Lock()
	on := m.settings.DupCheck
	m.mu.Unlock()
	if !on || name == "" {
		return ""
	}
	li := m.lib
	li.mu.Lock()
	if time.Since(li.built) > 10*time.Minute {
		li.mu.Unlock()
		m.rebuildLibrary()
		li.mu.Lock()
	}
	defer li.mu.Unlock()
	if c := li.byName[strings.ToLower(name)]; len(c) > 0 {
		return c[0]
	}
	return ""
}

// ---------- usage stats ----------

type usageStats struct {
	Day   string `json:"day"`
	Today int64  `json:"today"`
	Month string `json:"month"`
	Total int64  `json:"monthTotal"`
}

func (u *usageStats) add(n int) {
	now := time.Now()
	if d := now.Format("2006-01-02"); d != u.Day {
		u.Day, u.Today = d, 0
	}
	if mo := now.Format("2006-01"); mo != u.Month {
		u.Month, u.Total = mo, 0
	}
	u.Today += int64(n)
	u.Total += int64(n)
}

func (m *Manager) budgetLeftLocked() int64 {
	b := m.settings.Budget
	if !b.Enabled {
		return -1
	}
	return max(0, b.Limit-b.Used)
}
