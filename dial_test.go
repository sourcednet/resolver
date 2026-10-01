package resolver

import (
	"net"
	"net/http"
	"testing"
)

func TestPublicIP(t *testing.T) {
	for addr, want := range map[string]bool{
		"93.184.216.34":        true,
		"2606:4700::6810:85e5": true,
		"64:ff9b::5db8:d822":   true,  // NAT64 for 93.184.216.34
		"127.0.0.1":            false, // loopback
		"10.1.2.3":             false, // private
		"169.254.169.254":      false, // link-local: cloud metadata
		"100.64.1.1":           false, // carrier-grade NAT
		"0.1.2.3":              false, // "this network"
		"198.18.0.1":           false, // benchmarking
		"240.0.0.1":            false, // reserved
		"::ffff:10.0.0.1":      false, // IPv4-mapped private
		"64:ff9b::a00:1":       false, // NAT64 for 10.0.0.1
		"fd00::1":              false, // unique local
	} {
		if got := publicIP(net.ParseIP(addr)); got != want {
			t.Errorf("publicIP(%s) = %v, want %v", addr, got, want)
		}
	}
}

func TestPublicOnlyIgnoresProxies(t *testing.T) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if err := PublicOnly(&http.Client{Transport: tr}); err != nil {
		t.Fatal(err)
	}
	if tr.Proxy != nil {
		t.Fatal("a proxy would bypass the address check")
	}
}
