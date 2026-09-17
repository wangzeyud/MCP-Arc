// Package netutil holds small helpers for picking listen addresses that do not
// collide with whatever else is already running on the user's machine.
//
// Rationale: 8080/8081 are extremely popular ports (dev servers, other tools),
// so a desktop user very often has one of them taken. Instead of failing to
// start, MCP Arc scans upward for a free port and reports the address it
// actually bound, which is what the user must paste into their MCP client.
package netutil

import (
	"fmt"
	"net"
	"strconv"
	"time"
)

// maxPortProbes bounds how far we scan upward from the preferred port.
const maxPortProbes = 100

// dialTimeout bounds the "is something already listening here?" probe.
const dialTimeout = 250 * time.Millisecond

// FreeTCPPort returns the first TCP port >= preferred that is free to bind on
// host (host may be "" for all interfaces). changed reports whether the port had
// to be moved from preferred.
//
// It works by binding then immediately closing, which is inherently racy against
// another process grabbing the port in between — acceptable for a desktop helper
// where the server binds moments later.
func FreeTCPPort(host string, preferred int) (port int, changed bool) {
	for i := range maxPortProbes {
		p := preferred + i
		if p > 65535 {
			break
		}
		if portFree(host, p) {
			return p, i != 0
		}
	}
	// Nothing free in range: return the preferred port and let the real bind
	// produce the (clearer) error.
	return preferred, false
}

// portFree reports whether port can be claimed, checking BOTH that the address
// can be bound and that nothing already answers there.
//
// The connect check matters on Windows: a bind can succeed even while another
// process (one that set SO_REUSEADDR, e.g. Node.js) already owns the port, which
// would otherwise leave two servers fighting over the same port. A successful
// TCP connect means something is genuinely listening, so we treat it as taken.
func portFree(host string, port int) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false
	}
	_ = ln.Close()

	dialHost := host
	switch dialHost {
	case "", "0.0.0.0", "::", "[::]":
		dialHost = "127.0.0.1"
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(dialHost, strconv.Itoa(port)), dialTimeout)
	if err == nil {
		_ = conn.Close()
		return false
	}
	return true
}

// SplitListen splits a listen address like ":8081" or "127.0.0.1:8081" into its
// host and port parts.
func SplitListen(addr string) (host string, port int, err error) {
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		return "", 0, err
	}
	port, err = strconv.Atoi(p)
	if err != nil {
		return "", 0, fmt.Errorf("invalid port in %q: %w", addr, err)
	}
	return h, port, nil
}

// FreeListenAddr resolves a listen address to one whose port is free, scanning
// upward if the preferred port is taken. changed reports whether the port moved.
func FreeListenAddr(addr string) (resolved string, changed bool) {
	host, port, err := SplitListen(addr)
	if err != nil {
		return addr, false
	}
	free, moved := FreeTCPPort(host, port)
	if !moved {
		return addr, false
	}
	return net.JoinHostPort(host, strconv.Itoa(free)), true
}

// URLForListen builds a client-facing http URL (e.g. http://localhost:8081/sse)
// from a listen address plus a path. Wildcard hosts are rewritten to localhost
// because that is what the user actually types into their MCP client.
func URLForListen(addr, path string) string {
	host, port, err := SplitListen(addr)
	if err != nil {
		return ""
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "localhost"
	}
	return fmt.Sprintf("http://%s:%d%s", host, port, path)
}
