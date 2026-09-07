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
//	         Concurrent up to its slots AND its unified KV budget (acquireSlot).
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
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Custok/sofmat/internal/gateway"
)

const (
	tokCacheTTL = 3 * time.Minute
	tokCacheCap = 64
	// prefillSlots: how many prompts the prefill engine may ingest at once. Its
	// KV budget is unified across slots, so concurrency is ALSO capped by
	// prefillBudgetUse below — two 40k prompts fit in 100k, three do not.
	prefillSlots     = 4
	prefillBudgetUse = 0.80
	// prefillWait is how long a prompt waits for room on the prefill engine
	// before the gateway gives up on the handoff (and serves it decode-direct).
	//
	// It must stay SMALLER than what the prefill path saves, or waiting is a
	// guaranteed loss. At these sizes the saving is a few seconds (34k tokens:
	// ~25 s direct vs ~21 s through the prefill), so a long queue can only make
	// things worse. Measured with 45 s here: a 34 409-token request waited the
	// full 45 s for room on an engine that was busy generating, gave up, and the
	// decode then did the work in 22 s — 76 s total for 31 s of actual work, to
	// chase a 4 s saving. Five seconds absorbs a transient blip; beyond that,
	// going direct is simply the better trade.
	prefillWait = 5 * time.Second
)

