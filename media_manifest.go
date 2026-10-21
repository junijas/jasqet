package main

// Reading video playlists: HLS (.m3u8) and MPEG-DASH (.mpd).
// A playlist lists the small chunks a video player fetches one by one; we turn it
// into a plan (which chunks, in what order, which key) that the downloader follows.

import (
	"context"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	errDRM          = errors.New("this video is protected by DRM (encrypted for the site's own player only), so it can't be saved")
	errLive         = errors.New("this is a live stream. Jasqet can only save videos that have finished")
	errNotAPlaylist = errors.New("this link isn't a video playlist")
)

type MediaVariant struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	Width     int    `json:"width,omitempty"`
	Height    int    `json:"height,omitempty"`
	Bandwidth int64  `json:"bandwidth,omitempty"`
	EstSize   int64  `json:"estSize,omitempty"`
	Codecs    string `json:"codecs,omitempty"`
	AudioOnly bool   `json:"audioOnly,omitempty"`
}

type MediaInfo struct {
	Kind           string         `json:"kind"` // "hls" or "dash"
	URL            string         `json:"url"`
	Duration       float64        `json:"duration"`
	Live           bool           `json:"live"`
	DRM            bool           `json:"drm"`
	Variants       []MediaVariant `json:"variants"`
	Hosts          []string       `json:"hosts"`
	MediaPlaylists []string       `json:"mediaPlaylists,omitempty"`
}

// SegRef is one chunk to fetch. Len == 0 means the whole resource.
type SegRef struct {
	URL string  `json:"u"`
	Off int64   `json:"o,omitempty"`
	Len int64   `json:"l,omitempty"`
	Key *KeyRef `json:"k,omitempty"`
	D   float64 `json:"d,omitempty"` // seconds it plays (when known)
}

// KeyRef is an HLS AES-128 key (standard HLS encryption that every player handles).
type KeyRef struct {
	URL string `json:"u"`
	IV  string `json:"iv"`
}

type TrackPlan struct {
	FormatID string   `json:"formatId,omitempty"` // yt-dlp format (YouTube downloads)
	Role     string   `json:"role"`               // "main" or "audio"
	Init     *SegRef  `json:"init,omitempty"`
	Segs     []SegRef `json:"segs"`
	Ext      string   `json:"ext,omitempty"`      // file extension hint from the URLs
	SegStart float64  `json:"segStart,omitempty"` // when the first chunk starts (clips)
}

type MediaPlan struct {
	Fallback  bool        `json:"fallback,omitempty"` // yt-dlp downloads the parts
	Kind      string      `json:"kind"`
	Variant   string      `json:"variant"`
	AudioOnly bool        `json:"audioOnly"`
	Duration  float64     `json:"duration"`
	Tracks    []TrackPlan `json:"tracks"`
	Clip      *ClipSpec   `json:"clip,omitempty"`
	Subs      *SubRef     `json:"subs,omitempty"`
}

type reqOpts struct {
	headers    map[string]string
	cookies    map[string]string
	user, pass string // login, if the site asks for one
	authHost   string // the login (and an Authorization header) only go to this site; "" = the link asked for
}

func (m *Manager) newReq(ctx context.Context, rawURL string, o reqOpts) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", defaultUA)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Encoding", "identity")
	// a login only goes to the site it was given for, not to mirrors, pages or checksum lists
	loginHere := o.authHost == "" || strings.EqualFold(req.URL.Hostname(), o.authHost)
	for k, v := range o.headers {
		if v != "" && (loginHere || !strings.EqualFold(k, "Authorization")) {
			req.Header.Set(k, v)
		}
	}
	if o.user != "" && loginHere {
		req.SetBasicAuth(o.user, o.pass) // answered only when the server asks (see auth.go)
	}
	if len(o.cookies) > 0 {
		// Each site only gets its own cookies.
		req.Header.Del("Cookie")
		if c := o.cookies[strings.ToLower(req.URL.Hostname())]; c != "" {
			req.Header.Set("Cookie", c)
		}
	}
	return req, nil
}

// fetchSmall downloads a playlist or key (anything small) into memory.
func (m *Manager) fetchSmall(ctx context.Context, rawURL string, o reqOpts, limit int64) ([]byte, *url.URL, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(time.Duration(attempt) * time.Second):
			case <-ctx.Done():
				return nil, nil, "", ctx.Err()
			}
		}
		req, err := m.newReq(ctx, rawURL, o)
		if err != nil {
			return nil, nil, "", err
		}
		resp, err := m.client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			lastErr = fmt.Errorf("server replied %s", resp.Status)
			if resp.StatusCode < 500 && resp.StatusCode != 429 {
				return nil, nil, "", lastErr
			}
			continue
		}
		if err != nil {
			lastErr = err
			continue
		}
		return body, resp.Request.URL, resp.Header.Get("Content-Type"), nil
	}
	return nil, nil, "", lastErr
}

// ---------- probing (what qualities exist) ----------

type probeCacheEntry struct {
	info *MediaInfo
	at   time.Time
}

var (
	probeCacheMu sync.Mutex
	probeCache   = map[string]probeCacheEntry{}
)

