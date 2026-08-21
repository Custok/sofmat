//go:build linux

package agent

import (
	"os"
	"strconv"
	"strings"
)

var lastIdle, lastTotal uint64

// readCPU returns idle and total jiffies from /proc/stat's aggregate line.
func readCPU() (idle, total uint64) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "cpu ") {
			continue
		}
		for i, x := range strings.Fields(line)[1:] {
			v, _ := strconv.ParseUint(x, 10, 64)
			total += v
			if i == 3 { // idle is the 4th value
				idle = v
			}
		}
		return
	}
	return
}

func cpuPercent() int {
	idle, total := readCPU()
	di, dt := idle-lastIdle, total-lastTotal
	lastIdle, lastTotal = idle, total
	if dt == 0 || di > dt {
		return 0
	}
	return int((dt - di) * 100 / dt)
}

func hostStats() (cpu, usedMB, totalMB int) {
	b, _ := os.ReadFile("/proc/meminfo")
	var totalKB, availKB uint64
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		v, _ := strconv.ParseUint(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			totalKB = v
		case "MemAvailable:":
			availKB = v
		}
	}
	totalMB = int(totalKB / 1024)
	usedMB = int((totalKB - availKB) / 1024)
	return cpuPercent(), usedMB, totalMB
}

// Prime seeds the CPU-time baseline so the first read is accurate.
func Prime() { cpuPercent() }
