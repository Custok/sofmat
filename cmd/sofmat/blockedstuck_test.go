package main

import (
	"strings"
	"testing"
)

// REVISION 10-09: blocked se queda ENCENDIDO PARA SIEMPRE tras un solo rechazo,
// aunque despues se instale bien una version mas nueva.
//
// Escenario: v1 se rechaza (mal etiquetada). Sale v2, se instala bien.
// blocked deberia estar VACIO: el nodo esta al dia y sano.
func TestBlockedNoSeQuedaEncendidoTrasInstalarBienUnaVersionNueva(t *testing.T) {
	dir := t.TempDir()
	stateDir = dir
	oldV := version
	version = "202609080000" // el nodo corre algo ANTERIOR a v1 y v2
	defer func() { stateDir = ""; version = oldV }()

	// v1 rechazada: es lo que hace admitArtifact al ver un artefacto mal etiquetado
	st := loadUpdateState()
	st.Refused["sha-de-v1"] = "el release dice v1 y el binario declara v0"
	saveUpdateState(st)
	noteRefusedTag("202609090001", "sha256:v1digest")
	_ = spendAttempt("202609090001", "sha256:v1digest")

	if blockedReason() == "" {
		t.Fatal("con v1 rechazada, blocked tiene que decir algo (control)")
	}

	// sale v2 y se instala BIEN: es lo que hace applyUpdate al terminar
	_ = spendAttempt("202609090002", "sha256:v2digest")
	noteInstalled("202609090002", "sha-v2")
	version = "202609090002" // ahora corre v2

	if b := blockedReason(); b != "" {
		t.Fatalf("el nodo acaba de instalar v2 correctamente y blocked sigue diciendo: %q", b)
	}
}

// Y la variante con el contador agotado en v1, que tambien deberia dejar de
// importar en cuanto se instala algo mas nuevo.
func TestIntentosAgotadosDeUnaVersionViejaNoBloqueanTrasInstalarUnaNueva(t *testing.T) {
	dir := t.TempDir()
	stateDir = dir
	oldV := version
	version = "202609080000"
	defer func() { stateDir = ""; version = oldV }()

	for i := 0; i < maxAttemptsPerVersion; i++ {
		_ = spendAttempt("202609090001", "")
	}
	if !strings.Contains(blockedReason(), "intentos") {
		t.Fatal("control: con los 3 gastados en v1, blocked debe decirlo")
	}
	_ = spendAttempt("202609090002", "")
	noteInstalled("202609090002", "sha-v2")
	version = "202609090002"
	if b := blockedReason(); b != "" {
		t.Fatalf("v2 instalada bien y blocked sigue: %q", b)
	}
}

// El filtro de blockedReason tiene que valer POR SI SOLO, sin la purga de
// noteInstalled: cubre los ficheros de estado heredados de v2243..v1124, que
// nunca purgaron nada, y el hueco entre un rechazo y la siguiente instalacion.
// (Sin esta prueba, sabotear el filtro no ponia nada en rojo porque la purga lo
// tapaba.)
func TestElFiltroDeBlockedIgnoraVersionesSuperadasAunqueNadieHayaPurgado(t *testing.T) {
	dir := t.TempDir()
	stateDir = dir
	oldV := version
	defer func() { stateDir = ""; version = oldV }()

	// estado HEREDADO: entradas viejas que nadie purgo (asi quedaban antes)
	st := loadUpdateState()
	st.Attempts["202609090001"] = maxAttemptsPerVersion
	st.RefusedTags["202609090002"] = "sha256:x"
	st.Refused["sha-viejo"] = "el release dice v1 y el binario declara v0"
	saveUpdateState(st)

	version = "202609101124" // el nodo corre algo POSTERIOR a todo eso
	if b := blockedReason(); b != "" {
		t.Fatalf("entradas de versiones superadas no pueden bloquear: %q", b)
	}

	// CONTROL: una entrada de una version FUTURA si bloquea
	st = loadUpdateState()
	st.Attempts["202609120000"] = maxAttemptsPerVersion
	saveUpdateState(st)
	if b := blockedReason(); !strings.Contains(b, "202609120000") {
		t.Fatalf("una version futura agotada TIENE que bloquear: %q", b)
	}
}
