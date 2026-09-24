package coordinator

// End-to-end test of the disaggregated prefill→decode handoff with simulated
// engines: two fake llama-servers (prefill + decode, each with its own slot
// state dir) and two REAL soflink daemons fronting them (kv_state_dir = that
// dir), behind a third REAL soflink acting as the gateway. No network beyond
// loopback, no GPU.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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
	sidecar     bool   // save also writes <name>.dft (patched engine)
	completions []int  // prompt token counts received by /completion
	saved       []string
	erased      int
	// heldBySlot models WHERE the engine's KV lives: slot id -> n_prompt_tokens.
	// A real llama-server keeps a slot's prompt cache after the request ends and
	// reuses it only when a request is pinned to THAT slot; a pin to an evicted
	// slot reprocesses the whole prompt. Modelling placement per slot (not a
	// single "held" number that always sat on slot 0) is what lets a test
	// reproduce the stale-pin ping-pong: KV on slot X, the pin points at slot Y.
	heldBySlot map[int]int
	// lcp models the real llama-server's slot choice for an UNPINNED request: it
	// reuses the slot that already holds this conversation (longest common
	// prefix) and otherwise takes directSlot — which need not be the slot the
	// coordinator's ring guessed. slotConv remembers which conversation each
	// slot holds. Off by default so the older tests keep their fixed semantics.
	lcp        bool
	directSlot int
	slotConv   map[int]string
	leaked     []string // coordinator timing notes (x-sofmat-t-*) seen as request headers
	// queueBeforeFirstToken models the real llama-server: a streamed reply's HTTP
	// headers go out at once, the first chunk only after the queue in front of
	// the slot AND the prompt processing (promptMs, reported in timings). A
	// non-streamed reply sends everything together, after the generation too
	// (predictedMs).
	queueBeforeFirstToken time.Duration
	promptMs, predictedMs float64
	noPredictedMs         bool // timings without predicted_ms (older builds)
	// hangAfterFirstChunk: the engine streams one token and then stalls until
	// its request is cancelled (the client went away upstream).
	hangAfterFirstChunk bool
	// busyAfterAbort: the engine keeps the slot busy 2 s after the first chunk
	// whatever the client does (a real llama-server only notices a gone client
	// on its next write): what a request the client abandoned looks like.
	busyAfterAbort bool
	// toolCallReply: the model answers with TWO tool calls (streamed as deltas
	// keyed by index, like llama-server; whole when not streamed) instead of text.
	toolCallReply bool
	restored      []string
	restoreSlot   string
	restoreFail   string           // when set, /slots/N?action=restore answers 500 with this
	chats         []map[string]any // bodies received by /v1/chat/completions
	srv           *httptest.Server
}

func writeTestJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func newFakeEngine(t *testing.T) *fakeEngine {
	t.Helper()
	e := &fakeEngine{dir: t.TempDir(), busy: true, heldBySlot: map[int]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/slots", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		busy := e.busy
		// busy: slots 0-2 generating, only slot 3 idle (so a handoff must land in
		// 3). Each slot reports the KV it actually holds (heldBySlot), the way a
		// real llama-server does — the residency probe reads exactly this, per slot.
		out := make([]any, 4)
		for id := 0; id < 4; id++ {
			out[id] = map[string]any{
				"id": id, "is_processing": busy && id < 3,
				"n_prompt_tokens": float64(e.heldBySlot[id]),
			}
		}
		e.mu.Unlock()
		writeTestJSON(w, 200, out)
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
		n := len(content) / 4 // the fake tokenizer: 4 chars per token, id derived from the chars
		ids := make([]int, n)
		for i := range ids {
			h := 0
			for _, ch := range content[4*i : 4*i+4] {
				h = h*31 + int(ch)
			}
			ids[i] = 1000 + h%50000
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
			if e.sidecar { // patched engine: draft-context sidecar next to the state
				_ = os.WriteFile(filepath.Join(e.dir, name+".dft"), []byte("DFT:"+name), 0o644)
			}
			e.mu.Lock()
			e.saved = append(e.saved, name)
			e.mu.Unlock()
			writeTestJSON(w, 200, map[string]any{"id_slot": id, "filename": name, "n_saved": 8000, "n_written": 304087040,
				"timings": map[string]any{"save_ms": 199.0}})
		case "erase":
			e.mu.Lock()
			e.erased++
			if n, err := strconv.Atoi(id); err == nil {
				delete(e.heldBySlot, n) // the slot's KV is gone
			}
			e.mu.Unlock()
			writeTestJSON(w, 200, map[string]any{"id_slot": id, "n_erased": 8000})
		case "restore":
			e.mu.Lock()
			rf := e.restoreFail
			e.mu.Unlock()
			if rf != "" {
				writeTestJSON(w, 500, map[string]any{"error": map[string]any{"code": 500, "message": rf}})
				return
			}
			data, err := os.ReadFile(filepath.Join(e.dir, name))
			if err != nil || string(data) != "STATE:"+name {
				writeTestJSON(w, 400, map[string]any{"error": "state file not found in slot-save-path"})
				return
			}
			e.mu.Lock()
			e.restored = append(e.restored, name)
			e.restoreSlot = id
			if n, err := strconv.Atoi(id); err == nil {
				e.heldBySlot[n] = 8000 // the restored KV now lives in this slot
			}
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
		for k := range r.Header {
			if strings.HasPrefix(strings.ToLower(k), "x-sofmat-t-") {
				e.mu.Lock()
				e.leaked = append(e.leaked, k)
				e.mu.Unlock()
			}
		}
		promptTokens := 0
		if msgs, _ := json.Marshal(b["messages"]); len(msgs) > 0 {
			promptTokens = len(msgs) / 4
		}
		slotID := 0
		idSlotF, pinned := b["id_slot"].(float64)
		if pinned {
			slotID = int(idSlotF)
		}
		// the conversation's identity, the way LCP would see it: its first turn
		convKey := ""
		if msgs, _ := b["messages"].([]any); len(msgs) > 0 {
			for _, m := range msgs {
				mm, _ := m.(map[string]any)
				if mm != nil && mm["role"] != "system" {
					c, _ := mm["content"].(string)
					convKey = c
					break
				}
			}
		}
		e.mu.Lock()
		e.chats = append(e.chats, b)
		lcpHit := false
		if !pinned && e.lcp {
			slotID = e.directSlot
			for id, ck := range e.slotConv {
				if ck != "" && ck == convKey && e.heldBySlot[id] > 0 {
					slotID, lcpHit = id, true
					break
				}
			}
		}
		// The engine reuses cache ONLY when pinned to a slot that actually holds
		// this conversation's KV. A pin to an evicted slot reprocesses the whole
		// prompt (prompt_n = the full count, cache_n 0) — the exact ping-pong the
		// residency fix removes. A direct (unpinned) request processes the prompt
		// too. Either way, the prompt now lives in the slot the engine used, and
		// that is what the residency probe reads back on the next turn.
		promptN := float64(promptTokens)
		if promptN < 1 {
			promptN = 1
		}
		cacheN := 0.0
		if pinned && b["cache_prompt"] == true && e.heldBySlot[slotID] > 0 {
			promptN = 1
			cacheN = float64(e.heldBySlot[slotID])
		}
		if lcpHit {
			// the real engine: cache_n + prompt_n == the slot's new n_prompt_tokens
			cacheN = float64(e.heldBySlot[slotID])
			promptN = float64(promptTokens) - cacheN
			if promptN < 1 {
				promptN = 1
				cacheN = float64(promptTokens) - 1
			}
		}
		e.heldBySlot[slotID] = promptTokens
		if e.lcp {
			// like the real engine: the slot keeps the reply too (+ the stop token),
			// so /slots reports prompt + generated + 1, never the bare prompt
			e.heldBySlot[slotID] = promptTokens + 43 + 1
			if e.slotConv == nil {
				e.slotConv = map[int]string{}
			}
			e.slotConv[slotID] = convKey
		}
		e.mu.Unlock()
		tm := map[string]any{"prompt_n": promptN, "cache_n": cacheN, "predicted_n": 43, "predicted_per_second": 40.5}
		if e.promptMs > 0 {
			tm["prompt_ms"] = e.promptMs
			if !e.noPredictedMs {
				tm["predicted_ms"] = e.predictedMs
			}
		}
		if s, _ := b["stream"].(bool); s {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			if f, ok := w.(http.Flusher); ok {
				f.Flush() // headers on the wire before any queueing, like llama-server
			}
			if e.queueBeforeFirstToken > 0 {
				// some llama-server builds also emit an empty role chunk at once:
				// it carries no token and must not count as the first one
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n")
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
			time.Sleep(e.queueBeforeFirstToken + time.Duration(e.promptMs*float64(time.Millisecond)))
			if e.toolCallReply {
				// call 0 in two deltas (name, then arguments), call 1 in one; the
				// second write lands mid-event to exercise reassembly across reads
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"topic_post\",\"arguments\":\"\"}}]}}]}\n\n"+
					"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"a\\\":1}\"}}]}}]}\n\ndata: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,")
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				_, _ = io.WriteString(w, "\"id\":\"c2\",\"type\":\"function\",\"function\":{\"name\":\"rag_search\",\"arguments\":\"{}\"}}]}}]}\n\n")
			} else {
				_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hola\"}}]}\n\n")
			}
			if e.busyAfterAbort {
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				time.Sleep(2 * time.Second) // ignores the client: the slot stays busy
				return
			}
			if e.hangAfterFirstChunk {
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
				select {
				case <-r.Context().Done():
				case <-time.After(5 * time.Second):
				}
				return
			}
			last, _ := json.Marshal(map[string]any{"choices": []any{}, "timings": tm})
			_, _ = io.WriteString(w, "data: "+string(last)+"\n\n")
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		time.Sleep(e.queueBeforeFirstToken + time.Duration((e.promptMs+e.predictedMs)*float64(time.Millisecond)))
		message := map[string]any{"role": "assistant", "content": "resumen"}
		if e.toolCallReply {
			message = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{
				map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "topic_post", "arguments": "{}"}},
				map[string]any{"id": "c2", "type": "function", "function": map[string]any{"name": "rag_search", "arguments": "{}"}},
			}}
		}
		writeTestJSON(w, 200, map[string]any{
			"choices": []any{map[string]any{"message": message}},
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

// requestRecordIfAny is lastRequestRecord without the failure: false while the
// log is still empty.
func requestRecordIfAny(t *testing.T, base string) (map[string]any, bool) {
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
		return nil, false
	}
	return out.Requests[len(out.Requests)-1], true
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
		"messages":   []any{map[string]any{"role": "user", "content": longUser}},
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
	// erased twice on purpose: BEFORE the completion, because the KV is unified
	// and the slot's leftover cells would count against this prompt (the engine
	// answers HTTP 500 "Context size has been exceeded"), and AFTER the save so
	// the sequence does not linger in the budget.
	if len(pre.completions) != 1 || len(pre.saved) != 1 || pre.erased != 2 {
		t.Fatalf("prefill must run exactly one completion + save + erase: %+v", pre)
	}
	// the last token is held back (recurrent cache can't be truncated)
	wantTokens := (len(longUser) + len("<|im_start|>user\n<|im_end|>\n<|im_start|>assistant\n")) / 4
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
	if dec.restoreSlot != "3" {
		t.Fatalf("with slots 0-2 busy the restore must land in the idle slot 3, got %q", dec.restoreSlot)
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
	r := newRig(t, func(e *fakeEngine) string { return e.dir }, func(c *config.Config) { c.KVHandoff = "busy" })
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

// A patched prefill engine writes a draft-context sidecar; it must travel with the
// state, be recorded, and be cleaned up. An unpatched one (no sidecar) still works.
func TestDraftSidecarTravelsWhenPresent(t *testing.T) {
	r := newRig(t, func(e *fakeEngine) string { return e.dir })
	r.prefill.mu.Lock()
	r.prefill.sidecar = true
	r.prefill.mu.Unlock()
	code, _ := postChat(t, r.gateway.URL, map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": longUser}},
	})
	if code != 200 {
		t.Fatalf("chat failed: %d", code)
	}
	rec := lastRequestRecord(t, r.gateway.URL)
	if rec["dft"] != true || rec["dft_bytes"] == nil {
		t.Fatalf("sidecar must be fetched and recorded: %v", rec)
	}
	r.prefill.mu.Lock()
	name := r.prefill.saved[0]
	r.prefill.mu.Unlock()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		_, e1 := os.Stat(filepath.Join(r.decode.dir, name+".dft"))
		_, e2 := os.Stat(filepath.Join(r.prefill.dir, name+".dft"))
		if os.IsNotExist(e1) && os.IsNotExist(e2) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(r.decode.dir, name+".dft")); !os.IsNotExist(err) {
		t.Fatal("sidecar must be cleaned up on the decode node")
	}

	plain := newRig(t, func(e *fakeEngine) string { return e.dir })
	postChat(t, plain.gateway.URL, map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": longUser}},
	})
	if rec := lastRequestRecord(t, plain.gateway.URL); rec["admitted_via"] != "prefill" || rec["dft"] != false {
		t.Fatalf("no sidecar must still hand off, recorded as dft=false: %v", rec)
	}
}

