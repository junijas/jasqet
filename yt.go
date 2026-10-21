package main

// YouTube (and other video sites) through yt-dlp. yt-dlp tells us the video's
// qualities and their direct links; Jasqet then downloads those with its own
// many-connection engine and joins picture + sound into an MP4. Links are fetched
// fresh every time a download starts, so videos queued at night still work at
// dawn. If YouTube refuses our connections, yt-dlp downloads the parts itself.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	errToolsMissing = errors.New("YouTube support isn't set up yet. Try again and it sets itself up")
	errYTFallback   = errors.New("needs yt-dlp's own downloader")
)

type ytFragment struct {
	URL  string `json:"url"`
	Path string `json:"path"`
}

type ytFormat struct {
	FormatID        string            `json:"format_id"`
	Ext             string            `json:"ext"`
	VCodec          string            `json:"vcodec"`
	ACodec          string            `json:"acodec"`
	Height          *float64          `json:"height"`
	FPS             *float64          `json:"fps"`
	TBR             *float64          `json:"tbr"`
	ABR             *float64          `json:"abr"`
	Filesize        *float64          `json:"filesize"`
	FilesizeApprox  *float64          `json:"filesize_approx"`
	Protocol        string            `json:"protocol"`
	URL             string            `json:"url"`
	HTTPHeaders     map[string]string `json:"http_headers"`
	HasDRM          any               `json:"has_drm"`
	DynamicRange    string            `json:"dynamic_range"`
	FormatNote      string            `json:"format_note"`
	LanguagePref    *float64          `json:"language_preference"`
	Fragments       []ytFragment      `json:"fragments"`
	FragmentBaseURL string            `json:"fragment_base_url"`
}

type ytEntry struct {
	ID       string  `json:"id"`
	URL      string  `json:"url"`
	Title    string  `json:"title"`
	Duration float64 `json:"duration"`
}

type ytInfo struct {
	Type          string     `json:"_type"`
	ID            string     `json:"id"`
	Title         string     `json:"title"`
	Duration      float64    `json:"duration"`
	WebpageURL    string     `json:"webpage_url"`
	Extractor     string     `json:"extractor_key"`
	IsLive        bool       `json:"is_live"`
	LiveStatus    string     `json:"live_status"`
	Formats       []ytFormat `json:"formats"`
	Entries       []ytEntry  `json:"entries"`
	PlaylistCount int        `json:"playlist_count"`
	// some sites give one file without a formats list
	URL         string            `json:"url"`
	Ext         string            `json:"ext"`
	HTTPHeaders map[string]string `json:"http_headers"`

	Subtitles    map[string][]ytSubFormat `json:"subtitles"`
	AutoCaptions map[string][]ytSubFormat `json:"automatic_captions"`
}

type YTPlaylist struct {
	Title   string    `json:"title"`
	Count   int       `json:"count"`
	Entries []ytEntry `json:"entries"`
}

type YTProbe struct {
	Kind     string         `json:"kind"` // "yt"
	URL      string         `json:"url"`
	Title    string         `json:"title"`
	Site     string         `json:"site"`
	Duration float64        `json:"duration"`
	Live     bool           `json:"live"`
	DRM      bool           `json:"drm"`
	Variants []MediaVariant `json:"variants"`
	Playlist *YTPlaylist    `json:"playlist,omitempty"`
	Listing  bool           `json:"playlistLoading,omitempty"` // the list this video came from is still being read
	Setup    *ToolsStatus   `json:"setup,omitempty"`
	Subs     []SubRef       `json:"subs,omitempty"`
}

