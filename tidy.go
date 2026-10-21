package main

// Tidy names for videos from release sites: "[YTS.MX] Some.Movie.2021.1080p.
// WEBRip.x264.mp4" is saved as "Some Movie (2021).mp4", and
// "The.Office.US.S02E05.720p.WEB-DL.mkv" as "The Office US - S02E05.mkv" in a
// folder for the series. Names that don't look like a release are left alone.

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

var (
	tidyBrackets = regexp.MustCompile(`\[[^\]]*\]|\{[^}]*\}`)
	tidyParens   = regexp.MustCompile(`\(([^)]*)\)`)
	// tags with a dot inside ("H.264", "DDP5.1"), before dots become spaces
	tidyDotted  = regexp.MustCompile(`(?i)\b(h\.26[45]|(?:ddp|dd|aac|ac3|eac3|dts)[ .]?[257]\.[01]|[257]\.[01])\b`)
	tidyEpisode = regexp.MustCompile(`(?i)^s(\d{1,2})e(\d{1,3})(?:-?e\d{1,3})?$`)
	tidyYear    = regexp.MustCompile(`^(19[2-9]\d|20[0-4]\d)$`)
	tidyRes     = regexp.MustCompile(`(?i)^(\d{3,4}p|[248]k|uhd)$`)
)

const tidyMark = "\x00tag" // stands in for a tag removed whole

// Tags that only a release would have: one of these makes a name a release.
var tidyStrong = map[string]bool{
	"x264": true, "x265": true, "h264": true, "h265": true, "hevc": true, "avc": true, "xvid": true, "divx": true, "av1": true, "10bit": true,
	"webrip": true, "web-dl": true, "webdl": true, "web-rip": true, "bluray": true, "blu-ray": true, "brrip": true, "bdrip": true,
	"hdrip": true, "dvdrip": true, "dvdscr": true, "hdtv": true, "hdcam": true, "yts": true, "yify": true, "rarbg": true, "eztv": true,
	tidyMark: true,
}

// Tags that end a title once a name is known to be a release.
var tidyWeak = map[string]bool{
	"web": true, "cam": true, "ts": true, "proper": true, "repack": true, "extended": true, "unrated": true, "remastered": true,
	"imax": true, "hdr": true, "hdr10": true, "dv": true, "sdr": true, "8bit": true, "amzn": true, "nf": true, "dsnp": true,
	"hmax": true, "atvp": true, "aac": true, "ac3": true, "dts": true, "ddp": true, "dd": true, "eac3": true, "atmos": true,
	"truehd": true, "mp3": true, "6ch": true, "2ch": true, "dual": true, "multi": true, "esub": true, "esubs": true, "subs": true,
	"remux": true, "internal": true, "limited": true,
}

func tidyTag(t string, strong bool) bool {
	l := strings.ToLower(t)
	if tidyStrong[l] || tidyRes.MatchString(l) {
		return true
	}
	if i := strings.IndexByte(l, '-'); i > 0 && (tidyStrong[l[:i]] || tidyRes.MatchString(l[:i])) {
		return true // "x264-GROUP"
	}
	return !strong && tidyWeak[l]
}

// tidyVideoName gives the tidy name for a video file, and the series it
// belongs to (or ""). Other files, and names that aren't releases, come back
// as they are.
func tidyVideoName(name string) (string, string) {
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	if categoryOf(name) != "Video" || base == "" {
		return name, ""
	}
	dotted := strings.Count(base, ".")+strings.Count(base, "_") >= 2
	s := tidyBrackets.ReplaceAllString(base, " "+tidyMark+" ")
	s = tidyParens.ReplaceAllStringFunc(s, func(p string) string {
		if in := strings.TrimSpace(p[1 : len(p)-1]); tidyYear.MatchString(in) {
			return " " + in + " " // "(2021)": keep the year
		}
		return " "
	})
	s = tidyDotted.ReplaceAllString(s, " "+tidyMark+" ")
	var toks []string
	for _, t := range strings.Fields(strings.NewReplacer(".", " ", "_", " ").Replace(s)) {
		if strings.Trim(t, "-–—") != "" {
			toks = append(toks, t)
		}
	}
	// a group tag in brackets at the very start is just a site's name
	for len(toks) > 0 && toks[0] == tidyMark {
		toks = toks[1:]
	}
	release, ep := false, -1
	for i, t := range toks {
		if tidyTag(t, true) {
			release = true
		}
		if ep < 0 && tidyEpisode.MatchString(t) {
			ep = i
		}
	}
	end := len(toks) // where the technical part starts
	for i, t := range toks {
		if i > 0 && tidyTag(t, false) {
			end = i
			break
		}
	}
	head := toks[:end]
	named := false // something in front that isn't a tag
	for _, t := range head {
		named = named || !tidyTag(t, false)
	}
	if !named {
		return name, ""
	}
	yearLast := len(head) > 1 && tidyYear.MatchString(head[len(head)-1])
	if !release && ep < 0 && !(dotted && yearLast) {
		return name, ""
	}
	if ep >= 0 && ep < end {
		show := tidyWords(toks[:ep])
		if show == "" {
			return name, ""
		}
		m := tidyEpisode.FindStringSubmatch(toks[ep])
		sn, _ := strconv.Atoi(m[1])
		en, _ := strconv.Atoi(m[2])
		out := fmt.Sprintf("%s - S%02dE%02d", show, sn, en)
		if title := tidyWords(toks[ep+1 : end]); title != "" {
			out += " - " + title
		}
		return sanitizeFilename(out + ext), sanitizeFilename(show)
	}
	title := tidyWords(head)
	if yearLast {
		title = tidyWords(head[:len(head)-1]) + " (" + head[len(head)-1] + ")"
	}
	return sanitizeFilename(title + ext), ""
}

// tidyWords joins a title's words; one written all in small letters gets capitals.
func tidyWords(ws []string) string {
	s := strings.TrimSpace(strings.Join(ws, " "))
	if s != strings.ToLower(s) {
		return s
	}
	r := []rune(s)
	for i := range r {
		if i == 0 || r[i-1] == ' ' {
			r[i] = unicode.ToUpper(r[i])
		}
	}
	return string(r)
}
