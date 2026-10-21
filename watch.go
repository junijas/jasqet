package main

// Watch rules: Jasqet keeps an eye on a YouTube channel or playlist, a
// podcast, a GitHub project's releases or any web page, and downloads what's
// new, straight away or at your scheduled (cheap data) time.

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

type WatchRule struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	On      bool   `json:"on"`
	Kind    string `json:"kind"`    // "youtube", "podcast", "github", "page"
	URL     string `json:"url"`     //
	Filter  string `json:"filter"`  // only items whose name contains this (page: extensions like ".pdf .zip")
	Quality string `json:"quality"` // YouTube: "h:720", "h:1080", "audio", "best"
	Every   int    `json:"every"`   // hours between checks
	Later   bool   `json:"later"`   // download at the scheduled time
	Dir     string `json:"dir"`     // folder (empty = normal folder)
	// GetLatest: the first check also downloads the newest item (otherwise only
	// items that appear after the rule was made are downloaded)
	GetLatest bool `json:"getLatest"`

	Seen      []string  `json:"seen"`
	Primed    bool      `json:"primed"` // a check has worked once (what was there then is remembered)
	LastCheck time.Time `json:"lastCheck"`
	LastError string    `json:"lastError"`
	LastNew   int       `json:"lastNew"`
	Total     int       `json:"total"` // downloads started by this rule
}

type watchItem struct {
	ID    string // remembered so it's only downloaded once
	URL   string
	Title string
}

// watchLoop checks rules that are due. Checking uses very little data (a
// web page or a short list), but it still waits while downloads are on hold
// (metered connection, data budget used up, quiet mode).
func (m *Manager) watchLoop() {
	time.Sleep(20 * time.Second)
	for {
		m.mu.Lock()
		held := m.heldLocked()
		var due []string
		for _, r := range m.settings.Watch {
			every := time.Duration(max(r.Every, 1)) * time.Hour
			if r.On && time.Since(r.LastCheck) >= every {
				due = append(due, r.ID)
			}
		}
		m.mu.Unlock()
		if !held {
			for _, id := range due {
				m.CheckWatch(context.Background(), id)
			}
		}
		time.Sleep(5 * time.Minute)
	}
}

func (m *Manager) watchRule(id string) (WatchRule, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.settings.Watch {
		if r.ID == id {
			return r, true
		}
	}
	return WatchRule{}, false
}

func (m *Manager) updateWatch(id string, f func(r *WatchRule)) {
	m.mu.Lock()
	for i := range m.settings.Watch {
		if m.settings.Watch[i].ID == id {
			f(&m.settings.Watch[i])
		}
	}
	m.dirty = true
	m.mu.Unlock()
}

