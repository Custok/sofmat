package coordinator

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Model download from HuggingFace — ONLY the `unsloth` org (David's constraint).
// The panel browses unsloth repos, picks a repo, picks a quantization (a .gguf
// sibling), and downloads it into the models dir. A downloaded .gguf then shows
// up as a local model the Load picker can launch. Everything here is additive:
// new routes, no touch to chat/status/load — so it can't break the live panel.

// modelsDir is where downloaded GGUFs land — an ABSOLUTE, cwd-independent path so
// a user can launch the binary from anywhere (double-click, any shell) and always
// find and keep the same models. It sits next to the executable (portable, self-
// contained); falls back to <home>/soflink/models, then a relative dir. Override
// with SOFMAT_MODELS. The caller MkdirAll's it, so it need not pre-exist.
func modelsDir() string {
	if e := os.Getenv("SOFMAT_MODELS"); e != "" {
		return e
	}
	if exe, err := os.Executable(); err == nil {
		if d := filepath.Dir(exe); d != "" && d != "." {
			return filepath.Join(d, "models")
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, "soflink", "models")
	}
	return "models"
}

// hfListClient has a real timeout: listing must fail fast. Downloads use a
// separate no-timeout client (multi-GB GGUFs).
var hfListClient = &http.Client{Timeout: 15 * time.Second}
var hfDownloadClient = &http.Client{}

// splitPartRe matches a split-GGUF part ("model-00001-of-00003.gguf"): group 1 =
// base name, 2 = this part's index, 3 = total parts. Used to collapse a multi-file
// model into one row (only the first part is loadable).
var splitPartRe = regexp.MustCompile(`(?i)^(.*)-(\d+)-of-(\d+)\.gguf$`)

// modelFolder is the per-model subdirectory a download lands in: the filename with
// its split suffix (-00001-of-00003) and .gguf stripped, so every part of a split
// model shares one folder and the panel treats the folder as a single model.
func modelFolder(file string) string {
	b := filepath.Base(file)
	if m := splitPartRe.FindStringSubmatch(b); m != nil {
		return m[1]
	}
	return strings.TrimSuffix(b, ".gguf")
}

// ---- browse ---------------------------------------------------------------

// hfModels lists unsloth repos (top by downloads, optional search filter), so
// the panel shows the catalog. author=unsloth is hard-wired — no other org.
func (s *Server) hfModels(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	// filter=gguf: soflink solo carga/descarga GGUF, así el catálogo no muestra
	// repos NVFP4/AWQ/MLX cuyo listado de cuantizaciones saldría vacío.
	u := "https://huggingface.co/api/models?author=unsloth&filter=gguf&limit=80&sort=downloads&direction=-1"
	if q != "" {
		u += "&search=" + url.QueryEscape(q)
	}
	resp, err := hfListClient.Get(u)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	var raw []map[string]any
	if json.NewDecoder(resp.Body).Decode(&raw) != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "hf decode"})
		return
	}
	out := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		id := pstr(m["id"])
		if id == "" || !strings.HasPrefix(strings.ToLower(id), "unsloth/") {
			continue // belt-and-suspenders: never surface a non-unsloth repo
		}
		out = append(out, map[string]any{
			"id": id, "downloads": m["downloads"], "likes": m["likes"],
			"gguf": hasGGUFtag(m["tags"]),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out})
}

func hasGGUFtag(tags any) bool {
	if a, ok := tags.([]any); ok {
		for _, t := range a {
			if strings.EqualFold(pstr(t), "gguf") {
				return true
			}
		}
	}
	return false
}

// hfFiles lists the .gguf files (the QUANTIZATIONS) of a repo, with sizes, via
// the tree API — so the panel offers Q4_K_M / Q8_0 / BF16 / … with their weight.
func (s *Server) hfFiles(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	if !strings.HasPrefix(strings.ToLower(repo), "unsloth/") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "solo repos unsloth/"})
		return
	}
	// tree API is paginated for big repos; walk it fully (multi-part GGUFs and
	// many quants live in one repo).
	files := []map[string]any{}
	base := "https://huggingface.co/api/models/" + repo + "/tree/main?recursive=true&limit=1000"
	resp, err := hfListClient.Get(base)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	var tree []map[string]any
	if json.NewDecoder(resp.Body).Decode(&tree) != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "hf tree decode"})
		return
	}
	for _, e := range tree {
		p := pstr(e["path"])
		if !strings.HasSuffix(strings.ToLower(p), ".gguf") {
			continue
		}
		files = append(files, map[string]any{
			"file": p, "size": e["size"], "quant": quantOf(p),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"repo": repo, "files": files})
}