// Default mode "auto" on an idle decode: a cold 11k prompt is cheaper direct
// (decode-cheaper); a cold ~50k prompt is cheaper through the prefill node
// (its rate holds at long context); and the next turn of the same
// conversation only counts its NEW tokens (cache-hot → direct).
func TestAutoModeCostAndCacheAware(t *testing.T) {
	r := newRig(t, func(e *fakeEngine) string { return e.dir })
	r.decode.mu.Lock()
	r.decode.busy = false
	r.decode.mu.Unlock()

	postChat(t, r.gateway.URL, map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": longUser}}, // ~11k fake tokens
	})
	if rec := lastRequestRecord(t, r.gateway.URL); rec["admitted_via"] != "decode" || rec["admission"] != "decode-cheaper" {
		t.Fatalf("cold 11k on an idle decode must go direct as cheaper: %v", rec)
	}

	// a DIFFERENT text, so it shares no prefix with the previous prompt (cold)
	huge := strings.Repeat("Informe trimestral de la cooperativa: cifras, incidencias y planes. ", 3000) // ~51k fake tokens
	postChat(t, r.gateway.URL, map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": huge}},
	})
	rec := lastRequestRecord(t, r.gateway.URL)
	if rec["admitted_via"] != "prefill" || rec["prompt_n"] != 1.0 {
		t.Fatalf("cold 51k on an idle decode must go through the prefill (cheaper end-to-end): %v", rec)
	}
	r.prefill.mu.Lock()
	n := len(r.prefill.completions)
	r.prefill.mu.Unlock()
	if n != 1 {
		t.Fatalf("prefill must have run once: %d", n)
	}

	// next turn: same conversation + 2k new tokens → only the delta is new work
	postChat(t, r.gateway.URL, map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": huge},
			map[string]any{"role": "assistant", "content": "resumen"},
			map[string]any{"role": "user", "content": strings.Repeat("y ahora amplia esto. ", 400)},
		},
	})
	rec = lastRequestRecord(t, r.gateway.URL)
	if rec["admitted_via"] != "decode" || rec["admission"] != "cache-hot" {
		t.Fatalf("the next turn must ride the decode's cache: %v", rec)
	}
	if nt, _ := rec["new_tokens"].(float64); nt <= 0 || nt >= 8192 {
		t.Fatalf("new_tokens must be the delta only: %v", rec["new_tokens"])
	}
	r.prefill.mu.Lock()
	n = len(r.prefill.completions)
	r.prefill.mu.Unlock()
	if n != 1 {
		t.Fatalf("no second prefill for a cache-hot turn: %d", n)
	}
}

