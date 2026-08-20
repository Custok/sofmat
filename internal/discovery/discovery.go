// Package discovery finds soflink nodes on the local network without any
// configuration: it derives the host's own IPv4 subnet(s) from its interfaces
// and sweeps them for the well-known soflink port, so it works on whatever
// RFC 1918 range the user happens to run. A responder is only treated
// as a peer if it answers the soflink hello — a random service on the same port
// is ignored, and trust is still established later by the authenticated transport.
package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// DefaultPort is the well-known port a soflink node serves /soflink/hello on.
const DefaultPort = 50060

// Version is the soflink protocol/build tag reported in the hello.
const Version = "0.1.0"

// Hello is the identity a node returns on GET /soflink/hello.
type Hello struct {
	ID      string `json:"id"`
	Role    string `json:"role"`
	Version string `json:"version"`
}

// Peer is a soflink node found on the LAN.
type Peer struct {
	Addr    string `json:"addr"` // ip:port
	ID      string `json:"id"`
	Role    string `json:"role"`
	Version string `json:"version"`
}

// LocalCIDRs returns the IPv4 subnets this host is on (up, non-loopback), so a
// sweep covers "whatever range the user has" with no configuration. Subnets
// wider than /16 are skipped to keep the sweep bounded.
func LocalCIDRs() ([]*net.IPNet, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []*net.IPNet
	seen := map[string]bool{}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil {
				continue
			}
			ones, bits := ipn.Mask.Size()
			if bits != 32 || ones < 16 { // IPv4 only, and not too large
				continue
			}
			norm := &net.IPNet{IP: ipn.IP.Mask(ipn.Mask), Mask: ipn.Mask}
			if seen[norm.String()] {
				continue
			}
			seen[norm.String()] = true
			out = append(out, norm)
		}
	}
	return out, nil
}

// HostsIn enumerates the usable host IPs in a subnet (network + broadcast dropped).
func HostsIn(n *net.IPNet) []net.IP {
	base := n.IP.Mask(n.Mask).To4()
	if base == nil {
		return nil
	}
	var hosts []net.IP
	for cur := cloneIP(base); n.Contains(cur); inc(cur) {
		hosts = append(hosts, cloneIP(cur))
	}
	if len(hosts) >= 2 { // drop network (first) and broadcast (last)
		hosts = hosts[1 : len(hosts)-1]
	}
	return hosts
}

// Sweep probes port on every host of the given subnets and returns the ones that
// answer the soflink hello (excluding selfID). Fully concurrent, each probe
// bounded by timeout.
func Sweep(ctx context.Context, subnets []*net.IPNet, port int, timeout time.Duration, selfID string) []Peer {
	var targets []net.IP
	for _, s := range subnets {
		targets = append(targets, HostsIn(s)...)
	}
	client := &http.Client{Timeout: timeout}
	sem := make(chan struct{}, 256)
	var (
		mu    sync.Mutex
		peers []Peer
		wg    sync.WaitGroup
	)
	for _, ip := range targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(ip net.IP) {
			defer wg.Done()
			defer func() { <-sem }()
			addr := fmt.Sprintf("%s:%d", ip, port)
			h, ok := probe(ctx, client, addr)
			if !ok || h.ID == selfID {
				return
			}
			mu.Lock()
			peers = append(peers, Peer{Addr: addr, ID: h.ID, Role: h.Role, Version: h.Version})
			mu.Unlock()
		}(ip)
	}
	wg.Wait()
	return peers
}

func probe(ctx context.Context, c *http.Client, addr string) (Hello, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/soflink/hello", nil)
	if err != nil {
		return Hello{}, false
	}
	resp, err := c.Do(req)
	if err != nil {
		return Hello{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Hello{}, false
	}
	var h Hello
	if err := json.NewDecoder(resp.Body).Decode(&h); err != nil || h.ID == "" {
		return Hello{}, false
	}
	return h, true
}

// HelloHandler answers GET /soflink/hello with this node's identity, so a peer's
// LAN sweep recognizes it as soflink (a random open port won't answer this).
func HelloHandler(id, role string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(Hello{ID: id, Role: role, Version: Version})
	}
}

func cloneIP(ip net.IP) net.IP { c := make(net.IP, len(ip)); copy(c, ip); return c }

func inc(ip net.IP) {
	for i := len(ip) - 1; i >= 0; i-- {
		ip[i]++
		if ip[i] != 0 {
			break
		}
	}
}