func (m *Manager) ProbeMedia(ctx context.Context, rawURL string, o reqOpts) (*MediaInfo, error) {
	probeCacheMu.Lock()
	if e, ok := probeCache[rawURL]; ok && time.Since(e.at) < 3*time.Minute {
		probeCacheMu.Unlock()
		return e.info, nil
	}
	probeCacheMu.Unlock()

	body, final, ctype, err := m.fetchSmall(ctx, rawURL, o, 32<<20)
	if err != nil {
		return nil, err
	}
	var info *MediaInfo
	switch detectPlaylist(body, ctype) {
	case "hls":
		info, err = m.probeHLS(ctx, rawURL, final, string(body), o)
	case "dash":
		info, err = probeDASH(rawURL, final, body)
	default:
		return nil, errNotAPlaylist
	}
	if err != nil {
		return nil, err
	}
	probeCacheMu.Lock()
	probeCache[rawURL] = probeCacheEntry{info, time.Now()}
	probeCacheMu.Unlock()
	return info, nil
}

func detectPlaylist(body []byte, ctype string) string {
	head := strings.TrimLeft(string(body[:min(len(body), 2048)]), "\uFEFF \r\n\t")
	switch {
	case strings.HasPrefix(head, "#EXTM3U"):
		return "hls"
	case strings.Contains(head, "<MPD") || strings.Contains(ctype, "dash+xml"):
		return "dash"
	}
	return ""
}

func addHost(set map[string]bool, raw string) {
	if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
		set[strings.ToLower(u.Hostname())] = true
	}
}