func fv(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

func (f *ytFormat) drm() bool {
	switch v := f.HasDRM.(type) {
	case bool:
		return v
	case string:
		return v != "" && v != "false"
	}
	return false
}

func (f *ytFormat) hasVideo() bool { return f.VCodec != "" && f.VCodec != "none" }
func (f *ytFormat) hasAudio() bool { return f.ACodec != "" && f.ACodec != "none" }

func (f *ytFormat) size(dur float64) int64 {
	if s := fv(f.Filesize); s > 0 {
		return int64(s)
	}
	if s := fv(f.FilesizeApprox); s > 0 {
		return int64(s)
	}
	if t := fv(f.TBR); t > 0 && dur > 0 {
		return int64(t * 1000 / 8 * dur)
	}
	return 0
}

// protocolOK: formats Jasqet can download itself.
func (f *ytFormat) protocolOK() bool {
	p := f.Protocol
	return p == "https" || p == "http" || strings.HasPrefix(p, "m3u8") || p == "http_dash_segments"
}

// joinable: picture/sound Jasqet can put into an MP4 without ffmpeg.
func (f *ytFormat) joinable(haveFF bool) bool {
	if haveFF {
		return true
	}
	if strings.HasPrefix(f.Protocol, "m3u8") {
		return true // MPEG-TS chunks: H.264 / AAC
	}
	v, a := strings.ToLower(f.VCodec), strings.ToLower(f.ACodec)
	if f.hasVideo() && v != "unknown" && !(strings.HasPrefix(v, "avc") || strings.HasPrefix(v, "av01") || strings.HasPrefix(v, "hvc") || strings.HasPrefix(v, "hev") || strings.HasPrefix(v, "h264")) {
		return false
	}
	if f.hasAudio() && a != "unknown" && !(strings.HasPrefix(a, "mp4a") || a == "aac") {
		return false
	}
	return f.Ext == "mp4" || f.Ext == "m4a" || f.Ext == "mov" || f.Ext == "m4v"
}

func (f *ytFormat) usable(haveFF bool) bool {
	return !f.drm() && f.protocolOK() && f.URL != "" && f.joinable(haveFF) && f.Ext != "mhtml"
}

func (f *ytFormat) videoScore() float64 {
	s := fv(f.TBR)
	v := strings.ToLower(f.VCodec)
	switch {
	case strings.HasPrefix(v, "avc") || strings.HasPrefix(v, "h264"):
		s += 3e6 // plays everywhere, even old phones and TVs
	case strings.HasPrefix(v, "av01"):
		s += 2e6
	case strings.HasPrefix(v, "hvc") || strings.HasPrefix(v, "hev"):
		s += 1e6
	}
	if f.DynamicRange != "" && f.DynamicRange != "SDR" {
		s -= 5e6 // HDR looks washed out on most screens
	}
	if !f.hasAudio() {
		s += 1e5 // separate sound is usually better quality
	}
	if f.Protocol == "https" || f.Protocol == "http" {
		s += 5e4
	}
	s += fv(f.FPS) * 10
	return s
}

func (f *ytFormat) audioScore() float64 {
	s := fv(f.ABR)
	if s == 0 {
		s = fv(f.TBR)
	}
	if f.LanguagePref != nil {
		s += *f.LanguagePref * 1e4 // the video's original language first
	} else {
		s -= 5e4 // language unknown: only if nothing better
	}
	switch a := strings.ToLower(f.ACodec); {
	case strings.HasPrefix(a, "mp4a"):
		s += 1e3
	case a == "unknown":
		s -= 2e3
	}
	if strings.Contains(strings.ToLower(f.FormatNote), "original") {
		s += 5e3
	}
	if f.Protocol == "https" || f.Protocol == "http" {
		s += 3e3 // one file: fast, many connections
	}
	if strings.Contains(strings.ToLower(f.FormatID+" "+f.FormatNote), "drc") {
		s -= 500 // volume-squashed copy
	}
	return s
}

func normalizeFormats(info *ytInfo) {
	if len(info.Formats) == 0 && info.URL != "" {
		info.Formats = []ytFormat{{FormatID: "0", Ext: info.Ext, URL: info.URL, Protocol: "https", VCodec: "unknown", ACodec: "unknown", HTTPHeaders: info.HTTPHeaders}}
		if strings.HasPrefix(info.URL, "http:") {
			info.Formats[0].Protocol = "http"
		}
		if strings.Contains(strings.ToLower(info.URL), ".m3u8") {
			info.Formats[0].Protocol = "m3u8_native"
		}
	}
	for i := range info.Formats {
		f := &info.Formats[i]
		if f.VCodec == "" {
			f.VCodec = "unknown"
		}
		if f.ACodec == "" {
			f.ACodec = "unknown"
		}
	}
}

func bestAudio(info *ytInfo, haveFF bool, preferMP4 bool) *ytFormat {
	var best *ytFormat
	for i := range info.Formats {
		f := &info.Formats[i]
		if f.hasVideo() || !f.hasAudio() || !f.usable(haveFF) {
			continue
		}
		if ac := strings.ToLower(f.ACodec); preferMP4 && !haveFF && !strings.HasPrefix(ac, "mp4a") && ac != "unknown" && !strings.HasPrefix(f.Protocol, "m3u8") {
			continue
		}
		if best == nil || f.audioScore() > best.audioScore() {
			best = f
		}
	}
	return best
}

// pickFormats chooses picture + sound for a quality like "h:720", "best" or "audio".
func pickFormats(info *ytInfo, variant string, haveFF bool) (video, audio *ytFormat, audioOnly bool, err error) {
	if variant == "audio" {
		if a := bestAudio(info, haveFF, false); a != nil {
			return nil, a, true, nil
		}
		// no separate sound: take the smallest video with sound and keep only the sound
		var small *ytFormat
		for i := range info.Formats {
			f := &info.Formats[i]
			if f.hasAudio() && f.usable(haveFF) && (small == nil || fv(f.TBR) < fv(small.TBR)) {
				small = f
			}
		}
		if small == nil {
			return nil, nil, false, errors.New("couldn't find the sound of this video")
		}
		return small, nil, true, nil
	}
	maxH := math.Inf(1)
	if strings.HasPrefix(variant, "h:") {
		if h, e := strconv.Atoi(strings.TrimPrefix(variant, "h:")); e == nil {
			maxH = float64(h)
		}
	}
	var best *ytFormat
	for _, strict := range []bool{true, false} {
		for i := range info.Formats {
			f := &info.Formats[i]
			if !f.hasVideo() || !f.usable(haveFF) {
				continue
			}
			h := fv(f.Height)
			if strict && h > maxH {
				continue
			}
			if !f.hasAudio() && bestAudio(info, haveFF, true) == nil {
				continue // picture without any sound we could add
			}
			if best == nil || h > fv(best.Height) || (h == fv(best.Height) && f.videoScore() > best.videoScore()) {
				best = f
			}
		}
		if best != nil {
			break
		}
	}
	if best == nil {
		if anyDRM(info) {
			return nil, nil, false, errDRM
		}
		return nil, nil, false, errors.New("no downloadable quality found for this video")
	}
	if !best.hasAudio() {
		audio = bestAudio(info, haveFF, true)
	}
	return best, audio, false, nil
}

func anyDRM(info *ytInfo) bool {
	for i := range info.Formats {
		if info.Formats[i].drm() {
			return true
		}
	}
	return false
}

// ytVariants builds the quality menu: one entry per height, plus "Audio only".
func ytVariants(info *ytInfo, haveFF bool) []MediaVariant {
	heights := map[int]bool{}
	for i := range info.Formats {
		f := &info.Formats[i]
		if f.hasVideo() && f.usable(haveFF) {
			heights[int(fv(f.Height))] = true
		}
	}
	var hs []int
	for h := range heights {
		hs = append(hs, h)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(hs)))
	var out []MediaVariant
	for _, h := range hs {
		id := "h:" + strconv.Itoa(h)
		label := fmt.Sprintf("%dp", h)
		if h == 0 {
			id, label = "best", "Best quality"
		}
		v, a, _, err := pickFormats(info, id, haveFF)
		if err != nil || v == nil {
			continue
		}
		if int(fv(v.Height)) != h && h != 0 {
			continue
		}
		if fv(v.FPS) >= 48 {
			label += " " + strconv.Itoa(int(math.Round(fv(v.FPS))))
		}
		size := v.size(info.Duration)
		if a != nil {
			size += a.size(info.Duration)
		}
		out = append(out, MediaVariant{ID: id, Label: label, Height: h, EstSize: size, Codecs: v.VCodec})
	}
	if v, a, _, err := pickFormats(info, "audio", haveFF); err == nil {
		f := a
		if f == nil {
			f = v
		}
		est := int64(0)
		if a != nil {
			est = a.size(info.Duration)
		}
		_ = f
		out = append(out, MediaVariant{ID: "audio", Label: "Audio only", AudioOnly: true, EstSize: est})
	}
	return out
}

