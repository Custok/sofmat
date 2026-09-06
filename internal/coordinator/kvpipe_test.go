package coordinator

// End-to-end test of the disaggregated prefill→decode handoff with simulated
// engines: two fake llama-servers (prefill + decode, each with its own slot
// state dir) and two REAL soflink daemons fronting them (kv_state_dir = that
// dir), behind a third REAL soflink acting as the gateway. No network beyond
// loopback, no GPU.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Custok/sofmat/internal/config"
)

type fakeEngine struct {
	mu          sync.Mutex
	dir         string // its --slot-save-path
	busy        bool   // what GET /slots reports (is_processing)
	completions []int  // prompt token counts received by /completion
	saved       []string
	erased      int
	restored    []string
	restoreSlot string
	chats       []map[string]any // bodies received by /v1/chat/completions
	srv         *httptest.Server
}

func writeTestJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func newFakeEngine(t *testing.T) *fakeEngine {
	t.Helper()
	e := &fakeEngine{dir: t.TempDir(), busy: true}
	mux := http.NewServeMux()
	mux.HandleFunc("/slots", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		busy := e.busy
		e.mu.Unlock()
		writeTestJSON(w, 200, []any{
			map[string]any{"id": 0, "is_processing": busy},
			map[string]any{"id": 1, "is_processing": false},
		})
	})
	mux.HandleFunc("/apply-template", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		msgs, _ := b["messages"].([]any)
		var sb strings.Builder
		for _, m := range msgs {
			mm, _ := m.(map[string]any)
			sb.WriteString("<|im_start|>")
			sb.WriteString(mm["role"].(string))
			sb.WriteString("\n")
			c, _ := mm["content"].(string)
			sb.WriteString(c)
			sb.WriteString("<|im_end|>\n")
		}
		sb.WriteString("<|im_start|>assistant\n")
		writeTestJSON(w, 200, map[string]any{"prompt": sb.String()})
	})
	mux.HandleFunc("/tokenize", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		content, _ := b["content"].(string)
		n := len(content) / 4 // the fake tokenizer: 4 chars per token
		ids := make([]int, n)
		for i := range ids {
			ids[i] = 1000 + i
		}
		writeTestJSON(w, 200, map[string]any{"tokens": ids})
	})
	mux.HandleFunc("/completion", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		prompt, _ := b["prompt"].([]any)
		if np, _ := b["n_predict"].(float64); np != 0 {
			writeTestJSON(w, 400, map[string]any{"error": "prefill must not generate"})
			return
		}
		e.mu.Lock()
		e.completions = append(e.completions, len(prompt))
		e.mu.Unlock()
		writeTestJSON(w, 200, map[string]any{"content": "", "timings": map[string]any{
			"prompt_n": len(prompt), "prompt_per_second": 2000.0}})
	})
	mux.HandleFunc("/slots/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/slots/")
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		name, _ := b["filename"].(string)
		switch r.URL.Query().Get("action") {
		case "save":
			if err := os.WriteFile(filepath.Join(e.dir, name), []byte("STATE:"+name), 0o644); err != nil {
				writeTestJSON(w, 500, map[string]any{"error": err.Error()})
				return
			}
			e.mu.Lock()
			e.saved = append(e.saved, name)
			e.mu.Unlock()
			writeTestJSON(w, 200, map[string]any{"id_slot": id, "filename": name, "n_saved": 8000, "n_written": 304087040,
				"timings": map[string]any{"save_ms": 199.0}})
		case "erase":
			e.mu.Lock()
			e.erased++
			e.mu.Unlock()
			writeTestJSON(w, 200, map[string]any{"id_slot": id, "n_erased": 8000})
		case "restore":
			data, err := os.ReadFile(filepath.Join(e.dir, name))
			if err != nil || string(data) != "STATE:"+name {
				writeTestJSON(w, 400, map[string]any{"error": "state file not found in slot-save-path"})
				return
			}
			e.mu.Lock()
			e.restored = append(e.restored, name)
			e.restoreSlot = id
			e.mu.Unlock()
			writeTestJSON(w, 200, map[string]any{"id_slot": id, "filename": name, "n_restored": 8000,
				"timings": map[string]any{"restore_ms": 62.0}})
		default:
			writeTestJSON(w, 400, map[string]any{"error": "unknown action"})
		}
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		e.mu.Lock()
		e.chats = append(e.chats, b)
		e.mu.Unlock()
		// the engine reused the restored KV only when pinned to the slot that holds it
		promptN := 8001.0
		if _, pinned := b["id_slot"]; pinned && b["cache_prompt"] == true {
			promptN = 1
		}
		tm := map[string]any{"prompt_n": promptN, "cache_n": 8001 - promptN, "predicted_n": 43, "predicted_per_second": 40.5}
		if s, _ := b["stream"].(bool); s {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hola\"}}]}\n\n")
			last, _ := json.Marshal(map[string]any{"choices": []any{}, "timings": tm})
			_, _ = io.WriteString(w, "data: "+string(last)+"\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		writeTestJSON(w, 200, map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "resumen"}}},
			"timings": tm,
		})
	})
	e.srv = httptest.NewServer(mux)
	t.Cleanup(e.srv.Close)
	return e
}