// quantOf extracts a human quant label from a gguf filename (…-Q4_K_M.gguf →
// Q4_K_M; …-BF16-00001-of-00002.gguf → BF16).
func quantOf(name string) string {
	base := strings.TrimSuffix(filepath.Base(name), ".gguf")
	up := strings.ToUpper(base)
	for _, tag := range []string{"BF16", "F16", "F32", "Q2_K", "Q3_K_S", "Q3_K_M", "Q3_K_L", "Q4_K_S", "Q4_K_M", "Q4_0", "Q4_1", "Q5_K_S", "Q5_K_M", "Q5_0", "Q5_1", "Q6_K", "Q8_0", "IQ1_S", "IQ2_XXS", "IQ2_M", "IQ3_M", "IQ4_XS", "IQ4_NL"} {
		if strings.Contains(up, tag) {
			return tag
		}
	}
	return base
}

// ---- download -------------------------------------------------------------

type dlJob struct {
	Repo    string    `json:"repo"`
	File    string    `json:"file"`
	Total   int64     `json:"total"`
	Done    int64     `json:"done"`
	Status  string    `json:"status"` // downloading | done | error
	Error   string    `json:"error,omitempty"`
	Started time.Time `json:"-"`
	Rate    float64   `json:"-"` // velocidad instantánea (bytes/s), muestreada ~cada 1s en el loop
}

var dlReg = struct {
	mu   sync.Mutex
	jobs map[string]*dlJob // key = destination filename (basename)
}{jobs: map[string]*dlJob{}}

// hfDownload starts (or reports an already-running) download of one gguf from an
// unsloth repo into the models dir. Streams to <name>.part then renames on
// success, so a half file is never mistaken for a ready model.
func (s *Server) hfDownload(w http.ResponseWriter, r *http.Request) {
	var req struct{ Repo, File string }
	_ = json.NewDecoder(r.Body).Decode(&req)
	if !strings.HasPrefix(strings.ToLower(req.Repo), "unsloth/") || req.File == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "repo unsloth/ y file requeridos"})
		return
	}
	name := filepath.Base(req.File)
	dlReg.mu.Lock()
	if j, ok := dlReg.jobs[name]; ok && j.Status == "downloading" {
		dlReg.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": name, "already": true})
		return
	}
	job := &dlJob{Repo: req.Repo, File: req.File, Status: "downloading", Started: time.Now()}
	dlReg.jobs[name] = job
	dlReg.mu.Unlock()

	go s.runDownload(job, name)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": name})
}

func (s *Server) runDownload(job *dlJob, name string) {
	defer recoverProbe()
	fail := func(msg string) {
		dlReg.mu.Lock()
		job.Status, job.Error = "error", msg
		dlReg.mu.Unlock()
	}
	// Each model lands in its OWN subfolder (named for the model) so all parts of a
	// split GGUF share one folder and the panel lists it as a single model.
	folder := modelFolder(job.File)
	base := filepath.Base(job.File)
	destDir := filepath.Join(modelsDir(), folder)
	if err := os.MkdirAll(destDir, 0755); err != nil {
		fail(err.Error())
		return
	}
	src := "https://huggingface.co/" + job.Repo + "/resolve/main/" + job.File + "?download=true"
	resp, err := hfDownloadClient.Get(src)
	if err != nil {
		fail(err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fail(fmt.Sprintf("HF HTTP %d", resp.StatusCode))
		return
	}
	dlReg.mu.Lock()
	job.Total = resp.ContentLength
	dlReg.mu.Unlock()

	part := filepath.Join(destDir, base+".part")
	f, err := os.Create(part)
	if err != nil {
		fail(err.Error())
		return
	}
	buf := make([]byte, 1<<20) // 1 MiB
	lastAt, lastDone := time.Now(), int64(0)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				fail(werr.Error())
				return
			}
			dlReg.mu.Lock()
			job.Done += int64(n)
			// muestrea la velocidad instantánea cada ~1s (para ETA en el panel)
			if dt := time.Since(lastAt); dt >= time.Second {
				job.Rate = float64(job.Done-lastDone) / dt.Seconds()
				lastAt, lastDone = time.Now(), job.Done
			}
			dlReg.mu.Unlock()
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			f.Close()
			_ = os.Remove(part)
			fail(rerr.Error())
			return
		}
	}
	f.Close()
	if err := os.Rename(part, filepath.Join(destDir, base)); err != nil {
		fail(err.Error())
		return
	}
	dlReg.mu.Lock()
	job.Status = "done"
	dlReg.mu.Unlock()
}

