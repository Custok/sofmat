//go:build !windows && !linux

package agent

// Stub for platforms without a native host-stats reader (e.g. macOS): GPU stats
// still come from nvidia-smi; CPU/RAM report zero.
func hostStats() (cpu, usedMB, totalMB int) { return 0, 0, 0 }

// Prime is a no-op where there is no CPU-time baseline to seed.
func Prime() {}
