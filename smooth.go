package main

// Smooth mode (latency-aware throttling): downloads use the connection fully,
// but as soon as browsing or a call would start to lag (the "ping" rises),
// Jasqet eases off a little, then speeds up again when it's quiet.

import (
	"context"
	"net"
	"sort"
	"sync"
	"time"
)

type smoothState struct {
	mu       sync.Mutex
	samples  []rttSample // recent pings
	cap      int64       // current speed cap (bytes/s), 0 = none
	rtt      time.Duration
	base     time.Duration
	failures int
	note     string
}

type rttSample struct {
	at  time.Time
	rtt time.Duration
}

// setRate records a speed limit from one place ("user", "metered", "smooth")
// and applies the lowest one.
func (m *Manager) setRate(reason string, bps int64) {
	m.mu.Lock()
	if bps <= 0 {
		delete(m.rates, reason)
	} else {
		m.rates[reason] = bps
	}
	var low int64
	for _, r := range m.rates {
		if low == 0 || r < low {
			low = r
		}
	}
	m.mu.Unlock()
	if m.limiter.Rate() != low {
		m.limiter.SetRate(low)
		go m.torrentLimits()
	}
}

var pingTargets = []string{"1.1.1.1:443", "8.8.8.8:443", "9.9.9.9:443"}

// ping measures one round trip (opening a connection takes exactly one).
func ping(ctx context.Context, i int) (time.Duration, bool) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	start := time.Now()
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", pingTargets[i%len(pingTargets)])
	if err != nil {
		return 0, false
	}
	rtt := time.Since(start)
	c.Close()
	return rtt, true
}

func (m *Manager) smoothLoop() {
	ss := m.smooth
	i := 0
	for {
		m.mu.Lock()
		on := m.settings.Smooth
		m.mu.Unlock()
		busy := m.anyRunning()
		if !on {
			ss.mu.Lock()
			ss.cap, ss.note = 0, ""
			ss.mu.Unlock()
			m.setRate("smooth", 0)
			time.Sleep(3 * time.Second)
			continue
		}
		rtt, ok := ping(context.Background(), i)
		i++
		now := time.Now()
		ss.mu.Lock()
		if !ok {
			ss.failures++
			if ss.failures > 5 {
				ss.note = "Smooth mode can't measure your connection here (a proxy or firewall blocks it)"
				ss.cap = 0
			}
			ss.mu.Unlock()
			m.setRate("smooth", 0)
			time.Sleep(5 * time.Second)
			continue
		}
		ss.failures = 0
		ss.rtt = rtt
		ss.samples = append(ss.samples, rttSample{now, rtt})
		for len(ss.samples) > 0 && now.Sub(ss.samples[0].at) > 3*time.Minute {
			ss.samples = ss.samples[1:]
		}
		// baseline: the quietest pings recently (10th percentile)
		vals := make([]time.Duration, len(ss.samples))
		for k, s := range ss.samples {
			vals[k] = s.rtt
		}
		sort.Slice(vals, func(a, b int) bool { return vals[a] < vals[b] })
		ss.base = vals[len(vals)/10]
		base := ss.base
		capNow := ss.cap
		ss.mu.Unlock()

		if !busy {
			m.setRate("smooth", 0)
			ss.mu.Lock()
			ss.cap, ss.note = 0, ""
			ss.mu.Unlock()
			// keep learning the quiet ping, rarely (one tiny connection a minute), but
			// start watching at once when a download begins
			for k := 0; k < 60 && !m.anyRunning(); k++ {
				time.Sleep(time.Second)
			}
			continue
		}
		speed := m.totalSpeed()
		laggy := rtt > base+120*time.Millisecond && rtt > base*2
		fine := rtt < base+50*time.Millisecond
		switch {
		case laggy && len(vals) >= 5:
			target := int64(float64(speed) * 0.8)
			if capNow > 0 && capNow < speed {
				target = int64(float64(capNow) * 0.85)
			}
			capNow = max(target, 24<<10)
		case fine && capNow > 0:
			capNow = capNow + max(capNow/10, 16<<10)
			if speed > 0 && capNow > speed*3 {
				capNow = 0 // far above what's used: no cap needed
			}
		}
		ss.mu.Lock()
		ss.cap = capNow
		if capNow > 0 {
			ss.note = "Smooth mode is easing off so browsing and calls stay quick"
		} else {
			ss.note = ""
		}
		ss.mu.Unlock()
		m.setRate("smooth", capNow)
		time.Sleep(time.Second)
	}
}

func (m *Manager) anyRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, d := range m.downloads {
		d.mu.Lock()
		r := d.running && d.lanSrc == ""
		d.mu.Unlock()
		if r {
			return true
		}
	}
	return false
}

func (m *Manager) totalSpeed() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	var s float64
	for _, d := range m.downloads {
		d.mu.Lock()
		if d.running {
			s += d.speed
		}
		d.mu.Unlock()
	}
	return int64(s)
}

type SmoothInfo struct {
	On   bool   `json:"on"`
	Cap  int64  `json:"cap"`
	RTT  int64  `json:"rtt"`  // ms
	Base int64  `json:"base"` // ms
	Note string `json:"note,omitempty"`
}

func (m *Manager) SmoothInfo() SmoothInfo {
	m.mu.Lock()
	on := m.settings.Smooth
	m.mu.Unlock()
	ss := m.smooth
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return SmoothInfo{On: on, Cap: ss.cap, RTT: ss.rtt.Milliseconds(), Base: ss.base.Milliseconds(), Note: ss.note}
}
