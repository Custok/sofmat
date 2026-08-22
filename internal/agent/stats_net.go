package agent

import (
	"sync"
	"time"
)

// Network throughput is sampled the same way CPU is: each read of the OS byte
// counters is compared to the previous read, and the delta over the elapsed
// time gives a rate. State is process-global (one node = one sampler).
var (
	netMu   sync.Mutex
	netRx   uint64
	netTx   uint64
	netAt   time.Time
	netHave bool
)

// netRate returns network throughput (down, up) in Mbps, computed as the byte
// delta since the previous call over the elapsed time. The first call — and any
// call where the OS counters are unavailable — returns 0,0. Counter resets
// (reboot / interface flap) clamp to 0 instead of reporting a bogus spike.
func netRate() (rxMbps, txMbps float64) {
	rx, tx, ok := osNetBytes()
	if !ok {
		return 0, 0
	}
	now := time.Now()
	netMu.Lock()
	defer netMu.Unlock()
	prevRx, prevTx, prevAt, have := netRx, netTx, netAt, netHave
	netRx, netTx, netAt, netHave = rx, tx, now, true
	if !have {
		return 0, 0
	}
	dt := now.Sub(prevAt).Seconds()
	if dt <= 0 {
		return 0, 0
	}
	var drx, dtx uint64
	if rx >= prevRx {
		drx = rx - prevRx
	}
	if tx >= prevTx {
		dtx = tx - prevTx
	}
	// bytes/s → bits/s → Mbps (the panel divides by 1000 for Gb/s).
	return float64(drx) * 8 / dt / 1e6, float64(dtx) * 8 / dt / 1e6
}
