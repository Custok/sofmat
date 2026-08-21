// Package coordinator is the choreography lane: it wires the gateway's routing
// decisions to the transport handoff and the engine, and owns the
// wave/verify/rollback state machine for speculative decode. It holds no model
// weights — everything here is orchestration.
//
// Lane: coordinator-lane — synthesis + glue.
package coordinator

import (
	"net/http"

	"github.com/Custok/sofmat/internal/config"
)

// Run builds the gateway from config and starts the OpenAI-compatible serve
// loop. Decode is live; the prefill + KV-handoff path wires in when the engine
// binding lands.
func Run(cfg *config.Config) error {
	srv, err := NewServer(cfg)
	if err != nil {
		return err
	}
	loadRenames()        // restore display labels persisted from a prior run
	srv.startDiscovery() // self-populate the fleet from the LAN
	return http.ListenAndServe(cfg.Listen, srv.Handler())
}
