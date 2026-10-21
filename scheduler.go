package main

// Scheduler: start downloads at a set time (e.g. when night data is cheap),
// stop them at another, then optionally put the computer to sleep or shut it down.

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Schedule struct {
	Enabled   bool   `json:"enabled"`
	Start     string `json:"start"`     // "HH:MM"
	Stop      string `json:"stop"`      // "HH:MM", "" = run until finished
	Days      []int  `json:"days"`      // 0 = Sunday … 6 = Saturday; empty = every day
	Once      bool   `json:"once"`      // only the next time, then switch off
	ResumeAll bool   `json:"resumeAll"` // also continue paused / failed downloads
	WhenDone  string `json:"whenDone"`  // "", "sleep", "shutdown"
	KeepAwake bool   `json:"keepAwake"` // stop the computer sleeping while there's work to do

	Extra []SchedWindow `json:"extra"` // more windows, e.g. a weekend bundle
}

// SchedWindow is one time window when scheduled downloads may run.
type SchedWindow struct {
	Name  string `json:"name"`
	Start string `json:"start"`
	Stop  string `json:"stop"`
	Days  []int  `json:"days"`
}

func (s Schedule) windows() []SchedWindow {
	out := []SchedWindow{{Name: "main", Start: s.Start, Stop: s.Stop, Days: s.Days}}
	for _, w := range s.Extra {
		if _, ok := parseHM(w.Start); ok {
			out = append(out, w)
		}
	}
	return out
}

// insideAny reports whether now falls in any window, and when that window ends.
func (s Schedule) insideAny(now time.Time) (bool, time.Time) {
	for _, w := range s.windows() {
		for _, off := range []int{0, -1} {
			day := now.AddDate(0, 0, off)
			if !w.dayAllowed(day) {
				continue
			}
			ws, we, hasStop := w.window(day)
			if !ws.IsZero() && hasStop && !now.Before(ws) && now.Before(we) {
				return true, we
			}
		}
	}
	return false, time.Time{}
}

type schedState struct {
	LastStart string    `json:"lastStart"` // key of the last window we started
	Active    bool      `json:"active"`    // inside a window we started
	StopAt    time.Time `json:"stopAt"`
	Started   int       `json:"started"` // downloads this window started
}

type powerAction struct {
	Action string    `json:"action"` // "sleep" or "shutdown"
	At     time.Time `json:"at"`
}

const startGrace = 30 * time.Minute // with no stop time, still start if we're this late

func parseHM(s string) (int, bool) {
	parts := strings.SplitN(strings.TrimSpace(s), ":", 2)
	if len(parts) != 2 {
		return 0, false
	}
	h, e1 := strconv.Atoi(parts[0])
	mi, e2 := strconv.Atoi(parts[1])
	if e1 != nil || e2 != nil || h < 0 || h > 23 || mi < 0 || mi > 59 {
		return 0, false
	}
	return h*60 + mi, true
}

func (s SchedWindow) dayAllowed(t time.Time) bool {
	if len(s.Days) == 0 {
		return true
	}
	for _, d := range s.Days {
		if d == int(t.Weekday()) {
			return true
		}
	}
	return false
}

// window returns the start and end of the schedule window that begins on day.
func (s SchedWindow) window(day time.Time) (time.Time, time.Time, bool) {
	startMin, ok := parseHM(s.Start)
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	y, mo, d := day.Date()
	ws := time.Date(y, mo, d, startMin/60, startMin%60, 0, 0, day.Location())
	if stopMin, ok := parseHM(s.Stop); ok {
		dur := (stopMin - startMin + 1440) % 1440
		if dur == 0 {
			dur = 1440
		}
		return ws, ws.Add(time.Duration(dur) * time.Minute), true
	}
	return ws, ws.Add(startGrace), false
}

// NextRun describes when scheduled downloads will next start, for the UI.
func (s Schedule) NextRun(now time.Time) string {
	if !s.Enabled {
		return ""
	}
	var best time.Time
	bestDay := 0
	for _, w := range s.windows() {
		for i := 0; i < 8; i++ {
			day := now.AddDate(0, 0, i)
			if !w.dayAllowed(day) {
				continue
			}
			ws, _, _ := w.window(day)
			if ws.IsZero() || ws.Before(now) {
				continue
			}
			if best.IsZero() || ws.Before(best) {
				best, bestDay = ws, i
			}
			break
		}
	}
	if best.IsZero() {
		return ""
	}
	switch bestDay {
	case 0:
		return "today at " + best.Format("15:04")
	case 1:
		return "tomorrow at " + best.Format("15:04")
	}
	return best.Format("Mon 15:04")
}