func hostList(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for h := range set {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// ---------- HLS ----------

var attrRe = regexp.MustCompile(`([A-Z0-9-]+)=("[^"]*"|[^,]*)`)

func parseAttrs(s string) map[string]string {
	out := map[string]string{}
	for _, m := range attrRe.FindAllStringSubmatch(s, -1) {
		out[m[1]] = strings.Trim(m[2], `"`)
	}
	return out
}

func resolveRef(base *url.URL, ref string) string {
	u, err := base.Parse(strings.TrimSpace(ref))
	if err != nil {
		return ref
	}
	return u.String()
}

type hlsVariant struct {
	URI       string
	Bandwidth int64
	Width     int
	Height    int
	Codecs    string
	Audio     string
	Index     int
}

type hlsRendition struct {
	Type, Group, Name, Lang, URI string
	Default, AutoSelect          bool
}

type hlsMaster struct {
	Variants   []hlsVariant
	Renditions []hlsRendition
	DRM        bool
}

func parseHLSMaster(base *url.URL, text string) *hlsMaster {
	m := &hlsMaster{}
	lines := splitLines(text)
	for i := 0; i < len(lines); i++ {
		l := lines[i]
		switch {
		case strings.HasPrefix(l, "#EXT-X-STREAM-INF:"):
			a := parseAttrs(l[len("#EXT-X-STREAM-INF:"):])
			v := hlsVariant{Codecs: a["CODECS"], Audio: a["AUDIO"], Index: len(m.Variants)}
			v.Bandwidth, _ = strconv.ParseInt(a["BANDWIDTH"], 10, 64)
			if ab, _ := strconv.ParseInt(a["AVERAGE-BANDWIDTH"], 10, 64); ab > 0 {
				v.Bandwidth = ab
			}
			if r := strings.SplitN(a["RESOLUTION"], "x", 2); len(r) == 2 {
				v.Width, _ = strconv.Atoi(r[0])
				v.Height, _ = strconv.Atoi(r[1])
			}
			for j := i + 1; j < len(lines); j++ {
				if lines[j] != "" && !strings.HasPrefix(lines[j], "#") {
					v.URI = resolveRef(base, lines[j])
					i = j
					break
				}
			}
			if v.URI != "" {
				m.Variants = append(m.Variants, v)
			}
		case strings.HasPrefix(l, "#EXT-X-MEDIA:"):
			a := parseAttrs(l[len("#EXT-X-MEDIA:"):])
			r := hlsRendition{Type: a["TYPE"], Group: a["GROUP-ID"], Name: a["NAME"], Lang: a["LANGUAGE"],
				Default: a["DEFAULT"] == "YES", AutoSelect: a["AUTOSELECT"] == "YES"}
			if a["URI"] != "" {
				r.URI = resolveRef(base, a["URI"])
			}
			m.Renditions = append(m.Renditions, r)
		case strings.HasPrefix(l, "#EXT-X-SESSION-KEY:"):
			if isDRMKey(parseAttrs(l[len("#EXT-X-SESSION-KEY:"):])) {
				m.DRM = true
			}
		}
	}
	return m
}

func isDRMKey(a map[string]string) bool {
	method := a["METHOD"]
	if method == "" || method == "NONE" {
		return false
	}
	if method != "AES-128" {
		return true // SAMPLE-AES / SAMPLE-AES-CTR = FairPlay, Widevine, PlayReady
	}
	kf := a["KEYFORMAT"]
	return kf != "" && kf != "identity"
}

func splitLines(s string) []string {
	raw := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(raw))
	for _, l := range raw {
		out = append(out, strings.TrimSpace(l))
	}
	return out
}

type hlsMediaPlaylist struct {
	Track    TrackPlan
	Duration float64
	Live     bool
	DRM      bool
}

func parseHLSMediaPlaylist(base *url.URL, text string) (*hlsMediaPlaylist, error) {
	p := &hlsMediaPlaylist{}
	lines := splitLines(text)
	var seq int64
	var segDur float64
	var key *KeyRef
	var keyExplicitIV bool
	var pendingRange *[2]int64
	lastEnd := map[string]int64{}
	ended := false
	playlistType := ""
	idx := int64(0)
	for _, l := range lines {
		switch {
		case l == "":
		case strings.HasPrefix(l, "#EXT-X-MEDIA-SEQUENCE:"):
			seq, _ = strconv.ParseInt(strings.TrimPrefix(l, "#EXT-X-MEDIA-SEQUENCE:"), 10, 64)
		case strings.HasPrefix(l, "#EXT-X-PLAYLIST-TYPE:"):
			playlistType = strings.TrimPrefix(l, "#EXT-X-PLAYLIST-TYPE:")
		case l == "#EXT-X-ENDLIST":
			ended = true
		case strings.HasPrefix(l, "#EXTINF:"):
			v := strings.TrimPrefix(l, "#EXTINF:")
			if i := strings.IndexByte(v, ','); i >= 0 {
				v = v[:i]
			}
			segDur, _ = strconv.ParseFloat(strings.TrimSpace(v), 64)
		case strings.HasPrefix(l, "#EXT-X-BYTERANGE:"):
			r := parseByteRange(strings.TrimPrefix(l, "#EXT-X-BYTERANGE:"))
			pendingRange = &r
		case strings.HasPrefix(l, "#EXT-X-KEY:"):
			a := parseAttrs(l[len("#EXT-X-KEY:"):])
			if isDRMKey(a) {
				p.DRM = true
			}
			if a["METHOD"] == "AES-128" && !p.DRM {
				key = &KeyRef{URL: resolveRef(base, a["URI"])}
				keyExplicitIV = false
				if iv := strings.TrimPrefix(strings.TrimPrefix(a["IV"], "0x"), "0X"); iv != "" {
					key.IV = strings.ToLower(fmt.Sprintf("%032s", iv))
					keyExplicitIV = true
				}
			} else if a["METHOD"] == "NONE" {
				key = nil
			}
		case strings.HasPrefix(l, "#EXT-X-MAP:"):
			a := parseAttrs(l[len("#EXT-X-MAP:"):])
			if p.Track.Init == nil {
				ref := &SegRef{URL: resolveRef(base, a["URI"])}
				if br := a["BYTERANGE"]; br != "" {
					r := parseByteRange(br)
					ref.Len = r[0]
					if r[1] >= 0 {
						ref.Off = r[1]
					}
				}
				p.Track.Init = ref
			}
		case strings.HasPrefix(l, "#"):
		default:
			u := resolveRef(base, l)
			s := SegRef{URL: u}
			if pendingRange != nil {
				s.Len = pendingRange[0]
				if pendingRange[1] >= 0 {
					s.Off = pendingRange[1]
				} else {
					s.Off = lastEnd[u]
				}
				lastEnd[u] = s.Off + s.Len
				pendingRange = nil
			}
			if key != nil {
				k := *key
				if !keyExplicitIV {
					k.IV = fmt.Sprintf("%032x", seq+idx)
				}
				s.Key = &k
			}
			s.D = segDur
			p.Track.Segs = append(p.Track.Segs, s)
			p.Duration += segDur
			segDur = 0
			idx++
		}
	}
	if len(p.Track.Segs) == 0 {
		return nil, errors.New("the playlist has no video chunks")
	}
	p.Live = !ended && playlistType != "VOD"
	p.Track.Ext = extOfURL(p.Track.Segs[0].URL)
	return p, nil
}

func parseByteRange(s string) [2]int64 {
	parts := strings.SplitN(strings.TrimSpace(s), "@", 2)
	n, _ := strconv.ParseInt(parts[0], 10, 64)
	off := int64(-1)
	if len(parts) == 2 {
		off, _ = strconv.ParseInt(parts[1], 10, 64)
	}
	return [2]int64{n, off}
}

func extOfURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	p := strings.ToLower(u.Path)
	if i := strings.LastIndexByte(p, '.'); i >= 0 && len(p)-i <= 6 {
		return p[i+1:]
	}
	return ""
}

func codecIsVideo(c string) bool {
	c = strings.ToLower(c)
	for _, p := range []string{"avc", "hvc", "hev", "vp0", "vp8", "vp9", "av01", "dvh", "dva", "mp4v"} {
		if strings.Contains(c, p) {
			return true
		}
	}
	return false
}

func codecFamily(c string) string {
	c = strings.ToLower(c)
	switch {
	case strings.Contains(c, "hvc") || strings.Contains(c, "hev") || strings.Contains(c, "dvh"):
		return "HEVC"
	case strings.Contains(c, "av01"):
		return "AV1"
	case strings.Contains(c, "vp9") || strings.Contains(c, "vp09"):
		return "VP9"
	}
	return ""
}

func qualityLabel(height int, bw int64, codecs string) string {
	l := ""
	if height > 0 {
		l = fmt.Sprintf("%dp", height)
	} else if bw > 0 {
		l = fmt.Sprintf("%.1f Mbps", float64(bw)/1e6)
	} else {
		l = "Original quality"
	}
	if f := codecFamily(codecs); f != "" {
		l += " " + f
	}
	return l
}

