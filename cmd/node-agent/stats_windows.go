//go:build windows

package main

import (
	"os/exec"
	"strings"
	"syscall"
	"unsafe"
)

var (
	kernel32           = syscall.NewLazyDLL("kernel32.dll")
	procGlobalMemory   = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetSystemTimes = kernel32.NewProc("GetSystemTimes")
)

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

type filetime struct{ Low, High uint32 }

func (f filetime) u64() uint64 { return uint64(f.High)<<32 | uint64(f.Low) }

var lastIdle, lastKernel, lastUser uint64

// cpuPercent samples GetSystemTimes and returns busy% since the previous call.
// Windows counts idle time inside kernel time, so busy = (kernel+user) - idle.
func cpuPercent() int {
	var idle, kernel, user filetime
	r, _, _ := procGetSystemTimes.Call(
		uintptr(unsafe.Pointer(&idle)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)))
	if r == 0 {
		return 0
	}
	i, k, u := idle.u64(), kernel.u64(), user.u64()
	di, total := i-lastIdle, (k-lastKernel)+(u-lastUser)
	lastIdle, lastKernel, lastUser = i, k, u
	if total == 0 || di > total {
		return 0
	}
	return int((total - di) * 100 / total)
}

func hostStats() (cpu, usedMB, totalMB int) {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	procGlobalMemory.Call(uintptr(unsafe.Pointer(&m)))
	totalMB = int(m.TotalPhys / 1048576)
	usedMB = int((m.TotalPhys - m.AvailPhys) / 1048576)
	return cpuPercent(), usedMB, totalMB
}

// llamaCmdline returns the running llama-server command line (best effort).
func llamaCmdline() string {
	out, err := exec.Command("wmic", "process", "where",
		"name='llama-server.exe'", "get", "commandline", "/format:list").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "CommandLine=") {
			return strings.TrimSpace(strings.TrimPrefix(line, "CommandLine="))
		}
	}
	return ""
}
