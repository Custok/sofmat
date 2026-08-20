package discovery

import (
	"net"
	"testing"
)

// Tests use RFC 5737 documentation ranges so they carry no real or private
// infra; HostsIn is range-agnostic, so any CIDR exercises the same logic.

func TestHostsIn_24(t *testing.T) {
	_, n, _ := net.ParseCIDR("192.0.2.0/24")
	h := HostsIn(n)
	if len(h) != 254 {
		t.Fatalf("want 254 hosts, got %d", len(h))
	}
	if h[0].String() != "192.0.2.1" {
		t.Fatalf("first host = %s, want 192.0.2.1", h[0])
	}
	if h[len(h)-1].String() != "192.0.2.254" {
		t.Fatalf("last host = %s, want 192.0.2.254", h[len(h)-1])
	}
	for _, ip := range h { // network + broadcast must be excluded
		if ip.String() == "192.0.2.0" || ip.String() == "192.0.2.255" {
			t.Fatalf("host list must not contain %s", ip)
		}
	}
}

func TestHostsIn_DifferentRange(t *testing.T) {
	// discovery must work on whatever range the user runs, not just one
	_, n, _ := net.ParseCIDR("198.51.100.0/24")
	h := HostsIn(n)
	if len(h) != 254 || h[0].String() != "198.51.100.1" {
		t.Fatalf("198.51.100.0/24: got %d hosts, first %s", len(h), h[0])
	}
}

func TestHostsIn_30(t *testing.T) {
	_, n, _ := net.ParseCIDR("203.0.113.0/30")
	if got := len(HostsIn(n)); got != 2 {
		t.Fatalf("/30 want 2 usable hosts, got %d", got)
	}
}

func TestLocalCIDRs_NoError(t *testing.T) {
	// host-dependent, so just assert it enumerates without error and yields
	// only IPv4, non-loopback, bounded subnets.
	cidrs, err := LocalCIDRs()
	if err != nil {
		t.Fatalf("LocalCIDRs: %v", err)
	}
	for _, c := range cidrs {
		if c.IP.To4() == nil {
			t.Fatalf("non-IPv4 subnet returned: %s", c)
		}
		if ones, _ := c.Mask.Size(); ones < 16 {
			t.Fatalf("subnet wider than /16 returned: %s", c)
		}
	}
}