// kvTransport bounds the DIAL to the prefill/decode nodes: a host that is down
// (not just a closed port) must cost a couple of seconds once — the gateway's
// breaker then keeps the route closed — not a TCP timeout per long request.
var kvTransport = &http.Transport{
	DialContext:         (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	MaxIdleConns:        32,
	MaxIdleConnsPerHost: 8,
	IdleConnTimeout:     90 * time.Second,
}

// kvRoute is which engine does what for ONE request. The roles are not fixed
// any more: the prefill runs on the engine where the conversation does NOT
// live, so the request never lands on a card that is already generating for it,
// and the state is restored into the conversation's own engine so it keeps its
// cache. With the roles nailed to the config, the conversations living in the
// second engine could never take the handoff at all (they were served direct on
// the very engine that was prefilling for everyone else: 3 of 4 such requests
// came back empty, 52-160 s — live, 2026-09-07).
type kvRoute struct {
	prefillURL, prefillCtl string
	decodeURL, decodeCtl   string
}

// enginePool is the prefill slot bookkeeping of ONE engine (every engine can be
// asked to prefill now, so the pool cannot be global any more).
type enginePool struct {
	free     []int
	inflight int
	budget   int
}

type kvPipe struct {
	prefillURL string // prefill llama-server
	decodeURL  string // decode llama-server
	prefillCtl string // soflink on the prefill node (serves GET/DELETE /kv/<name>)
	decodeCtl  string // soflink on the decode node (POST /control/kv-fetch)
	client     *http.Client

	// An engine ingests several prompts at once, bounded by its slots AND by its
	// unified KV budget: serializing everything (the first design) made three
	// cold prompts queue 25 s each while 3 of its 4 slots idled.
	slotMu sync.Mutex
	pools  map[string]*enginePool // engine URL -> its prefill slots and budget
	routes map[string]kvRoute     // handoff id -> the route that produced it

	// resolve picks the route for a request (injected by the server, which knows
	// where each conversation lives). nil = use the configured default route.
	resolve func(gateway.Body) (kvRoute, bool)

	// what the prefill ENGINE's slots really hold. It also serves decode traffic
	// (decode2 and prefill are the same llama-server with two roles) and both
	// share ONE unified budget, so reserving prefill room without counting the
	// decode side overshoots exactly like the decode-side bug did.
	heldMu  sync.Mutex
	pHeld   map[string]int
	pHeldAt map[string]time.Time

	tokMu sync.Mutex
	toks  map[string]tokEntry // body hash → prompt token ids (Count → Prefill reuse)
	pend  map[string]int      // handoff_id → tokens of the saved state (room to free)
}

type tokEntry struct {
	ids []int
	at  time.Time
}

func newKVPipe(prefillURL, decodeURL, prefillCtl, decodeCtl string) *kvPipe {
	k := &kvPipe{
		prefillURL: strings.TrimRight(prefillURL, "/"),
		decodeURL:  strings.TrimRight(decodeURL, "/"),
		prefillCtl: strings.TrimRight(prefillCtl, "/"),
		decodeCtl:  strings.TrimRight(decodeCtl, "/"),
		client:     &http.Client{Timeout: 600 * time.Second, Transport: kvTransport},
		toks:       map[string]tokEntry{},
		pend:       map[string]int{},
		pools:      map[string]*enginePool{},
		routes:     map[string]kvRoute{},
		pHeld:      map[string]int{},
		pHeldAt:    map[string]time.Time{},
	}
	return k
}

// defaultRoute is the configured pair, used when nobody resolves a route.
func (k *kvPipe) defaultRoute() kvRoute {
	return kvRoute{prefillURL: k.prefillURL, prefillCtl: k.prefillCtl,
		decodeURL: k.decodeURL, decodeCtl: k.decodeCtl}
}

// routeFor picks the route for this request, falling back to the configured
// pair. A route whose two ends are the SAME engine is no route at all: the
// prompt would be "handed off" to the very engine that is going to serve it, so
// the caller is told to serve it direct instead.
func (k *kvPipe) routeFor(body gateway.Body) (kvRoute, error) {
	rt := k.defaultRoute()
	if k.resolve != nil {
		if r, ok := k.resolve(body); ok {
			rt = r
		}
	}
	if rt.prefillURL == "" || rt.decodeURL == "" {
		return rt, fmt.Errorf("%w: no hay ruta de prefill", gateway.ErrSkipHandoff)
	}
	if rt.prefillURL == rt.decodeURL {
		return rt, fmt.Errorf("%w: el prompt ya vive en el motor que lo va a servir", gateway.ErrSkipHandoff)
	}
	return rt, nil
}

// poolOf is the slot bookkeeping of one engine, created on first use.
func (k *kvPipe) poolOf(url string) *enginePool {
	p := k.pools[url]
	if p == nil {
		p = &enginePool{budget: defaultCtx}
		for i := 0; i < prefillSlots; i++ {
			p.free = append(p.free, i)
		}
		k.pools[url] = p
	}
	return p
}

// noteRoute remembers which route produced a state, so the handoff and the
// engine pick that follow it use the same pair.
func (k *kvPipe) noteRoute(name string, rt kvRoute, tokens int) {
	k.slotMu.Lock()
	if len(k.routes) > 64 {
		k.routes = map[string]kvRoute{}
		k.pend = map[string]int{}
	}
	k.routes[name] = rt
	k.pend[name] = tokens
	k.slotMu.Unlock()
}

func (k *kvPipe) routeOf(name string) (kvRoute, bool) {
	k.slotMu.Lock()
	defer k.slotMu.Unlock()
	rt, ok := k.routes[name]
	return rt, ok
}

// DecodeURLFor is where a handed-off request must be served: the engine its
// state was restored into.
func (k *kvPipe) DecodeURLFor(hid string) string {
	if rt, ok := k.routeOf(hid); ok {
		return rt.decodeURL
	}
	return k.decodeURL
}

// acquireSlot reserves a prefill slot and room in the engine's KV budget for a
// prompt of estTokens. Returns the slot and its release; ok=false when no room
// came free within prefillWait (the caller then falls back to decode-direct).
func (k *kvPipe) acquireSlot(engine string, estTokens int) (slot int, release func(), ok bool) {
	deadline := time.Now().Add(prefillWait)
	for {
		held := k.engineHeld(engine) // HTTP: never under slotMu
		k.slotMu.Lock()
		p := k.poolOf(engine)
		room := int(float64(p.budget) * prefillBudgetUse)
		used := p.inflight
		if held > used {
			// the engine may also be serving decode traffic; that cache occupies
			// the same unified budget as the prefill sequences
			used = held
		}
		if len(p.free) > 0 && (used+estTokens <= room || used == 0) {
			slot = p.free[0]
			p.free = p.free[1:]
			p.inflight += estTokens
			k.slotMu.Unlock()
			return slot, func() {
				k.slotMu.Lock()
				q := k.poolOf(engine)
				q.free = append(q.free, slot)
				q.inflight -= estTokens
				if q.inflight < 0 {
					q.inflight = 0
				}
				k.slotMu.Unlock()
				k.heldMu.Lock()
				delete(k.pHeldAt, engine) // the picture changed: re-read
				k.heldMu.Unlock()
			}, true
		}
		k.slotMu.Unlock()
		if time.Now().After(deadline) {
			return 0, func() {}, false
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// notePending records how many tokens a saved state carries, so the handoff can
// make room for exactly that much in the decode before restoring it.
func (k *kvPipe) notePending(name string, tokens int) {
	k.slotMu.Lock()
	if len(k.pend) > 32 {
		k.pend = map[string]int{}
	}
	k.pend[name] = tokens
	k.slotMu.Unlock()
}

func (k *kvPipe) takePending(name string) int {
	k.slotMu.Lock()
	defer k.slotMu.Unlock()
	n := k.pend[name]
	delete(k.pend, name)
	return n
}

// makeDecodeRoom frees room in the decode's UNIFIED budget for a state of need
// tokens and returns the slot to restore into.
//
// Why it exists: decodeRoom() is checked BEFORE the prefill, but the prefill
// takes ~30 s and the other clients keep filling the decode meanwhile. Record
// id 15 (2026-09-07) paid 26 s of prefill, got "No available space in KV cache"
// at the restore and the decode then reprocessed 58 102 tokens: 113 s for a
// request that should have taken 30. So the room is re-made here, at the last
// possible moment, evicting idle slots cheapest-first (least valuable cache) —
// and when even that is not enough the handoff is SKIPPED cleanly instead of
// failing, so the request is served direct and the breaker stays closed.
func (k *kvPipe) makeDecodeRoom(decodeURL string, need int, want string) (string, error) {
	slots := k.slotsAt(decodeURL)
	if slots == nil {
		return want, nil // cannot read: behave as before
	}
	k.slotMu.Lock()
	budget := k.poolOf(decodeURL).budget
	k.slotMu.Unlock()
	if budget <= 0 {
		budget = defaultCtx
	}
	type slotInfo struct {
		id   string
		held int
	}
	held, idle := 0, []slotInfo(nil)
	for _, s := range slots {
		n := 0
		if v, isNum := s["n_prompt_tokens"].(float64); isNum {
			n = int(v)
		}
		id := ""
		if v, isNum := s["id"].(float64); isNum {
			id = fmt.Sprintf("%d", int(v))
		}
		if busy, _ := s["is_processing"].(bool); busy || id == "" {
			held += n // generating: untouchable
			continue
		}
		held += n // erasable below, and subtracted as we erase
		idle = append(idle, slotInfo{id, n})
	}
	if len(idle) == 0 {
		return "", fmt.Errorf("%w: todos los slots del decode están generando", gateway.ErrSkipHandoff)
	}
	sort.Slice(idle, func(i, j int) bool { return idle[i].held < idle[j].held })
	// restore into the wanted slot when it is idle, else into the cheapest one
	target := idle[0]
	for _, s := range idle {
		if s.id == want {
			target = s
			break
		}
	}
	erase := func(id string) {
		if _, err := k.postJSON(fmt.Sprintf("%s/slots/%s?action=erase", decodeURL, id),
			map[string]any{}, 30*time.Second); err != nil {
			log.Printf("kvpipe: erase slot %s: %v", id, err)
			return
		}
		for i := range idle {
			if idle[i].id == id {
				held -= idle[i].held
				idle[i].held = 0
			}
		}
	}
	erase(target.id) // always: the restore needs its own slot empty
	for _, s := range idle {
		if held+need <= budget {
			break
		}
		if s.id == target.id || s.held == 0 {
			continue
		}
		erase(s.id)
	}
	if held+need > budget {
		return "", fmt.Errorf("%w: el decode sigue lleno tras vaciar los slots libres (%d ocupados + %d que hacen falta > %d)",
			gateway.ErrSkipHandoff, held, need, budget)
	}
	return target.id, nil
}

// setEngineBudget records one engine's real context size (probed at startup).
func (k *kvPipe) setEngineBudget(engine string, n int) {
	if n <= 0 || engine == "" {
		return
	}
	k.slotMu.Lock()
	k.poolOf(engine).budget = n
	k.slotMu.Unlock()
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
		c = &http.Client{Timeout: timeout, Transport: kvTransport}
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

// decodeSlots reads the decode engine's /slots (nil on any failure).
func (k *kvPipe) decodeSlots() []map[string]any { return k.slotsAt(k.decodeURL) }

// engineHeld is what an engine's slots hold that a new prompt cannot have,
// re-read at most once a second per engine. Any engine can be asked to prefill
// AND to decode, and both share ONE unified KV budget, so reserving prefill
// room without counting the decode side overshoots (see committedKV).
func (k *kvPipe) engineHeld(engine string) int {
	k.heldMu.Lock()
	defer k.heldMu.Unlock()
	if time.Since(k.pHeldAt[engine]) < time.Second {
		return k.pHeld[engine]
	}
	slots := k.slotsAt(engine)
	if slots == nil {
		// unreadable: keep the last reading (never assume empty) but stamp the
		// time anyway, or a dead engine gets dialled on every poll of the wait
		// loop instead of once a second.
		k.pHeldAt[engine] = time.Now()
		return k.pHeld[engine]
	}
	held := committedKV(slots)
	k.pHeld[engine], k.pHeldAt[engine] = held, time.Now()
	return held
}

func (k *kvPipe) slotsAt(base string) []map[string]any {
	c := &http.Client{Timeout: 1500 * time.Millisecond, Transport: kvTransport}
	resp, err := c.Get(base + "/slots")
	if err != nil {
		return nil
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var slots []map[string]any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&slots); err != nil {
		return nil
	}
	return slots
}

// decodeRoom reads what the decode engine's slots actually HOLD (llama.cpp keeps
// a slot's prompt cache after the request ends, so occupancy is not the same as
// "requests in flight") and returns the tokens still free once the cheapest idle
// slot is evicted, plus that slot. Returns ok=false when /slots cannot be read.
//
// Why it matters: a restore needs room in the engine's UNIFIED budget. Without
// this check the gateway spent a full prefill (27 s for 50k tokens) and only
// then got "Unable to restore slot: No available space in KV cache", so the
// decode re-processed the whole prompt: the worst of both paths.
func (k *kvPipe) decodeRoom(decodeURL string) (free int, evict string, evictHeld int, ok bool) {
	slots := k.slotsAt(decodeURL)
	if slots == nil {
		return 0, "", 0, false
	}
	// only the slots that are GENERATING hold KV we cannot take: the idle ones
	// are erased by makeDecodeRoom right before the restore.
	held := 0
	evictHeld = -1
	idle := 0
	for _, s := range slots {
		n := 0
		if v, isNum := s["n_prompt_tokens"].(float64); isNum {
			n = int(v)
		}
		busy, _ := s["is_processing"].(bool)
		if busy {
			held += n
			continue
		}
		idle++
		if evictHeld < 0 || n < evictHeld {
			evictHeld = n
			if v, isNum := s["id"].(float64); isNum {
				evict = fmt.Sprintf("%d", int(v))
			}
		}
	}
	if idle == 0 {
		return 0, "", 0, true // every slot generating: no room for a restore
	}
	if evictHeld < 0 {
		evictHeld = 0
	}
	k.slotMu.Lock()
	budget := k.poolOf(decodeURL).budget
	k.slotMu.Unlock()
	if budget <= 0 {
		budget = defaultCtx
	}
	return budget - held, evict, evictHeld, true
}

// DecodeBusy probes the decode engine's /slots: true when any slot is
// processing (a live request the handoff should protect). A failed probe reads
// as idle so a monitoring hiccup never forces the slower path.
func (k *kvPipe) DecodeBusy() bool {
	for _, s := range k.decodeSlots() {
		if b, _ := s["is_processing"].(bool); b {
			return true
		}
	}
	return false
}

// pickIdleSlot returns want when that slot is idle (or the probe fails), else
// the first idle slot; with every slot busy it keeps want (the restore queues).
func (k *kvPipe) pickIdleSlot(decodeURL, want string) string {
	slots := k.slotsAt(decodeURL)
	if slots == nil {
		return want
	}
	firstIdle := ""
	for _, s := range slots {
		id := ""
		if v, ok := s["id"].(float64); ok {
			id = fmt.Sprintf("%d", int(v))
		}
		busy, _ := s["is_processing"].(bool)
		if id == want && !busy {
			return want
		}
		if !busy && firstIdle == "" && id != "" {
			firstIdle = id
		}
	}
	if firstIdle != "" {
		return firstIdle
	}
	return want
}

// Tokens is the gateway's exact tokenizer (chat template applied): enables the
// cache-aware estimate (new tokens = total − common prefix with the last turn).
func (k *kvPipe) Tokens(body gateway.Body) ([]int, error) {
	return k.tokens(body)
}

// Count is the gateway's exact token counter (chat template applied).
func (k *kvPipe) Count(body gateway.Body) (int, error) {
	ids, err := k.tokens(body)
	if err != nil {
		return 0, err
	}
	return len(ids), nil
}

// ctlReachable checks that a node's soflink answers its discovery hello: the
// state cannot travel without the prefill node's /kv and the decode node's
// /control/kv-fetch, so this is asked BEFORE spending the prefill (measured:
// 33 s of prefill wasted when the prefill node's soflink was down after a
// reboot and the fetch failed afterwards).
func (k *kvPipe) ctlReachable(base string) error {
	c := &http.Client{Timeout: 1500 * time.Millisecond, Transport: kvTransport}
	resp, err := c.Get(base + "/soflink/hello")
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s/soflink/hello: HTTP %d", base, resp.StatusCode)
	}
	return nil
}

// Prefill processes the prompt (minus its last token) on the prefill engine
// and saves the slot state; the returned handoff_id is the state file name the
// decode node will pull. Metrics are returned as numbers for the request log.
func (k *kvPipe) Prefill(body gateway.Body, _ gateway.Headers) (gateway.Body, error) {
	rt, err := k.routeFor(body)
	if err != nil {
		return nil, err
	}
	if err := k.ctlReachable(rt.prefillCtl); err != nil {
		return nil, fmt.Errorf("prefill soflink unreachable: %w", err)
	}
	if err := k.ctlReachable(rt.decodeCtl); err != nil {
		return nil, fmt.Errorf("decode soflink unreachable: %w", err)
	}
	ids, err := k.tokens(body)
	if err != nil {
		return nil, err
	}
	if len(ids) < 2 {
		return nil, fmt.Errorf("prompt too short for a handoff (%d tokens)", len(ids))
	}
	// the decode must be able to host the restored state; checking AFTER the
	// prefill throws away all of that work (see decodeRoom).
	if free, _, _, ok := k.decodeRoom(rt.decodeURL); ok && free < len(ids) {
		return nil, fmt.Errorf("%w: el decode no tiene sitio (%d libres, hacen falta %d)",
			gateway.ErrSkipHandoff, free, len(ids))
	}

	name := fmt.Sprintf("sf-%s-%d.bin", bodyHash(body)[:12], time.Now().UnixMilli())

	slot, releaseSlot, ok := k.acquireSlot(rt.prefillURL, len(ids))
	if !ok {
		return nil, fmt.Errorf("%w: el prefill no tiene sitio para %d tokens en %s",
			gateway.ErrSkipHandoff, len(ids), prefillWait)
	}
	defer releaseSlot()

	t0 := time.Now()
	slotURL := fmt.Sprintf("%s/slots/%d", rt.prefillURL, slot)
	// whatever happens next, the prefill slot must not keep the sequence: its KV
	// budget is unified and a leftover would starve the following prefill.
	defer func() {
		if _, err := k.postJSON(slotURL+"?action=erase", map[string]any{}, 30*time.Second); err != nil {
			log.Printf("kvpipe: prefill erase: %v", err)
		}
	}()
	// empty it BEFORE processing too: the KV is unified, so its leftover cells
	// count against this prompt (llama-server answers HTTP 500 "Context size has
	// been exceeded" when they do).
	if _, err := k.postJSON(slotURL+"?action=erase", map[string]any{}, 30*time.Second); err != nil {
		log.Printf("kvpipe: erase previo al prefill (slot %d): %v", slot, err)
	}
	comp, err := k.postJSON(rt.prefillURL+"/completion", map[string]any{
		"prompt": ids[:len(ids)-1], "n_predict": 0, "id_slot": slot, "cache_prompt": true,
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
	k.noteRoute(name, rt, len(ids))
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
	rt, ok := k.routeOf(hid)
	if !ok {
		rt = k.defaultRoute()
	}
	src := rt.prefillCtl + "/kv/" + hid
	t0 := time.Now()
	ft, err := k.postJSON(rt.decodeCtl+"/control/kv-fetch", map[string]any{"url": src, "name": hid}, 300*time.Second)
	if err != nil {
		k.cleanup(hid)
		return nil, fmt.Errorf("kv-fetch: %w", err)
	}
	out := gateway.Body{"fetch_ms": msSince(t0)}
	if v, ok := ft["bytes"].(float64); ok {
		out["fetch_bytes"] = int64(v)
	}
	// draft-context sidecar (engine patched with docs/patches/llama-*-slot-save-dft):
	// best-effort — an unpatched prefill has none and the decode then drafts cold,
	// exactly as before. Recorded so the request log shows whether it travelled.
	out["dft"] = false
	if sd, err := k.postJSON(rt.decodeCtl+"/control/kv-fetch",
		map[string]any{"url": src + ".dft", "name": hid + ".dft"}, 120*time.Second); err == nil {
		out["dft"] = true
		if v, ok := sd["bytes"].(float64); ok {
			out["dft_bytes"] = int64(v)
		}
	}
	// a restore into a slot that is generating queues behind that stream (measured:
	// 31 s instead of 0.2 s for a 32k state) — prefer an idle slot, and free
	// enough of the unified budget for the state RIGHT NOW (the decode filled up
	// while the prefill ran; llama.cpp does not evict on its own for a restore).
	need := k.takePending(hid)
	slot, err = k.makeDecodeRoom(rt.decodeURL, need, k.pickIdleSlot(rt.decodeURL, slot))
	if err != nil {
		k.cleanup(hid)
		return nil, err
	}
	out["slot"] = slot
	t1 := time.Now()
	rs, err := k.postJSON(fmt.Sprintf("%s/slots/%s?action=restore", rt.decodeURL, slot),
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
			for _, name := range []string{hid, hid + ".dft"} {
				req, err := http.NewRequest(http.MethodDelete, base+"/kv/"+name, nil)
				if err != nil {
					continue
				}
				if resp, err := k.client.Do(req); err == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					resp.Body.Close()
				}
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