func (m *Manager) probeHLS(ctx context.Context, rawURL string, base *url.URL, text string, o reqOpts) (*MediaInfo, error) {
	info := &MediaInfo{Kind: "hls", URL: rawURL}
	hosts := map[string]bool{}
	addHost(hosts, rawURL)
	addHost(hosts, base.String())

	if !strings.Contains(text, "#EXT-X-STREAM-INF") {
		pl, err := parseHLSMediaPlaylist(base, text)
		if err != nil {
			return nil, err
		}
		info.Duration, info.Live, info.DRM = pl.Duration, pl.Live, pl.DRM
		collectHosts(hosts, &pl.Track)
		info.Variants = []MediaVariant{{ID: "orig", Label: "Original quality"}}
		info.Variants = append(info.Variants, MediaVariant{ID: "audio", Label: "Audio only", AudioOnly: true})
		info.Hosts = hostList(hosts)
		return info, nil
	}

	master := parseHLSMaster(base, text)
	info.DRM = master.DRM
	if len(master.Variants) == 0 {
		return nil, errors.New("the playlist lists no video qualities")
	}
	// Look inside the best quality to learn the length and spot DRM / live streams.
	best := master.Variants[0]
	for _, v := range master.Variants {
		info.MediaPlaylists = append(info.MediaPlaylists, v.URI)
		if v.Bandwidth > best.Bandwidth {
			best = v
		}
	}
	for _, r := range master.Renditions {
		if r.URI != "" {
			info.MediaPlaylists = append(info.MediaPlaylists, r.URI)
		}
	}
	if body, final, _, err := m.fetchSmall(ctx, best.URI, o, 32<<20); err == nil {
		if pl, err := parseHLSMediaPlaylist(final, string(body)); err == nil {
			info.Duration, info.Live = pl.Duration, pl.Live
			info.DRM = info.DRM || pl.DRM
			collectHosts(hosts, &pl.Track)
		}
	} else {
		return nil, err
	}
	addHost(hosts, best.URI)
	for _, r := range master.Renditions {
		if r.URI != "" {
			addHost(hosts, r.URI)
		}
	}

	audioBW := int64(0)
	if best.Audio != "" {
		audioBW = 128000
	}
	seen := map[string]int{}
	hasAudioOnly := false
	for _, v := range master.Variants {
		isVideo := v.Height > 0 || codecIsVideo(v.Codecs) || (v.Codecs == "" && v.Bandwidth > 200000)
		if !isVideo {
			hasAudioOnly = true
			continue
		}
		label := qualityLabel(v.Height, v.Bandwidth, v.Codecs)
		mv := MediaVariant{ID: fmt.Sprintf("v%d", v.Index), Label: label, Width: v.Width, Height: v.Height,
			Bandwidth: v.Bandwidth, Codecs: v.Codecs}
		if info.Duration > 0 {
			bw := v.Bandwidth
			if v.Audio != "" {
				bw += audioBW
			}
			mv.EstSize = int64(float64(bw) * info.Duration / 8)
		}
		if i, ok := seen[label]; ok {
			if mv.Bandwidth > info.Variants[i].Bandwidth {
				info.Variants[i] = mv
			}
			continue
		}
		seen[label] = len(info.Variants)
		info.Variants = append(info.Variants, mv)
	}
	sort.SliceStable(info.Variants, func(i, j int) bool {
		a, b := info.Variants[i], info.Variants[j]
		if a.Height != b.Height {
			return a.Height > b.Height
		}
		return a.Bandwidth > b.Bandwidth
	})
	av := MediaVariant{ID: "audio", Label: "Audio only", AudioOnly: true}
	if info.Duration > 0 {
		av.EstSize = int64(128000 * info.Duration / 8)
	}
	_ = hasAudioOnly
	info.Variants = append(info.Variants, av)
	info.Hosts = hostList(hosts)
	return info, nil
}

func collectHosts(hosts map[string]bool, t *TrackPlan) {
	if t.Init != nil {
		addHost(hosts, t.Init.URL)
	}
	for i, s := range t.Segs {
		if i < 3 || i == len(t.Segs)-1 {
			addHost(hosts, s.URL)
			if s.Key != nil {
				addHost(hosts, s.Key.URL)
			}
		}
	}
}

