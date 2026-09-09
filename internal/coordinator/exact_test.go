package coordinator

import (
	"strconv"
	"strings"
	"testing"

	"github.com/Custok/sofmat/internal/gateway"
)

// El caso real del 09-09: un cuerpo cuya ESTIMACION por bytes cae por debajo del
// umbral y cuyo tamaño REAL no cabe. Con la estimacion entra y revienta contra el
// motor; con el conteo exacto se rechaza en la admision.
func TestExactCountCatchesWhatTheByteEstimateLetsThrough(t *testing.T) {
	b := newDecodeBalancer(balTestNodes(1))
	usable := int(float64(b.nodes[0].budgetTokens()) * budgetUse)

	// relleno sintetico: ~1,9 bytes por token real, como el que se midio
	body := bodyWith("", strings.Repeat("t0 ", 60000)) // ~180 KB -> estima ~60k
	est := estBodyTokens(body)
	if est > usable {
		t.Fatalf("la prueba no vale: la estimacion (%d) ya se pasa de %d", est, usable)
	}
	exact := usable + 5000 // lo que el tokenizador dice de verdad
	extra := gateway.Headers{gateway.ExactTokensHeader: strconv.Itoa(exact)}

	if got := bodyTokens(body, extra); got <= usable {
		t.Fatalf("con el conteo exacto debe salir por encima del utilizable: %d <= %d", got, usable)
	}
	if got := bodyTokens(body, nil); got != est {
		t.Fatalf("sin cabecera debe quedarse en la estimacion: %d != %d", got, est)
	}

	// y el efecto que importa: con el exacto se rechaza, con la estimacion no
	if _, _, tooBig := b.tooBigForEveryEngine(bodyTokens(body, extra)); !tooBig {
		t.Fatal("con el conteo exacto tenia que rechazarse")
	}
	if _, _, tooBig := b.tooBigForEveryEngine(bodyTokens(body, nil)); tooBig {
		t.Fatal("con la estimacion NO se rechazaba: si aqui rechaza, la prueba no discrimina")
	}
}

// CONTROL 1: la reserva de respuesta se sigue contando sobre el conteo exacto.
// Si se perdiera, una conversacion que cabe justa dejaria al motor sin sitio
// para responder — el fallo original con otra cara.
func TestExactCountStillReservesTheReply(t *testing.T) {
	body := gateway.Body{"messages": []any{map[string]any{"role": "user", "content": "hola"}},
		"max_tokens": 4096}
	extra := gateway.Headers{gateway.ExactTokensHeader: "50000"}
	if got, want := bodyTokens(body, extra), 50000+4096; got != want {
		t.Fatalf("prompt exacto + respuesta: %d != %d", got, want)
	}
}

// CONTROL 2: una cabecera ausente, vacia, cero o ilegible NO puede romper nada:
// se vuelve a la estimacion. "No se cuanto es" no es "cero".
func TestBadExactHeaderFallsBackToTheEstimate(t *testing.T) {
	body := bodyWith("sys", strings.Repeat("x", 30000))
	est := estBodyTokens(body)
	for _, v := range []string{"", "0", "-5", "no-soy-un-numero", "12.5"} {
		if got := bodyTokens(body, gateway.Headers{gateway.ExactTokensHeader: v}); got != est {
			t.Fatalf("cabecera %q: %d, se esperaba la estimacion %d", v, got, est)
		}
	}
	if got := bodyTokens(body, nil); got != est {
		t.Fatalf("sin cabeceras: %d != %d", got, est)
	}
}

// CONTROL 3: cuando el exacto es MENOR que la estimacion (prosa: bytes/3
// sobreestima) tambien manda el exacto — si no, seguiriamos rechazando de mas.
func TestExactCountAlsoWinsWhenItIsSmaller(t *testing.T) {
	body := bodyWith("", strings.Repeat("palabra ", 40000)) // ~320 KB -> estima ~106k
	est := estBodyTokens(body)
	extra := gateway.Headers{gateway.ExactTokensHeader: "70000"}
	got := bodyTokens(body, extra)
	if got >= est {
		t.Fatalf("el exacto (%d) tenia que bajar de la estimacion (%d)", got, est)
	}
	if got != 70000+replyReserveFor(body) {
		t.Fatalf("%d != %d", got, 70000+replyReserveFor(body))
	}
}