// TestStalePinnedSlotDoesNotReprocess reproduces the production ping-pong of
// 2026-09-21 (measured 22/22 on a resident ~19k conversation). Between a user's
// turns, other tasks on the shared decode engine clear idle slots, so the slot
// convSlot remembers no longer holds this conversation's KV. The OLD aggregate
// residency check ("is SOME slot big") then saw a NEIGHBOUR's big slot, called
// the next turn cache-hot, pinned the evicted slot, and the decode reprocessed
// the whole prompt (cache_n 0, prompt_n ~18923); Finish Forgot the record and
// the following turn swung to a full prefill — both branches slow.
//
// With residency asked of the PINNED slot, the stale pin reads cold and the turn
// is restored through the handoff (prompt_n 1) instead of pinned to an empty
// slot. Without the fix this test fails: the follow-up is admitted cache-hot and
// the decode reprocesses the whole prompt. It relies on the fake modelling KV
// placement PER SLOT (heldBySlot) — a single held-on-slot-0 fake cannot express
// "KV on slot X, pin points at slot Y".
func TestStalePinnedSlotDoesNotReprocess(t *testing.T) {
	// kvMiss mirrors gateway.kvMissPromptTokens: past this many prompt tokens the
	// restored/cached KV was NOT reused and the prompt was re-processed.
	const kvMiss = 64
	r := newRig(t, func(e *fakeEngine) string { return e.dir }) // decode busy: only slot 3 idle

	convA := func(extra ...map[string]any) map[string]any {
		msgs := []any{map[string]any{"role": "user", "content": longUser}}
		for _, m := range extra {
			msgs = append(msgs, m)
		}
		return map[string]any{"messages": msgs}
	}

	// Turn 1: large cold conversation -> prefill + handoff, restored into slot 3.
	if code, _ := postChat(t, r.gateway.URL, convA()); code != 200 {
		t.Fatalf("turn 1 failed: %d", code)
	}
	if rec := lastRequestRecord(t, r.gateway.URL); rec["admitted_via"] != "prefill" || rec["prompt_n"] != 1.0 {
		t.Fatalf("turn 1 must hand off and reuse the restored KV: %v", rec)
	}
	r.decode.mu.Lock()
	landed, held3 := r.decode.restoreSlot, r.decode.heldBySlot[3]
	r.decode.mu.Unlock()
	if landed != "3" || held3 <= 0 {
		t.Fatalf("turn 1 KV must live in slot 3: landed=%q held=%d", landed, held3)
	}

	// Between turns: another task cleared David's idle slot 3, and a NEIGHBOUR
	// conversation now occupies a different slot (big). convSlot still points at 3.
	r.decode.mu.Lock()
	delete(r.decode.heldBySlot, 3) // David's KV evicted from its slot
	r.decode.heldBySlot[1] = 20000 // a neighbour's big slot (would fool the aggregate)
	r.decode.mu.Unlock()

	// Turn 2: same conversation, a small new turn on top (the whole history is
	// resent, so the estimate would want to call this cache-hot and pin slot 3).
	if code, _ := postChat(t, r.gateway.URL, convA(
		map[string]any{"role": "assistant", "content": "ok"},
		map[string]any{"role": "user", "content": "y ahora amplia el punto tres, por favor"},
	)); code != 200 {
		t.Fatalf("turn 2 failed: %d", code)
	}
	rec := lastRequestRecord(t, r.gateway.URL)
	pn, _ := rec["prompt_n"].(float64)
	if rec["admission"] == "cache-hot" && pn > kvMiss {
		t.Fatalf("stale pin: admitted cache-hot but the decode reprocessed the whole prompt "+
			"(prompt_n=%v) — pinned to an evicted slot. That is the ping-pong: %v", rec["prompt_n"], rec)
	}
	// the pinned slot really held (or was restored with) the KV, so the turn reuses it
	if pn != 1.0 {
		t.Fatalf("turn 2 must reuse the KV (prompt_n 1), got %v: %v", rec["prompt_n"], rec)
	}
}

// The engine picks the slot for a direct (unpinned) turn by content; the ring
// only guessed. Without learning the real slot the next turn's residency probe
// reads the guessed slot (empty), calls the prefix evicted, counts the whole
// resident prompt as new and only an idle decode keeps it off a handoff
// (measured 2026-09-23 22:16: five turns, hot_prefix 0, engine slot 1 vs
// recorded 3). RED before fix#6: no slot_engine, hot_prefix_tokens 0 on turn 2.
func TestCoordinatorLearnsEngineSlot(t *testing.T) {
	r := newRig(t, func(e *fakeEngine) string { return e.dir })
	r.decode.mu.Lock()
	r.decode.lcp, r.decode.directSlot = true, 1 // the engine will serve in slot 1
	r.decode.busy = false                       // idle decode: direct turns
	r.decode.mu.Unlock()
	// ~5k tokens: above the short-prompt cut-off (served on the decode) and below
	// the prefill threshold (no handoff) — the plain decode-direct turn.
	sys := strings.Repeat("Reglas de la flota LocalStation. ", 625)
	conv := func(extra ...map[string]any) map[string]any {
		msgs := []any{
			map[string]any{"role": "system", "content": sys},
			map[string]any{"role": "user", "content": "primera pregunta de la conversación"},
		}
		for _, m := range extra {
			msgs = append(msgs, m)
		}
		return map[string]any{"messages": msgs}
	}
	if code, _ := postChat(t, r.gateway.URL, conv()); code != 200 {
		t.Fatal("turn 1 failed")
	}
	rec := lastRequestRecord(t, r.gateway.URL)
	if rec["admitted_via"] != "decode" {
		t.Fatalf("test premise: turn 1 must be decode-direct: %v", rec)
	}
	if rec["slot_engine"] != "1" {
		t.Fatalf("turn 1 must learn the engine's slot (1): %v", rec)
	}
	if code, _ := postChat(t, r.gateway.URL, conv(
		map[string]any{"role": "assistant", "content": "una respuesta"},
		map[string]any{"role": "user", "content": "y ahora sigue"},
	)); code != 200 {
		t.Fatal("turn 2 failed")
	}
	rec = lastRequestRecord(t, r.gateway.URL)
	if rec["slot"] != "1" {
		t.Fatalf("turn 2 must be checked/pinned on the learned slot 1, got %v", rec["slot"])
	}
	if cn, _ := rec["cache_n"].(float64); cn <= 0 {
		t.Fatalf("turn 2 must reuse the cache in the engine's slot: %v", rec)
	}
	// Turn 1 was a cold miss, so Finish forgot the shared prefix (by design); turn
	// 2 re-recorded it as hot. Turn 3 is the first one whose residency probe can
	// credit the prefix — and it must credit it in the LEARNED slot.
	if code, _ := postChat(t, r.gateway.URL, conv(
		map[string]any{"role": "assistant", "content": "una respuesta"},
		map[string]any{"role": "user", "content": "y ahora sigue"},
		map[string]any{"role": "assistant", "content": "otra respuesta"},
		map[string]any{"role": "user", "content": "y termina"},
	)); code != 200 {
		t.Fatal("turn 3 failed")
	}
	rec = lastRequestRecord(t, r.gateway.URL)
	if hot, _ := rec["hot_prefix_tokens"].(float64); hot <= 0 {
		t.Fatalf("turn 3 must find its prefix resident in the learned slot: %v", rec)
	}
	if rec["admission"] != "prefix-hot" {
		t.Fatalf("turn 3 must be admitted prefix-hot on the learned slot, got %v", rec["admission"])
	}
}

