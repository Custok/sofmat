package coordinator

// Route policy — who may call what, declared where the routes are registered.
//
// The 2026-09-09 review enumerated all 52 routes and found the protection had
// drifted into something that looked deliberate and was not:
//
//	/api/eject       guarded          |  /control/eject      NOT guarded
//	/api/load        guarded          |  /control/load       NOT guarded
//	/api/models/*    guarded          |  /control/kill       NOT guarded
//	                                  |  /api/update/fleet   NOT guarded
//	                                  |  /api/setconfig      NOT guarded
//
// Two URLs reached the same eject; one asked for the key and the other did not.
// And almost nothing checked the METHOD, so a crawler or a browser prefetching
// a link could eject an engine, kill a process by port, or restart the whole
// fleet — measured: /api/genkey minted a live key on a GET and on a HEAD, and
// two operators tripped it by accident twenty seconds apart.
//
// So the wrappers below are the policy, and every route is registered through
// exactly one of them. Adding a route now means choosing its class.

import (
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// mut wraps a MUTATING route: it needs the API key and it needs POST.
// POST matters as much as the key: anything that only follows URLs — a
// prefetch, a crawler, an uptime monitor — issues GET and HEAD, and those must
// never reach an action.
func (s *Server) mut(next http.HandlerFunc) http.HandlerFunc {
	return s.guard(postOnly(next))
}

func postOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeJSON(w, http.StatusMethodNotAllowed,
				map[string]any{"error": "esta ruta cambia el estado: usa POST"})
			return
		}
		next(w, r)
	}
}

// peer wraps the node-to-node surface (/control/*, /kv/, /soflink/rename).
//
// These are not panel routes: one soflink calls another's to fetch a KV state,
// launch an engine or free a port. The nodes do NOT send a credential to each
// other — each has its own key — so requiring the key here would break the KV
// handoff. What they DO have is identity: a legitimate call comes from a host
// already listed in `nodes` or from this machine. That is a weaker boundary
// than a secret and an enormous improvement over none, which is what these
// routes had while their /api/ twins asked for a key.
//
// A valid API key is also accepted, so an operator can still drive them by hand.
//
// LIMIT, stated rather than discovered later: this protects against the LAN, not
// against the host. A container started with NetworkMode=host shares the host's
// stack, so :1357 is LOOPBACK to it — and loopback must pass, because the daemon
// calls its own control surface. Any origin-based filter has this hole; the same
// argument ruled out an iptables rule for the same job. Closing it needs a
// secret, not an origin. There is at least one such container on the fleet
// (a node-exporter), so this is a real exception and not a theoretical one.
func (s *Server) peer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.fromPeer(r) || s.authOK(r) {
			next(w, r)
			return
		}
		// Say who was turned away. These routes kill processes and delete state
		// files; a refusal with no record leaves nothing to look at the day
		// something unexpected knocks. Asked for by the node devs, and it is the
		// same reason the chat path logs its caller.
		log.Printf("peer-rechazado: from=%s ruta=%s metodo=%s", r.RemoteAddr, r.URL.Path, r.Method)
		writeJSON(w, http.StatusUnauthorized, map[string]any{
			"error": "ruta entre nodos: sólo desde un nodo declarado en la configuración, o con la API key"})
	}
}

// fromPeer reports whether the request comes from this machine or from a host
// this node already talks to. RemoteAddr only: X-Forwarded-For is set by the
// caller, so trusting it would let anyone claim to be a peer.
func (s *Server) fromPeer(r *http.Request) bool {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() {
		return true
	}
	for _, h := range s.peerHosts() {
		if h == ip.String() {
			return true
		}
	}
	return false
}

// peerHosts are the hosts named in the configuration: the node agents and the
// engine endpoints. Anything not in the config is not a peer.
func (s *Server) peerHosts() []string {
	if s.cfg == nil {
		return nil
	}
	var out []string
	add := func(raw string) {
		if raw == "" {
			return
		}
		if !strings.Contains(raw, "//") {
			raw = "http://" + raw
		}
		u, err := url.Parse(raw)
		if err != nil {
			return
		}
		h := u.Hostname()
		if ip := net.ParseIP(h); ip != nil {
			out = append(out, ip.String())
			return
		}
		if addrs, err := net.LookupHost(h); err == nil {
			out = append(out, addrs...)
		}
	}
	for _, n := range s.cfg.Nodes {
		add(n.Agent)
	}
	for _, i := range s.cfg.Instances {
		add(i.Endpoint)
	}
	return out
}
