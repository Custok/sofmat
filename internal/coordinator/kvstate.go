package coordinator

// KV state exchange between soflink nodes — the transport of the disaggregated
// prefill→decode handoff (F1). A node whose config sets kv_state_dir (= the
// --slot-save-path of its llama-server):
//
//	GET    /kv/<name>          serves a saved slot state to a peer
//	DELETE /kv/<name>          drops it once shipped
//	POST   /control/kv-fetch   {url, name[, sha256]} pulls a peer's state into
//	                           kv_state_dir, so `slots/<id>?action=restore` on
//	                           the local engine finds it under that name
//
// Names are strict basenames (no traversal, .bin only); the dir is opt-in per
// node; a node without it answers 503 and the gateway degrades to decode-direct.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var stateNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}\.bin$`)

// validStateName accepts only a plain file name a peer may address.
func validStateName(n string) bool {
	return stateNameRe.MatchString(n) && !strings.Contains(n, "..")
}

// maxStateBytes caps a fetched state (a 100k-token state of a 27B model is
// ~1.8 GB; this leaves room for larger contexts/models without being unbounded).
const maxStateBytes = int64(32) << 30

func (s *Server) kvStateDir() string { return s.cfg.KVStateDir }

// withSlotSavePath adds --slot-save-path <kv_state_dir> to a llama-server
// launch when this node has the dir configured and the caller did not set it,
// so every engine this daemon starts can save/restore slot states.
func (s *Server) withSlotSavePath(args []string) []string {
	dir := s.kvStateDir()
	if dir == "" {
		return args
	}
	for _, a := range args {
		if a == "--slot-save-path" {
			return args
		}
	}
	return append(append([]string{}, args...), "--slot-save-path", dir)
}

// kvFile serves (GET) or removes (DELETE) one saved state from kv_state_dir.
func (s *Server) kvFile(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/kv/")
	if !validStateName(name) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "nombre de estado inválido"})
		return
	}
	dir := s.kvStateDir()
	if dir == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "kv_state_dir no configurado en este nodo"})
		return
	}
	path := filepath.Join(dir, name)
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		f, err := os.Open(path)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "estado no encontrado"})
			return
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil || st.IsDir() {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "estado no encontrado"})
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Cache-Control", "no-store")
		http.ServeContent(w, r, name, st.ModTime(), f)
	case http.MethodDelete:
		if err := os.Remove(path); err != nil {
			if os.IsNotExist(err) {
				writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "estado no encontrado"})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		w.Header().Set("Allow", "GET, HEAD, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "método no permitido"})
	}
}

// controlKVFetch pulls a peer's saved state into kv_state_dir under `name`
// (written to a temp file, then renamed, so a restore never sees a partial
// file). Optional sha256 is verified. LAN-trust like the rest of /control.
func (s *Server) controlKVFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "POST requerido"})
		return
	}
	var body struct {
		URL    string `json:"url"`
		Name   string `json:"name"`
		SHA256 string `json:"sha256"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if !validStateName(body.Name) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "nombre de estado inválido"})
		return
	}
	if !strings.HasPrefix(body.URL, "http://") && !strings.HasPrefix(body.URL, "https://") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "url http(s) requerida"})
		return
	}
	dir := s.kvStateDir()
	if dir == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ok": false, "error": "kv_state_dir no configurado en este nodo"})
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	t0 := time.Now()
	client := &http.Client{Timeout: 300 * time.Second}
	resp, err := client.Get(body.URL)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "origen HTTP " + resp.Status})
		return
	}
	final := filepath.Join(dir, body.Name)
	tmp := final + ".part"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxStateBytes+1))
	cerr := f.Close()
	if err == nil {
		err = cerr
	}
	if err == nil && n > maxStateBytes {
		err = io.ErrShortBuffer
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if err == nil && body.SHA256 != "" && !strings.EqualFold(body.SHA256, sum) {
		err = errSHAMismatch
	}
	if err == nil {
		err = os.Rename(tmp, final)
	}
	if err != nil {
		_ = os.Remove(tmp)
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error(), "bytes": n})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "name": body.Name, "bytes": n,
		"sha256": sum, "ms": float64(time.Since(t0).Microseconds()) / 1000})
}

type shaMismatch struct{}

func (shaMismatch) Error() string { return "sha256 del estado no coincide" }

var errSHAMismatch error = shaMismatch{}
