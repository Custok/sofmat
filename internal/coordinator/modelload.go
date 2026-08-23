package coordinator

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Custok/sofmat/internal/gguf"
)

// modelLayers reads a gguf's block_count (layer count) from its header, so an
// individual load shows its real layers instead of "0 capas".
func modelLayers(name string) int {
	meta, err := gguf.ReadMetadata(filepath.Join(modelsDir(), name))
	if err != nil {
		return 0
	}
	arch, _ := meta["general.architecture"].(string)
	if arch == "" {
		return 0
	}
	switch n := meta[arch+".block_count"].(type) {
	case uint32:
		return int(n)
	case int64:
		return int(n)
	case uint64:
		return int(n)
	case int:
		return n
	case float64:
		return int(n)
	}
	return 0
}

// loadedReg tracks models this coordinator has launched, so the panel shows each
// as its own INSTANCE (served on its own endpoint) — separate from the config
// roles, and never touching the HUD's decode.
type loadedModel struct {
	Endpoint string `json:"endpoint"`
	Model    string `json:"model"`
	Node     string `json:"node"`
	Mode     string `json:"mode"`
	Layers   int    `json:"layers"`
}

var loadedReg = struct {
	mu     sync.Mutex
	models map[string]loadedModel
}{models: map[string]loadedModel{}}

func recordLoaded(m loadedModel) {
	loadedReg.mu.Lock()
	loadedReg.models[m.Endpoint] = m
	loadedReg.mu.Unlock()
}

// nextLoadPort assigns loaded models CONSECUTIVE ports starting at 1358 (right
// after the coordinator's :1357), skipping ones already taken by a live load.
func nextLoadPort() int {
	used := map[int]bool{}
	for _, m := range loadedModels() {
		if i := strings.LastIndex(m.Endpoint, ":"); i >= 0 {
			if p, _ := strconv.Atoi(m.Endpoint[i+1:]); p > 0 {
				used[p] = true
			}
		}
	}
	for p := 1358; p < 1400; p++ {
		if !used[p] {
			return p
		}
	}
	return 1358
}

// dropLoaded removes a loaded-model record (an ejected or dead instance).
func dropLoaded(endpoint string) {
	loadedReg.mu.Lock()
	delete(loadedReg.models, endpoint)
	loadedReg.mu.Unlock()
}

// setLoadedLayers fills a loaded model's layer count from its gguf header in the
// background, so the launch response returns instantly.
func setLoadedLayers(endpoint, name string) {
	defer recoverProbe()
	n := modelLayers(name)
	if n == 0 {
		return
	}
	loadedReg.mu.Lock()
	if m, ok := loadedReg.models[endpoint]; ok {
		m.Layers = n
		loadedReg.models[endpoint] = m
	}
	loadedReg.mu.Unlock()
}

func loadedModels() []loadedModel {
	loadedReg.mu.Lock()
	defer loadedReg.mu.Unlock()
	out := make([]loadedModel, 0, len(loadedReg.models))
	for _, m := range loadedReg.models {
		out = append(out, m)
	}
	return out
}

// gpuPlacement returns the freest GPU index and a --tensor-split weighted by FREE
// VRAM (GPUs under ~2 GB free get 0 layers), so an individual model lands on the
// GPU(s) with room and never onto one a prod model already fills (node-a's GPU0
// with LM Studio). split is empty for single-GPU or unified-memory nodes.
func (s *Server) gpuPlacement(nodeID string) (mainGPU int, split string) {
	mainGPU = -1
	base := s.agentBase(nodeID)
	if base == "" {
		return -1, ""
	}
	gpus := asMaps(s.getJSONFrom(base, "/gpu")["gpus"])
	if len(gpus) == 0 {
		return -1, ""
	}
	parts := make([]string, len(gpus))
	bestFree := -1.0
	anyRoom := false
	for i, g := range gpus {
		total := pnum(g["total_mb"])
		free := total - pnum(g["used_mb"])
		// A GPU more than ~70% full (or with <2 GB free) is treated as
		// prod-occupied and gets 0 layers, so a load never crowds the GPU a prod
		// model already fills (node-a's GPU0 with LM Studio).
		if free < 2000 || (total > 0 && free < total*0.30) {
			parts[i] = "0"
		} else {
			parts[i] = strconv.Itoa(int(free / 1024)) // weight ≈ GB free
			anyRoom = true
		}
		if free > bestFree {
			bestFree, mainGPU = free, pint(g["idx"])
		}
	}
	// The engine may enumerate GPUs in the reverse of nvidia-smi order (Vulkan
	// build on node-a): remap the placement to the engine's indices so the free
	// GPU — not the prod-full one — actually gets the layers.
	if s.cfg.GPUIndexReversed && len(gpus) > 1 {
		mainGPU = len(gpus) - 1 - mainGPU
		for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
			parts[i], parts[j] = parts[j], parts[i]
		}
	}
	if len(gpus) > 1 && anyRoom {
		split = strings.Join(parts, ",")
	}
	return
}

