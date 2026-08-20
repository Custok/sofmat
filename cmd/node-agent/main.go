// Command node-agent is sofmat's per-host leaf agent. The Go coordinator runs in
// a Linux container (Windows Application Control blocks unsigned native binaries)
// and so cannot read a host GPU or start/stop its llama-server. Each host runs
// this small agent instead: a sensor (GET /gpu, /cmdline) and an actuator (POST
// /control/load, /control/eject). The coordinator aggregates every node's /gpu
// and forwards control here, so a dashboard reads and drives the whole cluster
// THROUGH Go. Single static binary, zero dependencies — build once, drop on the
// node, point the coordinator config's node.agent at it.
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type gpu struct {
	Idx     int    `json:"idx"`
	Name    string `json:"name"`
	UsedMB  int    `json:"used_mb"`
	TotalMB int    `json:"total_mb"`
	Util    int    `json:"util"`
	Temp    int    `json:"temp"`
	Power   int    `json:"power"`
}

var nodeID = "node-a"

// llamaExe is the only binary /control/load is allowed to launch.
const llamaExe = "llama-server.exe"

func atoiField(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(strings.Split(s, ".")[0]))
	return n
}

// gpus shells out to nvidia-smi (portable across the fleet's hosts).
func gpus() []gpu {
	out, err := exec.Command("nvidia-smi",
		"--query-gpu=index,name,memory.used,memory.total,utilization.gpu,temperature.gpu,power.draw",
		"--format=csv,noheader,nounits").Output()
	gs := []gpu{}
	if err != nil {
		return gs
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, ",")
		if len(f) < 7 {
			continue
		}
		gs = append(gs, gpu{
			Idx: atoiField(f[0]), Name: strings.TrimSpace(f[1]),
			UsedMB: atoiField(f[2]), TotalMB: atoiField(f[3]),
			Util: atoiField(f[4]), Temp: atoiField(f[5]), Power: atoiField(f[6]),
		})
	}
	return gs
}

// gpuPayload matches the node-c/node-d agents so the coordinator treats every
// node uniformly: gpus[] plus host cpu/ram.
func gpuPayload() map[string]any {
	cpu, usedMB, totalMB := hostStats()
	return map[string]any{
		"node": nodeID, "gpus": gpus(),
		"cpu": cpu, "ram_used_mb": usedMB, "ram_total_mb": totalMB,
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// handleLoad launches llama-server with the args the caller supplies. The exe is
// allowlisted to llama-server.exe so the endpoint can't be turned into an
// arbitrary command runner.
func handleLoad(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Exe  string   `json:"exe"`
		Args []string `json:"args"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if strings.ToLower(filepath.Base(body.Exe)) != llamaExe {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "only " + llamaExe + " may be launched"})
		return
	}
	cmd := exec.Command(body.Exe, body.Args...)
	if err := cmd.Start(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pid": cmd.Process.Pid})
}

func handleEject(w http.ResponseWriter, r *http.Request) {
	err := exec.Command("taskkill", "/F", "/IM", llamaExe).Run()
	writeJSON(w, http.StatusOK, map[string]any{"ok": err == nil})
}

func main() {
	addr := flag.String("addr", ":50060", "listen address")
	flag.StringVar(&nodeID, "node", "node-a", "node id label")
	flag.Parse()

	hostStats() // prime the CPU-time baseline so the first /gpu reads true

	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "node": nodeID})
	})
	mux.HandleFunc("/gpu", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, gpuPayload())
	})
	mux.HandleFunc("/cmdline", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"cmdline": llamaCmdline()})
	})
	mux.HandleFunc("/control/load", handleLoad)
	mux.HandleFunc("/control/eject", handleEject)

	log.Printf("sofmat node-agent (%s) on %s", nodeID, *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}
