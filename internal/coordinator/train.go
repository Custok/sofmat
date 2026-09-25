package coordinator

// Training transaction — "en entrenamiento" fencing of production instances.
//
// Contract (topic fine-tuning-con-unsloth, 2026-09-25): the runner of the node
// that trains asks the coordinator to FENCE the instances it is about to stop
// (POST /api/train/begin), stops them itself — whoever launched the instance
// stops and starts it: a systemd unit with systemctl, an engine this
// coordinator launched is ejected here when asked (`parar`) —, trains, starts
// them again and closes the transaction (POST /api/train/end).
//
// While a transaction is open:
//   - a fenced DECODE leaves the balancer: no new conversation lands on it and a
//     request that can only go there is refused with ErrDecodeTraining instead
//     of waiting on an engine that is being stopped;
//   - a fenced PREFILL leaves the KV handoff: prompts go decode-direct;
//   - the panel shows the instance as "en entrenamiento · job · remonta antes
//     de HH:MM" instead of hiding it or calling it down.
//
// The transaction is persisted (train.local.json next to the config, override
// with SOFMAT_TRAIN), so a coordinator restart keeps the fence. A watchdog
// checks the reload deadline: an instance still down past it is raised again
// when this coordinator launched it (a preset whose main has a control plane),
// and reported either way — log always, WhatsApp when alerts are configured.
// The runner never remounts through the coordinator; it reports, and `end`
// verifies with /health.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// trainTx is one open training transaction: what was fenced, for which job,
// until when. Phase is "entrenando" until the runner calls end; "remontando"
// when end found something still down and left the transaction open for the
// watchdog.
type trainTx struct {
	Job       string    `json:"job"`
	Node      string    `json:"node,omitempty"`
	Instances []string  `json:"instancias"`
	Deadline  time.Time `json:"deadline"`
	Started   time.Time `json:"inicio"`
	Origin    string    `json:"origen,omitempty"`
	Phase     string    `json:"fase"`
	Alerted   bool      `json:"avisado"`
	Note      string    `json:"nota,omitempty"`
}

const (
	trainPhaseTraining = "entrenando"
	trainPhaseReload   = "remontando"
	// trainDrainDefault/Max bound how long begin waits for in-flight requests
	// on the fenced instances to finish before answering.
	trainDrainDefault = 90 * time.Second
	trainDrainMax     = 600 * time.Second
	// trainWaitDefault/Max bound how long end waits for /health after the
	// runner says it started the instances again.
	trainWaitDefault = 120 * time.Second
	trainWaitMax     = 600 * time.Second
	// trainDeadlineDefault applies when begin carries no deadline.
	trainDeadlineDefault = 6 * time.Hour
)

// trainTick is how often the watchdog looks at open transactions (a var so
// tests can call trainCheck directly instead of waiting).
var trainTick = 60 * time.Second

var trainState = struct {
	mu  sync.Mutex
	txs map[string]*trainTx // job -> transaction
}{txs: map[string]*trainTx{}}

// trainPath is where open transactions persist: next to the config by default
// (durable across restarts), overridable with SOFMAT_TRAIN.
func trainPath() string {
	if p := os.Getenv("SOFMAT_TRAIN"); p != "" {
		return p
	}
	return "train.local.json"
}

// loadTrain restores open transactions from disk at startup. An unreadable
// file is reported and ignored (no fence is worse than a stale one only if the
// engine is actually up, and the watchdog closes those).
func loadTrain() {
	b, err := os.ReadFile(trainPath())
	if err != nil {
		return
	}
	var list []*trainTx
	if err := json.Unmarshal(b, &list); err != nil {
		log.Printf("train: %s ilegible (%v): se ignora", trainPath(), err)
		return
	}
	trainState.mu.Lock()
	trainState.txs = map[string]*trainTx{}
	for _, t := range list {
		if t != nil && t.Job != "" {
			if t.Phase == "" {
				t.Phase = trainPhaseTraining
			}
			trainState.txs[t.Job] = t
		}
	}
	trainState.mu.Unlock()
}