// CheckWatch looks for new items now. The first check only remembers what's
// already there (so you don't get a whole channel at once) unless the rule
// was made with "also get the latest one".
func (m *Manager) CheckWatch(ctx context.Context, id string) (int, error) {
	r, ok := m.watchRule(id)
	if !ok {
		return 0, errors.New("that rule doesn't exist")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	items, err := m.watchItems(ctx, &r)
	if err != nil {
		m.updateWatch(id, func(w *WatchRule) { w.LastCheck, w.LastError = time.Now(), friendlyError(err) })
		return 0, err
	}
	seen := map[string]bool{}
	for _, s := range r.Seen {
		seen[s] = true
	}
	first := !r.Primed
	filter := strings.ToLower(strings.TrimSpace(r.Filter))
	var fresh []watchItem
	for _, it := range items {
		if seen[it.ID] {
			continue
		}
		if filter != "" && r.Kind != "page" && !strings.Contains(strings.ToLower(it.Title+" "+it.URL), filter) {
			continue
		}
		fresh = append(fresh, it)
	}
	started := 0
	if first {
		// remember everything; download only the newest one if asked (lists are newest first)
		if len(fresh) > 0 && r.GetLatest {
			fresh = fresh[:1]
		} else {
			fresh = nil
		}
	}
	var startErr error
	for _, it := range fresh {
		if err := m.startWatchItem(&r, it); err != nil {
			startErr = err
			continue
		}
		started++
	}
	m.updateWatch(id, func(w *WatchRule) {
		// Remember everything listed now (always kept, so nothing on a long page
		// is downloaded twice) plus up to 600 older ones.
		current := map[string]bool{}
		var keep []string
		for _, it := range items {
			if !current[it.ID] {
				current[it.ID] = true
				keep = append(keep, it.ID)
			}
		}
		var older []string
		for _, s := range w.Seen {
			if !current[s] {
				older = append(older, s)
			}
		}
		if len(older) > 600 {
			older = older[len(older)-600:]
		}
		w.Seen = append(older, keep...)
		w.Primed = true
		w.LastCheck = time.Now()
		w.LastError = ""
		if startErr != nil {
			w.LastError = friendlyError(startErr)
		}
		w.LastNew = started
		w.Total += started
	})
	m.saveSoon()
	return started, nil
}

func (m *Manager) startWatchItem(r *WatchRule, it watchItem) error {
	var ids []string
	var err error
	if r.Kind == "youtube" {
		q := r.Quality
		if q == "" {
			q = "h:720"
		}
		ids, err = m.AddYT(AddYTRequest{URL: it.URL, Variant: q, Title: it.Title, Later: r.Later, Dir: r.Dir})
	} else {
		ids, err = m.Add(AddRequest{URL: it.URL, Later: r.Later, Dir: r.Dir, Page: r.URL})
	}
	if err != nil {
		return err
	}
	for _, id := range ids {
		if d := m.get(id); d != nil {
			d.mu.Lock()
			d.WatchID = r.ID
			d.mu.Unlock()
		}
	}
	return nil
}

func (m *Manager) watchItems(ctx context.Context, r *WatchRule) ([]watchItem, error) {
	switch r.Kind {
	case "youtube":
		return m.watchYouTube(ctx, r)
	case "podcast":
		return m.watchPodcast(ctx, r)
	case "github":
		return m.watchGitHub(ctx, r)
	default:
		return m.watchPage(ctx, r)
	}
}

func (m *Manager) watchYouTube(ctx context.Context, r *WatchRule) ([]watchItem, error) {
	if err := m.toolsWait(ctx); err != nil {
		return nil, err
	}
	link := r.URL
	if u, err := url.Parse(link); err == nil && strings.Contains(u.Host, "youtube.com") {
		p := strings.TrimSuffix(u.Path, "/")
		if (strings.HasPrefix(p, "/@") || strings.HasPrefix(p, "/channel/") || strings.HasPrefix(p, "/c/") || strings.HasPrefix(p, "/user/")) && strings.Count(p, "/") <= 2 {
			u.Path = p + "/videos" // a channel: its latest videos
			link = u.String()
		}
	}
	out, err := m.ytRun(ctx, link, nil, pageArgs(link, "-J", "--flat-playlist", "--playlist-end", "20")...)
	if err != nil {
		return nil, err
	}
	var info ytInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return nil, err
	}
	var items []watchItem
	for _, e := range info.Entries {
		if e.ID == "" {
			continue
		}
		u := e.URL
		if u == "" || !strings.HasPrefix(u, "http") {
			u = "https://www.youtube.com/watch?v=" + e.ID
		}
		items = append(items, watchItem{ID: "yt:" + e.ID, URL: u, Title: e.Title})
	}
	if len(items) == 0 && info.ID != "" && info.Type != "playlist" {
		items = append(items, watchItem{ID: "yt:" + info.ID, URL: r.URL, Title: info.Title})
	}
	return items, nil
}

type rssFeed struct {
	Channel struct {
		Items []struct {
			Title     string `xml:"title"`
			GUID      string `xml:"guid"`
			Link      string `xml:"link"`
			Enclosure struct {
				URL string `xml:"url,attr"`
			} `xml:"enclosure"`
		} `xml:"item"`
	} `xml:"channel"`
	// Atom feeds
	Entries []struct {
		Title string `xml:"title"`
		ID    string `xml:"id"`
		Links []struct {
			Href string `xml:"href,attr"`
			Rel  string `xml:"rel,attr"`
		} `xml:"link"`
	} `xml:"entry"`
}

func (m *Manager) watchPodcast(ctx context.Context, r *WatchRule) ([]watchItem, error) {
	body, final, _, err := m.fetchSmall(ctx, r.URL, reqOpts{}, 16<<20)
	if err != nil {
		return nil, err
	}
	m.countBytes(len(body))
	var feed rssFeed
	if err := xml.Unmarshal(body, &feed); err != nil {
		return nil, errors.New("that link isn't a podcast feed (RSS). Look for an \"RSS\" link on the podcast's page")
	}
	var items []watchItem
	for _, it := range feed.Channel.Items {
		link := it.Enclosure.URL
		if link == "" {
			continue
		}
		id := it.GUID
		if id == "" {
			id = link
		}
		items = append(items, watchItem{ID: "rss:" + id, URL: resolveRef(final, link), Title: it.Title})
	}
	for _, e := range feed.Entries {
		for _, l := range e.Links {
			if l.Rel == "enclosure" {
				items = append(items, watchItem{ID: "rss:" + firstNonEmpty(e.ID, l.Href), URL: resolveRef(final, l.Href), Title: e.Title})
			}
		}
	}
	if len(items) > 50 {
		items = items[:50]
	}
	if len(items) == 0 {
		return nil, errors.New("the feed has no episodes with files")
	}
	return items, nil
}

