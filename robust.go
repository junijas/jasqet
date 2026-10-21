package main

// Downloads that keep going by themselves: a download that stops on a network
// problem (no internet for a while, the server down for maintenance, a dropped
// Wi-Fi) tries again on its own with growing pauses instead of sitting in the
// list as an error until someone notices. That matters most for downloads
// left running overnight. Also: an early "not enough disk space" check, and
// progress that is only saved once the data it counts is safely on the disk.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	maxAutoRetries = 30               // tries without any progress before giving up
	diskSpareBytes = 32 * 1024 * 1024 // keep this much free on the drive
)

// autoRetryWaits: how long to wait before each try (the last one repeats).
// About 2.5 hours of trying in all, longer while it keeps making progress.
var autoRetryWaits = []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}

// ftpBusyRe: FTP replies that mean "not now" (421 too many connections / closing,
// 425 / 426 data connection failed, 450 file busy).
var ftpBusyRe = regexp.MustCompile(`(^|[^0-9])(421|425|426|450)[ -]`)

// withoutResumeHint drops "press Resume" from an error shown while the download
// is about to try again by itself.
func withoutResumeHint(msg string) string {
	for _, cut := range []string{". Press Resume", " and press Resume", ", then press Resume"} {
		if i := strings.Index(msg, cut); i > 0 {
			return msg[:i]
		}
	}
	return msg
}

type noSpaceError struct{ need, free int64 }

func (e *noSpaceError) Error() string {
	return fmt.Sprintf("Not enough free space on the drive: this download needs %s more, but only %s is free. Free some space, then press Try again", humanSize(e.need), humanSize(max(0, e.free)))
}

// transientErr: is this the kind of problem that goes away by itself
// (no internet, a timeout, a busy or restarting server)?
func transientErr(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var ns *noSpaceError
	switch {
	case errors.As(err, &ns), errors.Is(err, errDuplicate), errors.Is(err, errTorrentRisky),
		errors.Is(err, errFileChanged), errors.Is(err, errServerIgnoredRange), errors.Is(err, errDRM),
		isDiskFull(err):
		return false
	}
	var se *statusError
	if errors.As(err, &se) {
		// 501 / 505: the server can't do this at all, it isn't busy
		return se.Code == 408 || se.Code == 429 || (se.Code >= 500 && se.Code != 501 && se.Code != 505)
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "certificate") || strings.Contains(msg, "x509") || strings.Contains(msg, "remote error: tls") {
		return false // a bad certificate or a refused secure connection doesn't fix itself
	}
	if ftpBusyRe.MatchString(msg) {
		return true // FTP: too many connections, or the data connection dropped
	}
	var oe *net.OpError
	var de *net.DNSError
	if errors.As(err, &oe) || errors.As(err, &de) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		return true
	}
	for _, k := range []string{
		"stalled", "closed early", "connection reset", "connection refused", "forcibly closed",
		"broken pipe", "no such host", "unreachable", "network is down", "no route to host",
		"i/o timeout", "timeout", "timed out", "temporary failure", "getaddrinfo", "name resolution",
		"server replied 5", "server replied 429", "server replied 408", "eof", "wsarecv", "wsasend",
		"server closed idle connection",
	} {
		if strings.Contains(msg, k) {
			return true
		}
	}
	return false
}

// autoRetryLocked decides whether a download that stopped on an error tries
// again by itself, and how long it waits first. Caller holds d.mu.
func (d *Download) autoRetryLocked(err error) (time.Duration, bool) {
	if d.Kind == "torrent" || !transientErr(err) {
		d.autoRetries = 0
		return 0, false
	}
	if !d.Resumable && !d.isStream() {
		// this server can't resume: every try starts again from the first
		// byte, so a few tries only (a flaky link could otherwise spend the
		// whole file's data again and again)
		if d.autoRetries >= 3 {
			return 0, false
		}
		d.autoRetries++
		return autoRetryWaits[min(d.autoRetries, len(autoRetryWaits))-1], true
	}
	if d.downloadedLocked() > d.startBytes {
		d.autoRetries = 0 // it was getting somewhere: start the waits from the beginning
	}
	if d.autoRetries >= maxAutoRetries {
		return 0, false
	}
	d.autoRetries++
	return autoRetryWaits[min(d.autoRetries, len(autoRetryWaits))-1], true
}

// renamePatiently renames a finished file. On Windows an antivirus or search
// indexer often opens a new file for a moment, and renaming it then fails
// with "used by another process": wait a little and try again rather than
// calling the whole download failed.
func renamePatiently(from, to string) error {
	var err error
	for i := 0; i < 10; i++ {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		if _, serr := os.Stat(from); serr != nil {
			return err // nothing to rename
		}
		time.Sleep(time.Duration(200*(i+1)) * time.Millisecond)
	}
	return err
}

// retriesDue: is a download waiting to try again whose time has come?
func (m *Manager) retriesDue(now time.Time) bool {
	m.mu.Lock()
	list := m.sortedLocked()
	m.mu.Unlock()
	for _, d := range list {
		d.mu.Lock()
		due := !d.running && d.Status == StQueued && !d.retryAt.IsZero() && !now.Before(d.retryAt)
		d.mu.Unlock()
		if due {
			return true
		}
	}
	return false
}

// retryNow: the network just changed (or came back), so downloads waiting to
// try again do so at once.
func (m *Manager) retryNow() {
	m.mu.Lock()
	list := m.sortedLocked()
	m.mu.Unlock()
	now := time.Now()
	for _, d := range list {
		d.mu.Lock()
		if !d.running && d.Status == StQueued && !d.retryAt.IsZero() {
			d.retryAt = now
		}
		d.mu.Unlock()
	}
}