// saveTrainLocked persists the open transactions (caller holds trainState.mu).
// Temp file + rename: a crash mid-write must not leave a half JSON that the
// next start would then ignore, silently dropping every fence.
func saveTrainLocked() {
	list := make([]*trainTx, 0, len(trainState.txs))
	for _, t := range trainState.txs {
		list = append(list, t)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Started.Before(list[j].Started) })
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return
	}
	tmp := trainPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		log.Printf("train: no se pudo guardar %s: %v", trainPath(), err)
		return
	}
	if err := os.Rename(tmp, trainPath()); err != nil {
		log.Printf("train: no se pudo reemplazar %s: %v", trainPath(), err)
	}
}

// openTrainTxs is a snapshot of the open transactions, oldest first.
func openTrainTxs() []trainTx {
	trainState.mu.Lock()
	defer trainState.mu.Unlock()
	out := make([]trainTx, 0, len(trainState.txs))
	for _, t := range trainState.txs {
		c := *t
		c.Instances = append([]string(nil), t.Instances...)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out
}

// trainingOf returns the transaction fencing an instance key, if any.
func trainingOf(key string) (trainTx, bool) {
	for _, t := range openTrainTxs() {
		for _, k := range t.Instances {
			if k == key {
				return t, true
			}
		}
	}
	return trainTx{}, false
}

// trainLabel is the card text for a fenced instance.
func trainLabel(t trainTx) string {
	return "en entrenamiento · job " + t.Job + " · remonta antes de " + t.Deadline.Local().Format("15:04")
}

// fence toggles the routing fences of one instance key: a decode leaves/rejoins
// the balancer, a prefill leaves/rejoins the KV handoff. Other roles (solo,
// loaded models) are not routed by the coordinator: only their card changes.
func (s *Server) fence(key string, on bool) {
	inst, ok := s.cfg.Instance(key)
	if !ok {
		return
	}
	if (inst.Role == "decode" || strings.HasPrefix(inst.Key, "decode")) && s.bal != nil {
		s.bal.setExcluded(inst.Endpoint, on)
	}
	if (inst.Key == "prefill" || inst.Role == "prefill") && s.kp != nil {
		s.kp.setDisabled(on)
	}
}

// applyTrainingFences re-applies the fences of the persisted transactions at
// startup, so a coordinator restart mid-training does not route to an engine
// that is stopped.
func (s *Server) applyTrainingFences() {
	for _, t := range openTrainTxs() {
		for _, k := range t.Instances {
			s.fence(k, true)
		}
		log.Printf("train: transacción abierta restaurada: job %s, %s en entrenamiento hasta %s",
			t.Job, strings.Join(t.Instances, ","), t.Deadline.Local().Format("2006-01-02 15:04"))
	}
}

// inflightOf is how many requests the coordinator still has in flight on an
// instance (what the drain waits for).
func (s *Server) inflightOf(key string) int {
	inst, ok := s.cfg.Instance(key)
	if !ok || inst.Endpoint == "" {
		return 0
	}
	n := 0
	if s.bal != nil {
		if d := s.bal.nodeByURL(inst.Endpoint); d != nil {
			i, _ := d.load()
			n += i
		}
	}
	if s.kp != nil && (inst.Key == "prefill" || inst.Role == "prefill") {
		n += s.kp.inflightOf(inst.Endpoint)
	}
	return n
}

// waitDrained waits until nothing is in flight on the given instances, or the
// budget runs out. Returns whether they drained.
func (s *Server) waitDrained(keys []string, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for {
		busy := 0
		for _, k := range keys {
			busy += s.inflightOf(k)
		}
		if busy == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// instanceUp is the health of an instance's endpoint right now.
func (s *Server) instanceUp(key string) bool {
	inst, ok := s.cfg.Instance(key)
	if !ok || inst.Endpoint == "" {
		return false
	}
	return s.getJSONFrom(inst.Endpoint, "/health") != nil
}

// waitUp waits until every instance answers /health, or the budget runs out,
// and reports each one's final state.
func (s *Server) waitUp(keys []string, budget time.Duration) map[string]bool {
	deadline := time.Now().Add(budget)
	for {
		out := map[string]bool{}
		allUp := true
		for _, k := range keys {
			out[k] = s.instanceUp(k)
			if !out[k] {
				allUp = false
			}
		}
		if allUp || time.Now().After(deadline) {
			return out
		}
		time.Sleep(2 * time.Second)
	}
}

// raiseInstance brings an instance up again through a preset this coordinator
// can launch (same endpoint or same key, main with a control plane). False when
// the instance is not the coordinator's to launch (a systemd unit of its node:
// its runner starts it).
func (s *Server) raiseInstance(key string) (bool, string) {
	inst, ok := s.cfg.Instance(key)
	if !ok {
		return false, "instancia desconocida"
	}
	for i := range s.cfg.Presets {
		p := &s.cfg.Presets[i]
		if p.Endpoint == "" || strings.TrimRight(p.Endpoint, "/") != strings.TrimRight(inst.Endpoint, "/") {
			continue
		}
		st := s.raisePreset(p)
		okv, _ := st["ok"].(bool)
		reason, _ := st["blocked"].(string)
		if reason == "" {
			reason, _ = st["error"].(string)
		}
		if reason == "" {
			reason, _ = st["action"].(string)
		}
		return okv, reason
	}
	return false, "sin preset lanzable para " + key + ": la remonta el runner de su nodo"
}

func parseDeadline(v string, now time.Time) (time.Time, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return now.Add(trainDeadlineDefault), nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("15:04", v, now.Location()); err == nil {
		d := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location())
		if !d.After(now) {
			d = d.Add(24 * time.Hour)
		}
		return d, nil
	}
	return time.Time{}, fmt.Errorf("deadline inválido %q: usa RFC3339 o HH:MM (hora local del coordinador)", v)
}

func clampSeconds(v int, def, max time.Duration) time.Duration {
	if v <= 0 {
		return def
	}
	d := time.Duration(v) * time.Second
	if d > max {
		return max
	}
	return d
}

func originOf(r *http.Request) string {
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return h
	}
	return r.RemoteAddr
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, k := range in {
		k = strings.TrimSpace(k)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, k)
	}
	return out
}