// ---------- running yt-dlp ----------

func (m *Manager) proxyArg() string {
	m.mu.Lock()
	p := m.settings.Proxy
	m.mu.Unlock()
	if p != "" && !strings.Contains(p, "://") {
		p = "http://" + p
	}
	return p
}

// webLink: an http(s) address with a host. Anything else handed to yt-dlp
// could be taken for one of its options ("--config-locations=…").
func webLink(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// pageArgs puts the page last, after "--", so yt-dlp never reads it as an option.
func pageArgs(pageURL string, opts ...string) []string {
	return append(append(opts[:len(opts):len(opts)], "--"), pageURL)
}

func isYouTube(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "youtu.be" || isHostOf(h, "youtube.com") || isHostOf(h, "youtube-nocookie.com")
}

// isHostOf: h is the site itself or one of its subdomains ("www.youtube.com"),
// never a look-alike that only ends the same ("evil-youtube.com").
func isHostOf(h, site string) bool {
	return h == site || strings.HasSuffix(h, "."+site)
}

// writeCookieFile turns per-site cookies into the file format yt-dlp reads.
// YouTube cookies are only passed as your YouTube session ("youtube.com"),
// which the extension sends only if you allowed it; others are left out.
func writeCookieFile(cookies map[string]string) string {
	if len(cookies) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("# Netscape HTTP Cookie File\n")
	exp := strconv.FormatInt(time.Now().Add(24*time.Hour).Unix(), 10)
	for host, header := range cookies {
		domain, sub, secure := host, "FALSE", "FALSE"
		if host == ytSessionKey {
			domain, sub, secure = ".youtube.com", "TRUE", "TRUE"
		} else if isYouTube("https://"+host+"/") || isHostOf(strings.ToLower(host), "google.com") {
			continue
		}
		for _, kv := range strings.Split(header, ";") {
			kv = strings.TrimSpace(kv)
			i := strings.IndexByte(kv, '=')
			if i <= 0 {
				continue
			}
			fmt.Fprintf(&b, "%s\t%s\t/\t%s\t%s\t%s\t%s\n", domain, sub, secure, exp, kv[:i], kv[i+1:])
		}
	}
	sweepCookieFiles(6 * time.Hour)
	f, err := os.CreateTemp("", "jasqet-cookies-*.txt")
	if err != nil {
		return ""
	}
	f.WriteString(b.String())
	f.Close()
	return f.Name()
}

// sweepCookieFiles removes yt-dlp's cookie files older than olderThan from the
// temp folder (also the ones under Jasqet's old name, TurboGet). A crash or
// power cut can leave them there, and they hold logins. Jasqet does it when
// it starts and each time it makes a new one.
func sweepCookieFiles(olderThan time.Duration) {
	old, _ := filepath.Glob(filepath.Join(os.TempDir(), "jasqet-cookies-*.txt"))
	prev, _ := filepath.Glob(filepath.Join(os.TempDir(), "turboget-cookies-*.txt"))
	for _, p := range append(old, prev...) {
		if st, err := os.Stat(p); err == nil && time.Since(st.ModTime()) > olderThan {
			os.Remove(p)
		}
	}
}

// ytdlpArgs: the options every yt-dlp call starts with. The proxy (it may
// hold a password) isn't one of them: see ytdlpCmd.
func (m *Manager) ytdlpArgs(cookieFile string) []string {
	args := []string{"--ignore-config", "--no-warnings", "--no-progress", "--force-ipv4"}
	if d := denoPath(); d != "" {
		args = append(args, "--js-runtimes", "deno:"+d)
	}
	if cookieFile != "" {
		args = append(args, "--cookies", cookieFile)
	}
	return args
}

// ytdlpCmd makes every yt-dlp command (tests look at it without running it).
// The proxy goes in its environment, not on its command line: other people
// on the same computer can see a program's command line, password and all,
// but not its environment. yt-dlp uses these when it has no --proxy.
func (m *Manager) ytdlpCmd(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, ytdlpPath(), args...)
	hideWindow(cmd)
	if p := m.proxyArg(); p != "" {
		env := []string{}
		for _, kv := range os.Environ() {
			k, _, _ := strings.Cut(kv, "=")
			switch strings.ToLower(k) {
			case "http_proxy", "https_proxy", "no_proxy":
				continue // ours goes for every site, as --proxy did
			}
			env = append(env, kv)
		}
		// lowercase too: on Linux and Mac those win over the uppercase ones
		cmd.Env = append(env, "HTTP_PROXY="+p, "HTTPS_PROXY="+p, "http_proxy="+p, "https_proxy="+p)
	}
	return cmd
}

