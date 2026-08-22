package coordinator

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Version is this binary's build stamp (YYYYMMDDHHMM), set by main from the
// -ldflags value so the panel can show which build is running and when it updated.
var Version = "dev"

// GitHubToken, when set from config.local.json, authenticates the updater's
// GitHub API calls (5000 req/h instead of the anonymous 60/h per shared IP).
var GitHubToken string

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
	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/repos/Custok/sofmat/releases/latest", nil)
	if GitHubToken != "" {
		req.Header.Set("Authorization", "Bearer "+GitHubToken)
	}
	resp, err := c.Do(req)
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
	// Fleet-wide: which nodes run a build older than the latest release? The panel
	// only shows the "actualizar" button when at least one node is behind — so it
	// disappears once the whole fleet is on the newest version.
	behind := []string{}
	if avail != "" {
		type res struct{ id, ver string }
		ch := make(chan res, len(s.cfg.Nodes))
		var wg sync.WaitGroup
		for _, n := range s.cfg.Nodes {
			base := strings.TrimRight(n.Agent, "/")
			if base == "" {
				continue
			}
			wg.Add(1)
			go func(id, base string) {
				defer wg.Done()
				ver := ""
				cl := &http.Client{Timeout: 1500 * time.Millisecond}
				if resp, err := cl.Get(base + "/api/version"); err == nil {
					var vv struct {
						Current string `json:"current"`
					}
					_ = json.NewDecoder(resp.Body).Decode(&vv)
					resp.Body.Close()
					ver = vv.Current
				}
				ch <- res{id, ver}
			}(n.ID, base)
		}
		wg.Wait()
		close(ch)
		for r := range ch { // unreachable / unknown version → not flagged (can't update it anyway)
			if r.ver != "" && r.ver != "dev" && r.ver < avail {
				behind = append(behind, r.id)
			}
		}
		sort.Strings(behind)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"current":       cur,
		"available":     avail,
		"autoupdate":    AutoUpdateOn(),
		"pending":       cur != "dev" && avail != "" && avail > cur,
		"fleet_pending": len(behind) > 0,
		"behind":        behind,
	})
}

// panelSetAutoUpdate toggles the runtime auto-updater from the header checkbox.
func (s *Server) panelSetAutoUpdate(w http.ResponseWriter, r *http.Request) {
	SetAutoUpdate(r.URL.Query().Get("on") == "true")
	writeJSON(w, http.StatusOK, map[string]any{"autoupdate": AutoUpdateOn()})
}

// UpdateNow is main's checkAndUpdate, wired in so the panel's "actualizar ahora"
// button can trigger an immediate self-update instead of waiting for the timer.
var UpdateNow func()

// panelUpdateNow fires an immediate update check (swap + re-exec if a newer release
// exists) from the header button.
func (s *Server) panelUpdateNow(w http.ResponseWriter, r *http.Request) {
	if UpdateNow == nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "update no disponible (dev / --no-update)"})
		return
	}
	go UpdateNow() // may os.Exit after swapping the binary and re-exec'ing into the new one
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "updating": true})
}

// panelUpdateFleet triggers "actualizar ahora" on EVERY node: it POSTs /api/update
// to each remote node's own soflink (so each self-updates from GitHub), then fires
// the local update last. One click updates the whole fleet instead of only the
// coordinator — the fan-out is one request per node, so it doesn't add GitHub load
// beyond what each node would poll anyway.
func (s *Server) panelUpdateFleet(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{}
	var selfID string
	for _, n := range s.cfg.Nodes {
		base := strings.TrimRight(n.Agent, "/")
		if base == "" {
			continue
		}
		if s.isSelfHost(hostOf(base)) {
			selfID = n.ID // do the coordinator last: its update re-execs the process
			continue
		}
		resp, err := s.client.Post(base+"/api/update", "application/json", nil)
		if err != nil {
			out[n.ID] = "error: " + err.Error()
			continue
		}
		resp.Body.Close()
		out[n.ID] = "updating"
	}
	if selfID != "" {
		out[selfID] = "updating (local)"
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "nodes": out})
	// Local update last — it may os.Exit + re-exec, so fire it AFTER the response
	// is written so the panel gets the per-node result.
	if selfID != "" && UpdateNow != nil {
		go func() { time.Sleep(700 * time.Millisecond); UpdateNow() }()
	}
}