// trainBegin opens a transaction: fences the instances, drains them, and (when
// asked) stops the ones this coordinator can reach. POST + API key (mut).
//
//	{"job":"job_x","node":"n63","instancias":["decode"],"deadline":"07:00",
//	 "drain_s":90,"parar":false,"nota":"..."}
func (s *Server) trainBegin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Job       string   `json:"job"`
		Node      string   `json:"node"`
		Instances []string `json:"instancias"`
		Deadline  string   `json:"deadline"`
		DrainS    int      `json:"drain_s"`
		Stop      bool     `json:"parar"`
		Note      string   `json:"nota"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "body JSON inválido: " + err.Error()})
		return
	}
	req.Job = strings.TrimSpace(req.Job)
	req.Instances = dedupe(req.Instances)
	if req.Job == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "job requerido"})
		return
	}
	if len(req.Instances) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "instancias requeridas: claves de instances[] de la config (decode, prefill, ...)"})
		return
	}
	for _, k := range req.Instances {
		if _, ok := s.cfg.Instance(k); !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("instancia desconocida: %q", k)})
			return
		}
	}
	now := time.Now()
	deadline, err := parseDeadline(req.Deadline, now)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	trainState.mu.Lock()
	if t, ok := trainState.txs[req.Job]; ok {
		c := *t
		trainState.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{"error": "el job ya tiene una transacción abierta", "transaccion": c})
		return
	}
	for _, t := range trainState.txs {
		for _, k := range t.Instances {
			for _, want := range req.Instances {
				if k == want {
					job := t.Job
					trainState.mu.Unlock()
					writeJSON(w, http.StatusConflict, map[string]any{"error": fmt.Sprintf("%s ya está en entrenamiento por el job %s", k, job)})
					return
				}
			}
		}
	}
	tx := &trainTx{Job: req.Job, Node: req.Node, Instances: req.Instances, Deadline: deadline,
		Started: now, Origin: originOf(r), Phase: trainPhaseTraining, Note: req.Note}
	trainState.txs[req.Job] = tx
	saveTrainLocked()
	trainState.mu.Unlock()
	for _, k := range tx.Instances {
		s.fence(k, true)
	}
	log.Printf("train: job %s abre transacción: %s en entrenamiento hasta %s (nodo %s, origen %s)",
		tx.Job, strings.Join(tx.Instances, ","), deadline.Local().Format("2006-01-02 15:04"), tx.Node, tx.Origin)

	drained := s.waitDrained(tx.Instances, clampSeconds(req.DrainS, trainDrainDefault, trainDrainMax))
	rows := make([]map[string]any, 0, len(tx.Instances))
	for _, k := range tx.Instances {
		inst, _ := s.cfg.Instance(k)
		row := map[string]any{"key": k, "endpoint": inst.Endpoint, "inflight": s.inflightOf(k), "up": s.instanceUp(k)}
		if req.Stop && inst.Endpoint != "" {
			s.ejectEndpoint(inst.Endpoint)
			row["parada_pedida"] = true
			row["up"] = s.instanceUp(k)
		}
		rows = append(rows, row)
	}
	if !drained {
		log.Printf("train: job %s: el drenaje agotó su presupuesto con peticiones en vuelo", tx.Job)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "job": tx.Job, "deadline": tx.Deadline.Format(time.RFC3339),
		"drenado": drained, "instancias": rows,
	})
}

// trainEnd closes a transaction once the instances answer again. POST + API
// key (mut).
//
//	{"job":"job_x","levantar":false,"espera_s":120,"forzar":false}
//
// levantar asks the coordinator to raise the instances it can launch (presets);
// a systemd unit is started by its node's runner before calling end. If
// something is still down after espera_s the transaction stays open in phase
// "remontando" (fence kept, watchdog armed) and the reply says so with 202;
// forzar closes it regardless.
func (s *Server) trainEnd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Job   string `json:"job"`
		Raise bool   `json:"levantar"`
		WaitS int    `json:"espera_s"`
		Force bool   `json:"forzar"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "body JSON inválido: " + err.Error()})
		return
	}
	req.Job = strings.TrimSpace(req.Job)
	trainState.mu.Lock()
	t, ok := trainState.txs[req.Job]
	var tx trainTx
	if ok {
		tx = *t
	}
	trainState.mu.Unlock()
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "el job no tiene una transacción abierta"})
		return
	}
	raised := map[string]any{}
	if req.Raise {
		for _, k := range tx.Instances {
			okr, why := s.raiseInstance(k)
			raised[k] = map[string]any{"ok": okr, "detalle": why}
		}
	}
	ups := s.waitUp(tx.Instances, clampSeconds(req.WaitS, trainWaitDefault, trainWaitMax))
	rows := make([]map[string]any, 0, len(tx.Instances))
	allUp := true
	for _, k := range tx.Instances {
		inst, _ := s.cfg.Instance(k)
		rows = append(rows, map[string]any{"key": k, "endpoint": inst.Endpoint, "up": ups[k]})
		if !ups[k] {
			allUp = false
		}
	}
	if allUp || req.Force {
		s.closeTrain(tx.Job, map[bool]string{true: "instancias arriba", false: "cierre forzado"}[allUp])
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "job": tx.Job, "todas_arriba": allUp, "instancias": rows, "levantar": raised})
		return
	}
	trainState.mu.Lock()
	if t, ok := trainState.txs[tx.Job]; ok {
		t.Phase = trainPhaseReload
		saveTrainLocked()
	}
	trainState.mu.Unlock()
	log.Printf("train: job %s: el runner cerró pero sigue caído: %v; la transacción queda en remonte hasta %s",
		tx.Job, downKeys(ups), tx.Deadline.Local().Format("15:04"))
	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": false, "job": tx.Job, "todas_arriba": false, "instancias": rows, "levantar": raised,
		"nota": "la transacción sigue abierta (fase remontando): se cierra sola cuando respondan /health o la vigila el coordinador al vencer el deadline; forzar:true la cierra ya",
	})
}

