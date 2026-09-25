package coordinator

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Custok/sofmat/internal/config"
	"github.com/Custok/sofmat/internal/gateway"
)

// resetTrainState points the persistence at a scratch file and empties the
// in-memory transactions (they are package state).
func resetTrainState(t *testing.T) {
	t.Helper()
	t.Setenv("SOFMAT_TRAIN", filepath.Join(t.TempDir(), "train.json"))
	trainState.mu.Lock()
	trainState.txs = map[string]*trainTx{}
	trainState.mu.Unlock()
}

// fakeLlamaHealth answers /health and /slots like llama-server; nothing else.
func fakeLlamaHealth(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/slots":
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func postJSON(t *testing.T, h http.Handler, path string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(b)))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// A fenced engine takes no new conversation, loses the ones pinned to it on
// their next turn, and with every engine fenced a request is refused at once.
func TestBalancerSkipsFencedEngine(t *testing.T) {
	b := newDecodeBalancer([]struct{ Name, URL string }{{"d1", "http://d1"}, {"d2", "http://d2"}})
	if !b.setExcluded("http://d1", true) {
		t.Fatal("setExcluded: engine not found")
	}
	for i := 0; i < 6; i++ {
		n := b.chooseNode(fmt.Sprintf("conv-%d", i), 0)
		if n == nil || n.url != "http://d2" {
			t.Fatalf("conv-%d landed on %v, want d2 only while d1 is fenced", i, n)
		}
	}
	b.remember("pinned", b.nodes[0])
	if n := b.chooseNode("pinned", 0); n == nil || n.url != "http://d2" {
		t.Fatalf("pinned conversation stayed on the fenced engine: %v", n)
	}
	b.setExcluded("http://d2", true)
	if _, _, err := b.pick("conv-x", 10); !errors.Is(err, ErrDecodeTraining) {
		t.Fatalf("pick with every engine fenced: err=%v, want ErrDecodeTraining", err)
	}
	b.setExcluded("http://d1", false)
	n, done, err := b.pick("conv-y", 10)
	if err != nil || n == nil || n.url != "http://d1" {
		t.Fatalf("after lifting the fence: n=%v err=%v", n, err)
	}
	done()
}

// A fenced prefill drops out of the handoff the way a missing one does: the
// gateway is told to serve direct.
func TestKVPipeDisabledSkipsHandoff(t *testing.T) {
	k := newKVPipe("http://pre", "http://dec", "http://pre-ctl", "http://dec-ctl")
	if _, err := k.routeFor(gateway.Body{}); err != nil {
		t.Fatalf("baseline route: %v", err)
	}
	k.setDisabled(true)
	if _, err := k.routeFor(gateway.Body{}); !errors.Is(err, gateway.ErrSkipHandoff) {
		t.Fatalf("disabled pipe: err=%v, want ErrSkipHandoff", err)
	}
	k.setDisabled(false)
	if _, err := k.routeFor(gateway.Body{}); err != nil {
		t.Fatalf("re-enabled route: %v", err)
	}
}

// begin fences + persists + refuses overlaps; the panel shows the fence; end
// lifts it once the engine answers again.
func TestTrainBeginEndFencesAndPersists(t *testing.T) {
	resetTrainState(t)
	dec := fakeLlamaHealth(t)
	pre := fakeLlamaHealth(t)
	cfg := &config.Config{Instances: []config.Instance{
		{Key: "decode", Role: "decode", Endpoint: dec.URL, Main: "node-d"},
		{Key: "prefill", Role: "prefill", Endpoint: pre.URL, Main: "node-c"},
	}}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	deadline := time.Now().Add(time.Hour).Format(time.RFC3339)
	code, out := postJSON(t, h, "/api/train/begin", map[string]any{
		"job": "job_1", "node": "n63", "instancias": []string{"decode"}, "deadline": deadline, "drain_s": 1})
	if code != http.StatusOK {
		t.Fatalf("begin: %d %v", code, out)
	}
	if out["drenado"] != true {
		t.Fatalf("begin: drenado=%v, want true (nothing in flight)", out["drenado"])
	}
	if _, _, err := s.bal.pick("conv", 10); !errors.Is(err, ErrDecodeTraining) {
		t.Fatalf("fenced decode still admitted: %v", err)
	}
	if b, err := os.ReadFile(trainPath()); err != nil || !bytes.Contains(b, []byte(`"job_1"`)) {
		t.Fatalf("transaction not persisted: err=%v body=%s", err, b)
	}
	if code, _ := postJSON(t, h, "/api/train/begin", map[string]any{"job": "job_1", "instancias": []string{"prefill"}}); code != http.StatusConflict {
		t.Fatalf("same job twice: %d, want 409", code)
	}
	if code, _ := postJSON(t, h, "/api/train/begin", map[string]any{"job": "job_2", "instancias": []string{"decode"}}); code != http.StatusConflict {
		t.Fatalf("same instance twice: %d, want 409", code)
	}
	if code, _ := postJSON(t, h, "/api/train/begin", map[string]any{"job": "job_3", "instancias": []string{"nope"}}); code != http.StatusBadRequest {
		t.Fatalf("unknown instance: %d, want 400", code)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	var st struct {
		Instances []map[string]any `json:"instances"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &st)
	found := false
	for _, ic := range st.Instances {
		if ic["key"] == "decode" {
			found = true
			if ic["training"] != true {
				t.Fatalf("decode card not marked training: %v", ic)
			}
		}
	}
	if !found {
		t.Fatalf("no decode card while training: %v", st.Instances)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/train", nil))
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"job_1"`)) {
		t.Fatalf("/api/train: %s", rec.Body.String())
	}
	code, out = postJSON(t, h, "/api/train/end", map[string]any{"job": "job_1", "espera_s": 1})
	if code != http.StatusOK || out["todas_arriba"] != true {
		t.Fatalf("end: %d %v", code, out)
	}
	n, done, err := s.bal.pick("conv2", 10)
	if err != nil || n == nil {
		t.Fatalf("decode not back after end: %v", err)
	}
	done()
	if len(openTrainTxs()) != 0 {
		t.Fatalf("transaction still open after end: %v", openTrainTxs())
	}
	if code, _ := postJSON(t, h, "/api/train/end", map[string]any{"job": "job_1"}); code != http.StatusNotFound {
		t.Fatalf("end twice: %d, want 404", code)
	}
}

