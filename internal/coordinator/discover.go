package coordinator

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/Custok/sofmat/internal/discovery"
)

// startDiscovery runs a periodic LAN sweep on the soflink port so the panel
// self-populates with every soflink node on the network — the DGX and anyone
// else — with zero configuration. Peers answer /soflink/hello (and now /gpu),
// so each self-reports its own hardware.
func (s *Server) startDiscovery() {
	port := portOf(s.cfg.Listen)
	if port == 0 {
		port = 1357
	}
	go func() {
		defer func() { _ = recover() }()
		for {
			if subnets, err := discovery.LocalCIDRs(); err == nil && len(subnets) > 0 {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				peers := discovery.Sweep(ctx, subnets, port, 400*time.Millisecond, s.selfID())
				cancel()
				s.peerMu.Lock()
				s.peers = peers
				s.peerMu.Unlock()
			}
			s.reconcileRenames() // heal labels from peers (missing keys only)
			time.Sleep(20 * time.Second)
		}
	}()
}

// discoveredPeers returns a copy of the last sweep's soflink nodes.
func (s *Server) discoveredPeers() []discovery.Peer {
	s.peerMu.Lock()
	defer s.peerMu.Unlock()
	out := make([]discovery.Peer, len(s.peers))
	copy(out, s.peers)
	return out
}

func portOf(listen string) int {
	if i := strings.LastIndex(listen, ":"); i >= 0 {
		n, _ := strconv.Atoi(listen[i+1:])
		return n
	}
	return 0
}