func (m *Manager) runYtDlp(ctx context.Context, cookies map[string]string, args ...string) ([]byte, error) {
	if !m.tools.ready() {
		return nil, errToolsMissing
	}
	cf := writeCookieFile(cookies)
	if cf != "" {
		defer os.Remove(cf)
	}
	full := append(m.ytdlpArgs(cf), args...)
	cmd := m.ytdlpCmd(ctx, full...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ytError(stderr.String(), err)
	}
	return out, nil
}

var ytErrRe = regexp.MustCompile(`(?m)^ERROR:\s*(?:\[[^\]]+\]\s*)?(?:[\w-]+:\s*)?(.+)$`)

func ytError(stderr string, err error) error {
	msg := ""
	if m := ytErrRe.FindAllStringSubmatch(stderr, -1); len(m) > 0 {
		msg = m[len(m)-1][1]
	} else {
		msg = lastLine(stderr)
	}
	low := strings.ToLower(msg)
	switch {
	case strings.Contains(low, "confirm your age"):
		return errors.New("YouTube only shows this video to signed-in adults, so it can't be downloaded")
	case isBotCheck(low):
		return errBotCheck
	case strings.Contains(low, "private video"):
		return errors.New("this video is private")
	case strings.Contains(low, "members"):
		return errors.New("this video is for channel members only")
	case strings.Contains(low, "unsupported url"):
		return errors.New("the site helper doesn't know this website")
	case strings.Contains(low, "drm"):
		return errDRM
	case strings.Contains(low, "live event will begin") || strings.Contains(low, "premieres in"):
		return errors.New("this video hasn't started yet")
	case strings.Contains(low, "video unavailable") || strings.Contains(low, "not available"):
		return errors.New("this video isn't available (removed, blocked in your country, or the link is wrong)")
	case strings.Contains(low, "unable to download") || strings.Contains(low, "timed out") || strings.Contains(low, "getaddrinfo"):
		return errors.New("couldn't reach the website. Check your internet")
	}
	if msg == "" {
		msg = err.Error()
	}
	return errors.New(msg)
}