// prefill_threshold_tokens (config) raises the estimated size from which the
// prefill+handoff route is even considered: with the tool catalogue in the
// prefix every new HUD conversation (~15-17k) crossed the default 6 144, was
// handed off when the decode was busy, and its next turn reprocessed the whole
// prompt (hybrid restore, no checkpoints — measured on David's turn 2026-09-23
// 22:31: 10.6 s + 11.8 s). A high threshold keeps conversations decode-direct
// and reserves the handoff for one-shot batches. RED before: longUser hands off.
func TestPrefillThresholdFromConfig(t *testing.T) {
	r := newRig(t, func(e *fakeEngine) string { return e.dir }, func(c *config.Config) { c.PrefillThresholdTokens = 40000 })
	if code, _ := postChat(t, r.gateway.URL, map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": longUser}},
	}); code != 200 {
		t.Fatal("chat failed")
	}
	rec := lastRequestRecord(t, r.gateway.URL)
	if rec["admitted_via"] != "decode" || rec["admission"] != "small-new-prefill" {
		t.Fatalf("an 11k prompt under a 40k threshold must stay decode-direct: %v", rec)
	}
	r.prefill.mu.Lock()
	defer r.prefill.mu.Unlock()
	if len(r.prefill.completions) != 0 {
		t.Fatalf("no prefill must run under the raised threshold: %+v", r.prefill.completions)
	}
}

// Waits are ALWAYS written (0 = did not wait, -1 = not measurable), as two
// separate numbers — the balancer's budget wait and the engine-side wait before
// the first byte — on both the JSON and the streaming path. A field that only
// appears in the bad case makes null mean two things (measured the hard way
// 2026-09-23 with slot_engine). RED before: none of the fields existed.
func TestWaitFieldsAlwaysWritten(t *testing.T) {
	r := newRig(t, func(e *fakeEngine) string { return e.dir })
	for _, stream := range []bool{false, true} {
		body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": longUser}}}
		if stream {
			body["stream"] = true
		}
		if code, _ := postChat(t, r.gateway.URL, body); code != 200 {
			t.Fatalf("chat (stream=%v) failed: %d", stream, code)
		}
		rec := lastRequestRecord(t, r.gateway.URL)
		wb, ok := rec["wait_budget_ms"].(float64)
		if !ok || wb < 0 {
			t.Fatalf("stream=%v: wait_budget_ms must always be written (>= 0): %v", stream, rec["wait_budget_ms"])
		}
		fb, ok := rec["first_byte_ms"].(float64)
		if !ok || fb < 0 {
			t.Fatalf("stream=%v: first_byte_ms must be measured on a served request: %v", stream, rec["first_byte_ms"])
		}
		if _, ok := rec["wait_slot_ms"].(float64); !ok {
			t.Fatalf("stream=%v: wait_slot_ms must always be written: %v", stream, rec["wait_slot_ms"])
		}
	}
	// and the timing notes never travel to the engine as headers
	r.decode.mu.Lock()
	defer r.decode.mu.Unlock()
	if len(r.decode.leaked) != 0 {
		t.Fatalf("timing notes leaked to the engine as headers: %v", r.decode.leaked)
	}
}

