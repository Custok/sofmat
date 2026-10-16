package coordinator

import (
	"net/http/httptest"
	"testing"

	"github.com/Custok/sofmat/internal/gateway"
)

// A client cannot name itself: whatever it sends under the stamp's header names
// is replaced by what the coordinator saw (RemoteAddr, its User-Agent, the path).
func TestStampClientOverwritesWhatTheClientClaims(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.RemoteAddr = "192.0.2.7:51234"
	r.Header.Set("User-Agent", "agente/1.0")
	r.Header.Set("X-Forwarded-For", "198.51.100.1")
	h := gateway.Headers{
		"x-sofmat-client-ip": "203.0.113.9", // lower-case spoof
		gateway.RouteHeader:  "/otra",
	}
	stampClient(h, r)
	if h[gateway.ClientIPHeader] != "192.0.2.7" || h[gateway.ClientUAHeader] != "agente/1.0" ||
		h[gateway.ClientFwdHeader] != "198.51.100.1" || h[gateway.RouteHeader] != "/v1/chat/completions" {
		t.Fatalf("stamp wrong: %v", h)
	}
	if _, spoof := h["x-sofmat-client-ip"]; spoof {
		t.Fatal("the client's own claim survived next to the stamp")
	}
}
