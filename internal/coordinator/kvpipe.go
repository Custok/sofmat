package coordinator

// kvPipe — the disaggregated prefill→decode drivers the gateway sequences (F1 of
// docs/design/kv-handoff-desagregado.md). Three injected callables:
//
//	Count:   apply-template + tokenize on the PREFILL engine → the exact prompt
//	         token ids as the decode will see them (same gguf + --jinja on both
//	         engines ⇒ deterministic template; verified identical across nodes).
//	Prefill: /completion with tokens[:-1], n_predict 0 → slots/0 save → erase.
//	         The last token is held back because a hybrid (recurrent) cache
//	         cannot be truncated: the decode must then receive the IDENTICAL
//	         prompt and only process that one token (timings.prompt_n = 1).
//	         Serialized: the prefill's KV budget is unified across its slots.
//	Handoff: the DECODE node's soflink pulls the state straight from the prefill
//	         node's soflink (/kv/<name>, one hop over the LAN) into the decode
//	         engine's --slot-save-path → slots/<slot> restore → both copies
//	         deleted (best-effort).
//
// Measured on the F0 spike (2026-09-06, 27B Q6_K, 10GbE): state ≈ 19 KB/token
// + 150 MiB; handoff (save + fetch + restore) 0.35 / 0.9 / 2.2 s at 8k / 32k /
// 100k tokens vs 3.5 / 14.7 / 66 s re-processing the prompt on the decode.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Custok/sofmat/internal/gateway"
)

const (
	kvPrefillSlot = 0 // the prefill engine's slot used for every handoff (serialized)
	tokCacheTTL   = 3 * time.Minute
	tokCacheCap   = 64
)

type kvPipe struct {
	prefillURL string // prefill llama-server
	decodeURL  string // decode llama-server
	prefillCtl string // soflink on the prefill node (serves GET/DELETE /kv/<name>)
	decodeCtl  string // soflink on the decode node (POST /control/kv-fetch)
	client     *http.Client

	mu sync.Mutex // one prefill at a time: unified KV budget on the prefill engine

	tokMu sync.Mutex
	toks  map[string]tokEntry // body hash → prompt token ids (Count → Prefill reuse)
}

type tokEntry struct {
	ids []int
	at  time.Time
}

func newKVPipe(prefillURL, decodeURL, prefillCtl, decodeCtl string) *kvPipe {
	return &kvPipe{
		prefillURL: strings.TrimRight(prefillURL, "/"),
		decodeURL:  strings.TrimRight(decodeURL, "/"),
		prefillCtl: strings.TrimRight(prefillCtl, "/"),
		decodeCtl:  strings.TrimRight(decodeCtl, "/"),
		client:     &http.Client{Timeout: 600 * time.Second, Transport: gateway.PooledTransport()},
		toks:       map[string]tokEntry{},
	}
}

// templateFields are the chat fields that shape the templated prompt; the same
// subset goes to /apply-template so the ids match what the decode will build
// from the full request.
var templateFields = []string{"messages", "tools", "tool_choice", "chat_template_kwargs",
	"reasoning_format", "add_generation_prompt", "parallel_tool_calls"}

func templateBody(body gateway.Body) map[string]any {
	out := map[string]any{}
	for _, k := range templateFields {
		if v, ok := body[k]; ok {
			out[k] = v
		}
	}
	return out
}