// planHLS picks the chunks for the chosen quality.
func (m *Manager) planHLS(ctx context.Context, rawURL string, base *url.URL, text, variant string, o reqOpts) (*MediaPlan, error) {
	plan := &MediaPlan{Kind: "hls", Variant: variant, AudioOnly: variant == "audio"}
	if !strings.Contains(text, "#EXT-X-STREAM-INF") {
		pl, err := parseHLSMediaPlaylist(base, text)
		if err != nil {
			return nil, err
		}
		if err := checkPlayable(pl); err != nil {
			return nil, err
		}
		pl.Track.Role = "main"
		plan.Duration = pl.Duration
		plan.Tracks = []TrackPlan{pl.Track}
		return plan, nil
	}

	master := parseHLSMaster(base, text)
	if master.DRM {
		return nil, errDRM
	}
	var chosen *hlsVariant
	var audioURI string
	if plan.AudioOnly {
		// Prefer a separate audio stream; otherwise the smallest quality, keeping only its sound.
		for i := range master.Variants {
			v := &master.Variants[i]
			if v.Height == 0 && v.Codecs != "" && !codecIsVideo(v.Codecs) {
				if chosen == nil || v.Bandwidth > chosen.Bandwidth {
					chosen = v
				}
			}
		}
		if chosen == nil {
			if r := pickRendition(master.Renditions, ""); r != nil {
				audioURI = r.URI
			}
		}
		if chosen == nil && audioURI == "" {
			for i := range master.Variants {
				v := &master.Variants[i]
				if chosen == nil || v.Bandwidth < chosen.Bandwidth {
					chosen = v
				}
			}
		}
	} else {
		idx, _ := strconv.Atoi(strings.TrimPrefix(variant, "v"))
		if idx < 0 || idx >= len(master.Variants) || !strings.HasPrefix(variant, "v") {
			idx = 0
			for i, v := range master.Variants {
				if v.Bandwidth > master.Variants[idx].Bandwidth {
					idx = i
				}
			}
		}
		chosen = &master.Variants[idx]
		if chosen.Audio != "" {
			if r := pickRendition(master.Renditions, chosen.Audio); r != nil {
				audioURI = r.URI
			}
		}
	}

	if chosen != nil {
		body, final, _, err := m.fetchSmall(ctx, chosen.URI, o, 32<<20)
		if err != nil {
			return nil, err
		}
		pl, err := parseHLSMediaPlaylist(final, string(body))
		if err != nil {
			return nil, err
		}
		if err := checkPlayable(pl); err != nil {
			return nil, err
		}
		pl.Track.Role = "main"
		plan.Duration = pl.Duration
		plan.Tracks = append(plan.Tracks, pl.Track)
	}
	if audioURI != "" {
		body, final, _, err := m.fetchSmall(ctx, audioURI, o, 32<<20)
		if err != nil {
			return nil, err
		}
		pl, err := parseHLSMediaPlaylist(final, string(body))
		if err != nil {
			return nil, err
		}
		if err := checkPlayable(pl); err != nil {
			return nil, err
		}
		pl.Track.Role = "audio"
		if plan.Duration == 0 {
			plan.Duration = pl.Duration
		}
		plan.Tracks = append(plan.Tracks, pl.Track)
	}
	if len(plan.Tracks) == 0 {
		return nil, errors.New("couldn't find that quality in the playlist")
	}
	return plan, nil
}

func checkPlayable(pl *hlsMediaPlaylist) error {
	if pl.DRM {
		return errDRM
	}
	if pl.Live {
		return errLive
	}
	return nil
}

func pickRendition(rs []hlsRendition, group string) *hlsRendition {
	var best *hlsRendition
	for i := range rs {
		r := &rs[i]
		if r.Type != "AUDIO" || r.URI == "" || (group != "" && r.Group != group) {
			continue
		}
		if best == nil || (r.Default && !best.Default) {
			best = r
		}
	}
	return best
}

// ---------- DASH ----------

type mpdXML struct {
	Type     string      `xml:"type,attr"`
	Duration string      `xml:"mediaPresentationDuration,attr"`
	BaseURL  []string    `xml:"BaseURL"`
	Periods  []periodXML `xml:"Period"`
}

type periodXML struct {
	Duration string     `xml:"duration,attr"`
	BaseURL  []string   `xml:"BaseURL"`
	Sets     []adaptXML `xml:"AdaptationSet"`
}

type cpXML struct {
	Scheme string `xml:"schemeIdUri,attr"`
}

type adaptXML struct {
	MimeType    string          `xml:"mimeType,attr"`
	ContentType string          `xml:"contentType,attr"`
	Codecs      string          `xml:"codecs,attr"`
	Lang        string          `xml:"lang,attr"`
	BaseURL     []string        `xml:"BaseURL"`
	CP          []cpXML         `xml:"ContentProtection"`
	Template    *segTemplateXML `xml:"SegmentTemplate"`
	List        *segListXML     `xml:"SegmentList"`
	Base        *segBaseXML     `xml:"SegmentBase"`
	Reps        []repXML        `xml:"Representation"`
	Roles       []cpValXML      `xml:"Role"`
}

type cpValXML struct {
	Value string `xml:"value,attr"`
}

type repXML struct {
	ID        string          `xml:"id,attr"`
	MimeType  string          `xml:"mimeType,attr"`
	Codecs    string          `xml:"codecs,attr"`
	Bandwidth int64           `xml:"bandwidth,attr"`
	Width     int             `xml:"width,attr"`
	Height    int             `xml:"height,attr"`
	BaseURL   []string        `xml:"BaseURL"`
	CP        []cpXML         `xml:"ContentProtection"`
	Template  *segTemplateXML `xml:"SegmentTemplate"`
	List      *segListXML     `xml:"SegmentList"`
	Base      *segBaseXML     `xml:"SegmentBase"`
}

type segTemplateXML struct {
	Media       string  `xml:"media,attr"`
	Init        string  `xml:"initialization,attr"`
	StartNumber *int64  `xml:"startNumber,attr"`
	Timescale   int64   `xml:"timescale,attr"`
	Duration    int64   `xml:"duration,attr"`
	Timeline    *tlXML  `xml:"SegmentTimeline"`
	InitElem    *initEl `xml:"Initialization"`
}

type tlXML struct {
	S []struct {
		T *int64 `xml:"t,attr"`
		D int64  `xml:"d,attr"`
		R int64  `xml:"r,attr"`
	} `xml:"S"`
}