// Load a downloaded model with an operator-chosen placement: which node(s), which
// is main, and whether it runs INDIVIDUAL (one node) or as a UNIÓN CON ROLES (a
// multi-node pipeline: main llama-server + rpc-server workers, weights split by
// --tensor-split). This builds the launch spec ({exe,args}) and, when the main
// node exposes a control agent (node-agent /control/load, today node-a), fires
// it; otherwise it returns the plan for review. It never touches the coordinator's
// own decode endpoint, so loading a model here can't hijack the live HUD.

// llamaExePath is the host llama-server the control agent is allowlisted to
// launch. Real machine path is provided by config.local.json (gitignored) or the
// SOFMAT_LLAMA env; the default is a bare PATH lookup, so no host path ships.
func (s *Server) llamaExePath() string {
	if e := os.Getenv("SOFMAT_LLAMA"); e != "" {
		return e
	}
	if s.cfg.LlamaExe != "" {
		return s.cfg.LlamaExe
	}
	// Auto-discover so the user never has to configure it: look next to the binary
	// and in common bundle sub-dirs, then a known local build path, then PATH.
	name := "llama-server"
	if runtime.GOOS == "windows" {
		name = "llama-server.exe"
	}
	cands := []string{}
	if exe, err := os.Executable(); err == nil {
		d := filepath.Dir(exe)
		cands = append(cands,
			filepath.Join(d, name),
			filepath.Join(d, "inference", name),
			filepath.Join(d, "llama", name),
			filepath.Join(d, "bin", name),
		)
	}
	if home, err := os.UserHomeDir(); err == nil {
		cands = append(cands, filepath.Join(home, ".docker", "bin", "inference", name))
	}
	for _, c := range cands {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return name
}

// rpcExePath is the host ggml-rpc-server a worker node launches to join a UNIÓN
// pipeline. From SOFMAT_RPC / config.local.json (rpc_exe); auto-discovered next to
// the binary and in common bundle sub-dirs; else a bare PATH lookup, so no host
// path ships. TODO(fleet-load): actually spawn this on each worker node's control
// plane (:1357 /control/rpc) when launching a multi-node UNIÓN — today modelsLoad's
// union branch only returns the plan (workers assumed already running).
func (s *Server) rpcExePath() string {
	if e := os.Getenv("SOFMAT_RPC"); e != "" {
		return e
	}
	if s.cfg.RpcExe != "" {
		return s.cfg.RpcExe
	}
	name := "ggml-rpc-server"
	if runtime.GOOS == "windows" {
		name = "ggml-rpc-server.exe"
	}
	cands := []string{}
	if exe, err := os.Executable(); err == nil {
		d := filepath.Dir(exe)
		cands = append(cands,
			filepath.Join(d, name),
			filepath.Join(d, "inference", name),
			filepath.Join(d, "llama", name),
			filepath.Join(d, "bin", name),
		)
	}
	for _, c := range cands {
		if fi, err := os.Stat(c); err == nil && !fi.IsDir() {
			return c
		}
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return name
}

// hostModelsDir is where the HOST sees the downloaded models (the host-side path
// the launch -m flag needs, distinct from the container's write path). From
// config.local.json / SOFMAT_MODELS_HOST; defaults to the models dir.
func (s *Server) hostModelsDir() string {
	if e := os.Getenv("SOFMAT_MODELS_HOST"); e != "" {
		return e
	}
	if s.cfg.ModelsHostDir != "" {
		return s.cfg.ModelsHostDir
	}
	return modelsDir()
}

// rpcPort is the ggml-rpc-server port workers listen on for a unión pipeline.
const rpcPort = 50052

type loadNode struct {
	ID   string `json:"id"`
	GPUs int    `json:"gpus"`
}

type loadReq struct {
	File   string     `json:"file"`
	Mode   string     `json:"mode"` // individual | union
	Nodes  []loadNode `json:"nodes"`
	Main   string     `json:"main"`
	Ctx    int        `json:"ctx"`
	Port   int        `json:"port"`
	NGL    int        `json:"ngl"`
	DryRun bool       `json:"dry_run"`
}

// agentBase returns a node's control/sensor agent base URL from the config.
func (s *Server) agentBase(id string) string {
	for _, n := range s.cfg.Nodes {
		if n.ID == id {
			return n.Agent
		}
	}
	return ""
}

// hasControlAgent reports whether a node exposes the node-agent control plane
// (/control/load). Today that's the Windows node-agent on :50060; the DGX daemon
// (:1357) and the Linux sensors don't launch llama-server.
func hasControlAgent(agent string) bool {
	a := strings.TrimRight(agent, "/")
	// :1357 = a node running the soflink daemon (control now built in); :50060 =
	// the legacy standalone node-agent.
	return strings.HasSuffix(a, ":50060") || strings.HasSuffix(a, ":1357")
}

// modelsLoad builds the launch spec for the chosen placement and, when it can,
// fires it on the main node.
func (s *Server) modelsLoad(w http.ResponseWriter, r *http.Request) {
	var req loadReq
	if json.NewDecoder(r.Body).Decode(&req) != nil || req.File == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "file requerido"})
		return
	}
	// req.File is a path RELATIVE to the models dir: either a legacy flat
	// "model.gguf" or a per-model "folder/model.gguf". Preserve the subfolder
	// (filepath.Base would drop it and break folder-based models), while
	// rejecting traversal so a client can't reach outside the models dir.
	rel := strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(req.File)), "/")
	if parts := strings.Split(rel, "/"); rel == "" || strings.Contains(rel, "..") || len(parts) > 2 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "nombre de modelo inválido: " + req.File})
		return
	}
	name := filepath.FromSlash(rel)
	// only a real downloaded model may be launched.
	if _, err := os.Stat(filepath.Join(modelsDir(), name)); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "modelo no está descargado: " + name})
		return
	}
	if req.Mode == "" {
		req.Mode = "individual"
	}
	if req.Ctx <= 0 {
		req.Ctx = 8192
	}
	if req.Port <= 0 {
		req.Port = nextLoadPort()
	}
	if req.NGL <= 0 {
		req.NGL = 99
	}
	if req.Main == "" && len(req.Nodes) > 0 {
		req.Main = req.Nodes[0].ID
	}
	if req.Main == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "elige un nodo main"})
		return
	}

	modelPath := filepath.Join(s.hostModelsDir(), filepath.FromSlash(name)) // resolves the per-model subfolder
	alias := strings.TrimSuffix(filepath.Base(name), ".gguf")

	args := []string{
		"-m", modelPath,
		"--host", "0.0.0.0",
		"--port", strconv.Itoa(req.Port),
		"-ngl", strconv.Itoa(req.NGL),
		"-c", strconv.Itoa(req.Ctx),
		"--jinja",
		"-a", alias,
	}
	// land on the freest GPU of the main node so a prod-filled GPU can't OOM the
	// load (the crash we saw when Vulkan grabbed the busy GPU0).
	// Individual: place weights on the node's GPUs weighted by FREE VRAM, so the
	// model never piles onto a GPU a prod model already fills (node-a's GPU0 with
	// LM Studio) — a near-full GPU gets 0 layers. Union builds its cross-node split below.
	if req.NGL > 0 && req.Mode != "union" {
		if mg, split := s.gpuPlacement(req.Main); mg >= 0 {
			args = append(args, "--main-gpu", strconv.Itoa(mg))
			if split != "" {
				args = append(args, "--tensor-split", split)
			}
		}
	}

	plan := map[string]any{"mode": req.Mode, "main": req.Main, "model": name}

	if req.Mode == "union" {
		// Union con roles: main hosts the front + its shard; the other selected
		// nodes run rpc-server and receive shards. Weight split ∝ declared GPUs.
		workers := []string{}
		split := []string{}
		totalGPU := 0
		for _, n := range req.Nodes {
			totalGPU += maxi(n.GPUs, 1)
		}
		for _, n := range req.Nodes {
			g := maxi(n.GPUs, 1)
			split = append(split, strconv.Itoa(g))
			if n.ID != req.Main {
				ip := hostOf(s.agentBase(n.ID))
				if ip != "" {
					workers = append(workers, ip+":"+strconv.Itoa(rpcPort))
				}
			}
		}
		if len(workers) > 0 {
			args = append(args, "--rpc", strings.Join(workers, ","))
		}
		args = append(args, "--tensor-split", strings.Join(split, ","))
		plan["workers"] = workers
		plan["tensor_split"] = split
		// TODO(fleet-load): launch ggml-rpc-server (s.rpcExePath()) on each worker
		// node's control plane (:1357) before firing the main, instead of assuming
		// the workers are already up. Left as a plan step to avoid a partial fleet
		// launch that could disturb a running prod decode.
		plan["rpc_exe"] = s.rpcExePath()
		plan["note"] = "los nodos worker deben ejecutar ggml-rpc-server:" + strconv.Itoa(rpcPort) + " (pendiente fleet-load: rpc_exe en los nodos worker)"
	}

	exe := s.llamaExePath()
	spec := map[string]any{"exe": exe, "args": args}
	endpoint := "http://" + hostOf(s.agentBase(req.Main)) + ":" + strconv.Itoa(req.Port)
	plan["spec"] = spec
	plan["endpoint"] = endpoint

	agent := s.agentBase(req.Main)
	// The daemon launches on its OWN host IN-PROCESS — zero-config: the local node
	// needs nothing in the config and no node-agent. Remote nodes launch via their
	// control agent (their soflink daemon on :1357, or the legacy node-agent :50060).
	local := s.isSelfHost(hostOf(agent))
	canLaunch := (local || hasControlAgent(agent)) && req.Mode == "individual"
	plan["can_launch"] = canLaunch
	plan["local"] = local // so the panel only trusts the pid-alive crash check on THIS host

	// Preview only (dry_run) or a placement we can't actuate yet (union which needs
	// rpc-server workers, or a remote main with no control): return the plan so the
	// panel shows exactly what WOULD run, without firing anything.
	if req.DryRun || !canLaunch {
		plan["launched"] = false
		if req.Mode == "union" {
			plan["blocked"] = "modo unión: los workers necesitan ggml-rpc-server:" + strconv.Itoa(rpcPort) + " (fase de fleet-load)"
		} else if !local {
			plan["blocked"] = "el nodo main no expone control (/control/load): que corra el binario soflink (o el node-agent)"
		}
		writeJSON(w, http.StatusOK, plan)
		return
	}

	// Fire it: launch the allowlisted llama-server on the main node — in-process if
	// that's THIS host, else via its control agent. Serves the model on its OWN
	// endpoint; never touches the coordinator's decode/HUD.
	var out map[string]any
	if local {
		out = launchLlama(exe, args)
	} else {
		body, _ := json.Marshal(spec)
		resp, err := s.client.Post(agent+"/control/load", "application/json", bytes.NewReader(body))
		if err != nil {
			plan["launched"] = false
			plan["error"] = err.Error()
			writeJSON(w, http.StatusBadGateway, plan)
			return
		}
		defer resp.Body.Close()
		_ = json.NewDecoder(resp.Body).Decode(&out)
	}
	plan["launched"] = out["ok"] == true
	plan["agent_reply"] = out
	if out["ok"] == true {
		recordLoaded(loadedModel{Endpoint: endpoint, Model: alias, Node: req.Main, Mode: req.Mode})
		go setLoadedLayers(endpoint, name) // reading the gguf header can be slow — never block the launch response on it
	}
	writeJSON(w, http.StatusOK, plan)
}