// hfProgress reports the state of every download this run has seen.
func (s *Server) hfProgress(w http.ResponseWriter, r *http.Request) {
	dlReg.mu.Lock()
	out := make([]map[string]any, 0, len(dlReg.jobs))
	for name, j := range dlReg.jobs {
		pct := 0.0
		if j.Total > 0 {
			pct = round1(float64(j.Done) / float64(j.Total) * 100)
		}
		speed, eta := 0.0, 0.0
		if j.Status == "downloading" {
			speed = j.Rate
			if j.Rate > 0 && j.Total > j.Done {
				eta = round1(float64(j.Total-j.Done) / j.Rate)
			}
		}
		out = append(out, map[string]any{
			"id": name, "repo": j.Repo, "file": j.File,
			"total": j.Total, "done": j.Done, "pct": pct, "status": j.Status, "error": j.Error,
			"speed": speed, "eta": eta,
		})
	}
	dlReg.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"downloads": out})
}

// localModels lists the ready .gguf in the models dir (ignoring .part), so the
// panel — and the Load picker — can offer downloaded models.
func (s *Server) localModels(w http.ResponseWriter, r *http.Request) {
	dir := modelsDir()
	ents, _ := os.ReadDir(dir)
	out := []map[string]any{}
	for _, e := range ents {
		// Each model lives in its OWN subfolder, so a folder = one model (a split
		// GGUF's parts collapse into a single row naturally). "file" is the loadable
		// part as a path relative to the models dir ("<folder>/<file>.gguf").
		if e.IsDir() {
			if load, total := loadableGGUF(filepath.Join(dir, e.Name())); load != "" {
				rel := e.Name() + "/" + load
				out = append(out, map[string]any{
					"file": rel, "size": total, "quant": quantOf(load),
					"path": filepath.Join(dir, e.Name(), load),
				})
			}
			continue
		}
		// Legacy flat .gguf at the top level (downloaded before the per-folder layout):
		// still collapse split parts into one row.
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".gguf") {
			continue
		}
		if m := splitPartRe.FindStringSubmatch(name); m != nil {
			if p, _ := strconv.Atoi(m[2]); p != 1 {
				continue
			}
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		size := info.Size()
		if m := splitPartRe.FindStringSubmatch(name); m != nil {
			size = 0
			for _, e2 := range ents {
				if om := splitPartRe.FindStringSubmatch(e2.Name()); om != nil && om[1] == m[1] && om[3] == m[3] {
					if i2, err := e2.Info(); err == nil {
						size += i2.Size()
					}
				}
			}
		}
		out = append(out, map[string]any{
			"file": name, "size": size, "quant": quantOf(name),
			"path": filepath.Join(dir, name),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": out, "dir": dir})
}

// loadableGGUF returns the single loadable .gguf in a model folder (the first part
// of a split, or the lone file) and the TOTAL size of all its .gguf parts. Empty
// name if the folder holds no ready .gguf.
func loadableGGUF(sub string) (string, int64) {
	ents, _ := os.ReadDir(sub)
	var load string
	var total int64
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(n), ".gguf") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		total += info.Size()
		if m := splitPartRe.FindStringSubmatch(n); m != nil {
			if p, _ := strconv.Atoi(m[2]); p == 1 {
				load = n
			}
		} else if load == "" {
			load = n
		}
	}
	return load, total
}

// modelsDelete borra un .gguf del disco (models dir). Endurecido contra path
// traversal: solo acepta un nombre simple (sin rutas ni "..") acabado en .gguf y
// que exista dentro del models dir. Nunca borra fuera de ahí.
func (s *Server) modelsDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		File string `json:"file"`
	}
	if json.NewDecoder(r.Body).Decode(&req) != nil || strings.TrimSpace(req.File) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "file requerido"})
		return
	}
	name := filepath.ToSlash(strings.TrimSpace(req.File))
	if !strings.HasSuffix(strings.ToLower(name), ".gguf") || strings.Contains(name, "..") || strings.HasPrefix(name, "/") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "nombre inválido"})
		return
	}
	dir := modelsDir()
	parts := strings.Split(name, "/")
	var target, label string
	switch {
	case len(parts) == 1 && parts[0] != "": // legacy flat file → delete the file
		target, label = filepath.Join(dir, parts[0]), parts[0]
	case len(parts) == 2 && parts[0] != "" && parts[1] != "": // folder/file → delete the whole model folder (all parts)
		target, label = filepath.Join(dir, parts[0]), parts[0]
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "nombre inválido"})
		return
	}
	if _, err := os.Stat(target); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "el modelo no existe"})
		return
	}
	if err := os.RemoveAll(target); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"error": err.Error(),
			"hint":  "si el modelo está cargado, haz Eject antes de borrar (el fichero está en uso)",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": label})
}
