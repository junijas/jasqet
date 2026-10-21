//go:build linux

package main

import (
	"net"
	"net/http"
	"net/netip"
	"os"
)

// otherUser: the request came from another account on this computer. When
// that can't be told (the socket isn't listed), it says no: the code it
// guards still only works once.
func otherUser(r *http.Request) bool {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	la, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if la == nil {
		return false
	}
	me, err := netip.ParseAddrPort(la.String())
	if err != nil {
		return false
	}
	// the caller's own end of the connection: from peer to me
	for _, t := range []struct {
		file string
		v6   bool
	}{{"/proc/net/tcp", false}, {"/proc/net/tcp6", true}} {
		b, err := os.ReadFile(t.file)
		if err != nil {
			continue
		}
		if uid, ok := socketOwner(b, peer, me, t.v6); ok {
			return uid != os.Getuid()
		}
	}
	return false
}
