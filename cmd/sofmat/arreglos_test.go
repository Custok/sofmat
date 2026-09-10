package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Pruebas de la revision del 10-09: cada una cubre un fallo demostrado, con su
// control. Lo que valen no es "ahora pasa" sino que el caso malo y el bueno se
// distingan.

// ---------- fail-closed sobre estado corrupto ----------

// Un fichero de estado ROTO no puede leerse como vacio: eso borraba todas las
// guardas en silencio.
func TestEstadoCorruptoNoSeLeeComoVacio(t *testing.T) {
	dir := t.TempDir()
	stateDir = dir
	defer func() { stateDir = ""; stateCorrupt = "" }()

	_ = os.WriteFile(filepath.Join(dir, "soflink-update-state.json"), []byte(`{"refused": {"a":`), 0o644)
	loadUpdateState()
	if stateCorrupt == "" {
		t.Fatal("un JSON truncado tiene que marcar el estado como corrupto")
	}
	if !strings.Contains(blockedReason(), "ilegible") {
		t.Fatalf("y blocked tiene que contarlo: %q", blockedReason())
	}
}

// CONTROL: un fichero que NO existe es estado vacio legitimo, no corrupcion.
func TestSinFicheroDeEstadoNoEsCorrupcion(t *testing.T) {
	dir := t.TempDir()
	stateDir = dir
	defer func() { stateDir = ""; stateCorrupt = "" }()
	loadUpdateState()
	if stateCorrupt != "" {
		t.Fatalf("sin fichero no hay corrupcion: %q", stateCorrupt)
	}
	if blockedReason() != "" {
		t.Fatalf("y blocked vacio: %q", blockedReason())
	}
}

// Con el estado corrupto, el updater NO instala aunque haya version nueva.
func TestConEstadoCorruptoNoSeActualiza(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("artefacto de mentira = script sh")
	}
	api, descargas := releaseFalso(t, "202609110000", artefacto("202609110000"), true)
	self, reexecd := banco(t, "202609101124", api)
	_ = os.WriteFile(filepath.Join(filepath.Dir(self), "soflink-update-state.json"), []byte("{{{"), 0o644)
	defer func() { stateCorrupt = "" }()

	checkAndUpdate(false)

	if *descargas != 0 || *reexecd != "" {
		t.Fatal("con el estado ilegible no se descarga ni se instala nada (fail-closed)")
	}
	_, res := LastCheck()
	if !strings.Contains(res, "NO me actualizo") {
		t.Fatalf("y lo dice: %q", res)
	}
}

// ---------- escritura atomica ----------

func TestElEstadoSeEscribeAtomicamente(t *testing.T) {
	dir := t.TempDir()
	stateDir = dir
	defer func() { stateDir = "" }()
	_ = spendAttempt("202609110000", "sha256:x")
	if _, err := os.Stat(filepath.Join(dir, "soflink-update-state.json.tmp")); err == nil {
		t.Fatal("no puede quedar el temporal: el rename lo consume")
	}
	if _, err := os.Stat(filepath.Join(dir, "soflink-update-state.json")); err != nil {
		t.Fatal("y el fichero final tiene que estar")
	}
}

// ---------- el panic ya no es mudo ----------

func TestUnPanicEnElUpdaterDejaConstancia(t *testing.T) {
	oldV, oldAPI, oldSelf := version, releasesAPI, selfPath
	version = "202609101124"
	releasesAPI = "http://127.0.0.1:1/x"
	selfPath = func() string { panic("boom de prueba") }
	limpiarUltimoChequeo()
	defer func() { version, releasesAPI, selfPath = oldV, oldAPI, oldSelf }()

	// Forzamos el panic dentro del camino: sin red no llega a selfPath, asi que
	// lo provocamos desde un release falso valido.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/latest") {
			fmt.Fprintf(w, `{"tag_name":"v202609110000","assets":[{"name":%q,"browser_download_url":"http://127.0.0.1:1/a"}]}`, assetName())
			return
		}
	}))
	defer srv.Close()
	releasesAPI = srv.URL + "/latest"
	stateDir = t.TempDir()
	defer func() { stateDir = "" }()

	checkAndUpdate(false) // selfPath hace panic dentro de applyUpdate

	_, res := LastCheck()
	if !strings.Contains(res, "PANIC") || !strings.Contains(res, "boom de prueba") {
		t.Fatalf("el panic tiene que quedar registrado con su causa: %q", res)
	}
	if updating.Load() {
		t.Fatal("y el candado tiene que soltarse")
	}
}

// ---------- presupuesto acotado al arrancar ----------

// Al arrancar, UN solo intento de descarga: si el servidor se cuelga, el
// gateway no puede quedarse 15 minutos sin puerto.
func TestAlArrancarSoloSeIntentaUnaVezLaDescarga(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	t0 := time.Now()
	err := bajarArtefacto(srv.URL, filepath.Join(t.TempDir(), "x.new"), "", true)
	if err == nil {
		t.Fatal("tenia que fallar")
	}
	if n != 1 {
		t.Fatalf("al arrancar es UN intento, hubo %d", n)
	}
	if time.Since(t0) > 5*time.Second {
		t.Fatalf("y sin pausas de reintento: %v", time.Since(t0))
	}
}

// CONTROL: fuera del arranque siguen siendo 3.
func TestFueraDelArranqueSiguenSiendoTresIntentos(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	_ = bajarArtefacto(srv.URL, filepath.Join(t.TempDir(), "x.new"), "", false)
	if int(n) != descargasPorIntento {
		t.Fatalf("fuera del arranque son %d intentos, hubo %d", descargasPorIntento, n)
	}
}

// ---------- constancia de lo instalado ----------

func TestAlInstalarQuedaConstanciaYSePurgaLoSuperado(t *testing.T) {
	dir := t.TempDir()
	stateDir = dir
	defer func() { stateDir = "" }()

	// basura de versiones viejas que antes se quedaba para siempre
	for i := 0; i < maxAttemptsPerVersion; i++ {
		_ = spendAttempt("202609090001", "sha256:a")
	}
	noteRefusedTag("202609090002", "sha256:b")
	// y una entrada de una version FUTURA, que si debe sobrevivir
	_ = spendAttempt("202609120000", "sha256:f")

	noteInstalled("202609101124", "deadbeef")

	st := loadUpdateState()
	if st.Installed.Version != "202609101124" || st.Installed.Sha != "deadbeef" || st.Installed.At == "" {
		t.Fatalf("la constancia de lo instalado no esta: %+v", st.Installed)
	}
	if _, ok := st.Attempts["202609090001"]; ok {
		t.Fatal("los intentos de una version superada tenian que purgarse")
	}
	if _, ok := st.RefusedTags["202609090002"]; ok {
		t.Fatal("el rechazo de una version superada tenia que purgarse")
	}
	if _, ok := st.Attempts["202609120000"]; !ok {
		t.Fatal("CONTROL: lo de una version FUTURA tiene que quedarse")
	}
}
