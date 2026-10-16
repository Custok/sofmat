package coordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/Custok/sofmat/internal/gateway"
)

// (2026-10-16, David: «arregla la puta pérdida de historial») The hybrid model
// can only go back to a context checkpoint, and the engine takes one where each
// processed span BEGINS. A user turn used to be one span that started where the
// previous turn diverged, so the checkpoint landed there again: every turn the
// HUD rewrites the previous assistant message (tool_calls -> final text) and
// the engine fell back to the start of the history (20 186) and reprocessed it
// whole — and it never recovered, because the new span began at that same spot.
// Fix: at the start of each user turn, process the prompt up to the end of the
// user's message first (n_predict 0). The real request then begins its span at
// the assistant header, so a checkpoint sits right before the message that the
// next turn will change; that turn reprocesses only the last reply and its own
// message, not the whole history.
const userCkptMarker = "<|im_start|>assistant"

var userCkptClient = &http.Client{Timeout: 180 * time.Second}

// lastRoleIsUser reports whether the conversation's last message is the user's:
// only then is this the first request of a user turn (tool rounds end in "tool").
func lastRoleIsUser(body gateway.Body) bool {
	msgs, _ := body["messages"].([]any)
	if len(msgs) < 2 {
		return false
	}
	m, _ := msgs[len(msgs)-1].(map[string]any)
	role, _ := m["role"].(string)
	return role == "user"
}

func postEngine(ctx context.Context, url string, body any) (map[string]any, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := userCkptClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("HTTP %d: %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %v", resp.StatusCode, out["error"])
	}
	return out, nil
}

// checkpointUserTurn runs the first pass and pins the request to the slot it
// used. Best-effort: on any failure the request goes on exactly as before.
func (s *Server) checkpointUserTurn(ctx context.Context, decodeURL string, plan *gateway.Plan) {
	if !lastRoleIsUser(plan.Body) {
		return
	}
	t0 := time.Now()
	tb := gateway.Body{}
	for k, v := range plan.Body {
		if k != "stream" && k != "stream_options" && k != "id_slot" {
			tb[k] = v
		}
	}
	tpl, err := postEngine(ctx, decodeURL+"/apply-template", tb)
	if err != nil {
		plan.Note("user_ckpt_error", "apply-template: "+err.Error())
		return
	}
	prompt, _ := tpl["prompt"].(string)
	cut := strings.LastIndex(prompt, userCkptMarker)
	if cut <= 0 {
		plan.Note("user_ckpt_error", "sin cabecera de respuesta en la plantilla")
		return
	}
	pass := map[string]any{"prompt": prompt[:cut], "n_predict": 0, "cache_prompt": true}
	if v, ok := plan.Body["id_slot"]; ok {
		pass["id_slot"] = v
	}
	comp, err := postEngine(ctx, decodeURL+"/completion", pass)
	if err != nil {
		plan.Note("user_ckpt_error", "completion: "+err.Error())
		log.Printf("checkpoint de turno: %v", err)
		return
	}
	plan.Note("user_ckpt_ms", msSince(t0))
	if tm, _ := comp["timings"].(map[string]any); tm != nil {
		plan.Note("user_ckpt_cache_n", tm["cache_n"])
		plan.Note("user_ckpt_prompt_n", tm["prompt_n"])
	}
	// the real request must land in the slot that now holds the checkpoint
	if _, pinned := plan.Body["id_slot"]; !pinned {
		if v, ok := comp["id_slot"].(float64); ok && v >= 0 {
			plan.Body["id_slot"] = int(v)
			plan.Note("user_ckpt_slot", int(v))
		}
	}
}
