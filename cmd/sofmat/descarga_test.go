package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// Lo que cierran estas pruebas, medido en .63 la noche del 09-10:
//
//	46 fallos de auto-update, TODOS "Client.Timeout exceeded while awaiting
//	headers", 19 de ellos seguidos contra la misma version — y 274 exitos.
//	O sea: intermitente. Y cada fallo gastaba un intento de los 3, asi que tres
//	hipos seguidos dejaban el nodo clavado con un artefacto perfecto al otro lado.
//
// Un binario de mentira que pesa lo que hace falta para pasar el control de
// tamaño (>1 MB).
func artefactoValido(declara string) []byte {
	return []byte("#!/bin/sh\necho \"soflink " + declara + "\"\nexit 0\n" +
		"#" + strings.Repeat("y", 1_100_000) + "\n")
}

// EL CASO: la descarga falla unas cuantas veces y luego va. Antes eso era un
// fallo completo; ahora se reintenta dentro de la misma vuelta.
func TestUnaDescargaQueFallaYLuegoVaSeReintenta(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) < 3 { // los dos primeros mueren
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write(artefactoValido("202609101100"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	stateDir = dir
	defer func() { stateDir = "" }()
	dest := filepath.Join(dir, "soflink.new")

	if err := bajarArtefacto(srv.URL, dest, "", false); err != nil {
		t.Fatalf("tenia que acabar consiguiendolo: %v", err)
	}
	if n != 3 {
		t.Fatalf("esperaba 3 intentos, hubo %d", n)
	}
	if fi, err := os.Stat(dest); err != nil || fi.Size() < 1_000_000 {
		t.Fatalf("el artefacto no ha quedado bien: %v", err)
	}
}

// CONTROL: si falla SIEMPRE, se rinde y lo marca como fallo de RED. Sin esto,
// la de arriba pasaria con un bucle infinito o con un reintento que no clasifica.
func TestSiLaDescargaNuncaVaSeRindeYLoMarcaComoRed(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	dir := t.TempDir()
	err := bajarArtefacto(srv.URL, filepath.Join(dir, "x.new"), "", false)
	if err == nil {
		t.Fatal("tenia que fallar")
	}
	if !errors.Is(err, ErrDescarga) {
		t.Fatalf("un fallo de red tiene que ser reconocible como tal: %v", err)
	}
	if int(n) != descargasPorIntento {
		t.Fatalf("esperaba %d intentos, hubo %d", descargasPorIntento, n)
	}
}

// EL OTRO CASO, el que dejaba clavado el nodo: un fallo de red DEVUELVE el
// intento. El presupuesto de 3 es contra artefactos malos, no contra la linea.
func TestUnFalloDeRedNoGastaIntento(t *testing.T) {
	dir := t.TempDir()
	stateDir = dir
	defer func() { stateDir = "" }()

	if err := spendAttempt("202609101100", "sha256:aa"); err != nil {
		t.Fatal(err)
	}
	if got := loadUpdateState().Attempts["202609101100"]; got != 1 {
		t.Fatalf("deberia haber 1 intento gastado, hay %d", got)
	}

	refundAttempt("202609101100") // lo que hace checkAndUpdate ante ErrDescarga

	if got, ok := loadUpdateState().Attempts["202609101100"]; ok && got != 0 {
		t.Fatalf("el intento tenia que devolverse, quedan %d", got)
	}
	// y por tanto el nodo NO se ata: puede seguir intentandolo indefinidamente
	for i := 0; i < 10; i++ {
		if err := spendAttempt("202609101100", "sha256:aa"); err != nil {
			refundAttempt("202609101100")
			continue
		}
		refundAttempt("202609101100")
	}
	if err := spendAttempt("202609101100", "sha256:aa"); err != nil {
		t.Fatalf("tras diez fallos de red devueltos, aun debe poder intentarlo: %v", err)
	}
}

// CONTROL del contador: un fallo que NO es de red sigue gastando intento y
// sigue atando el nodo a la tercera. Sin esto, la de arriba pasaria con un
// contador desactivado del todo, que es el otro modo de fallo.
func TestUnFalloQueNoEsDeRedSIGastaIntento(t *testing.T) {
	dir := t.TempDir()
	stateDir = dir
	defer func() { stateDir = "" }()

	for i := 1; i <= maxAttemptsPerVersion; i++ {
		if err := spendAttempt("202609101100", "sha256:bb"); err != nil {
			t.Fatalf("el intento %d debia permitirse: %v", i, err)
		}
		// sin refund: aqui el fallo NO es de red
	}
	if err := spendAttempt("202609101100", "sha256:bb"); err == nil {
		t.Fatal("a la cuarta tiene que cortar: el cinturon sigue puesto")
	}
}

// refundAttempt nunca baja de cero ni inventa entradas.
func TestElReintegroNoBajaDeCero(t *testing.T) {
	dir := t.TempDir()
	stateDir = dir
	defer func() { stateDir = "" }()

	refundAttempt("no-existe")
	if len(loadUpdateState().Attempts) != 0 {
		t.Fatal("no puede crear entradas de la nada")
	}
	_ = spendAttempt("v", "")
	refundAttempt("v")
	refundAttempt("v")
	if got, ok := loadUpdateState().Attempts["v"]; ok && got < 0 {
		t.Fatalf("ha bajado de cero: %d", got)
	}
}

// El cliente de descarga NO puede ser el de la API: 8 s cubren la lectura del
// cuerpo y convierten cualquier lentitud en un fallo total.
func TestElClienteDeDescargaTienePresupuestoGeneroso(t *testing.T) {
	api := &http.Client{Timeout: apiTimeout}
	if downloadClient.Timeout <= api.Timeout {
		t.Fatalf("la descarga no puede correr con el reloj de la API: %v vs %v",
			downloadClient.Timeout, api.Timeout)
	}
	if downloadClient.Timeout < 60_000_000_000 { // 60 s
		t.Fatalf("presupuesto demasiado corto para un binario: %v", downloadClient.Timeout)
	}
}
