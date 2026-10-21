package main

// Which account a local connection comes from, read from Linux's socket
// tables (/proc/net/tcp and tcp6). Only peer_linux.go uses it; it's here so
// the tests can check it on every system.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
)

// procAddr writes an address the way /proc/net/tcp does: the IP as 32-bit
// words in this computer's byte order, then the port, in hex.
func procAddr(ap netip.AddrPort, v6 bool) string {
	var ip []byte
	if v6 {
		b := ap.Addr().As16() // an IPv4 address as ::ffff:a.b.c.d
		ip = b[:]
	} else {
		if !ap.Addr().Unmap().Is4() {
			return ""
		}
		b := ap.Addr().Unmap().As4()
		ip = b[:]
	}
	var sb strings.Builder
	for i := 0; i < len(ip); i += 4 {
		fmt.Fprintf(&sb, "%08X", binary.NativeEndian.Uint32(ip[i:i+4]))
	}
	fmt.Fprintf(&sb, ":%04X", ap.Port())
	return sb.String()
}

// socketOwner: the account (uid) that owns the end at local of a TCP
// connection to remote, from the text of /proc/net/tcp or tcp6. ok is false
// if the connection isn't listed.
func socketOwner(table []byte, local, remote netip.AddrPort, v6 bool) (uid int, ok bool) {
	l, r := procAddr(local, v6), procAddr(remote, v6)
	if l == "" || r == "" {
		return 0, false
	}
	for _, line := range bytes.Split(table, []byte("\n")) {
		f := strings.Fields(string(line))
		// sl local_address rem_address st tx:rx tr:when retrnsmt uid ...
		if len(f) < 8 || !strings.EqualFold(f[1], l) || !strings.EqualFold(f[2], r) {
			continue
		}
		if n, err := strconv.Atoi(f[7]); err == nil {
			return n, true
		}
	}
	return 0, false
}