// ytInfoFor asks yt-dlp about one video (fresh links every time).
func (m *Manager) ytInfoFor(ctx context.Context, pageURL string, cookies map[string]string) (*ytInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := m.ytRun(ctx, pageURL, cookies, pageArgs(pageURL, "-J", "--no-playlist")...)
	if err != nil && ctx.Err() == nil && !isUserFacing(err) {
		// yt-dlp may be out of date (YouTube changes often): update and try once more
		if m.UpdateYtDlp(context.Background()) == nil {
			out, err = m.ytRun(ctx, pageURL, cookies, pageArgs(pageURL, "-J", "--no-playlist")...)
		}
	}
	if err != nil {
		return nil, err
	}
	var info ytInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return nil, errors.New("couldn't read the video details")
	}
	if info.Type == "playlist" && len(info.Entries) == 0 {
		return nil, errors.New("this page has no videos")
	}
	normalizeFormats(&info)
	return &info, nil
}

func isUserFacing(err error) bool {
	s := err.Error()
	for _, k := range []string{"private", "members", "adults", "robot", "isn't available", "hasn't started", "DRM", "doesn't know this website", "check your internet"} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

func (m *Manager) ytPlaylist(ctx context.Context, pageURL string, cookies map[string]string) (*YTPlaylist, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := m.ytRun(ctx, pageURL, cookies, pageArgs(pageURL, "-J", "--flat-playlist", "--yes-playlist")...)
	if err != nil {
		return nil, err
	}
	var info ytInfo
	if err := json.Unmarshal(out, &info); err != nil {
		return nil, err
	}
	if info.Type != "playlist" || len(info.Entries) == 0 {
		return nil, nil
	}
	pl := &YTPlaylist{Title: info.Title, Count: len(info.Entries)}
	for _, e := range info.Entries {
		u := e.URL
		if u == "" && e.ID != "" && isYouTube(pageURL) {
			u = "https://www.youtube.com/watch?v=" + e.ID
		}
		if u != "" {
			pl.Entries = append(pl.Entries, ytEntry{URL: u, Title: e.Title, Duration: e.Duration})
		}
	}
	return pl, nil
}

var (
	ytProbeMu    sync.Mutex
	ytProbeCache = map[string]struct {
		p  *YTProbe
		at time.Time
	}{}
	ytLists = map[string]*ytListJob{} // under ytProbeMu
)

// ytVideoInfo and ytListOf ask yt-dlp about one video or a list (tests put
// stand-ins here).
var (
	ytVideoInfo = (*Manager).ytInfoFor
	ytListOf    = (*Manager).ytPlaylist
)

// ytListJob: the list a video was opened from (watch?v=…&list=…). A Mix can
// take a minute to read, so the video's own qualities don't wait for it: the
// list is read in the background and added to the menu once it's there.
type ytListJob struct {
	done bool
	pl   *YTPlaylist
	at   time.Time
}

// listFor: the list of pageURL if it has been read; else it starts reading
// it (once) and says it's still loading.
func (m *Manager) listFor(pageURL string, cookies map[string]string) (pl *YTPlaylist, loading bool) {
	ytProbeMu.Lock()
	defer ytProbeMu.Unlock()
	if j := ytLists[pageURL]; j != nil && (!j.done || time.Since(j.at) < 10*time.Minute) {
		return j.pl, !j.done
	}
	if len(ytLists) > 50 { // old ones go
		for k, j := range ytLists {
			if j.done && time.Since(j.at) > 10*time.Minute {
				delete(ytLists, k)
			}
		}
	}
	j := &ytListJob{at: time.Now()}
	ytLists[pageURL] = j
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		pl, _ := ytListOf(m, ctx, pageURL, cookies)
		ytProbeMu.Lock()
		j.pl, j.done, j.at = pl, true, time.Now()
		ytProbeMu.Unlock()
	}()
	return nil, true
}

// withList: p with the list it came from, or "still loading".
func (m *Manager) withList(p *YTProbe, pageURL string, cookies map[string]string) *YTProbe {
	if p.Playlist != nil || !hasPlaylistParam(pageURL) {
		return p
	}
	q := *p
	q.Playlist, q.Listing = m.listFor(pageURL, cookies)
	if q.Title == "" && q.Playlist != nil {
		q.Title = q.Playlist.Title
	}
	return &q
}

func hasPlaylistParam(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return u.Query().Get("list") != "" || strings.Contains(u.Path, "/playlist")
}