// bodyHash keys the token cache by the template-relevant content only.
func bodyHash(body gateway.Body) string {
	b, _ := json.Marshal(templateBody(body))
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// postJSON posts a JSON body and decodes a JSON reply; a non-2xx status is an
// error carrying the first bytes of the reply (engine errors are JSON too).
func (k *kvPipe) postJSON(url string, body any, timeout time.Duration) (map[string]any, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c := k.client
	if timeout > 0 {
		c = &http.Client{Timeout: timeout, Transport: gateway.PooledTransport()}
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("%s: HTTP %d %s", url, resp.StatusCode, truncate(string(data), 200))
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("%s: %v", url, err)
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// tokens returns the prompt token ids of body as the engine sees it: the chat
// template applied by the prefill engine, then tokenized (add_special, like the
// engine's own chat path). Cached briefly so Count and Prefill share one trip.
func (k *kvPipe) tokens(body gateway.Body) ([]int, error) {
	h := bodyHash(body)
	k.tokMu.Lock()
	if e, ok := k.toks[h]; ok && time.Since(e.at) < tokCacheTTL {
		k.tokMu.Unlock()
		return e.ids, nil
	}
	k.tokMu.Unlock()

	tpl, err := k.postJSON(k.prefillURL+"/apply-template", templateBody(body), 60*time.Second)
	if err != nil {
		return nil, fmt.Errorf("apply-template: %w", err)
	}
	prompt, _ := tpl["prompt"].(string)
	if prompt == "" {
		return nil, fmt.Errorf("apply-template: empty prompt")
	}
	tk, err := k.postJSON(k.prefillURL+"/tokenize", map[string]any{
		"content": prompt, "add_special": true, "with_pieces": false}, 120*time.Second)
	if err != nil {
		return nil, fmt.Errorf("tokenize: %w", err)
	}
	raw, _ := tk["tokens"].([]any)
	ids := make([]int, 0, len(raw))
	for _, v := range raw {
		f, ok := v.(float64)
		if !ok {
			return nil, fmt.Errorf("tokenize: non-numeric token %v", v)
		}
		ids = append(ids, int(f))
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("tokenize: no tokens")
	}

	k.tokMu.Lock()
	if len(k.toks) >= tokCacheCap {
		for key, e := range k.toks { // drop stale first, else anything
			if time.Since(e.at) >= tokCacheTTL || len(k.toks) >= tokCacheCap {
				delete(k.toks, key)
			}
		}
	}
	k.toks[h] = tokEntry{ids: ids, at: time.Now()}
	k.tokMu.Unlock()
	return ids, nil
}

// DecodeBusy probes the decode engine's /slots: true when any slot is
// processing (a live request the handoff should protect). A failed probe reads
// as idle so a monitoring hiccup never forces the slower path.
func (k *kvPipe) DecodeBusy() bool {
	c := &http.Client{Timeout: 1500 * time.Millisecond, Transport: gateway.PooledTransport()}
	resp, err := c.Get(k.decodeURL + "/slots")
	if err != nil {
		return false
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var slots []map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&slots); err != nil {
		return false
	}
	for _, s := range slots {
		if b, _ := s["is_processing"].(bool); b {
			return true
		}
	}
	return false
}

// Count is the gateway's exact token counter (chat template applied).
func (k *kvPipe) Count(body gateway.Body) (int, error) {
	ids, err := k.tokens(body)
	if err != nil {
		return 0, err
	}
	return len(ids), nil
}

// Prefill processes the prompt (minus its last token) on the prefill engine
// and saves the slot state; the returned handoff_id is the state file name the
// decode node will pull. Metrics are returned as numbers for the request log.
func (k *kvPipe) Prefill(body gateway.Body, _ gateway.Headers) (gateway.Body, error) {
	ids, err := k.tokens(body)
	if err != nil {
		return nil, err
	}
	if len(ids) < 2 {
		return nil, fmt.Errorf("prompt too short for a handoff (%d tokens)", len(ids))
	}
	name := fmt.Sprintf("sf-%s-%d.bin", bodyHash(body)[:12], time.Now().UnixMilli())

	k.mu.Lock()
	defer k.mu.Unlock()

	t0 := time.Now()
	slotURL := fmt.Sprintf("%s/slots/%d", k.prefillURL, kvPrefillSlot)
	// whatever happens next, the prefill slot must not keep the sequence: its KV
	// budget is unified and a leftover would starve the following prefill.
	defer func() {
		if _, err := k.postJSON(slotURL+"?action=erase", map[string]any{}, 30*time.Second); err != nil {
			log.Printf("kvpipe: prefill erase: %v", err)
		}
	}()
	comp, err := k.postJSON(k.prefillURL+"/completion", map[string]any{
		"prompt": ids[:len(ids)-1], "n_predict": 0, "id_slot": kvPrefillSlot, "cache_prompt": true,
	}, 0)
	if err != nil {
		return nil, fmt.Errorf("prefill completion: %w", err)
	}
	prefillMS := msSince(t0)
	out := gateway.Body{"handoff_id": name, "tokens": len(ids), "prefill_ms": prefillMS}
	if tm, _ := comp["timings"].(map[string]any); tm != nil {
		if v, ok := tm["prompt_per_second"].(float64); ok {
			out["prefill_pp"] = v
		}
		if v, ok := tm["prompt_n"].(float64); ok {
			out["prefill_n"] = int(v)
		}
	}
	t1 := time.Now()
	sv, err := k.postJSON(slotURL+"?action=save", map[string]any{"filename": name}, 120*time.Second)
	if err != nil {
		return nil, fmt.Errorf("prefill save: %w", err)
	}
	out["save_ms"] = msSince(t1)
	if v, ok := sv["n_written"].(float64); ok {
		out["state_bytes"] = int64(v)
	}
	if v, ok := sv["n_saved"].(float64); ok {
		out["n_saved"] = int(v)
	}
	return out, nil
}

// Handoff pulls the state from the prefill node into the decode node (its
// soflink writes it into the decode engine's --slot-save-path) and restores it
// into slot on the decode engine. Both copies are deleted afterwards
// (best-effort, asynchronous).
func (k *kvPipe) Handoff(hid, slot string) (gateway.Body, error) {
	if !validStateName(hid) {
		return nil, fmt.Errorf("invalid state name %q", hid)
	}
	src := k.prefillCtl + "/kv/" + hid
	t0 := time.Now()
	ft, err := k.postJSON(k.decodeCtl+"/control/kv-fetch", map[string]any{"url": src, "name": hid}, 300*time.Second)
	if err != nil {
		k.cleanup(hid)
		return nil, fmt.Errorf("kv-fetch: %w", err)
	}
	out := gateway.Body{"fetch_ms": msSince(t0)}
	if v, ok := ft["bytes"].(float64); ok {
		out["fetch_bytes"] = int64(v)
	}
	t1 := time.Now()
	rs, err := k.postJSON(fmt.Sprintf("%s/slots/%s?action=restore", k.decodeURL, slot),
		map[string]any{"filename": hid}, 120*time.Second)
	k.cleanup(hid)
	if err != nil {
		return nil, fmt.Errorf("decode restore: %w", err)
	}
	out["restore_ms"] = msSince(t1)
	if v, ok := rs["n_restored"].(float64); ok {
		out["n_restored"] = int(v)
	}
	return out, nil
}

// cleanup deletes the shipped state on both nodes (best-effort, async): the
// restored KV lives in the decode slot now, the file has no further use.
func (k *kvPipe) cleanup(hid string) {
	go func() {
		for _, base := range []string{k.decodeCtl, k.prefillCtl} {
			req, err := http.NewRequest(http.MethodDelete, base+"/kv/"+hid, nil)
			if err != nil {
				continue
			}
			if resp, err := k.client.Do(req); err == nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
			}
		}
	}()
}

func msSince(t time.Time) float64 {
	return float64(time.Since(t).Microseconds()) / 1000
}

// sseTimings extracts the engine "timings" object from the tail of an SSE
// chat stream (llama-server attaches it to the final chunk), so the streaming
// path can Finish the request record like the JSON path does. Returns nil when
// the stream carried none.
func sseTimings(tail []byte) map[string]any {
	var found map[string]any
	for _, line := range bytes.Split(tail, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if !bytes.Contains(payload, []byte(`"timings"`)) {
			continue
		}
		var obj map[string]any
		if json.Unmarshal(payload, &obj) != nil {
			continue
		}
		if tm, ok := obj["timings"].(map[string]any); ok {
			found = tm
		}
	}
	return found
}