// soflinkFor starts a REAL daemon whose kv_state_dir is the engine's dir.
func soflinkFor(t *testing.T, dir string) *httptest.Server {
	t.Helper()
	s, err := NewServer(&config.Config{KVStateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

type rig struct {
	prefill, decode *fakeEngine
	gateway         *httptest.Server
}

func newRig(t *testing.T, decodeKVDir func(*fakeEngine) string, mutate ...func(*config.Config)) *rig {
	t.Helper()
	pre, dec := newFakeEngine(t), newFakeEngine(t)
	preCtl := soflinkFor(t, pre.dir)
	decCtl := soflinkFor(t, decodeKVDir(dec))
	cfg := &config.Config{
		Nodes: []config.Node{
			{ID: "node-c", Agent: preCtl.URL, GPUs: 2},
			{ID: "node-d", Agent: decCtl.URL, GPUs: 2},
		},
		Instances: []config.Instance{
			{Key: "decode", Role: "decode", Endpoint: dec.srv.URL, Main: "node-d"},
			{Key: "prefill", Role: "prefill", Endpoint: pre.srv.URL, Main: "node-c"},
		},
	}
	for _, m := range mutate {
		m(cfg)
	}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !s.gw.Disaggregated() {
		t.Fatal("gateway must be disaggregated with prefill + decode + agents configured")
	}
	gw := httptest.NewServer(s.Handler())
	t.Cleanup(gw.Close)
	return &rig{prefill: pre, decode: dec, gateway: gw}
}

func postChat(t *testing.T, base string, body map[string]any) (int, []byte) {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(base+"/v1/chat/completions", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func lastRequestRecord(t *testing.T, base string) map[string]any {
	t.Helper()
	resp, err := http.Get(base + "/api/requests?n=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Requests []map[string]any `json:"requests"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || len(out.Requests) == 0 {
		t.Fatalf("no request record: %v", err)
	}
	return out.Requests[len(out.Requests)-1]
}

var longUser = strings.Repeat("La flota LocalStation sirve modelos Qwen con llama.cpp. ", 800) // ~44k chars → ~11k fake tokens

func TestHandoffEndToEnd(t *testing.T) {
	r := newRig(t, func(e *fakeEngine) string { return e.dir })
	code, data := postChat(t, r.gateway.URL, map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": longUser}},
		"max_tokens": 64, "chat_template_kwargs": map[string]any{"enable_thinking": false},
	})
	if code != 200 || !bytes.Contains(data, []byte("resumen")) {
		t.Fatalf("chat failed: %d %s", code, data)
	}
	pre, dec := r.prefill, r.decode
	pre.mu.Lock()
	defer pre.mu.Unlock()
	dec.mu.Lock()
	defer dec.mu.Unlock()
	if len(pre.completions) != 1 || len(pre.saved) != 1 || pre.erased != 1 {
		t.Fatalf("prefill must run exactly one completion + save + erase: %+v", pre)
	}
	// the last token is held back (recurrent cache can't be truncated)
	wantTokens := (len(longUser)+len("<|im_start|>user\n<|im_end|>\n<|im_start|>assistant\n")) / 4
	if pre.completions[0] != wantTokens-1 {
		t.Fatalf("prefill must process tokens[:-1]: got %d, want %d", pre.completions[0], wantTokens-1)
	}
	if len(dec.restored) != 1 || dec.restored[0] != pre.saved[0] {
		t.Fatalf("decode must restore the saved state: %+v vs %+v", dec.restored, pre.saved)
	}
	if len(dec.chats) != 1 {
		t.Fatalf("decode must get one chat: %d", len(dec.chats))
	}
	chat := dec.chats[0]
	slot, ok := chat["id_slot"].(float64)
	if !ok || chat["cache_prompt"] != true {
		t.Fatalf("decode chat must be pinned to the restored slot with cache_prompt: %v", chat)
	}
	if dec.restoreSlot != strings.TrimSuffix(strings.TrimSuffix(jsonNum(slot), ".0"), ".") {
		t.Fatalf("restore slot %q != chat id_slot %v", dec.restoreSlot, slot)
	}
	if _, ok := chat["messages"]; !ok {
		t.Fatal("decode must receive the original messages (re-templated by the engine)")
	}
	rec := lastRequestRecord(t, r.gateway.URL)
	if rec["admitted_via"] != "prefill" || rec["kv_miss"] != false || rec["prompt_n"] != 1.0 {
		t.Fatalf("record must show a successful handoff: %v", rec)
	}
	if rec["tokens"] != float64(wantTokens) || rec["n_restored"] != 8000.0 || rec["state_bytes"] != 304087040.0 {
		t.Fatalf("record metrics wrong: %v", rec)
	}
	for _, k := range []string{"prefill_ms", "save_ms", "fetch_ms", "restore_ms", "handoff_ms", "total_ms", "tg_tokps"} {
		if _, ok := rec[k]; !ok {
			t.Fatalf("record lacks %s: %v", k, rec)
		}
	}
	// both copies of the state are deleted once restored (best-effort, async)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, e1 := os.Stat(filepath.Join(pre.dir, pre.saved[0]))
		_, e2 := os.Stat(filepath.Join(dec.dir, pre.saved[0]))
		if os.IsNotExist(e1) && os.IsNotExist(e2) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("state files must be cleaned up on both nodes")
}

func jsonNum(f float64) string {
	b, _ := json.Marshal(f)
	return string(b)
}

// Default policy ("busy"): an idle decode takes the direct path even for a
// long prompt; "always" offloads regardless of the decode's state.
func TestIdleDecodeStaysDirectUnlessAlways(t *testing.T) {
	r := newRig(t, func(e *fakeEngine) string { return e.dir })
	r.decode.mu.Lock()
	r.decode.busy = false
	r.decode.mu.Unlock()
	code, _ := postChat(t, r.gateway.URL, map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": longUser}},
	})
	if code != 200 {
		t.Fatalf("chat failed: %d", code)
	}
	r.prefill.mu.Lock()
	n := len(r.prefill.completions)
	r.prefill.mu.Unlock()
	if n != 0 {
		t.Fatal("idle decode must not offload the prefill")
	}
	if rec := lastRequestRecord(t, r.gateway.URL); rec["admission"] != "decode-idle" || rec["admitted_via"] != "decode" {
		t.Fatalf("record wrong: %v", rec)
	}

	always := newRig(t, func(e *fakeEngine) string { return e.dir }, func(c *config.Config) { c.KVHandoff = "always" })
	always.decode.mu.Lock()
	always.decode.busy = false
	always.decode.mu.Unlock()
	postChat(t, always.gateway.URL, map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": longUser}},
	})
	always.prefill.mu.Lock()
	n = len(always.prefill.completions)
	always.prefill.mu.Unlock()
	if n != 1 {
		t.Fatal("kv_handoff=always must offload even on an idle decode")
	}
	if rec := lastRequestRecord(t, always.gateway.URL); rec["admitted_via"] != "prefill" {
		t.Fatalf("record wrong: %v", rec)
	}
}

func TestShortPromptStaysDecodeDirect(t *testing.T) {
	r := newRig(t, func(e *fakeEngine) string { return e.dir })
	code, _ := postChat(t, r.gateway.URL, map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hola, ¿qué tal?"}},
	})
	if code != 200 {
		t.Fatalf("chat failed: %d", code)
	}
	r.prefill.mu.Lock()
	defer r.prefill.mu.Unlock()
	if len(r.prefill.completions) != 0 || len(r.prefill.saved) != 0 {
		t.Fatalf("short prompt must never touch the prefill: %+v", r.prefill)
	}
	r.decode.mu.Lock()
	defer r.decode.mu.Unlock()
	if _, ok := r.decode.chats[0]["id_slot"]; ok {
		t.Fatal("decode-direct must leave slot selection to the engine")
	}
	if rec := lastRequestRecord(t, r.gateway.URL); rec["admitted_via"] != "decode" {
		t.Fatalf("record wrong: %v", rec)
	}
}

// A decode node without kv_state_dir can't receive the state: the request must
// still be answered (decode-direct) and the record must say why.
func TestHandoffFailureDegradesToDecode(t *testing.T) {
	r := newRig(t, func(*fakeEngine) string { return "" })
	code, data := postChat(t, r.gateway.URL, map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": longUser}},
	})
	if code != 200 || !bytes.Contains(data, []byte("resumen")) {
		t.Fatalf("request must survive: %d %s", code, data)
	}
	r.decode.mu.Lock()
	defer r.decode.mu.Unlock()
	if _, ok := r.decode.chats[0]["id_slot"]; ok {
		t.Fatal("a failed handoff must not pin the decode slot")
	}
	if len(r.decode.restored) != 0 {
		t.Fatal("nothing to restore after a failed fetch")
	}
	rec := lastRequestRecord(t, r.gateway.URL)
	he, _ := rec["handoff_error"].(string)
	if rec["admitted_via"] != "decode-fallback" || !strings.Contains(he, "kv-fetch") {
		t.Fatalf("fallback must be visible with its cause: %v", rec)
	}
	// the prefill slot was still released
	r.prefill.mu.Lock()
	defer r.prefill.mu.Unlock()
	if r.prefill.erased != 1 {
		t.Fatalf("prefill slot must be erased even when the handoff fails: %+v", r.prefill)
	}
}