// ProbeYT gives the quality menu for a page (and its playlist, if any).
func (m *Manager) ProbeYT(ctx context.Context, pageURL string, cookies map[string]string) (*YTProbe, error) {
	if !webLink(pageURL) {
		return nil, errors.New("that isn't a web address. It should start with http:// or https://")
	}
	if !m.tools.ready() {
		st := m.ToolsStatus()
		return &YTProbe{Kind: "yt", URL: pageURL, Setup: &st}, nil
	}
	m.maybeUpdate()
	ytProbeMu.Lock()
	if c, ok := ytProbeCache[pageURL]; ok && time.Since(c.at) < 10*time.Minute {
		ytProbeMu.Unlock()
		return m.withList(c.p, pageURL, cookies), nil
	}
	ytProbeMu.Unlock()

	onlyPL := false
	if u, err := url.Parse(pageURL); err == nil {
		path := strings.ToLower(u.Path)
		single := strings.HasPrefix(path, "/watch") || strings.HasPrefix(path, "/shorts/") || strings.HasPrefix(path, "/embed/") || strings.HasPrefix(path, "/live/") || strings.HasPrefix(path, "/v/")
		if strings.Contains(path, "/playlist") || (isYouTube(pageURL) && !single && u.Hostname() != "youtu.be") {
			onlyPL = true // a playlist or channel page: just list its videos
		}
	}
	p := &YTProbe{Kind: "yt", URL: pageURL}
	if onlyPL {
		pl, _ := ytListOf(m, ctx, pageURL, cookies)
		if pl == nil {
			return nil, errors.New("no videos found on this page")
		}
		p.Playlist, p.Title = pl, pl.Title
	} else {
		if hasPlaylistParam(pageURL) {
			m.listFor(pageURL, cookies) // start reading the list alongside
		}
		info, ierr := ytVideoInfo(m, ctx, pageURL, cookies)
		if ierr != nil {
			if pl, _ := m.listFor(pageURL, cookies); pl != nil && hasPlaylistParam(pageURL) {
				return m.withList(p, pageURL, cookies), nil // the video can't be read, its list can
			}
			return nil, ierr
		}
		if info.WebpageURL != "" && !hasPlaylistParam(pageURL) {
			p.URL = info.WebpageURL
		}
		p.Title, p.Site, p.Duration = info.Title, info.Extractor, info.Duration
		p.Live = info.IsLive || info.LiveStatus == "is_live" || info.LiveStatus == "is_upcoming"
		p.Variants = ytVariants(info, ffmpegPath() != "")
		p.Subs = ytSubtitleChoices(info)
		if len(p.Variants) == 0 && anyDRM(info) {
			p.DRM = true
		}
	}
	ytProbeMu.Lock()
	ytProbeCache[pageURL] = struct {
		p  *YTProbe
		at time.Time
	}{p, time.Now()}
	ytProbeMu.Unlock()
	return m.withList(p, pageURL, cookies), nil
}

// ---------- adding ----------

// subLangRe: a subtitle language code ("en", "pt-BR", "zh-Hans", "en-orig").
var subLangRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

type AddYTRequest struct {
	URL      string            `json:"url"`
	Variant  string            `json:"variant"`
	Quality  string            `json:"quality"`
	Title    string            `json:"title"`
	EstSize  int64             `json:"estSize"`
	Later    bool              `json:"later"`
	Cookies  map[string]string `json:"cookies"`
	Headers  map[string]string `json:"headers"`
	Playlist *YTPlaylist       `json:"playlist"`
	Dir      string            `json:"dir"`
	Fallback bool              `json:"fallback"` // let yt-dlp download the parts from the start
	Clip     *ClipSpec         `json:"clip"`     // only this part of the video
	Duration float64           `json:"duration"` // the video's length (for the clip size estimate)
	Subs     string            `json:"subs"`     // subtitle language to save as .srt
}

