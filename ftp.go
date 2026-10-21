package main

// FTP downloads: several connections at once and resuming, like HTTP.

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/jlaffaye/ftp"
)

func isFTP(raw string) bool { return strings.HasPrefix(strings.ToLower(raw), "ftp://") }

func (m *Manager) ftpConnect(ctx context.Context, d *Download) (*ftp.ServerConn, string, error) {
	d.mu.Lock()
	raw, user, pass := d.URL, d.User, d.Password
	d.mu.Unlock()
	u, err := url.Parse(raw)
	if err != nil {
		return nil, "", err
	}
	host := u.Host
	if u.Port() == "" {
		host += ":21"
	}
	c, err := ftp.Dial(host, ftp.DialWithContext(ctx), ftp.DialWithTimeout(20*time.Second))
	if err != nil {
		return nil, "", err
	}
	if user == "" {
		user, pass = "anonymous", "anonymous@jasqet"
		if u.User != nil {
			user = u.User.Username()
			if p, ok := u.User.Password(); ok {
				pass = p
			}
		}
	}
	if err := c.Login(user, pass); err != nil {
		c.Quit()
		return nil, "", fmt.Errorf("the FTP server didn't accept the login (%v). Add your username and password in Add link → Login", err)
	}
	return c, u.Path, nil
}

func (m *Manager) probeFTP(ctx context.Context, d *Download) error {
	c, p, err := m.ftpConnect(ctx, d)
	if err != nil {
		return err
	}
	defer c.Quit()
	size, err := c.FileSize(p)
	if err != nil {
		if strings.Contains(err.Error(), "550") {
			return errors.New("file not found on the FTP server (550)")
		}
		size = -1 // some servers don't say the size
	}
	resumable := false
	if size > 1 {
		if r, err := c.RetrFrom(p, uint64(size/2)); err == nil {
			r.Close()
			resumable = true
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.Size, d.Resumable = size, resumable
	if d.FileName == "" {
		d.FileName = path.Base(p)
	}
	d.FileName = sanitizeFilename(d.FileName)
	return nil
}

func (m *Manager) fetchFTP(ctx context.Context, d *Download, seg *Segment, f *os.File) (bool, error) {
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stall := time.AfterFunc(stallTimeout, cancel)
	defer stall.Stop()

	c, p, err := m.ftpConnect(reqCtx, d)
	if err != nil {
		return false, err
	}
	d.mu.Lock()
	pos, resumable := seg.Pos, d.Resumable
	if !resumable && pos != 0 {
		seg.Pos, pos = 0, 0
	}
	d.mu.Unlock()
	r, err := c.RetrFrom(p, uint64(pos))
	if err != nil {
		c.Quit()
		return false, err
	}
	stop := context.AfterFunc(reqCtx, func() { r.Close(); c.Quit() })
	defer func() {
		if stop() {
			r.Close() // we stopped reading early: close the data connection
			c.Quit()
		}
	}()
	return m.pump(ctx, reqCtx, d, seg, f, r, stall, -1, false)
}
