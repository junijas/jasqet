package main

// Learning when the internet is fastest. While something downloads, Jasqet
// notes the best speed it reaches in each hour of the day; after a few days it
// can say "your internet is usually 3× faster from 1 to 5 AM" and move the
// scheduler there. Nothing leaves the computer: it's just these 24 numbers.

import (
	"fmt"
	"sort"
	"time"
)

type hourSpeed struct {
	Avg  float64 `json:"avg"`  // the hour's usual best speed (bytes/s)
	Days int     `json:"days"` // how many times it was measured
}

type speedLearn struct {
	Hours     [24]hourSpeed `json:"hours"`
	Dismissed time.Time     `json:"dismissed,omitempty"` // "Not now" on the suggestion

	key  int         // the hour being measured now (yyyymmddhh)
	peak float64     // its best 10-second speed so far
	n    int         // half-second readings in it
	win  [20]float64 // the last 10 seconds
}

const (
	learnWindow  = 4   // hours the suggestion covers
	learnMinRead = 120 // a minute of downloading in an hour before it counts
)

func hourKey(t time.Time) int { return ((t.Year()*100+int(t.Month()))*100+t.Day())*100 + t.Hour() }

// sample takes one half-second reading of the whole download speed. Only
// readings without any speed limit (yours, metered, Smooth mode) say what the
// line can do.
func (l *speedLearn) sample(now time.Time, speed float64, measuring bool) {
	if k := hourKey(now); k != l.key {
		l.fold()
		*l = speedLearn{Hours: l.Hours, Dismissed: l.Dismissed, key: k}
	}
	if !measuring {
		return
	}
	l.win[l.n%len(l.win)] = speed
	l.n++
	if l.n >= len(l.win) {
		var sum float64
		for _, x := range l.win {
			sum += x
		}
		l.peak = max(l.peak, sum/float64(len(l.win)))
	}
}

// fold adds the hour just measured to what that hour is usually like.
func (l *speedLearn) fold() {
	if l.key == 0 || l.n < learnMinRead || l.peak <= 0 {
		return
	}
	h := &l.Hours[l.key%100]
	if h.Days == 0 {
		h.Avg = l.peak
	} else {
		h.Avg = h.Avg*0.7 + l.peak*0.3 // recent days count more
	}
	h.Days = min(h.Days+1, 999)
}

// SpeedInsight: what Jasqet has learned, for the window.
type SpeedInsight struct {
	Hours   [24]int64 `json:"hours"`   // each hour's usual best speed (0 = not known yet)
	Known   int       `json:"known"`   // hours measured on at least two days
	From    int       `json:"from"`    // the fastest four hours: From to To
	To      int       `json:"to"`      //
	Factor  float64   `json:"factor"`  // how much faster than usual
	Suggest bool      `json:"suggest"` // worth offering to move the scheduler there
}

func (l *speedLearn) insight(now time.Time, sc Schedule) *SpeedInsight {
	in := &SpeedInsight{}
	var known []float64
	for h, hs := range l.Hours {
		if hs.Days >= 2 {
			in.Hours[h] = int64(hs.Avg)
			known = append(known, hs.Avg)
		}
	}
	in.Known = len(known)
	if in.Known == 0 {
		return nil
	}
	if in.Known < 8 {
		return in // still learning
	}
	best := 0.0
	for s := 0; s < 24; s++ {
		var sum float64
		n := 0
		for i := 0; i < learnWindow; i++ {
			if v := in.Hours[(s+i)%24]; v > 0 {
				sum += float64(v)
				n++
			}
		}
		if n >= learnWindow-1 && sum/float64(n) > best {
			best, in.From = sum/float64(n), s
		}
	}
	if best == 0 {
		return in
	}
	in.To = (in.From + learnWindow) % 24
	sort.Float64s(known)
	in.Factor = float64(int(best/known[len(known)/2]*10)) / 10 // against the middle hour
	in.Suggest = in.Factor >= 1.5 && now.Sub(l.Dismissed) > 14*24*time.Hour && !sc.startsWithin(in.From, learnWindow)
	return in
}

// startsWithin: the scheduler is on and starts within the hours from…from+n.
func (s Schedule) startsWithin(from, n int) bool {
	m, ok := parseHM(s.Start)
	return s.Enabled && ok && (m/60-from+24)%24 < n
}

// UseBestHours moves the scheduler's start to the fastest hours, or (no)
// puts the suggestion away for two weeks.
func (m *Manager) UseBestHours(yes bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dirty = true
	in := m.learn.insight(time.Now(), m.settings.Schedule)
	if !yes || in == nil || in.Factor == 0 {
		m.learn.Dismissed = time.Now()
		return
	}
	sc := &m.settings.Schedule
	sc.Start, sc.Enabled = fmt.Sprintf("%02d:00", in.From), true
	if sc.Stop != "" { // a stop time stays a stop time, at the end of those hours
		sc.Stop = fmt.Sprintf("%02d:00", in.To)
	}
}