// end with the engine still down keeps the fence and the transaction (phase
// remontando); the watchdog reports once past the deadline; forzar closes it.
func TestTrainEndKeepsFenceWhileDown(t *testing.T) {
	resetTrainState(t)
	dec := fakeLlamaHealth(t)
	cfg := &config.Config{Instances: []config.Instance{{Key: "decode", Role: "decode", Endpoint: dec.URL, Main: "node-d"}}}
	s, err := NewServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	past := time.Now().Add(-time.Minute).Format(time.RFC3339)
	if code, out := postJSON(t, h, "/api/train/begin", map[string]any{"job": "job_d", "instancias": []string{"decode"}, "deadline": past, "drain_s": 1}); code != http.StatusOK {
		t.Fatalf("begin: %d %v", code, out)
	}
	dec.Close() // the runner stopped it and it did not come back
	code, out := postJSON(t, h, "/api/train/end", map[string]any{"job": "job_d", "espera_s": 1})
	if code != http.StatusAccepted || out["todas_arriba"] != false {
		t.Fatalf("end with the engine down: %d %v, want 202 and todas_arriba=false", code, out)
	}
	txs := openTrainTxs()
	if len(txs) != 1 || txs[0].Phase != trainPhaseReload {
		t.Fatalf("transaction should stay open in phase remontando: %+v", txs)
	}
	if _, _, err := s.bal.pick("conv", 10); !errors.Is(err, ErrDecodeTraining) {
		t.Fatalf("fence lifted although the engine is down: %v", err)
	}
	s.trainCheck(time.Now())
	txs = openTrainTxs()
	if len(txs) != 1 || !txs[0].Alerted {
		t.Fatalf("watchdog did not report the instance still down: %+v", txs)
	}
	if code, _ := postJSON(t, h, "/api/train/end", map[string]any{"job": "job_d", "forzar": true, "espera_s": 1}); code != http.StatusOK {
		t.Fatalf("forced end: %d", code)
	}
	if len(openTrainTxs()) != 0 {
		t.Fatal("forced end left the transaction open")
	}
	if n, done, err := s.bal.pick("conv3", 10); err != nil || n == nil {
		t.Fatalf("fence not lifted by the forced end: %v", err)
	} else {
		done()
	}
}

// Open transactions survive a restart: saved, reloaded, fences re-applied.
func TestTrainPersistenceRoundTrip(t *testing.T) {
	resetTrainState(t)
	trainState.mu.Lock()
	trainState.txs["job_p"] = &trainTx{Job: "job_p", Instances: []string{"decode"},
		Deadline: time.Now().Add(time.Hour).Round(time.Second), Started: time.Now().Round(time.Second), Phase: trainPhaseTraining}
	saveTrainLocked()
	trainState.txs = map[string]*trainTx{}
	trainState.mu.Unlock()
	loadTrain()
	txs := openTrainTxs()
	if len(txs) != 1 || txs[0].Job != "job_p" || len(txs[0].Instances) != 1 || txs[0].Instances[0] != "decode" {
		t.Fatalf("round trip: %+v", txs)
	}
	dec := fakeLlamaHealth(t)
	s, err := NewServer(&config.Config{Instances: []config.Instance{{Key: "decode", Role: "decode", Endpoint: dec.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	s.applyTrainingFences()
	if _, _, err := s.bal.pick("conv", 10); !errors.Is(err, ErrDecodeTraining) {
		t.Fatalf("fence not re-applied after reload: %v", err)
	}
}

func TestParseDeadlineForms(t *testing.T) {
	now := time.Date(2026, 9, 25, 23, 30, 0, 0, time.Local)
	if d, err := parseDeadline("", now); err != nil || !d.Equal(now.Add(trainDeadlineDefault)) {
		t.Fatalf("empty: %v %v", d, err)
	}
	if d, err := parseDeadline("07:00", now); err != nil || d.Day() != 26 || d.Hour() != 7 {
		t.Fatalf("HH:MM past midnight: %v %v", d, err)
	}
	if d, err := parseDeadline("23:45", now); err != nil || d.Day() != 25 || d.Hour() != 23 || d.Minute() != 45 {
		t.Fatalf("HH:MM later today: %v %v", d, err)
	}
	if d, err := parseDeadline("2026-09-26T07:00:00+02:00", now); err != nil || d.Hour() != 7 {
		t.Fatalf("RFC3339: %v %v", d, err)
	}
	if _, err := parseDeadline("mañana", now); err == nil {
		t.Fatal("garbage accepted as a deadline")
	}
}
