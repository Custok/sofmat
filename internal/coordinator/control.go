package coordinator

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// procReg tracks llama-server processes this daemon launched, so the panel can
// tell a CRASH (the model failed to load and the process exited) from a slow load
// — without waiting out the poll timeout.
var procReg = struct {
	mu   sync.Mutex
	dead map[int]bool
}{dead: map[int]bool{}}

func markDead(pid int) { procReg.mu.Lock(); procReg.dead[pid] = true; procReg.mu.Unlock() }
func isDead(pid int) bool {
	procReg.mu.Lock()
	defer procReg.mu.Unlock()
	return procReg.dead[pid]
}

// controlAlive reports whether a llama-server this daemon launched is still alive
// (false only once it has exited). Unknown pids answer alive=true, so a model on
// another node just falls back to the poll timeout.
func (s *Server) controlAlive(w http.ResponseWriter, r *http.Request) {
	pid := 0
	if p := r.URL.Query().Get("pid"); p != "" {
		for _, c := range p {
			if c < '0' || c > '9' {
				pid = -1
				break
			}
			pid = pid*10 + int(c-'0')
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"alive": pid <= 0 || !isDead(pid)})
}

// isSelfHost reports whether host refers to THIS machine, so the coordinator can
// launch models on its own node in-process (zero-config: no control agent, no
// node-agent, nothing in the config needed for the local node to work).
func (s *Server) isSelfHost(host string) bool {
	if host == "" || host == "localhost" || strings.HasPrefix(host, "127.") {
		return true
	}
	if s.cfg.PublicURL != "" && hostOf(s.cfg.PublicURL) == host {
		return true
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.String() == host {
				return true
			}
		}
	}
	return false
}

// The control plane, merged INTO the daemon so a node runs ONE binary (no separate
// node-agent): the same soflink that serves the panel also launches and stops
// llama-server on its own host. Cross-platform (Windows / Linux / macOS).

// isLlamaServer allowlists the only binary /control/load may launch, on any OS, so
// the endpoint can't be turned into an arbitrary command runner.
func isLlamaServer(exe string) bool {
	b := strings.ToLower(filepath.Base(exe))
	return b == "llama-server" || b == "llama-server.exe"
}

// launchLlama starts llama-server (allowlisted) with the exe's own dir as CWD so
// its co-located backend libs/GPU runtime resolve, capturing output to a log so a
// crash on model load is diagnosable. Returns the node-agent-compatible reply.
func launchLlama(exe string, args []string) map[string]any {
	if !isLlamaServer(exe) {
		return map[string]any{"ok": false, "error": "solo se puede lanzar llama-server"}
	}
	cmd := exec.Command(exe, args...)
	cmd.Dir = filepath.Dir(exe)
	if logf, err := os.Create(filepath.Join(filepath.Dir(exe), "llama-launch.log")); err == nil {
		cmd.Stdout, cmd.Stderr = logf, logf
	}
	if err := cmd.Start(); err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	pid := cmd.Process.Pid
	// reap the child and record its exit, so /control/alive can flag a load crash
	// fast instead of the panel waiting out the whole poll timeout.
	go func() { _ = cmd.Wait(); markDead(pid) }()
	return map[string]any{"ok": true, "pid": pid}
}

// controlLoad launches llama-server with the caller's args (POST /control/load),
// so a remote coordinator can drive this node exactly like the old node-agent.
func (s *Server) controlLoad(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Exe  string   `json:"exe"`
		Args []string `json:"args"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	res := launchLlama(body.Exe, body.Args)
	code := http.StatusOK
	if res["ok"] != true {
		code = http.StatusBadRequest
	}
	writeJSON(w, code, res)
}

// controlEject kills every llama-server on this host (all loaded instances).
func (s *Server) controlEject(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": ejectAllLlama()})
}

func ejectAllLlama() bool {
	var err error
	if runtime.GOOS == "windows" {
		err = exec.Command("taskkill", "/F", "/IM", "llama-server.exe").Run()
	} else {
		err = exec.Command("pkill", "-f", "llama-server").Run()
	}
	return err == nil
}

// controlKill stops just the llama-server LISTENING on a given port, so ejecting
// one loaded model doesn't take down the others.
func (s *Server) controlKill(w http.ResponseWriter, r *http.Request) {
	port := r.URL.Query().Get("port")
	if port == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "port requerido"})
		return
	}
	killed := killLlamaByPort(port)
	writeJSON(w, http.StatusOK, map[string]any{"ok": len(killed) > 0, "killed": killed})
}

func killLlamaByPort(port string) []string {
	killed := []string{}
	if runtime.GOOS == "windows" {
		out, _ := exec.Command("netstat", "-ano", "-p", "tcp").Output()
		for _, line := range strings.Split(string(out), "\n") {
			f := strings.Fields(line)
			if len(f) < 5 || f[3] != "LISTENING" || !strings.HasSuffix(f[1], ":"+port) {
				continue
			}
			_ = exec.Command("taskkill", "/F", "/PID", f[4]).Run()
			killed = append(killed, f[4])
		}
	} else {
		out, _ := exec.Command("sh", "-c", "lsof -ti tcp:"+port+" -sTCP:LISTEN 2>/dev/null").Output()
		for _, pid := range strings.Fields(string(out)) {
			_ = exec.Command("kill", "-9", pid).Run()
			killed = append(killed, pid)
		}
	}
	return killed
}
