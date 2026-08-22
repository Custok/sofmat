package coordinator

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Version is this binary's build stamp (YYYYMMDDHHMM), set by main from the
// -ldflags value so the panel can show which build is running and when it updated.
var Version = "dev"

// autoUpdate mirrors whether the runtime auto-updater is active; the panel's
// checkbox reads and toggles it live (the periodic updater checks AutoUpdateOn()).
var autoUpdate atomic.Bool

func init() { autoUpdate.Store(true) }

// SetAutoUpdate / AutoUpdateOn let main seed the flag and the update loop read it.
func SetAutoUpdate(on bool) { autoUpdate.Store(on) }
func AutoUpdateOn() bool     { return autoUpdate.Load() }

// latest-release cache so the panel can show "versión disponible" without hitting
// the GitHub API on every status tick. Refreshed asynchronously (never blocks a
// request); ~10-min TTL.
var (
	latestMu   sync.Mutex
	latestVer  string
	latestAt   time.Time
	refreshing atomic.Bool
)

func latestRelease() string {
	latestMu.Lock()
	v, stale := latestVer, time.Since(latestAt) > 10*time.Minute || latestVer == ""
	latestMu.Unlock()
	if stale {
		go refreshLatest()
	}
	return v
}

func refreshLatest() {
	if !refreshing.CompareAndSwap(false, true) {
		return
	}
	defer refreshing.Store(false)
	c := &http.Client{Timeout: 6 * time.Second}
	resp, err := c.Get("https://api.github.com/repos/Custok/sofmat/releases/latest")
	if err != nil || resp == nil {
		return
	}
	defer resp.Body.Close()
	var rel struct {
		Tag string `json:"tag_name"`
	}
	if json.NewDecoder(resp.Body).Decode(&rel) != nil {
		return
	}
	if v := strings.TrimPrefix(rel.Tag, "v"); v != "" {
		latestMu.Lock()
		latestVer, latestAt = v, time.Now()
		latestMu.Unlock()
	}
}

// panelVersion feeds the header: current build, the newest published build, and
// whether auto-update is on / an update is pending.
func (s *Server) panelVersion(w http.ResponseWriter, r *http.Request) {
	cur, avail := Version, latestRelease()
	writeJSON(w, http.StatusOK, map[string]any{
		"current":    cur,
		"available":  avail,
		"autoupdate": AutoUpdateOn(),
		"pending":    cur != "dev" && avail != "" && avail > cur,
	})
}

// panelSetAutoUpdate toggles the runtime auto-updater from the header checkbox.
func (s *Server) panelSetAutoUpdate(w http.ResponseWriter, r *http.Request) {
	SetAutoUpdate(r.URL.Query().Get("on") == "true")
	writeJSON(w, http.StatusOK, map[string]any{"autoupdate": AutoUpdateOn()})
}