var ghRepoRe = regexp.MustCompile(`github\.com/([\w.-]+)/([\w.-]+)`)

func (m *Manager) watchGitHub(ctx context.Context, r *WatchRule) ([]watchItem, error) {
	mm := ghRepoRe.FindStringSubmatch(r.URL)
	if mm == nil {
		return nil, errors.New("paste a GitHub project link like https://github.com/owner/project")
	}
	repo := strings.TrimSuffix(mm[2], ".git")
	api := fmt.Sprintf("https://api.github.com/repos/%s/%s/releases?per_page=5", mm[1], repo)
	body, _, _, err := m.fetchSmall(ctx, api, reqOpts{headers: map[string]string{"Accept": "application/vnd.github+json"}}, 8<<20)
	if err != nil {
		return nil, err
	}
	m.countBytes(len(body))
	var rels []struct {
		Tag        string `json:"tag_name"`
		Name       string `json:"name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
		Assets     []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(body, &rels); err != nil {
		return nil, errors.New("GitHub didn't send the list of releases (the project may not publish any)")
	}
	var items []watchItem
	for _, rel := range rels {
		if rel.Draft || rel.Prerelease {
			continue
		}
		for _, a := range rel.Assets {
			items = append(items, watchItem{ID: "gh:" + a.URL, URL: a.URL, Title: rel.Tag + " " + a.Name})
		}
		if len(items) > 0 {
			break // only the newest release
		}
	}
	return items, nil
}

func (m *Manager) watchPage(ctx context.Context, r *WatchRule) ([]watchItem, error) {
	body, final, _, err := m.fetchSmall(ctx, r.URL, reqOpts{}, 8<<20)
	if err != nil {
		return nil, err
	}
	m.countBytes(len(body))
	exts := map[string]bool{}
	var words []string
	for _, f := range strings.Fields(strings.ToLower(r.Filter)) {
		if strings.HasPrefix(f, ".") {
			exts[f] = true
		} else {
			words = append(words, f)
		}
	}
	seen := map[string]bool{}
	var items []watchItem
	for _, mm := range hrefRe.FindAllSubmatch(body, -1) {
		ref := string(mm[1])
		if ref == "" {
			continue
		}
		abs := resolveRef(final, strings.ReplaceAll(ref, "&amp;", "&"))
		u, err := url.Parse(abs)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || seen[abs] {
			continue
		}
		seen[abs] = true
		ext := strings.ToLower(path.Ext(u.Path))
		if len(exts) > 0 && !exts[ext] {
			continue
		}
		if len(exts) == 0 && (ext == "" || ext == ".html" || ext == ".htm" || ext == ".php" || ext == ".aspx" || ext == ".css" || ext == ".js" || ext == ".png" || ext == ".svg" || ext == ".ico" || ext == ".jpg" || ext == ".gif" || ext == ".webp") {
			continue // only links to files
		}
		low := strings.ToLower(abs)
		ok := true
		for _, w := range words {
			if !strings.Contains(low, w) {
				ok = false
			}
		}
		if ok {
			items = append(items, watchItem{ID: "url:" + abs, URL: abs, Title: path.Base(u.Path)})
		}
	}
	return items, nil
}

// SaveWatch replaces the list of rules (keeping what each rule has seen).
func (m *Manager) SaveWatch(rules []WatchRule) {
	m.mu.Lock()
	old := map[string]WatchRule{}
	for _, r := range m.settings.Watch {
		old[r.ID] = r
	}
	for i := range rules {
		r := &rules[i]
		if r.ID == "" {
			r.ID = newID()
		}
		if o, ok := old[r.ID]; ok {
			if o.URL == r.URL && o.Kind == r.Kind {
				r.Seen, r.LastCheck, r.Total, r.LastNew, r.LastError, r.Primed = o.Seen, o.LastCheck, o.Total, o.LastNew, o.LastError, o.Primed
			} else {
				r.Seen, r.LastCheck, r.Primed = nil, time.Time{}, false
			}
		}
		if r.Every <= 0 {
			r.Every = 6
		}
		if r.Kind == "" {
			r.Kind = guessWatchKind(r.URL)
		}
		if r.Name == "" {
			r.Name = hostOf(r.URL)
		}
	}
	m.settings.Watch = rules
	m.dirty = true
	m.mu.Unlock()
}

func guessWatchKind(u string) string {
	low := strings.ToLower(u)
	switch {
	case isYouTube(u):
		return "youtube"
	case strings.Contains(low, "github.com/"):
		return "github"
	case strings.Contains(low, "rss") || strings.Contains(low, "feed") || strings.HasSuffix(low, ".xml") || strings.Contains(low, "podcast"):
		return "podcast"
	}
	return "page"
}