func TestStreamingGoesThroughHandoff(t *testing.T) {
	r := newRig(t, func(e *fakeEngine) string { return e.dir })
	code, data := postChat(t, r.gateway.URL, map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": longUser}},
		"stream":   true,
	})
	if code != 200 || !bytes.Contains(data, []byte(`"content":"hola"`)) || !bytes.Contains(data, []byte("[DONE]")) {
		t.Fatalf("stream must pass through: %d %s", code, data)
	}
	r.decode.mu.Lock()
	chat := r.decode.chats[0]
	r.decode.mu.Unlock()
	if chat["stream"] != true || chat["cache_prompt"] != true {
		t.Fatalf("streamed decode must keep stream and carry the slot pin: %v", chat)
	}
	if _, ok := chat["id_slot"]; !ok {
		t.Fatal("streamed decode must be pinned to the restored slot")
	}
	// Finish runs after the stream ends (deferred): give it a moment
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(r.gateway.URL + "/api/requests?n=1")
		if err == nil {
			var out struct {
				Requests []map[string]any `json:"requests"`
			}
			_ = json.NewDecoder(resp.Body).Decode(&out)
			resp.Body.Close()
			if len(out.Requests) == 1 {
				rec := out.Requests[0]
				if rec["admitted_via"] != "prefill" || rec["prompt_n"] != 1.0 || rec["kv_miss"] != false {
					t.Fatalf("streamed record must carry the final-chunk timings: %v", rec)
				}
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("streamed request never recorded")
}

func TestKVStateEndpoints(t *testing.T) {
	dir := t.TempDir()
	node := soflinkFor(t, dir)
	peerDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(peerDir, "sf-abc-1.bin"), bytes.Repeat([]byte("k"), 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	peer := soflinkFor(t, peerDir)

	// traversal / bad names are rejected before touching the disk
	for _, bad := range []string{"/kv/../x.bin", "/kv/a%2Fb.bin", "/kv/notes.txt", "/kv/.hidden.bin"} {
		resp, err := http.Get(node.URL + bad)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 && resp.StatusCode != 404 {
			t.Fatalf("%s must be rejected, got %d", bad, resp.StatusCode)
		}
	}
	// fetch pulls the peer's file under the given name (sha verified)
	body, _ := json.Marshal(map[string]any{"url": peer.URL + "/kv/sf-abc-1.bin", "name": "sf-abc-1.bin",
		"sha256": "wrong"})
	resp, err := http.Post(node.URL+"/control/kv-fetch", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatal("sha mismatch must fail the fetch")
	}
	if _, err := os.Stat(filepath.Join(dir, "sf-abc-1.bin.part")); !os.IsNotExist(err) {
		t.Fatal("a failed fetch must not leave a partial file")
	}
	body, _ = json.Marshal(map[string]any{"url": peer.URL + "/kv/sf-abc-1.bin", "name": "sf-abc-1.bin"})
	resp, err = http.Post(node.URL+"/control/kv-fetch", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if resp.StatusCode != 200 || out["bytes"] != float64(1<<20) {
		t.Fatalf("fetch failed: %d %v", resp.StatusCode, out)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "sf-abc-1.bin")); err != nil || len(got) != 1<<20 {
		t.Fatalf("fetched file wrong: %v %d", err, len(got))
	}
	// serve + delete
	resp, err = http.Get(node.URL + "/kv/sf-abc-1.bin")
	if err != nil {
		t.Fatal(err)
	}
	n, _ := io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || n != 1<<20 {
		t.Fatalf("serve wrong: %d %d", resp.StatusCode, n)
	}
	req, _ := http.NewRequest(http.MethodDelete, node.URL+"/kv/sf-abc-1.bin", nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if _, err := os.Stat(filepath.Join(dir, "sf-abc-1.bin")); !os.IsNotExist(err) {
		t.Fatal("delete must remove the file")
	}
	// a node without the dir refuses politely
	off := soflinkFor(t, "")
	resp, _ = http.Get(off.URL + "/kv/sf-abc-1.bin")
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("node without kv_state_dir must answer 503, got %d", resp.StatusCode)
	}
}

func TestSlotSavePathInjected(t *testing.T) {
	s, _ := NewServer(&config.Config{KVStateDir: "/tmp/kv"})
	args := s.withSlotSavePath([]string{"-m", "x.gguf"})
	if strings.Join(args, " ") != "-m x.gguf --slot-save-path /tmp/kv" {
		t.Fatalf("flag not injected: %v", args)
	}
	args = s.withSlotSavePath([]string{"-m", "x.gguf", "--slot-save-path", "/other"})
	if strings.Contains(strings.Join(args, " "), "/tmp/kv") {
		t.Fatal("caller's flag must win")
	}
	s2, _ := NewServer(&config.Config{})
	if a := s2.withSlotSavePath([]string{"-m", "x.gguf"}); len(a) != 2 {
		t.Fatal("no dir → no flag")
	}
}

func TestSSETimingsExtraction(t *testing.T) {
	tail := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data: {\"choices\":[],\"timings\":{\"prompt_n\":1,\"cache_n\":8000}}\n\n" +
		"data: [DONE]\n\n")
	tm := sseTimings(tail)
	if tm == nil || tm["prompt_n"] != 1.0 || tm["cache_n"] != 8000.0 {
		t.Fatalf("timings not extracted: %v", tm)
	}
	if sseTimings([]byte("data: [DONE]\n\n")) != nil {
		t.Fatal("no timings → nil")
	}
}
