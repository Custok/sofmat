// Command soflink is the per-node soflink entrypoint. Today it exposes LAN
// discovery: `soflink discover` derives this host's own subnet(s) and sweeps them
// for other soflink nodes, so a fleet forms with zero configuration.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/Custok/sofmat/internal/discovery"
)

func main() {
	cmd := "discover"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "discover":
		discover()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (try: discover)\n", cmd)
		os.Exit(2)
	}
}

func discover() {
	subnets, err := discovery.LocalCIDRs()
	if err != nil {
		fmt.Fprintln(os.Stderr, "discover: could not read local interfaces:", err)
		os.Exit(1)
	}
	if len(subnets) == 0 {
		fmt.Println("no local IPv4 subnet found — nothing to sweep")
		return
	}
	names := make([]string, len(subnets))
	for i, s := range subnets {
		names[i] = s.String()
	}
	fmt.Printf("sweeping %v for soflink on :%d …\n", names, discovery.DefaultPort)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	peers := discovery.Sweep(ctx, subnets, discovery.DefaultPort, 400*time.Millisecond, selfID())

	fmt.Printf("found %d soflink node(s):\n", len(peers))
	for _, p := range peers {
		fmt.Printf("  %-21s id=%s role=%s v=%s\n", p.Addr, p.ID, p.Role, p.Version)
	}
}

func selfID() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}
