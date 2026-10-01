package resolver

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"syscall"
)

// ErrPrivateAddress means a publisher's domain resolved to an address a
// public resolver must not connect to.
var ErrPrivateAddress = errors.New("refusing to connect to a private, loopback, or link-local address")

// PublicOnly makes client refuse connections to private, loopback,
// link-local, and unspecified addresses. A resolver fetches whatever URL it
// is asked about, so without this anyone could make it probe internal
// services (SSRF). Local test networks turn it off.
func PublicOnly(client *http.Client) error {
	tr, ok := client.Transport.(*http.Transport)
	if !ok {
		return fmt.Errorf("PublicOnly needs an *http.Transport, got %T", client.Transport)
	}
	d := &net.Dialer{Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || !publicIP(ip) {
			return fmt.Errorf("%w: %s", ErrPrivateAddress, host)
		}
		return nil
	}}
	tr.DialContext = d.DialContext
	// A proxy would make the check above see only the proxy's address,
	// while the proxy reaches the internal host for us.
	tr.Proxy = nil
	return nil
}

// specialRanges are addresses that aren't private by Go's definition but
// still aren't the public internet: carrier-grade NAT, "this network",
// IETF protocol assignments, benchmarking, and reserved.
var specialRanges = parseCIDRs("100.64.0.0/10", "0.0.0.0/8", "192.0.0.0/24", "198.18.0.0/15", "240.0.0.0/4")

// nat64 is the well-known NAT64 prefix: its addresses carry an IPv4
// address in their last 32 bits, which must be public too.
var nat64 = parseCIDRs("64:ff9b::/96")[0]

func parseCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, len(cidrs))
	for i, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic(err)
		}
		out[i] = n
	}
	return out
}

func publicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	for _, n := range specialRanges {
		if n.Contains(ip) {
			return false
		}
	}
	if nat64.Contains(ip) {
		return publicIP(ip[len(ip)-4:])
	}
	return true
}
