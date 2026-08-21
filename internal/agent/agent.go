// Package agent is the per-node sensor folded into the soflink daemon: GPU
// telemetry (nvidia-smi) plus host CPU/RAM, served at /gpu. Because every node
// that runs soflink now exposes itself — no separate sensor process — the fleet
// self-populates through LAN discovery: a coordinator sweeps the subnet, and
// each soflink it finds answers /gpu with its own hardware.
package agent

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
)

// GPU is one card's live telemetry.
type GPU struct {
	Idx     int    `json:"idx"`
	Name    string `json:"name"`
	UsedMB  int    `json:"used_mb"`
	TotalMB int    `json:"total_mb"`
	Util    int    `json:"util"`
	Temp    int    `json:"temp"`
	Power   int    `json:"power"`
}

func atoiField(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(strings.Split(s, ".")[0]))
	return n
}

// GPUs shells out to nvidia-smi (portable across the fleet's hosts).
func GPUs() []GPU {
	out, err := exec.Command("nvidia-smi",
		"--query-gpu=index,name,memory.used,memory.total,utilization.gpu,temperature.gpu,power.draw",
		"--format=csv,noheader,nounits").Output()
	gs := []GPU{}
	if err != nil {
		return gs
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, ",")
		if len(f) < 7 {
			continue
		}
		gs = append(gs, GPU{
			Idx: atoiField(f[0]), Name: strings.TrimSpace(f[1]),
			UsedMB: atoiField(f[2]), TotalMB: atoiField(f[3]),
			Util: atoiField(f[4]), Temp: atoiField(f[5]), Power: atoiField(f[6]),
		})
	}
	return gs
}

// Payload is the node telemetry the coordinator aggregates — identical shape to
// the standalone node-agent so old and new nodes look the same.
func Payload(nodeID string) map[string]any {
	cpu, usedMB, totalMB := hostStats()
	return map[string]any{
		"node": nodeID, "gpus": GPUs(),
		"cpu": cpu, "ram_used_mb": usedMB, "ram_total_mb": totalMB,
	}
}

// Handler serves GET /gpu with this node's telemetry, so the daemon IS the node
// sensor.
func Handler(nodeID string) http.HandlerFunc {
	Prime() // prime any CPU-time baseline so the first read is true
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(Payload(nodeID))
	}
}