// wait_slot_ms is the queue in front of the slot: the time the engine takes to
// START on the prompt. It was derived as first_byte_ms - prompt_ms, but the
// first byte the coordinator sees is the HTTP HEADER, which llama-server sends
// BEFORE processing the prompt (live 2026-09-23 id 13: headers 27 ms, prompt
// 5 847 ms) — so the subtraction undercounted the wait by exactly prompt_ms
// and read 0 under real contention. The wait must come from the first TOKEN
// (streamed) minus the prompt processing; a non-streamed reply's headers come
// after the generation too, so that also comes off.
func TestWaitSlotIsFirstTokenMinusPrompt(t *testing.T) {
	r := newRig(t, func(e *fakeEngine) string { return e.dir })
	// a short prompt may be balanced onto either engine: both queue the same way
	for _, e := range []*fakeEngine{r.decode, r.prefill} {
		e.queueBeforeFirstToken = 300 * time.Millisecond
		e.promptMs, e.predictedMs = 200, 50
	}
	for _, stream := range []bool{true, false} {
		body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hola"}}}
		if stream {
			body["stream"] = true
		}
		if code, _ := postChat(t, r.gateway.URL, body); code != 200 {
			t.Fatalf("chat (stream=%v) failed: %d", stream, code)
		}
		rec := lastRequestRecord(t, r.gateway.URL)
		if rec["admitted_via"] != "decode" {
			t.Fatalf("test premise: a short prompt goes decode-direct: %v", rec)
		}
		ws, _ := rec["wait_slot_ms"].(float64)
		// the engine queued 300 ms: anything below ~250 is the old undercount
		// (headers arrive at once, minus prompt_ms => 0); anything at or above
		// 300 + prompt is counting the prompt processing as waiting.
		if ws < 250 || ws >= 480 {
			t.Fatalf("stream=%v: wait_slot_ms must be the queue before the slot (~300 ms), got %v (record %v)", stream, ws, rec)
		}
		wantSrc := "whole-reply"
		if stream {
			wantSrc = "first-token"
			ft, ok := rec["first_token_ms"].(float64)
			if !ok || ft < 500 {
				t.Fatalf("a streamed reply records its first token (>= queue+prompt = 500 ms): %v", rec["first_token_ms"])
			}
		}
		// the derivation is named on the row: streamed and whole-reply figures
		// come from different arithmetic and must not be compared blindly
		if rec["wait_slot_src"] != wantSrc {
			t.Fatalf("stream=%v: wait_slot_src must be %q, got %v", stream, wantSrc, rec["wait_slot_src"])
		}
		if fm, ok := rec["finish_ms"].(float64); !ok || fm < 0 {
			t.Fatalf("stream=%v: finish_ms must be written on every served row, got %v", stream, rec["finish_ms"])
		}
	}
	// a whole reply WITHOUT predicted_ms cannot be derived: headers minus prompt
	// is the undercount fix#11 removed, so the row says -1, not a plausible number
	for _, e := range []*fakeEngine{r.decode, r.prefill} {
		e.noPredictedMs = true
	}
	if code, _ := postChat(t, r.gateway.URL, map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hola"}}}); code != 200 {
		t.Fatal("chat failed")
	}
	rec := lastRequestRecord(t, r.gateway.URL)
	if ws, _ := rec["wait_slot_ms"].(float64); ws != -1 || rec["wait_slot_src"] != "" {
		t.Fatalf("whole reply without predicted_ms: wait_slot_ms must be -1 with an empty source, got %v / %v", rec["wait_slot_ms"], rec["wait_slot_src"])
	}
}

// Every record says how many tool calls the model returned (tool_calls_n):
// distinct calls, not deltas, streamed or whole. The HUD claimed actions in
// turns that called nothing (2026-09-24, "acabo de publicar el post" with zero
// calls); the guard for that needs a count taken outside the HUD.
func TestToolCallsCountedPerRequest(t *testing.T) {
	r := newRig(t, func(e *fakeEngine) string { return e.dir })
	body := func(stream bool) map[string]any {
		return map[string]any{"stream": stream, "messages": []any{map[string]any{"role": "user", "content": "hola"}}}
	}
	for _, stream := range []bool{true, false} {
		for _, e := range []*fakeEngine{r.decode, r.prefill} {
			e.toolCallReply = false
		}
		if code, _ := postChat(t, r.gateway.URL, body(stream)); code != 200 {
			t.Fatalf("chat (stream=%v) failed: %d", stream, code)
		}
		if n, _ := lastRequestRecord(t, r.gateway.URL)["tool_calls_n"].(float64); n != 0 {
			t.Fatalf("stream=%v: a text reply has 0 tool calls, got %v", stream, n)
		}
		for _, e := range []*fakeEngine{r.decode, r.prefill} {
			e.toolCallReply = true
		}
		if code, _ := postChat(t, r.gateway.URL, body(stream)); code != 200 {
			t.Fatalf("chat (stream=%v) failed: %d", stream, code)
		}
		rec := lastRequestRecord(t, r.gateway.URL)
		if n, _ := rec["tool_calls_n"].(float64); n != 2 {
			t.Fatalf("stream=%v: two tool calls (three deltas) must count 2, got %v (record %v)", stream, rec["tool_calls_n"], rec)
		}
	}
}

// conv_wait_ms in the config turns fix#15 on for the served gateway: two
// overlapping requests of one conversation are served one after the other and
// the second row says how long it waited. Unset, both go at once (0).
func TestConvWaitFromConfig(t *testing.T) {
	for _, on := range []bool{true, false} {
		r := newRig(t, func(e *fakeEngine) string { return e.dir }, func(c *config.Config) {
			if on {
				c.ConvWaitMs = 5000
			}
		})
		for _, e := range []*fakeEngine{r.decode, r.prefill} {
			e.queueBeforeFirstToken = 300 * time.Millisecond
			e.promptMs, e.predictedMs = 10, 10
		}
		body := map[string]any{"stream": true, "messages": []any{map[string]any{"role": "user", "content": "hola"}}}
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { defer wg.Done(); postChat(t, r.gateway.URL, body) }()
		time.Sleep(50 * time.Millisecond)
		if code, _ := postChat(t, r.gateway.URL, body); code != 200 {
			t.Fatalf("on=%v: chat failed: %d", on, code)
		}
		rec := lastRequestRecord(t, r.gateway.URL)
		wg.Wait()
		w, ok := rec["wait_conv_ms"].(float64)
		if !ok {
			t.Fatalf("on=%v: wait_conv_ms must always be written: %v", on, rec)
		}
		if on && w < 200 {
			t.Fatalf("conv_wait_ms set: the second request must wait for the first (~250 ms), got %v", w)
		}
		if !on && w != 0 {
			t.Fatalf("conv_wait_ms unset: no waiting (0), got %v", w)
		}
	}
}

