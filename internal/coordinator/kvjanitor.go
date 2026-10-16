package coordinator

// Handoff state files are ~930 MB each (a 20k-token prompt of the 27B), and the
// only thing that deletes them is this coordinator. The first design deleted
// them fire-and-forget after the restore: a soflink that did not answer at that
// moment, or a coordinator that died between the save and the restore, left
// the file on the node for good. Nine of them (2.7 GB) were found on .51 in
// October, abandoned since September, and nobody could tell whose they were.
//
// So every state is written down BEFORE it can exist (before the save), and it
// stays written down until each node has confirmed it is gone (2xx, or 404 =
// never there / already gone). The list lives in kvpending.local.json, so a
// restart picks up what the previous process left behind: anything in that
// file at startup belongs to a handoff that can no longer finish.

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"sort"
	"time"
)

// kvJanitorEvery is how often the unconfirmed deletes are retried; a state
// younger than kvOrphanAge may still be in flight (a prefill of 100k tokens
// plus its fetch takes minutes, never an hour) and is left alone.
const (
	kvJanitorEvery = 10 * time.Minute
	kvOrphanAge    = time.Hour
)

// kvPendingPath is where the unconfirmed states persist (next to the config,
// like renames.local.json); override with SOFMAT_KVPENDING.
func kvPendingPath() string {
	if p := os.Getenv("SOFMAT_KVPENDING"); p != "" {
		return p
	}
	return "kvpending.local.json"
}

// kvPending is one state that may still exist on the listed soflink agents.
type kvPending struct {
	Ctls  []string  `json:"ctls"`
	Since time.Time `json:"since"`
}

// track writes a state down before it is created. ctls are the soflink agents
// that may end up holding a copy (the prefill writes it, the decode fetches it).
func (k *kvPipe) track(name string, ctls ...string) {
	k.gcMu.Lock()
	defer k.gcMu.Unlock()
	if k.gc == nil {
		k.gc = map[string]kvPending{}
	}
	var cs []string
	for _, c := range ctls {
		if c != "" && !contains(cs, c) {
			cs = append(cs, c)
		}
	}
	k.gc[name] = kvPending{Ctls: cs, Since: time.Now()}
	k.saveGCLocked()
}

// ctlsOf is where a state may live: what track recorded, else the configured
// pair (a state the coordinator never tracked — older code, or a test).
func (k *kvPipe) ctlsOf(name string) []string {
	k.gcMu.Lock()
	p, ok := k.gc[name]
	k.gcMu.Unlock()
	if ok && len(p.Ctls) > 0 {
		return p.Ctls
	}
	return []string{k.decodeCtl, k.prefillCtl}
}

// purge deletes the state (and its draft sidecar) on every node that may hold
// it, and forgets it only when every node has confirmed. Returns whether it is
// gone everywhere.
func (k *kvPipe) purge(name string) bool {
	ok := true
	for _, base := range k.ctlsOf(name) {
		for _, n := range []string{name, name + ".dft"} {
			if !k.deleteState(base, n) {
				ok = false
			}
		}
	}
	if ok {
		k.gcMu.Lock()
		if _, had := k.gc[name]; had {
			delete(k.gc, name)
			k.saveGCLocked()
		}
		k.gcMu.Unlock()
	}
	return ok
}

// deleteState is one DELETE /kv/<name>; 2xx and 404 both mean "not there now".
func (k *kvPipe) deleteState(base, name string) bool {
	req, err := http.NewRequest(http.MethodDelete, base+"/kv/"+name, nil)
	if err != nil {
		return false
	}
	c := k.client
	if k.gcClient != nil {
		c = k.gcClient
	}
	resp, err := c.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode/100 == 2 || resp.StatusCode == http.StatusNotFound
}

// sweep retries the deletes of every state older than minAge. minAge 0 = all
// of them (startup: the process that tracked them is gone).
func (k *kvPipe) sweep(minAge time.Duration) (gone, left int) {
	k.gcMu.Lock()
	var names []string
	for n, p := range k.gc {
		if time.Since(p.Since) >= minAge {
			names = append(names, n)
		}
	}
	k.gcMu.Unlock()
	sort.Strings(names)
	for _, n := range names {
		if k.purge(n) {
			gone++
		} else {
			left++
		}
	}
	if gone+left > 0 {
		log.Printf("kvpipe: barrido de estados de traspaso: %d borrados, %d sin confirmar", gone, left)
	}
	return gone, left
}

// startJanitor loads what a previous process left unconfirmed, retries it at
// once, and then keeps retrying every kvJanitorEvery.
func (k *kvPipe) startJanitor(path string) {
	k.gcMu.Lock()
	k.gcPath = path
	if b, err := os.ReadFile(path); err == nil {
		var m map[string]kvPending
		if json.Unmarshal(b, &m) == nil {
			if k.gc == nil {
				k.gc = map[string]kvPending{}
			}
			for n, p := range m {
				if validStateName(n) {
					k.gc[n] = p
				}
			}
		}
	}
	k.gcMu.Unlock()
	go func() {
		k.sweep(0)
		t := time.NewTicker(kvJanitorEvery)
		defer t.Stop()
		for range t.C {
			k.sweep(kvOrphanAge)
		}
	}()
}

// saveGCLocked persists the list (caller holds gcMu). No path = memory only
// (tests, or a coordinator built without the janitor).
func (k *kvPipe) saveGCLocked() {
	if k.gcPath == "" {
		return
	}
	b, err := json.MarshalIndent(k.gc, "", "  ")
	if err != nil {
		return
	}
	tmp := k.gcPath + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, k.gcPath)
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}