// controlBaseForHost returns the soflink control-plane base URL for a host IP: the
// configured node's agent when that agent exposes control, else the daemon's own
// port on that IP (:1357). This lets an eject reach the node that actually HOSTS an
// endpoint even when the model isn't in this coordinator's loaded registry (e.g. an
// externally-launched prod decode).
func (s *Server) controlBaseForHost(ip string) string {
	for _, n := range s.cfg.Nodes {
		if hostOf(n.Agent) == ip && hasControlAgent(n.Agent) {
			return strings.TrimRight(n.Agent, "/")
		}
	}
	return "http://" + ip + ":1357"
}

// modelsEject stops one loaded model: it kills the llama-server on that port on the
// node that HOSTS the endpoint — resolved from the endpoint's IP, not from a fixed
// control node and not only from this coordinator's loaded registry (an external
// decode like the prod one isn't tracked in loadedReg). Drops it from the registry
// without touching the other instances.
func (s *Server) modelsEject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Endpoint string `json:"endpoint"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Endpoint == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "endpoint requerido"})
		return
	}
	ip := hostOf(req.Endpoint)
	port := ""
	if hp := hostPort(req.Endpoint); hp != "" {
		if i := strings.LastIndex(hp, ":"); i >= 0 {
			port = hp[i+1:]
		}
	}
	if port != "" {
		if s.isSelfHost(ip) {
			killLlamaByPort(port) // in-process on this host (no node-agent needed)
		} else {
			base := s.controlBaseForHost(ip)
			if resp, err := s.client.Post(base+"/control/kill?port="+port, "application/json", nil); err == nil {
				_ = resp.Body.Close()
			}
		}
	}
	dropLoaded(req.Endpoint)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// modelsProbe checks whether a just-loaded model's endpoint answers /health yet,
// so the panel can confirm it came up.
func (s *Server) modelsProbe(w http.ResponseWriter, r *http.Request) {
	ep := r.URL.Query().Get("endpoint")
	if ep == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "endpoint requerido"})
		return
	}
	c := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := c.Get(ep + "/health")
	up := err == nil && resp != nil && resp.StatusCode == http.StatusOK
	if resp != nil {
		resp.Body.Close()
	}
	writeJSON(w, http.StatusOK, map[string]any{"up": up})
}

func maxi(a, b int) int {
	if a > b {
		return a
	}
	return b
}
