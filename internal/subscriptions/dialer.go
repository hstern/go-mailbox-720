package subscriptions

import (
	"fmt"
	"net"
	"net/http"
	"syscall"
	"time"
)

// GuardedClient returns an *http.Client hardened against SSRF for the
// notificationUrl handshake. Its Transport is a clone of http.DefaultTransport
// whose dialer carries a Control hook that inspects every IP the connection is
// about to dial — after DNS resolution and after any redirect target is
// re-dialed — and aborts the dial when the IP is internal (see [blockedIP]).
//
// Checking at dial time on the resolved IP, rather than parsing the hostname, is
// what closes the DNS-rebinding hole: a name that resolves to a public address
// at validation time but to 169.254.169.254 (or 127.0.0.1, 10.x, ...) at dial
// time is still rejected, because the Control hook sees the concrete IP the
// kernel is about to connect to.
//
// GuardedClient only hardens the transport/dialer; it does not set
// CheckRedirect. Production callers should pass this client to
// [VerifyNotificationURL], which copies it and refuses redirects on the copy —
// so the redirect-refusal posture is preserved while every dialed IP (including
// the original target) is still IP-filtered by the Control hook.
func GuardedClient() *http.Client {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		// http.DefaultTransport is always an *http.Transport in the stdlib; guard
		// defensively so a future change can't silently drop the IP filter.
		transport = &http.Transport{}
	}
	cloned := transport.Clone()

	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		// Control runs once per dialed address, after name resolution, with the
		// concrete network/IP the socket is about to connect to. Returning an error
		// here aborts the dial before any packet leaves the host, which is exactly
		// what we want for an SSRF guard: redirects and DNS-rebinding both funnel
		// through a fresh dial, so each resolved IP is re-checked.
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return fmt.Errorf("subscriptions: parse dial address %q: %w", address, err)
			}
			ip := net.ParseIP(host)
			if ip == nil {
				return fmt.Errorf("subscriptions: dial address %q is not an IP", host)
			}
			if blockedIP(ip) {
				return fmt.Errorf("subscriptions: refusing to dial internal address %s", ip)
			}
			return nil
		},
	}
	cloned.DialContext = dialer.DialContext

	return &http.Client{Transport: cloned}
}

// metadataIPv4 is the AWS/GCP/Azure link-local instance-metadata endpoint; it
// falls inside 169.254.0.0/16 (link-local) but is called out explicitly so the
// intent is unmistakable and the block survives any future narrowing of the
// link-local check.
var metadataIPv4 = net.IPv4(169, 254, 169, 254)

// metadataIPv6 is the IMDS endpoint reachable over IPv6 on EC2 (fd00:ec2::254).
// It is a ULA (fc00::/7) and so already covered by IsPrivate, but is named
// explicitly for the same defence-in-depth reason as metadataIPv4.
var metadataIPv6 = net.ParseIP("fd00:ec2::254")

// blockedIP reports whether ip names a destination that the notificationUrl
// handshake must never reach. It is the SSRF classification core: client-
// supplied URLs (and anything they redirect or rebind to) must not be able to
// reach the host's own loopback, private RFC1918 / ULA ranges, link-local
// space, the unspecified address, multicast, or the cloud instance-metadata
// service. Everything else (public unicast) is allowed.
func blockedIP(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.Equal(metadataIPv4) || ip.Equal(metadataIPv6) {
		return true
	}
	return ip.IsLoopback() || // 127.0.0.0/8, ::1
		ip.IsPrivate() || // RFC1918 (10/8, 172.16/12, 192.168/16) + ULA fc00::/7
		ip.IsLinkLocalUnicast() || // 169.254.0.0/16, fe80::/10
		ip.IsLinkLocalMulticast() || // 224.0.0.0/24, ff02::/16
		ip.IsUnspecified() || // 0.0.0.0, ::
		ip.IsMulticast() // 224.0.0.0/4, ff00::/8
}
