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
	return nil
}

func publicIP(ip net.IP) bool {
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() || ip.IsInterfaceLocalMulticast())
}
