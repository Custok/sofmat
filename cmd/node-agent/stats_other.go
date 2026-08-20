//go:build !windows

package main

// Non-Windows stub so the module builds and vets on the Linux CI/container. The
// agent ships as a Windows .exe for node-a; hosts on other OSes run their own
// sensor. GPU stats still come from nvidia-smi in main.go.
func hostStats() (cpu, usedMB, totalMB int) { return 0, 0, 0 }
func llamaCmdline() string                  { return "" }
