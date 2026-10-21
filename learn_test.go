package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// a day of downloading: from each hour of the day, two minutes at its speed
func learnDay(l *speedLearn, day time.Time, speed func(h int) float64) {
	for h := 0; h < 24; h++ {
		t := time.Date(day.Year(), day.Month(), day.Day(), h, 10, 0, 0, time.Local)
		for i := 0; i < 240; i++ {
			l.sample(t.Add(time.Duration(i)*500*time.Millisecond), speed(h), true)
		}
	}
}

func TestLearnsTheFastestHours(t *testing.T) {
	var l speedLearn
	night := func(h int) float64 { // fast from 1 to 5 AM, slow by day
		if h >= 1 && h < 5 {
			return 3e6
		}
		return 1e6
	}
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.Local)
	learnDay(&l, day, night)
	if in := l.insight(day, Schedule{}); in != nil {
		t.Fatalf("after one day it already claims to know: %+v", in)
	}
	learnDay(&l, day.AddDate(0, 0, 1), night)
	learnDay(&l, day.AddDate(0, 0, 2), night)
	now := day.AddDate(0, 0, 3)
	in := l.insight(now, Schedule{Start: "04:00"}) // the scheduler as it comes: off
	if in == nil || in.From != 1 || in.To != 5 || in.Factor < 2.9 || in.Factor > 3.1 || !in.Suggest {
		t.Fatalf("insight: %+v", in)
	}
	// readings while a speed limit held things back, or under a minute in an hour, teach nothing
	var lim speedLearn
	for i := 0; i < 2000; i++ {
		lim.sample(day.Add(time.Duration(i)*time.Second), 9e6, false)
	}
	for i := 0; i < 100; i++ {
		lim.sample(day.Add(2*time.Hour+time.Duration(i)*500*time.Millisecond), 9e6, true)
	}
	lim.sample(day.Add(5*time.Hour), 0, false)
	if lim.Hours[0].Days != 0 || lim.Hours[2].Days != 0 {
		t.Fatalf("learned from limited or short readings: %+v", lim.Hours)
	}
	// already scheduled in those hours, or "Not now": no suggestion
	if l.insight(now, Schedule{Start: "02:30", Enabled: true}).Suggest {
		t.Fatal("suggested hours the scheduler already uses")
	}
	l.Dismissed = now
	if l.insight(now.Add(24*time.Hour), Schedule{}).Suggest || !l.insight(now.Add(15*24*time.Hour), Schedule{}).Suggest {
		t.Fatal("Not now should last two weeks")
	}
	// even speeds all day: nothing worth suggesting
	var flat speedLearn
	for i := 0; i < 3; i++ {
		learnDay(&flat, day.AddDate(0, 0, i), func(int) float64 { return 2e6 })
	}
	if in := flat.insight(now, Schedule{}); in == nil || in.Suggest {
		t.Fatalf("flat speeds: %+v", in)
	}
}

func TestQualitiesSayHowMuchDataIsLeft(t *testing.T) {
	// a video playlist with one quality: 800 kbit/s for 100 s, about 10 MB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/master.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=800000,RESOLUTION=640x360\nlow.m3u8\n")
		case "/low.m3u8":
			io.WriteString(w, "#EXTM3U\n#EXT-X-TARGETDURATION:10\n"+strings.Repeat("#EXTINF:10,\nseg.ts\n", 10)+"#EXT-X-ENDLIST\n")
		}
	}))
	defer srv.Close()
	s, h := newTestServer(t)
	probe := func() map[string]any {
		w := call(h, "POST", "/api/media/probe", "http://127.0.0.1:9614", `{"url":"`+srv.URL+`/master.m3u8"}`, keyHeader, s.key.window)
		var out map[string]any
		if json.Unmarshal(w.Body.Bytes(), &out) != nil || out["variants"] == nil {
			t.Fatalf("probe: %d %s", w.Code, w.Body.String())
		}
		return out
	}
	if b := probe()["budget"]; b != float64(-1) {
		t.Fatalf("no budget set, but the answer says %v", b)
	}
	s.m.SetBudget(5<<20, 0)
	out := probe()
	est := out["variants"].([]any)[0].(map[string]any)["estSize"].(float64)
	if out["budget"] != float64(5<<20) || est <= 5<<20 {
		t.Fatalf("budget %v, quality ~%v bytes", out["budget"], est)
	}
}

func TestUseBestHoursMovesTheScheduler(t *testing.T) {
	m := newTestManager(t)
	day := time.Now().AddDate(0, 0, -3)
	for i := 0; i < 3; i++ {
		learnDay(&m.learn, day.AddDate(0, 0, i), func(h int) float64 {
			if h >= 13 && h < 17 {
				return 4e6
			}
			return 1e6
		})
	}
	m.mu.Lock()
	m.settings.Schedule = Schedule{Start: "04:00", Stop: "06:00"}
	m.mu.Unlock()
	m.UseBestHours(true)
	_, s := m.Snapshot()
	if sc := s.Schedule; !sc.Enabled || sc.Start != "13:00" || sc.Stop != "17:00" {
		t.Fatalf("schedule: %+v", sc)
	}
	if e := m.Extras(); e.Speeds == nil || e.Speeds.Suggest {
		t.Fatalf("after using them, still suggested: %+v", e.Speeds)
	}
}