// fix#15 must not make a conversation wait for a request its client already
// ABANDONED (a fleet reviewer, 2026-09-24 09:18: the HUD's guards abort a stream and
// fire the next call at once; id 61 lived 21 s before its cut was seen). The
// gate is released the moment the abort is detected — the client's context
// cancels the engine call and the read returns — not when the engine's slot
// finally goes idle. Here the fake engine stays busy 2 s after the abort and
// the next request of the same conversation still goes through at once.
func TestAbortedRequestReleasesConversationGate(t *testing.T) {
	r := newRig(t, func(e *fakeEngine) string { return e.dir }, func(c *config.Config) { c.ConvWaitMs = 30000 })
	for _, e := range []*fakeEngine{r.decode, r.prefill} {
		e.busyAfterAbort = true
	}
	raw, _ := json.Marshal(map[string]any{"stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hola"}}})
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.gateway.URL+"/v1/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	cancel() // the client abandons the request while the engine is still busy
	resp.Body.Close()
	t0 := time.Now()
	// the next request of the SAME conversation: served now, not after the 2 s
	for _, e := range []*fakeEngine{r.decode, r.prefill} {
		e.busyAfterAbort = false
	}
	if code, _ := postChat(t, r.gateway.URL, map[string]any{"stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hola"}}}); code != 200 {
		t.Fatalf("chat after the abort failed: %d", code)
	}
	if took := time.Since(t0); took > 1200*time.Millisecond {
		t.Fatalf("the request after an abort waited for the abandoned one (%v): the gate must be released at the abort", took)
	}
	rec := lastRequestRecord(t, r.gateway.URL)
	if w, _ := rec["wait_conv_ms"].(float64); w > 1000 {
		t.Fatalf("wait_conv_ms after an abort must be ~0, got %v (record %v)", w, rec)
	}
}

// A stream the CLIENT cuts (2026-09-23 id 61: the HUD's no-progress guard
// aborted a workflow step at 21 s while the engine was streaming fine) must
// record how long the stream lived and how many events the engine had sent,
// so a cut is told from a stall without asking anyone.
func TestAbortedStreamRecordsLifetime(t *testing.T) {
	r := newRig(t, func(e *fakeEngine) string { return e.dir })
	for _, e := range []*fakeEngine{r.decode, r.prefill} {
		e.hangAfterFirstChunk = true
	}
	raw, _ := json.Marshal(map[string]any{"stream": true,
		"messages": []any{map[string]any{"role": "user", "content": "hola"}}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, r.gateway.URL+"/v1/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	if _, err := resp.Body.Read(buf); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	time.Sleep(400 * time.Millisecond)
	cancel()
	resp.Body.Close()
	// the record is written when the gateway notices the client is gone
	var rec map[string]any
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if got, ok := requestRecordIfAny(t, r.gateway.URL); ok {
			rec = got
			break
		}
	}
	if rec == nil {
		t.Fatal("the aborted request never reached the request log")
	}
	if sm, _ := rec["stream_ms"].(float64); sm < 350 {
		t.Fatalf("stream_ms must say how long the stream lived before the cut (>= 350 ms): %v (record %v)", rec["stream_ms"], rec)
	}
	if ev, _ := rec["engine_events"].(float64); ev < 1 {
		t.Fatalf("engine_events must count what the engine sent before the cut: %v (record %v)", rec["engine_events"], rec)
	}
}

// Every handoff carries its own evidence of room in the request log (what the
// slots held, what was erased to fit the restore, the budget, the need), and
// every request records what the pool held when it arrived. Evidence only:
// the fields never change a decision, but without them "who emptied the slot
// between two turns" is answered by reading an engine log that only writes
// errors.
func TestHandoffRecordsRoomEvidence(t *testing.T) {
	r := newRig(t, func(e *fakeEngine) string { return e.dir })
	if code, _ := postChat(t, r.gateway.URL, map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": longUser}},
	}); code != 200 {
		t.Fatal("chat failed")
	}
	rec := lastRequestRecord(t, r.gateway.URL)
	if rec["admitted_via"] != "prefill" {
		t.Fatalf("test premise: the large prompt must hand off: %v", rec)
	}
	for _, k := range []string{"room_held_before", "room_erased_slots", "room_erased_tokens", "room_budget", "room_need"} {
		if _, ok := rec[k].(float64); !ok {
			t.Fatalf("handoff record must carry %s: %v", k, rec)
		}
	}
	if erased, _ := rec["room_erased_slots"].(float64); erased < 1 {
		t.Fatalf("the target slot is always erased before a restore, got %v", rec["room_erased_slots"])
	}
	if need, _ := rec["room_need"].(float64); need <= 0 {
		t.Fatalf("room_need must be the saved state's tokens, got %v", rec["room_need"])
	}
	if _, ok := rec["pool_held_at_admit"].(float64); !ok {
		t.Fatalf("every request must record pool_held_at_admit: %v", rec)
	}
}

// A short prompt is served on the PREFILL engine's chat endpoint, NOT on the
// decode engine. These throwaway, no-state calls (emotion tagging, tool routing)
// must not land on the decode engine, because the decode runs unified KV and every
// new task on it clears the idle slots — which was evicting the resident big
// conversation between a user's turns (measured 2026-09-21). The prefill's HANDOFF
// machinery (completion/save) is still untouched: the short prompt rides the
// engine's ordinary chat endpoint, and slot selection is left to the engine.
func TestShortPromptGoesToPrefillEngine(t *testing.T) {
	r := newRig(t, func(e *fakeEngine) string { return e.dir })
	code, _ := postChat(t, r.gateway.URL, map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hola, ¿qué tal?"}},
	})
	if code != 200 {
		t.Fatalf("chat failed: %d", code)
	}
	r.prefill.mu.Lock()
	defer r.prefill.mu.Unlock()
	// handoff machinery untouched: served, not prefilled+handed off
	if len(r.prefill.completions) != 0 || len(r.prefill.saved) != 0 {
		t.Fatalf("short prompt must not use the prefill/handoff machinery: %+v", r.prefill)
	}
	// served on the prefill ENGINE's chat endpoint
	if len(r.prefill.chats) != 1 {
		t.Fatalf("short prompt must be served on the prefill engine: %d chats", len(r.prefill.chats))
	}
	if _, ok := r.prefill.chats[0]["id_slot"]; ok {
		t.Fatal("decode-direct must leave slot selection to the engine")
	}
	r.decode.mu.Lock()
	defer r.decode.mu.Unlock()
	// and NEVER on the decode engine, whose idle slots we are protecting
	if len(r.decode.chats) != 0 {
		t.Fatalf("short prompt must not hit the decode engine: %d chats", len(r.decode.chats))
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
	if r.prefill.erased != 2 {
		t.Fatalf("prefill slot must be erased before AND after, even when the handoff fails: %+v", r.prefill)
	}
}