func downKeys(ups map[string]bool) []string {
	var out []string
	for k, up := range ups {
		if !up {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// closeTrain lifts the fences and forgets the transaction.
func (s *Server) closeTrain(job, why string) {
	trainState.mu.Lock()
	t, ok := trainState.txs[job]
	if !ok {
		trainState.mu.Unlock()
		return
	}
	keys := append([]string(nil), t.Instances...)
	delete(trainState.txs, job)
	saveTrainLocked()
	trainState.mu.Unlock()
	for _, k := range keys {
		s.fence(k, false)
	}
	log.Printf("train: job %s cerrado (%s): %s vuelve al servicio", job, why, strings.Join(keys, ","))
}

// trainList is the open transactions (panel and HUD read it; no key needed,
// like /api/status).
func (s *Server) trainList(w http.ResponseWriter, r *http.Request) {
	list := openTrainTxs()
	rows := make([]map[string]any, 0, len(list))
	for _, t := range list {
		insts := make([]map[string]any, 0, len(t.Instances))
		for _, k := range t.Instances {
			inst, _ := s.cfg.Instance(k)
			insts = append(insts, map[string]any{"key": k, "endpoint": inst.Endpoint, "up": s.instanceUp(k)})
		}
		rows = append(rows, map[string]any{
			"job": t.Job, "node": t.Node, "fase": t.Phase, "inicio": t.Started.Format(time.RFC3339),
			"deadline": t.Deadline.Format(time.RFC3339), "avisado": t.Alerted, "nota": t.Note,
			"instancias": insts,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"transacciones": rows})
}

// trainWatchdog runs trainCheck every trainTick.
func (s *Server) trainWatchdog() {
	for {
		time.Sleep(trainTick)
		func() {
			defer recoverProbe()
			s.trainCheck(time.Now())
		}()
	}
}

// trainCheck closes transactions whose instances are back (phase remontando,
// or any phase once the deadline passed) and, past the deadline with something
// still down, tries to raise what this coordinator launched and reports once.
func (s *Server) trainCheck(now time.Time) {
	for _, t := range openTrainTxs() {
		ups := map[string]bool{}
		allUp := true
		for _, k := range t.Instances {
			ups[k] = s.instanceUp(k)
			if !ups[k] {
				allUp = false
			}
		}
		if allUp && (t.Phase == trainPhaseReload || now.After(t.Deadline)) {
			s.closeTrain(t.Job, "instancias arriba (vigilante)")
			continue
		}
		if now.Before(t.Deadline) {
			continue
		}
		down := downKeys(ups)
		details := []string{}
		stillDown := []string{}
		for _, k := range down {
			okr, why := s.raiseInstance(k)
			if okr && s.waitUp([]string{k}, 30*time.Second)[k] {
				details = append(details, k+": remontada por el coordinador")
				continue
			}
			stillDown = append(stillDown, k)
			details = append(details, k+": "+why)
		}
		if len(stillDown) == 0 {
			s.closeTrain(t.Job, "remontada por el vigilante")
			continue
		}
		if t.Alerted {
			continue
		}
		msg := fmt.Sprintf("sofmat: el job %s dejó parado %s para entrenar y a la hora límite (%s) sigue caído. %s. Revisa el nodo %s.",
			t.Job, strings.Join(stillDown, ","), t.Deadline.Local().Format("15:04"), strings.Join(details, "; "), t.Node)
		log.Printf("train: AVISO: %s", msg)
		if err := s.sendWhatsApp(msg); err != nil {
			log.Printf("train: aviso WhatsApp no enviado: %v", err)
		} else {
			log.Printf("train: aviso WhatsApp enviado para el job %s", t.Job)
		}
		trainState.mu.Lock()
		if cur, ok := trainState.txs[t.Job]; ok {
			cur.Alerted = true
			saveTrainLocked()
		}
		trainState.mu.Unlock()
	}
}

// sendWhatsApp delivers a text through the openclaw gateway container on this
// host (docker exec, the CLI reads the gateway token from its own config once
// the env override is unset). Success is only the CLI's confirmation line.
func (s *Server) sendWhatsApp(text string) error {
	a := s.cfg.Alerts
	if a.WhatsAppTarget == "" {
		return errors.New("sin destino: alerts.whatsapp_target vacío en la config")
	}
	container := a.WhatsAppContainer
	if container == "" {
		container = "openclaw-openclaw-gateway-1"
	}
	docker := a.DockerExe
	if docker == "" {
		docker = "docker"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, docker, "exec", container, "sh", "-c",
		`unset OPENCLAW_GATEWAY_TOKEN; node dist/index.js message send --channel whatsapp --target "$1" -m "$2"`,
		"sh", a.WhatsAppTarget, text)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	if !strings.Contains(string(out), "Sent via gateway") {
		return fmt.Errorf("sin confirmación del gateway: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// trainModelName is the display name of a configured gguf path (basename
// without the extension; a Windows path is not basename'd by filepath on
// Linux, hence the manual split).
func trainModelName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		p = p[i+1:]
	}
	return strings.TrimSuffix(p, ".gguf")
}