func (m *Manager) AddYT(r AddYTRequest) ([]string, error) {
	type item struct{ url, title string }
	var items []item
	dir := r.Dir
	if r.Playlist != nil && len(r.Playlist.Entries) > 0 {
		width := len(strconv.Itoa(len(r.Playlist.Entries)))
		for i, e := range r.Playlist.Entries {
			t := e.Title
			if t == "" {
				t = "Video"
			}
			items = append(items, item{e.URL, fmt.Sprintf("%0*d - %s", width, i+1, t)})
		}
		if dir == "" {
			m.mu.Lock()
			base := m.settings.DownloadDir
			if m.settings.Categorize {
				base = filepath.Join(base, "Video")
			}
			m.mu.Unlock()
			name := sanitizeFilename(r.Playlist.Title)
			if name == "download" {
				name = "Playlist"
			}
			dir = filepath.Join(base, name)
		}
	} else {
		items = append(items, item{r.URL, r.Title})
	}
	m.mu.Lock()
	conns := min(m.settings.Connections, 8) // YouTube dislikes too many connections at once
	m.mu.Unlock()
	cookies := map[string]string{}
	for h, c := range r.Cookies {
		cookies[strings.ToLower(h)] = c
	}
	if c := cookies[ytSessionKey]; c != "" {
		// your YouTube session is kept once, encrypted, not with each download
		m.saveYTSession(c)
		delete(cookies, ytSessionKey)
	}
	ext := ".mp4"
	if r.Variant == "audio" {
		ext = ".m4a"
	}
	kind := "yt"
	if r.Variant == "subs" { // only the subtitles
		if r.Subs == "" {
			return nil, errors.New("choose a subtitle language first")
		}
		if !subLangRe.MatchString(r.Subs) {
			return nil, errors.New("that subtitle language isn't one Jasqet knows")
		}
		kind, ext = "subs", "."+strings.TrimSuffix(r.Subs, "-orig")+".srt"
	}
	var ids []string
	now := time.Now()
	for i, it := range items {
		u, err := url.Parse(it.url)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
			continue
		}
		title := strings.TrimSpace(it.title)
		if title == "" {
			title = "Video " + now.Format("2006-01-02 15.04")
		}
		d := &Download{
			ID: newID(), Kind: kind, URL: it.url, Variant: r.Variant, Quality: r.Quality,
			FileName: sanitizeFilename(title) + ext, Connections: max(conns, 1),
			Headers: r.Headers, Cookies: cookies, Size: -1, Status: StQueued,
			AddedAt: now.Add(time.Duration(i) * time.Millisecond),
		}
		if len(items) == 1 {
			d.EstSize = r.EstSize
		}
		d.YTFallback = r.Fallback
		d.Subs = r.Subs
		if kind == "subs" {
			d.EstSize = 0
		}
		if kind == "yt" && r.Clip != nil && len(items) == 1 && (r.Clip.Start > 0 || r.Clip.End > 0) {
			c := *r.Clip
			d.Clip = &c
			d.FileName = strings.TrimSuffix(d.FileName, ext) + " (" + c.label() + ")" + ext
			if d.EstSize > 0 && r.Playlist == nil {
				d.EstSize = clipEstimateDur(d.EstSize, r.Duration, c)
			}
		}
		if dir != "" {
			d.Dir, d.DirChosen = dir, true
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
		return nil, errors.New("no videos to add")
	}
	m.saveSoon()
	m.schedule()
	return ids, nil
}

// ---------- planning a download ----------

func ytHeaders(f *ytFormat, base map[string]string) map[string]string {
	h := map[string]string{}
	for k, v := range base {
		h[k] = v
	}
	for k, v := range f.HTTPHeaders {
		if strings.EqualFold(k, "Accept-Encoding") || strings.EqualFold(k, "Cookie") {
			continue
		}
		h[k] = v
	}
	return h
}

// buildYTPlan asks yt-dlp for fresh links and turns the chosen quality into chunks.
func (m *Manager) buildYTPlan(ctx context.Context, d *Download) (*MediaPlan, error) {
	d.mu.Lock()
	pageURL, variant, cookies, base, fallback := d.URL, d.Variant, d.Cookies, d.Headers, d.YTFallback
	d.mu.Unlock()
	if err := m.toolsFor(ctx, d); err != nil {
		return nil, err
	}
	info, err := m.ytInfoFor(ctx, pageURL, cookies)
	if err != nil {
		return nil, err
	}
	if info.IsLive || info.LiveStatus == "is_live" || info.LiveStatus == "is_upcoming" {
		return nil, errLive
	}
	haveFF := ffmpegPath() != ""
	video, audio, audioOnly, err := pickFormats(info, variant, haveFF)
	if err != nil {
		return nil, err
	}
	plan := &MediaPlan{Kind: "yt", Variant: variant, AudioOnly: audioOnly, Duration: info.Duration}
	d.mu.Lock()
	subLang := d.Subs
	d.mu.Unlock()
	plan.Subs = pickSubtitle(info, subLang)
	var chosen []*ytFormat
	var roles []string
	if video != nil {
		chosen, roles = append(chosen, video), append(roles, "main")
	}
	if audio != nil {
		role := "audio"
		if video == nil {
			role = "main"
		}
		chosen, roles = append(chosen, audio), append(roles, role)
	}
	headers := ytHeaders(chosen[0], base)
	d.mu.Lock()
	d.Headers = headers
	d.mu.Unlock()
	o := reqOpts{headers: headers, cookies: cookies}
	for i, f := range chosen {
		var t TrackPlan
		if !fallback {
			t, err = m.ytTrack(ctx, f, roles[i], reqOpts{headers: ytHeaders(f, base), cookies: cookies})
		}
		if fallback || errors.Is(err, errYTFallback) {
			plan.Fallback = true
			t = TrackPlan{Role: roles[i], Segs: []SegRef{{URL: f.URL}}, FormatID: f.FormatID}
			err = nil
		}
		if err != nil {
			return nil, err
		}
		t.FormatID = f.FormatID
		plan.Tracks = append(plan.Tracks, t)
	}
	_ = o
	return plan, nil
}

