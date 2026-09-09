package coordinator

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// Esperar sólo sirve contra la contención. Una conversación que no cabe ni en un
// motor vacío no va a caber porque otro termine: retenerla waitBudget entero y
// rechazarla igual es lo que produjo, el 09-09, 240 s de silencio y seis
// reintentos del cliente. Medido: 94 822 tokens contra 100 096 x 0.97 = 97 093,
// pasada por 16 tokens.
func TestConversationBiggerThanTheEngineIsRefusedAtOnce(t *testing.T) {
	b := newDecodeBalancer(balTestNodes(2))
	usable := int(float64(b.nodes[0].budgetTokens()) * budgetUse)

	orig := waitBudgetForTest
	waitBudgetForTest = 3 * time.Second // si esperase, se notaría
	defer func() { waitBudgetForTest = orig }()

	start := time.Now()
	n, done, err := b.pick("k", usable+1) // un solo token por encima de lo admisible
	done()
	elapsed := time.Since(start)

	if err == nil || n != nil {
		t.Fatalf("una conversación mayor que el motor debe rechazarse, got node=%v err=%v", n, err)
	}
	if !errors.Is(err, ErrEngineFull) {
		t.Fatalf("el rechazo tiene que ser identificable: %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("rechazó tras %v: esperó pudiendo saber ya que no cabía (waitBudget=%v)", elapsed, waitBudgetForTest)
	}
	// el mensaje es para un humano que ve fallar su editor: tiene que decir
	// cuánto se pasa y que esperar no habría servido
	for _, want := range []string{"no cabe", "vacíos", "Reduce el contexto"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("el error debe explicar qué hacer; falta %q en: %v", want, err)
		}
	}
}

// CONTROL 1: lo que sí es contención tiene que seguir esperando. Si este test
// pasa a fallar, el arreglo habrá convertido en fallo lo que era espera, que es
// justo lo que la constante waitBudget existe para evitar.
func TestTransientContentionStillWaits(t *testing.T) {
	b := newDecodeBalancer(balTestNodes(1))
	usable := int(float64(b.nodes[0].budgetTokens()) * budgetUse)
	b.nodes[0].claim(usable) // ocupado AHORA, pero la petición cabría en él vacío

	orig := waitBudgetForTest
	waitBudgetForTest = 400 * time.Millisecond
	defer func() { waitBudgetForTest = orig }()

	start := time.Now()
	if _, done, err := b.pick("k", usable/2); err == nil {
		done()
		t.Fatal("con el motor lleno no debería admitir")
	}
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Fatalf("rechazó en %v sin esperar: la contención transitoria debe esperar, no fallar", elapsed)
	}
}

// CONTROL 2: un presupuesto que aún no se ha sondeado no puede provocar un
// rechazo. "No sé cuánto cabe" y "no cabe" son cosas distintas.
func TestUnprobedBudgetNeverRefuses(t *testing.T) {
	b := newDecodeBalancer(balTestNodes(1))
	b.nodes[0].budget.Store(0) // sonda sin responder
	if _, _, tooBig := b.tooBigForEveryEngine(1 << 30); tooBig {
		t.Fatal("un presupuesto desconocido se ha convertido en un rechazo")
	}
}

// CONTROL 3: si UN motor sí tiene sitio, no se rechaza aunque el otro sea menor.
func TestFitsInTheRoomiestEngine(t *testing.T) {
	b := newDecodeBalancer(balTestNodes(2))
	b.nodes[0].budget.Store(10000)
	b.nodes[1].budget.Store(100096)
	tok := int(float64(10000)*budgetUse) + 5000 // no cabe en el pequeño, sí en el grande
	if _, _, tooBig := b.tooBigForEveryEngine(tok); tooBig {
		t.Fatal("se rechazó algo que cabe en el motor más grande")
	}
}