// The prefill node's soflink being down (e.g. not relaunched after a reboot)
// must be detected BEFORE the prefill runs: no engine work, immediate fallback.
func TestPrefillSoflinkDownSkipsPrefillWork(t *testing.T) {
	pre, dec := newFakeEngine(t), newFakeEngine(t)
	preCtl := soflinkFor(t, pre.dir)
	decCtl := soflinkFor(t, dec.dir)
	cfg := &config.Config{
		Nodes: []config.Node{{ID: "node-c", Agent: preCtl.URL, GPUs: 2}, {ID: "node-d", Agent: decCtl.URL, GPUs: 2}},
		Instances: []config.Instance{
			{Key: "decode", Role: "decode", Endpoint: dec.srv.URL, Main: "node-d"},
			{Key: "prefill", Role: "prefill", Endpoint: pre.srv.URL, Main: "node-c"},
		},
		KVHandoff: "always",
	}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(s.Handler())
	t.Cleanup(gw.Close)
	preCtl.Close() // the prefill node's soflink dies

	code, data := postChat(t, gw.URL, map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": longUser}},
	})
	if code != 200 || !bytes.Contains(data, []byte("resumen")) {
		t.Fatalf("request must survive: %d %s", code, data)
	}
	pre.mu.Lock()
	n := len(pre.completions)
	pre.mu.Unlock()
	if n != 0 {
		t.Fatalf("no prefill work when its soflink is unreachable: %d completions", n)
	}
	rec := lastRequestRecord(t, gw.URL)
	pe, _ := rec["prefill_error"].(string)
	if rec["admitted_via"] != "decode-fallback" || !strings.Contains(pe, "prefill soflink unreachable") {
		t.Fatalf("cause must be recorded: %v", rec)
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

// TestRestoreOutOfRoomSkipsWithoutTrippingBreaker.
//
// makeDecodeRoom frees space from a reading of /slots, and the restore lands a
// moment later — by which time another request may have taken it. The engine
// then answers "No available space in KV cache". That is the SAME condition
// makeDecodeRoom already treats as "serve this one direct", discovered later;
// it used to arrive as a component failure and open the prefill breaker for
// 30 s, taking the handoff away from every OTHER request too. Measured on .63:
// six such refusals, each costing half a minute of degraded routing that
// nothing reported.
//
// The second request is the assertion that matters: with the breaker open it
// would not reach the prefill at all.
func TestRestoreOutOfRoomSkipsWithoutTrippingBreaker(t *testing.T) {
	r := newRig(t, func(e *fakeEngine) string { return e.dir })
	r.decode.mu.Lock()
	r.decode.restoreFail = "No available space in KV cache"
	r.decode.mu.Unlock()

	// two DIFFERENT conversations on purpose: the same prompt twice takes the
	// cache-hot short-circuit and never reaches the prefill, so the test would
	// pass while proving nothing — the vacuous-test trap of 2026-09-08.
	for i, who := range []string{"proyecto uno", "proyecto dos"} {
		code, data := postChat(t, r.gateway.URL, map[string]any{
			"messages": []any{map[string]any{"role": "user", "content": who + " " + longUser}},
		})
		if code != 200 || !bytes.Contains(data, []byte("resumen")) {
			t.Fatalf("request %d must survive: %d %s", i+1, code, data)
		}
	}

	rec := lastRequestRecord(t, r.gateway.URL)
	if _, isError := rec["handoff_error"]; isError {
		t.Fatalf("a full decode is not a broken one: %v", rec)
	}
	if skipped, _ := rec["handoff_skipped"].(string); !strings.Contains(skipped, "llen") {
		t.Fatalf("the skip must say the decode filled up: %v", rec)
	}

	r.prefill.mu.Lock()
	saves := len(r.prefill.saved)
	r.prefill.mu.Unlock()
	if saves < 2 {
		t.Fatalf("the breaker closed the prefill after a FULL decode: only %d prefill(s) for 2 requests", saves)
	}
}

// TestRestoreRealFailureStillTripsBreaker is the control: turning every restore
// error into a skip would pass the test above and hide a genuinely broken
// engine behind a permanently "skipped" handoff.
func TestRestoreRealFailureStillTripsBreaker(t *testing.T) {
	r := newRig(t, func(e *fakeEngine) string { return e.dir })
	r.decode.mu.Lock()
	r.decode.restoreFail = "CUDA error: an illegal memory access was encountered"
	r.decode.mu.Unlock()

	code, data := postChat(t, r.gateway.URL, map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": longUser}},
	})
	if code != 200 || !bytes.Contains(data, []byte("resumen")) {
		t.Fatalf("the request must survive even a real failure: %d %s", code, data)
	}
	rec := lastRequestRecord(t, r.gateway.URL)
	he, _ := rec["handoff_error"].(string)
	if he == "" {
		t.Fatalf("a real engine failure must be recorded as an error, not as a skip: %v", rec)
	}
	if !strings.Contains(he, "illegal memory access") {
		t.Fatalf("the cause must survive: %v", rec)
	}
}
