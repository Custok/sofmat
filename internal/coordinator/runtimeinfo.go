package coordinator

// /api/runtime — what the process knows about itself.
//
// The night of 2026-09-08/09 was spent hunting two leaks (990 orphan soflink
// processes on .51, ~11.4 GB; a re-exec loop that restarted 113 times) and
// EVERY measurement had to come from outside the daemon: ps, systemctl
// show NRestarts, /proc/<pid>/status. The process itself could not say how
// many goroutines it was running or how many descriptors it held, so a leak
// was only visible once it was big enough to show up in the host's numbers.
//
// This endpoint is the missing instrument. It is read-only, allocates nothing
// per call beyond the map, and answers the three questions that would have
// caught both leaks on their first hour instead of their thousandth process:
// goroutines, open descriptors, and how long this PID has been alive.

import (
	"net/http"
	"os"
	"runtime"
	"time"
)

var processStart = time.Now()

// openFDs counts this process's open descriptors. Linux exposes them as
// /proc/self/fd; on Windows there is no equivalent cheap count, so it reports
// -1 rather than a number that would be wrong.
func openFDs() int {
	f, err := os.Open("/proc/self/fd")
	if err != nil {
		return -1
	}
	defer f.Close()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return -1
	}
	return len(names) - 1 // the descriptor of this very directory
}

func (s *Server) apiRuntime(w http.ResponseWriter, r *http.Request) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	writeJSON(w, http.StatusOK, map[string]any{
		"version":    Version,
		"pid":        os.Getpid(),
		"uptime_s":   int(time.Since(processStart).Seconds()),
		"goroutines": runtime.NumGoroutine(),
		"open_fds":   openFDs(),
		"heap_mb":    float64(m.HeapAlloc) / (1 << 20),
		"sys_mb":     float64(m.Sys) / (1 << 20),
		"gc_cycles":  m.NumGC,
		"num_cpu":    runtime.NumCPU(),
		"go_version": runtime.Version(),
		"os":         runtime.GOOS,
		"sampled_at": time.Now().Format(time.RFC3339),
	})
}
