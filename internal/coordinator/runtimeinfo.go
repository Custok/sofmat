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
	"bytes"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
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

// childCounts walks /proc for processes whose parent is this one: total, and
// how many are zombies. Both devs asked for this independently — it is the
// defect that took 990 processes to become visible through ps, and the
// process could have reported it at three.
func childCounts() (children, zombies int) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return -1, -1
	}
	me := os.Getpid()
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue // it exited between the listing and the read: normal
		}
		// "pid (comm) state ppid …" — comm can contain spaces AND parens, so
		// the fields must be taken from AFTER the last ')', never by splitting
		// the whole line.
		i := bytes.LastIndexByte(b, ')')
		if i < 0 || i+2 >= len(b) {
			continue
		}
		f := strings.Fields(string(b[i+2:]))
		if len(f) < 2 {
			continue
		}
		if ppid, err := strconv.Atoi(f[1]); err != nil || ppid != me {
			continue
		}
		children++
		if f[0] == "Z" {
			zombies++
		}
	}
	return children, zombies
}

func (s *Server) apiRuntime(w http.ResponseWriter, r *http.Request) {
	children, zombies := childCounts()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	writeJSON(w, http.StatusOK, map[string]any{
		"version":  Version,
		"pid":      os.Getpid(),
		"uptime_s": int(time.Since(processStart).Seconds()),
		// started_at is an ABSOLUTE mark on purpose, asked for by both devs
		// independently: uptime_s only means something against a previous
		// reading, and last night's restart loop stayed invisible precisely
		// because every window was anchored to a clock or a file instead of
		// to the event. Anchor the window to the event.
		"started_at":      processStart.Format(time.RFC3339),
		"children":        children,
		"zombie_children": zombies,
		"goroutines":      runtime.NumGoroutine(),
		"open_fds":        openFDs(),
		"heap_mb":         float64(m.HeapAlloc) / (1 << 20),
		"sys_mb":          float64(m.Sys) / (1 << 20),
		"gc_cycles":       m.NumGC,
		"num_cpu":         runtime.NumCPU(),
		"go_version":      runtime.Version(),
		"os":              runtime.GOOS,
		"sampled_at":      time.Now().Format(time.RFC3339),
	})
}