type segListXML struct {
	Timescale int64   `xml:"timescale,attr"`
	Duration  int64   `xml:"duration,attr"`
	Init      *initEl `xml:"Initialization"`
	URLs      []struct {
		Media      string `xml:"media,attr"`
		MediaRange string `xml:"mediaRange,attr"`
	} `xml:"SegmentURL"`
}

type segBaseXML struct {
	Init *initEl `xml:"Initialization"`
}

type initEl struct {
	SourceURL string `xml:"sourceURL,attr"`
	Range     string `xml:"range,attr"`
}

var isoDurRe = regexp.MustCompile(`^P(?:(\d+(?:\.\d+)?)D)?(?:T(?:(\d+(?:\.\d+)?)H)?(?:(\d+(?:\.\d+)?)M)?(?:(\d+(?:\.\d+)?)S)?)?$`)

func parseISODuration(s string) float64 {
	m := isoDurRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0
	}
	f := func(i int) float64 { v, _ := strconv.ParseFloat(m[i], 64); return v }
	return f(1)*86400 + f(2)*3600 + f(3)*60 + f(4)
}

type dashRep struct {
	set, rep   int
	r          repXML
	a          *adaptXML
	video      bool
	audio      bool
	base       *url.URL
	drm        bool
	codecs     string
	periodDur  float64
	periodBase *url.URL
}

func parseMPD(final *url.URL, body []byte) (*mpdXML, error) {
	var mpd mpdXML
	dec := xml.NewDecoder(strings.NewReader(string(body)))
	dec.Strict = false
	if err := dec.Decode(&mpd); err != nil {
		return nil, fmt.Errorf("couldn't read the video manifest: %w", err)
	}
	if len(mpd.Periods) == 0 {
		return nil, errors.New("the video manifest is empty")
	}
	return &mpd, nil
}

func joinBase(base *url.URL, bases []string) *url.URL {
	if len(bases) == 0 || strings.TrimSpace(bases[0]) == "" {
		return base
	}
	u, err := base.Parse(strings.TrimSpace(bases[0]))
	if err != nil {
		return base
	}
	return u
}