func (m *Manager) ytTrack(ctx context.Context, f *ytFormat, role string, o reqOpts) (TrackPlan, error) {
	t := TrackPlan{Role: role}
	switch {
	case f.Protocol == "https" || f.Protocol == "http":
		size := int64(fv(f.Filesize))
		if size <= 0 {
			s, err := m.remoteSize(ctx, f.URL, o)
			if err != nil {
				return t, errYTFallback
			}
			size = s
		}
		const piece = 4 << 20 // small pieces: YouTube slows down long requests
		for off := int64(0); off < size; off += piece {
			t.Segs = append(t.Segs, SegRef{URL: f.URL, Off: off, Len: min(piece, size-off)})
		}
	case strings.HasPrefix(f.Protocol, "m3u8"):
		body, final, _, err := m.fetchSmall(ctx, f.URL, o, 32<<20)
		if err != nil {
			return t, errYTFallback
		}
		text := string(body)
		if strings.Contains(text, "#EXT-X-STREAM-INF") {
			plan, err := m.planHLS(ctx, f.URL, final, text, "", o)
			if err != nil {
				return t, err
			}
			t = plan.Tracks[0]
			t.Role = role
			return t, nil
		}
		pl, err := parseHLSMediaPlaylist(final, text)
		if err != nil {
			return t, err
		}
		if err := checkPlayable(pl); err != nil {
			return t, err
		}
		t = pl.Track
		t.Role = role
	case f.Protocol == "http_dash_segments":
		base, _ := url.Parse(f.FragmentBaseURL)
		for _, fr := range f.Fragments {
			u := fr.URL
			if u == "" && base != nil {
				u = resolveRef(base, fr.Path)
			}
			if u == "" {
				return t, errYTFallback
			}
			t.Segs = append(t.Segs, SegRef{URL: u})
		}
	default:
		return t, errYTFallback
	}
	if len(t.Segs) == 0 {
		return t, errYTFallback
	}
	return t, nil
}

// ---------- plan B: let yt-dlp download the parts ----------

var ytProgRe = regexp.MustCompile(`TGP (\S+) (\S+) (\S+)`)

func (m *Manager) ytFallbackDownload(ctx context.Context, d *Download, plan *MediaPlan, dir string) error {
	d.mu.Lock()
	pageURL, cookies, conns := d.URL, d.Cookies, d.Connections
	d.segState = make([]byte, len(plan.Tracks))
	d.segDone, d.segBytes, d.segInflight = 0, 0, 0
	d.Status = StDownloading
	d.mu.Unlock()
	for ti, t := range plan.Tracks {
		final := filepath.Join(dir, fmt.Sprintf("t%d_%06d", ti, 0))
		if fileExists(final) {
			st, _ := os.Stat(final)
			d.mu.Lock()
			d.segState[ti] = 2
			d.segDone++
			d.segBytes += st.Size()
			d.mu.Unlock()
			continue
		}
		d.mu.Lock()
		d.segState[ti] = 1
		d.mu.Unlock()
		cf := writeCookieFile(cookies)
		args := append(m.ytdlpArgs(cf), "-f", t.FormatID, "--no-playlist", "--newline", "--progress",
			"--progress-template", "download:TGP %(progress.downloaded_bytes)s %(progress.total_bytes)s %(progress.total_bytes_estimate)s",
			"--concurrent-fragments", strconv.Itoa(max(1, conns)), "--no-mtime", "--no-part",
			"-o", filepath.Join(dir, fmt.Sprintf("yt%d.%%(ext)s", ti)), "--", pageURL)
		// --no-progress from the base args would hide our progress lines
		for i, a := range args {
			if a == "--no-progress" {
				args = append(args[:i], args[i+1:]...)
				break
			}
		}
		cmd := m.ytdlpCmd(ctx, args...)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		stdout, _ := cmd.StdoutPipe()
		if err := cmd.Start(); err != nil {
			if cf != "" {
				os.Remove(cf) // it holds cookies: never left behind
			}
			return err
		}
		var last int64
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			if mm := ytProgRe.FindStringSubmatch(sc.Text()); mm != nil {
				n, _ := strconv.ParseFloat(mm[1], 64)
				d.mu.Lock()
				d.segInflight += int64(n) - last
				d.mu.Unlock()
				last = int64(n)
			}
		}
		err := cmd.Wait()
		if cf != "" {
			os.Remove(cf)
		}
		d.mu.Lock()
		d.segInflight -= last
		d.mu.Unlock()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return ytError(stderr.String(), err)
		}
		got, _ := filepath.Glob(filepath.Join(dir, fmt.Sprintf("yt%d.*", ti)))
		if len(got) == 0 {
			return errors.New("yt-dlp finished but no file appeared")
		}
		if err := os.Rename(got[0], final); err != nil {
			return err
		}
		st, _ := os.Stat(final)
		d.mu.Lock()
		d.segState[ti] = 2
		d.segDone++
		if st != nil {
			d.segBytes += st.Size()
		}
		d.mu.Unlock()
	}
	return nil
}
