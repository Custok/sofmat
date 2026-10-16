package coordinator

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestMain keeps the janitor's list of every coordinator built by the tests out
// of the source tree (kvPipeFrom starts it with the default relative path).
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "kvpending")
	if err == nil {
		os.Setenv("SOFMAT_KVPENDING", filepath.Join(dir, "kvpending.local.json"))
	}
	code := m.Run()
	if err == nil {
		os.RemoveAll(dir)
	}
	os.Exit(code)
}

// fakeAgent is a soflink /kv/ endpoint: it holds files, deletes them, and can
// be told to fail (a node that is down or answering 5xx at cleanup time).
type fakeAgent struct {
	mu    sync.Mutex
	files map[string]bool
	down  atomic.Bool
	srv   *httptest.Server
}

func newFakeAgent(t *testing.T, names ...string) *fakeAgent {
	a := &fakeAgent{files: map[string]bool{}}
	for _, n := range names {
		a.files[n] = true
	}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.down.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/kv/")
		a.mu.Lock()
		defer a.mu.Unlock()
		if r.Method != http.MethodDelete {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if !a.files[name] {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		delete(a.files, name)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *fakeAgent) has(name string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.files[name]
}

// A node that fails the delete at cleanup time must not lose the state: the
// first design forgot it and the file stayed on the node for weeks.
func TestJanitorKeepsUnconfirmedDeleteAndRetries(t *testing.T) {
	pre := newFakeAgent(t, "sf-a.bin")
	dec := newFakeAgent(t, "sf-a.bin")
	k := newKVPipe("http://p", "http://d", pre.srv.URL, dec.srv.URL)
	k.track("sf-a.bin", pre.srv.URL, dec.srv.URL)

	pre.down.Store(true)
	if k.purge("sf-a.bin") {
		t.Fatal("purge reported gone while the prefill node refused the delete")
	}
	if !pre.has("sf-a.bin") || dec.has("sf-a.bin") {
		t.Fatalf("want prefill copy kept, decode copy gone; pre=%v dec=%v", pre.has("sf-a.bin"), dec.has("sf-a.bin"))
	}
	if _, left := k.sweep(time.Hour); left != 0 {
		t.Fatal("a fresh state (younger than the orphan age) must be left alone")
	}

	pre.down.Store(false)
	if gone, left := k.sweep(0); gone != 1 || left != 0 {
		t.Fatalf("sweep = %d gone, %d left; want 1, 0", gone, left)
	}
	if pre.has("sf-a.bin") {
		t.Fatal("the retry did not delete the prefill copy")
	}
	if len(k.gc) != 0 {
		t.Fatalf("confirmed state still listed: %v", k.gc)
	}
}

// The list survives a restart, and the new process retries it at once: what a
// dead coordinator left behind belongs to a handoff that can never finish.
func TestJanitorPicksUpWhatAPreviousProcessLeft(t *testing.T) {
	pre := newFakeAgent(t, "sf-b.bin")
	path := filepath.Join(t.TempDir(), "kvpending.local.json")

	old := newKVPipe("http://p", "http://d", pre.srv.URL, pre.srv.URL)
	old.gcPath = path
	old.track("sf-b.bin", pre.srv.URL)
	// the old process dies here: saved, never handed off, never cleaned

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the list was not persisted: %v", err)
	}
	var m map[string]kvPending
	if json.Unmarshal(b, &m) != nil || len(m["sf-b.bin"].Ctls) != 1 {
		t.Fatalf("persisted list unreadable or wrong: %s", b)
	}

	k := newKVPipe("http://p", "http://d", pre.srv.URL, pre.srv.URL)
	k.startJanitor(path)
	deadline := time.Now().Add(5 * time.Second)
	for pre.has("sf-b.bin") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if pre.has("sf-b.bin") {
		t.Fatal("the new process did not delete the state the old one left")
	}
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, _ := os.ReadFile(path); !strings.Contains(string(b), "sf-b.bin") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the confirmed state is still in the persisted list")
}

// A state already gone (404) counts as deleted, not as a failure to retry
// forever — the .dft sidecar no longer exists from llama.cpp 2851b3b on.
func TestJanitorNotFoundIsGone(t *testing.T) {
	a := newFakeAgent(t)
	k := newKVPipe("http://p", "http://d", a.srv.URL, a.srv.URL)
	k.track("sf-c.bin", a.srv.URL)
	if !k.purge("sf-c.bin") {
		t.Fatal("404 on every node must count as gone")
	}
}

// A stream that ends in an error event instead of the timings must say why on
// the row (rows 1241/1287 of 2026-10-16 had no engine fields and no cause).
func TestSSEErrorReadsTheEngineError(t *testing.T) {
	tail := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hola\"}}]}\n\n" +
		"data: {\"error\":{\"code\":500,\"message\":\"Failed to parse tool call\",\"type\":\"server_error\"}}\n\n")
	if got := sseError(tail); got != "Failed to parse tool call" {
		t.Fatalf("sseError = %q", got)
	}
	if got := sseError([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"\\\"error\\\" en el texto\"}}]}\n")); got != "" {
		t.Fatalf("model text mentioning error must not count, got %q", got)
	}
	if sseTimings(tail) != nil {
		t.Fatal("test premise: no timings in this tail")
	}
}
