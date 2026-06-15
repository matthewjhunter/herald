package feeds

import (
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// dialControl is the net.Dialer Control hook used by newSafeClient.
// Production code uses safeControl, which rejects non-public addresses.
// Tests that need to reach loopback (httptest.NewServer binds 127.0.0.1)
// swap this to a permissive no-op via usePermissiveDialForTesting.
var dialControl = safeControl

// isDisallowedIP reports whether an IP must not be dialed: loopback,
// link-local (incl. 169.254.0.0/16 and fe80::/10), private (RFC1918 /
// fc00::/7 ULA), unspecified, and multicast. Public unicast addresses pass.
func isDisallowedIP(ip net.IP) bool {
	return ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsPrivate() ||
		ip.IsUnspecified() || ip.IsMulticast()
}

// safeControl is a net.Dialer Control hook: it runs after DNS resolution,
// once per resolved address, with the concrete IP:port about to be dialed.
// Returning an error aborts that dial.
func safeControl(network, address string, c syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("safedial: bad address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("safedial: non-IP dial target %q", host)
	}
	if isDisallowedIP(ip) {
		return fmt.Errorf("safedial: refusing to dial non-public address %s", ip)
	}
	return nil
}

// permissiveControl is a no-op dial control used only in tests. It allows
// loopback and private addresses so httptest.NewServer targets work.
func permissiveControl(network, address string, c syscall.RawConn) error {
	return nil
}

// UsePermissiveDialForTesting swaps dialControl to a no-op that allows any
// address. Call it in TestMain; the returned function restores the original.
// Production code always runs with safeControl (the module-level default).
// This function is intended for use in tests only.
func UsePermissiveDialForTesting() (restore func()) {
	prev := dialControl
	dialControl = permissiveControl
	return func() { dialControl = prev }
}

// newSafeClient returns an http.Client that refuses non-public destinations
// (checked at dial time, so redirects and DNS rebinding are covered), allows
// only http/https, and applies the given overall timeout.
//
// The dial control function is read from the dialControl package variable at
// each dial, not captured at construction time, so TestMain can swap the
// variable once before any tests run and all clients created during that run
// pick up the permissive control.
func newSafeClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second,
		Control: func(network, address string, c syscall.RawConn) error {
			return dialControl(network, address, c)
		},
	}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("safedial: stopped after 10 redirects")
			}
			if s := req.URL.Scheme; s != "http" && s != "https" {
				return fmt.Errorf("safedial: refusing redirect to scheme %q", s)
			}
			return nil
		},
	}
}

// AllowedFetchScheme reports whether a URL scheme may be fetched.
func AllowedFetchScheme(scheme string) bool {
	return scheme == "http" || scheme == "https"
}
