package subscriptions

import (
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestBlockedIP(t *testing.T) {
	tests := []struct {
		name string
		ip   string
		want bool
	}{
		// Blocked: loopback.
		{name: "ipv4 loopback", ip: "127.0.0.1", want: true},
		{name: "ipv6 loopback", ip: "::1", want: true},
		// Blocked: private RFC1918.
		{name: "private 10/8", ip: "10.1.2.3", want: true},
		{name: "private 192.168/16", ip: "192.168.0.1", want: true},
		{name: "private 172.16/12", ip: "172.16.0.1", want: true},
		// Blocked: cloud metadata (also link-local / ULA, called out explicitly).
		{name: "metadata ipv4", ip: "169.254.169.254", want: true},
		{name: "metadata ipv6", ip: "fd00:ec2::254", want: true},
		// Blocked: link-local.
		{name: "link-local unicast", ip: "169.254.10.20", want: true},
		{name: "link-local ipv6", ip: "fe80::1", want: true},
		// Blocked: ULA fc00::/7 (fc and fd halves).
		{name: "ula fc00", ip: "fc00::1", want: true},
		{name: "ula fd00", ip: "fd12:3456::1", want: true},
		// Blocked: unspecified.
		{name: "unspecified ipv4", ip: "0.0.0.0", want: true},
		{name: "unspecified ipv6", ip: "::", want: true},
		// Blocked: multicast.
		{name: "multicast ipv4", ip: "239.1.2.3", want: true},
		{name: "multicast ipv6", ip: "ff02::1", want: true},
		// Allowed: public unicast.
		{name: "public 8.8.8.8", ip: "8.8.8.8", want: false},
		{name: "public 1.1.1.1", ip: "1.1.1.1", want: false},
		{name: "public ipv6", ip: "2606:4700:4700::1111", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("ParseIP(%q) returned nil", tc.ip)
			}
			if got := blockedIP(ip); got != tc.want {
				t.Errorf("blockedIP(%s) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}
}

func TestBlockedIPNil(t *testing.T) {
	if !blockedIP(nil) {
		t.Error("blockedIP(nil) = false, want true (unparseable IPs must be blocked)")
	}
}

// TestGuardedClientRejectsLoopback proves the Control hook fires before the
// connection is made: a GET to a loopback address must fail at dial time with the
// guard's "refusing to dial internal address" error, not with an ordinary
// connection-refused. The port need not be open — the dial is aborted first.
func TestGuardedClientRejectsLoopback(t *testing.T) {
	client := GuardedClient()
	// Bind a listener only to obtain a real, currently-unused loopback port; we
	// close it immediately so a connection would otherwise be refused — proving the
	// failure below comes from the guard, not from the OS rejecting the connect.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	resp, err := client.Get("http://" + addr)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("GuardedClient().Get(loopback) = nil error, want dial-time block")
	}
	if !strings.Contains(err.Error(), "refusing to dial internal address") {
		t.Fatalf("error = %v, want a dial-time block from the Control hook", err)
	}
}

// TestGuardedClientUsesDefaultTransportClone is a light guard that GuardedClient
// produces a usable client with an *http.Transport that is not the shared
// default (so mutating its DialContext cannot affect other callers).
func TestGuardedClientUsesDefaultTransportClone(t *testing.T) {
	client := GuardedClient()
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", client.Transport)
	}
	if tr == http.DefaultTransport {
		t.Error("GuardedClient reused http.DefaultTransport; want a clone")
	}
	if tr.DialContext == nil {
		t.Error("GuardedClient transport has nil DialContext; the guard would not fire")
	}
}