// checkSchedule runs every few seconds from the manager loop.
func (m *Manager) checkSchedule(now time.Time) {
	m.mu.Lock()
	s := m.settings.Schedule
	st := m.sched
	m.mu.Unlock()

	if s.Enabled && !st.Active {
	windows:
		for wi, w := range s.windows() {
			for _, off := range []int{0, -1} { // a window may have started yesterday (e.g. 23:00 to 05:00)
				day := now.AddDate(0, 0, off)
				if !w.dayAllowed(day) {
					continue
				}
				ws, we, hasStop := w.window(day)
				if ws.IsZero() {
					break
				}
				key := fmt.Sprintf("%d %s", wi, ws.Format("2006-01-02 15:04"))
				if !now.Before(ws) && now.Before(we) && st.LastStart != key {
					n := m.startScheduled(s)
					m.mu.Lock()
					m.sched.Started = n
					m.sched.LastStart = key
					m.sched.Active = true
					m.sched.StopAt = time.Time{}
					if hasStop {
						m.sched.StopAt = we
					}
					if s.Once && !hasStop {
						m.settings.Schedule.Enabled = false
					}
					m.dirty = true
					m.mu.Unlock()
					break windows
				}
			}
		}
	}

	m.mu.Lock()
	st = m.sched
	m.mu.Unlock()
	if !st.Active {
		return
	}
	if !st.StopAt.IsZero() && !now.Before(st.StopAt) {
		// Another window (e.g. a weekend bundle) may carry straight on.
		if in, until := s.insideAny(now); in && s.Enabled {
			m.mu.Lock()
			m.sched.StopAt = until
			m.mu.Unlock()
			return
		}
		m.stopScheduled()
		m.endWindow(s)
		return
	}
	if !m.anyBusy() {
		m.endWindow(s)
	}
}

func (m *Manager) endWindow(s Schedule) {
	m.mu.Lock()
	m.sched.Active = false
	if s.Once {
		m.settings.Schedule.Enabled = false
	}
	if (s.WhenDone == "sleep" || s.WhenDone == "shutdown") && m.sched.Started > 0 {
		m.power = &powerAction{Action: s.WhenDone, At: time.Now().Add(90 * time.Second)}
	}
	m.dirty = true
	m.mu.Unlock()
}

func (m *Manager) anyBusy() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.downloads {
		d.mu.Lock()
		busy := d.running || d.Status == StQueued
		d.mu.Unlock()
		if busy {
			return true
		}
	}
	return false
}

func (m *Manager) startScheduled(s Schedule) int {
	n := 0
	m.mu.Lock()
	for _, d := range m.downloads {
		d.mu.Lock()
		switch {
		case d.Status == StScheduled:
			d.Status = StQueued
			d.Scheduled = true
			n++
		case s.ResumeAll && !d.running && (d.Status == StPaused || d.Status == StError):
			d.Status = StQueued
			d.Scheduled = true
			n++
		}
		d.mu.Unlock()
	}
	m.dirty = true
	m.mu.Unlock()
	m.schedule()
	return n
}

// stopScheduled pauses what the schedule started; it continues in the next window.
func (m *Manager) stopScheduled() {
	m.mu.Lock()
	for _, d := range m.downloads {
		d.mu.Lock()
		if d.Scheduled {
			if d.running {
				d.stopReason = "schedule"
				d.cancel()
			} else if d.Status == StQueued {
				d.Status = StScheduled
			}
		}
		d.mu.Unlock()
	}
	m.dirty = true
	m.mu.Unlock()
}

// powerTick carries out a pending sleep / shutdown once its countdown ends.
func (m *Manager) powerTick(now time.Time) {
	m.mu.Lock()
	p := m.power
	if p != nil && !now.Before(p.At) {
		m.power = nil
	}
	m.mu.Unlock()
	if p != nil && !now.Before(p.At) {
		m.save()
		if err := doPower(p.Action); err != nil {
			fmt.Println("power action failed:", err)
		}
	}
}

func (m *Manager) CancelPower() {
	m.mu.Lock()
	m.power = nil
	m.mu.Unlock()
}

// wantAwake: keep the computer from sleeping while downloading, or while
// downloads are waiting for tonight's schedule.
func (m *Manager) wantAwake() bool {
	m.mu.Lock()
	s := m.settings.Schedule
	m.mu.Unlock()
	if !s.KeepAwake {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.heldLocked() && !m.holds["network"] {
		return false // paused (metered network, budget used up, quiet mode): nothing to stay awake for
	}
	soon := time.Now().Add(time.Minute)
	for _, d := range m.downloads {
		d.mu.Lock()
		// a download that tries again by itself in 5 minutes doesn't keep the computer up meanwhile
		w := d.running || d.Status == StQueued && d.retryAt.Before(soon) || (s.Enabled && d.Status == StScheduled)
		d.mu.Unlock()
		if w {
			return true
		}
	}
	return false
}
