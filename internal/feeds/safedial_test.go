package feeds

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIsDisallowedIP(t *testing.T) {
	cases := []struct {
		ip         string
		disallowed bool
	}{
		// Loopback
		{"127.0.0.1", true},
		{"::1", true},
		// Link-local (cloud metadata endpoint)
		{"169.254.169.254", true},
		// Private RFC1918
		{"10.0.0.1", true},
		{"192.168.1.1", true},
		{"172.16.0.1", true},
		// ULA (fc00::/7)
		{"fc00::1", true},
		// Unspecified
		{"0.0.0.0", true},
		// Public -- must NOT be disallowed
		{"8.8.8.8", false},
		{"1.1.1.1", false},
		{"2606:4700:4700::1111", false}, // Cloudflare public IPv6
	}

	for _, tc := range cases {
		ip := net.ParseIP(tc.ip)
		if ip == nil {
			t.Fatalf("failed to parse test IP %q", tc.ip)
		}
		got := isDisallowedIP(ip)
		if got != tc.disallowed {
			t.Errorf("isDisallowedIP(%s) = %v, want %v", tc.ip, got, tc.disallowed)
		}
	}
}

func TestAllowedFetchScheme(t *testing.T) {
	cases := []struct {
		scheme  string
		allowed bool
	}{
		{"http", true},
		{"https", true},
		{"file", false},
		{"ftp", false},
		{"gopher", false},
		{"", false},
	}

	for _, tc := range cases {
		got := AllowedFetchScheme(tc.scheme)
		if got != tc.allowed {
			t.Errorf("AllowedFetchScheme(%q) = %v, want %v", tc.scheme, got, tc.allowed)
		}
	}
}

// TestSafeClient_RefusesLoopback verifies that a client built with the strict
// safeControl hook refuses to dial a loopback address. httptest.NewServer binds
// to 127.0.0.1, which is exactly what must be blocked in production.
//
// NOTE: TestMain (fetcher_test.go) installs permissiveControl for the whole
// test binary. This test temporarily restores safeControl so it can assert the
// guard works.
func TestSafeClient_RefusesLoopback(t *testing.T) {
	// Restore strict dial control for this specific test.
	// TestMain sets dialControl = permissiveControl for the whole binary.
	// This test needs safeControl to verify that the guard fires, so temporarily
	// restore strictness and put the permissive control back when done.
	saved := dialControl
	dialControl = safeControl
	defer func() { dialControl = saved }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer srv.Close()

	client := newSafeClient(5e9) // 5 s
	_, err := client.Get(srv.URL)
	if err == nil {
		t.Fatal("expected error dialing loopback, got nil")
	}
	if !strings.Contains(err.Error(), "non-public") {
		t.Errorf("expected 'non-public' in error, got: %v", err)
	}
}