// dashReps lists every audio/video representation of the first period.
func dashReps(final *url.URL, mpd *mpdXML) ([]dashRep, float64) {
	total := parseISODuration(mpd.Duration)
	mbase := joinBase(final, mpd.BaseURL)
	p := mpd.Periods[0]
	pdur := parseISODuration(p.Duration)
	if pdur == 0 {
		pdur = total
	}
	if total == 0 {
		for _, pp := range mpd.Periods {
			total += parseISODuration(pp.Duration)
		}
	}
	pbase := joinBase(mbase, p.BaseURL)
	var out []dashRep
	for si := range p.Sets {
		a := &p.Sets[si]
		abase := joinBase(pbase, a.BaseURL)
		for ri, r := range a.Reps {
			d := dashRep{set: si, rep: ri, r: r, a: a, base: joinBase(abase, r.BaseURL), periodDur: pdur, periodBase: pbase}
			mime := firstNonEmpty(r.MimeType, a.MimeType)
			d.codecs = firstNonEmpty(r.Codecs, a.Codecs)
			ct := a.ContentType
			d.video = strings.HasPrefix(mime, "video") || ct == "video" || (mime == "" && codecIsVideo(d.codecs))
			d.audio = !d.video && (strings.HasPrefix(mime, "audio") || ct == "audio" || strings.Contains(d.codecs, "mp4a") || strings.Contains(d.codecs, "opus") || strings.Contains(d.codecs, "ac-3") || strings.Contains(d.codecs, "ec-3"))
			d.drm = len(a.CP) > 0 || len(r.CP) > 0
			if d.video || d.audio {
				out = append(out, d)
			}
		}
	}
	return out, total
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func dashVariantID(r dashRep) string { return fmt.Sprintf("d%d.%d", r.set, r.rep) }

func probeDASH(rawURL string, final *url.URL, body []byte) (*MediaInfo, error) {
	mpd, err := parseMPD(final, body)
	if err != nil {
		return nil, err
	}
	reps, total := dashReps(final, mpd)
	info := &MediaInfo{Kind: "dash", URL: rawURL, Duration: total, Live: mpd.Type == "dynamic"}
	hosts := map[string]bool{}
	addHost(hosts, rawURL)
	addHost(hosts, final.String())
	audio := bestDashAudio(reps)
	seen := map[string]int{}
	for _, r := range reps {
		addHost(hosts, r.base.String())
		if r.drm {
			info.DRM = true
		}
		if !r.video {
			continue
		}
		label := qualityLabel(r.r.Height, r.r.Bandwidth, r.codecs)
		v := MediaVariant{ID: dashVariantID(r), Label: label, Width: r.r.Width, Height: r.r.Height, Bandwidth: r.r.Bandwidth, Codecs: r.codecs}
		bw := r.r.Bandwidth
		if audio != nil {
			bw += audio.r.Bandwidth
		}
		v.EstSize = int64(float64(bw) * total / 8)
		if i, ok := seen[label]; ok {
			if v.Bandwidth > info.Variants[i].Bandwidth {
				info.Variants[i] = v
			}
			continue
		}
		seen[label] = len(info.Variants)
		info.Variants = append(info.Variants, v)
	}
	sort.SliceStable(info.Variants, func(i, j int) bool {
		a, b := info.Variants[i], info.Variants[j]
		if a.Height != b.Height {
			return a.Height > b.Height
		}
		return a.Bandwidth > b.Bandwidth
	})
	if audio != nil {
		info.Variants = append(info.Variants, MediaVariant{ID: "audio", Label: "Audio only", AudioOnly: true,
			Bandwidth: audio.r.Bandwidth, EstSize: int64(float64(audio.r.Bandwidth) * total / 8), Codecs: audio.codecs})
	}
	if len(info.Variants) == 0 {
		return nil, errors.New("no video found in the manifest")
	}
	info.Hosts = hostList(hosts)
	return info, nil
}

func bestDashAudio(reps []dashRep) *dashRep {
	var best *dashRep
	score := func(r *dashRep) int64 {
		s := r.r.Bandwidth
		for _, role := range r.a.Roles {
			if role.Value == "main" {
				s += 1 << 40
			}
		}
		if strings.Contains(r.codecs, "mp4a") {
			s += 1 << 39 // AAC fits in MP4 everywhere
		}
		return s
	}
	for i := range reps {
		r := &reps[i]
		if r.audio && !r.drm && (best == nil || score(r) > score(best)) {
			best = r
		}
	}
	return best
}

var tmplRe = regexp.MustCompile(`\$(RepresentationID|Number|Time|Bandwidth)(?:%0(\d+)d)?\$`)

func fillTemplate(t string, repID string, bw int64, number, tm int64) string {
	out := tmplRe.ReplaceAllStringFunc(t, func(s string) string {
		m := tmplRe.FindStringSubmatch(s)
		var v string
		switch m[1] {
		case "RepresentationID":
			return repID
		case "Number":
			v = strconv.FormatInt(number, 10)
		case "Time":
			v = strconv.FormatInt(tm, 10)
		case "Bandwidth":
			v = strconv.FormatInt(bw, 10)
		}
		if m[2] != "" {
			w, _ := strconv.Atoi(m[2])
			for len(v) < w {
				v = "0" + v
			}
		}
		return v
	})
	return strings.ReplaceAll(out, "$$", "$")
}

func parseRange(s string) (off, n int64, ok bool) {
	parts := strings.SplitN(strings.TrimSpace(s), "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	a, e1 := strconv.ParseInt(parts[0], 10, 64)
	b, e2 := strconv.ParseInt(parts[1], 10, 64)
	if e1 != nil || e2 != nil || b < a {
		return 0, 0, false
	}
	return a, b - a + 1, true
}

// dashTrack builds the list of chunks for one representation.
// A representation stored as one big file gets size -1 and is split later.
func dashTrack(r dashRep, role string) (TrackPlan, error) {
	t := TrackPlan{Role: role}
	tmpl := r.r.Template
	if tmpl == nil {
		tmpl = r.a.Template
	} else if r.a.Template != nil {
		merged := *r.a.Template
		if tmpl.Media != "" {
			merged.Media = tmpl.Media
		}
		if tmpl.Init != "" {
			merged.Init = tmpl.Init
		}
		if tmpl.StartNumber != nil {
			merged.StartNumber = tmpl.StartNumber
		}
		if tmpl.Timescale != 0 {
			merged.Timescale = tmpl.Timescale
		}
		if tmpl.Duration != 0 {
			merged.Duration = tmpl.Duration
		}
		if tmpl.Timeline != nil {
			merged.Timeline = tmpl.Timeline
		}
		tmpl = &merged
	}
	list := r.r.List
	if list == nil {
		list = r.a.List
	}
	id, bw := r.r.ID, r.r.Bandwidth
	switch {
	case tmpl != nil && tmpl.Media != "":
		ts := tmpl.Timescale
		if ts == 0 {
			ts = 1
		}
		start := int64(1)
		if tmpl.StartNumber != nil {
			start = *tmpl.StartNumber
		}
		initT := tmpl.Init
		if initT == "" && tmpl.InitElem != nil {
			initT = tmpl.InitElem.SourceURL
		}
		if initT != "" {
			t.Init = &SegRef{URL: resolveRef(r.base, fillTemplate(initT, id, bw, 0, 0))}
		}
		num := start
		if tmpl.Timeline != nil && len(tmpl.Timeline.S) > 0 {
			var cur int64
			end := int64(math.MaxInt64)
			if r.periodDur > 0 {
				end = int64(r.periodDur * float64(ts))
			}
			for i, s := range tmpl.Timeline.S {
				if s.T != nil {
					cur = *s.T
				}
				if s.D <= 0 {
					continue
				}
				repeat := s.R
				if repeat < 0 {
					next := end
					if i+1 < len(tmpl.Timeline.S) && tmpl.Timeline.S[i+1].T != nil {
						next = *tmpl.Timeline.S[i+1].T
					}
					if next == math.MaxInt64 {
						return t, errLive
					}
					repeat = (next-cur+s.D-1)/s.D - 1
				}
				for k := int64(0); k <= repeat; k++ {
					t.Segs = append(t.Segs, SegRef{URL: resolveRef(r.base, fillTemplate(tmpl.Media, id, bw, num, cur)), D: float64(s.D) / float64(ts)})
					cur += s.D
					num++
					if len(t.Segs) > 200000 {
						return t, errors.New("the video manifest is too large")
					}
				}
			}
		} else if tmpl.Duration > 0 {
			if r.periodDur <= 0 {
				return t, errLive
			}
			count := int64(math.Ceil(r.periodDur * float64(ts) / float64(tmpl.Duration)))
			for k := int64(0); k < count; k++ {
				segD := float64(tmpl.Duration) / float64(ts)
				if k == count-1 {
					segD = math.Max(0.001, r.periodDur-float64(k)*segD)
				}
				t.Segs = append(t.Segs, SegRef{URL: resolveRef(r.base, fillTemplate(tmpl.Media, id, bw, start+k, k*tmpl.Duration)), D: segD})
			}
		}
	case list != nil && len(list.URLs) > 0:
		if list.Init != nil {
			ref := &SegRef{URL: r.base.String()}
			if list.Init.SourceURL != "" {
				ref.URL = resolveRef(r.base, list.Init.SourceURL)
			}
			if off, n, ok := parseRange(list.Init.Range); ok {
				ref.Off, ref.Len = off, n
			}
			t.Init = ref
		}
		for _, su := range list.URLs {
			s := SegRef{URL: r.base.String()}
			if su.Media != "" {
				s.URL = resolveRef(r.base, su.Media)
			}
			if off, n, ok := parseRange(su.MediaRange); ok {
				s.Off, s.Len = off, n
			}
			t.Segs = append(t.Segs, s)
		}
	default:
		// The whole representation is one file (SegmentBase / plain BaseURL).
		t.Segs = []SegRef{{URL: r.base.String(), Len: -1}}
	}
	if len(t.Segs) == 0 {
		return t, errors.New("couldn't work out the video chunks from the manifest")
	}
	t.Ext = extOfURL(t.Segs[0].URL)
	return t, nil
}

func (m *Manager) planDASH(ctx context.Context, final *url.URL, body []byte, variant string, o reqOpts) (*MediaPlan, error) {
	mpd, err := parseMPD(final, body)
	if err != nil {
		return nil, err
	}
	if mpd.Type == "dynamic" {
		return nil, errLive
	}
	reps, total := dashReps(final, mpd)
	plan := &MediaPlan{Kind: "dash", Variant: variant, Duration: total, AudioOnly: variant == "audio"}
	audio := bestDashAudio(reps)
	if !plan.AudioOnly {
		var video *dashRep
		for i := range reps {
			if reps[i].video && (dashVariantID(reps[i]) == variant) {
				video = &reps[i]
			}
		}
		if video == nil { // fall back to the best quality
			for i := range reps {
				if reps[i].video && (video == nil || reps[i].r.Bandwidth > video.r.Bandwidth) {
					video = &reps[i]
				}
			}
		}
		if video == nil {
			return nil, errors.New("no video found in the manifest")
		}
		if video.drm {
			return nil, errDRM
		}
		t, err := dashTrack(*video, "main")
		if err != nil {
			return nil, err
		}
		plan.Tracks = append(plan.Tracks, t)
	}
	if audio != nil {
		t, err := dashTrack(*audio, "audio")
		if err != nil {
			return nil, err
		}
		plan.Tracks = append(plan.Tracks, t)
	} else if plan.AudioOnly {
		return nil, errors.New("this video has no separate sound track")
	}
	if len(mpd.Periods) > 1 {
		plan.Duration = total
	}
	// Split single-file representations into 4 MB pieces so many connections can work on them.
	for ti := range plan.Tracks {
		tr := &plan.Tracks[ti]
		if len(tr.Segs) == 1 && tr.Segs[0].Len == -1 {
			size, err := m.remoteSize(ctx, tr.Segs[0].URL, o)
			if err != nil {
				return nil, err
			}
			u := tr.Segs[0].URL
			tr.Segs = nil
			const piece = 4 << 20
			for off := int64(0); off < size; off += piece {
				tr.Segs = append(tr.Segs, SegRef{URL: u, Off: off, Len: min(piece, size-off)})
			}
		}
	}
	return plan, nil
}

func (m *Manager) remoteSize(ctx context.Context, rawURL string, o reqOpts) (int64, error) {
	req, err := m.newReq(ctx, rawURL, o)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", "bytes=0-0")
	resp, err := m.client.Do(req)
	if err != nil {
		return 0, err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return 0, fmt.Errorf("server replied %s", resp.Status)
	}
	if total := parseContentRangeTotal(resp.Header.Get("Content-Range")); total > 0 {
		return total, nil
	}
	if resp.ContentLength > 0 {
		return resp.ContentLength, nil
	}
	return 0, errors.New("the server didn't say how big the video file is")
}

// BuildMediaPlan fetches the playlist again and picks the chunks for the chosen quality.
func (m *Manager) BuildMediaPlan(ctx context.Context, rawURL, variant string, o reqOpts) (*MediaPlan, error) {
	body, final, ctype, err := m.fetchSmall(ctx, rawURL, o, 32<<20)
	if err != nil {
		return nil, err
	}
	switch detectPlaylist(body, ctype) {
	case "hls":
		return m.planHLS(ctx, rawURL, final, string(body), variant, o)
	case "dash":
		return m.planDASH(ctx, final, body, variant, o)
	}
	return nil, errNotAPlaylist
}

func hexIV(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 16 {
		return make([]byte, 16)
	}
	return b
}
