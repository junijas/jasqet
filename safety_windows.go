//go:build windows

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

func mpCmdRun() string {
	var cands []string
	if pd := os.Getenv("ProgramData"); pd != "" {
		found, _ := filepath.Glob(filepath.Join(pd, `Microsoft\Windows Defender\Platform\*\MpCmdRun.exe`))
		sort.Sort(sort.Reverse(sort.StringSlice(found))) // newest platform first
		cands = append(cands, found...)
	}
	if pf := os.Getenv("ProgramFiles"); pf != "" {
		cands = append(cands, filepath.Join(pf, `Windows Defender\MpCmdRun.exe`))
	}
	for _, c := range cands {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

var threatRe = regexp.MustCompile(`(?m)Threat\s*:\s*(.+)$`)

// defenderScan asks Windows Defender to scan the file (it doesn't delete
// anything itself; Jasqet just reports what it says).
func defenderScan(ctx context.Context, p string) (string, bool) {
	exe := mpCmdRun()
	if exe == "" {
		return "", false
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-Scan", "-ScanType", "3", "-File", p, "-DisableRemediation")
	hideWindow(cmd)
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		return "", false
	}
	switch code {
	case 0:
		return "Windows Defender: no threats found", false
	case 2:
		if m := threatRe.FindSubmatch(out); m != nil {
			return strings.TrimSpace(string(m[1])), true
		}
		return "a threat", true
	}
	return "", false
}

// markFromInternet adds the same "downloaded from the internet" tag browsers
// add, so Windows SmartScreen and Office's Protected View look at the file
// before it's opened.
func markFromInternet(p string, d *Download) {
	d.mu.Lock()
	host, ref := withoutLogin(d.URL), withoutLogin(d.Page)
	d.mu.Unlock()
	if len(host) > 1500 || strings.HasPrefix(host, "magnet:") {
		host = ""
	}
	if len(ref) > 1500 {
		ref = ""
	}
	tag := "[ZoneTransfer]\r\nZoneId=3\r\n"
	if ref != "" {
		tag += "ReferrerUrl=" + ref + "\r\n"
	}
	if host != "" {
		tag += "HostUrl=" + host + "\r\n"
	}
	st, err := os.Stat(p)
	if err != nil {
		return
	}
	if !st.IsDir() {
		os.WriteFile(p+":Zone.Identifier", []byte(tag), 0o644)
		return
	}
	filepath.Walk(p, func(f string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() { // every file, as browsers do: a .docx or .pdf gets Protected View too
			os.WriteFile(f+":Zone.Identifier", []byte(tag), 0o644)
		}
		return nil
	})
}

// keepInternetTag gives a copy the same tag as the original (a copy to
// another drive leaves it behind).
func keepInternetTag(src, dst string) {
	if tag, err := os.ReadFile(src + ":Zone.Identifier"); err == nil {
		os.WriteFile(dst+":Zone.Identifier", tag, 0o644)
	}
}
