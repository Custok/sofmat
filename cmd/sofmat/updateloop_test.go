package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// artefacto fabrica algo que pesa lo que pesa un binario (el control de tamano
// exige > 1 MB) y que responde a "version" diciendo lo que se le mande. Si
// declara != lo que promete el release, es el artefacto rancio del 07-09.
func artefacto(declara string) []byte {
	cuerpo := "#!/bin/sh\necho \"soflink " + declara + "\"\nexit 0\n"
	return []byte(cuerpo + "#" + strings.Repeat("x", 1_100_000) + "\n")
}

// releaseFalso levanta un GitHub de mentira y cuenta cuantas veces se DESCARGA
// el asset: esa cuenta es la que distingue "no se instalo" de "no se reintento".
func releaseFalso(t *testing.T, tag string, cuerpo []byte, publicarDigest bool) (api string, descargas *int32) {
	t.Helper()
	var n int32
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/asset", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		_, _ = w.Write(cuerpo)
	})
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		dig := ""
		if publicarDigest {
			s := sha256.Sum256(cuerpo)
			dig = ", \"digest\": \"sha256:" + hex.EncodeToString(s[:]) + "\""
		}
		fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"name":%q,"browser_download_url":%q%s}]}`,
			"v"+tag, assetName(), srv.URL+"/asset", dig)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL + "/releases/latest", &n
}

// banco monta un soflink "en produccion": un fichero que corre, el estado al
// lado, el release apuntando al servidor falso y reexec desactivado (el de
// verdad no vuelve nunca).
func banco(t *testing.T, corriendo, api string) (self string, reexecd *string) {
	t.Helper()
	dir := t.TempDir()
	self = filepath.Join(dir, "soflink")
	if err := os.WriteFile(self, artefacto(corriendo), 0o755); err != nil {
		t.Fatal(err)
	}
	var llamado string

	oldSelf, oldAPI, oldRe, oldVer, oldState := selfPath, releasesAPI, reexec, version, stateDir
	selfPath = func() string { return self }
	releasesAPI = api
	reexec = func(p string) error { llamado = p; return nil }
	version = corriendo
	stateDir = dir
	t.Cleanup(func() {
		selfPath, releasesAPI, reexec, version, stateDir = oldSelf, oldAPI, oldRe, oldVer, oldState
	})
	return self, &llamado
}

func declara(t *testing.T, path string) string {
	t.Helper()
	v, err := declaredVersion(path)
	if err != nil {
		t.Fatalf("el fichero que corre no responde a version: %v", err)
	}
	return v
}

// ESTE es el bucle del 07-09, entero: GitHub anuncia una version, el artefacto
// que sirve declara otra. Antes: se instalaba, arrancaba diciendo la vieja,
// volvia a verse desactualizado, se actualizaba otra vez. 281 arranques.
func TestElBucleDel0709NoSeRepite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("el artefacto de mentira es un script sh")
	}
	api, descargas := releaseFalso(t, "202609092200", artefacto("202609091430"), true)
	self, reexecd := banco(t, "202609091815", api)

	checkAndUpdate()

	if v := declara(t, self); v != "202609091815" {
		t.Fatalf("han sustituido el binario que corre por uno rancio: ahora declara %s", v)
	}
	if *reexecd != "" {
		t.Fatal("ha re-ejecutado: eso es exactamente la vuelta del bucle")
	}
	if *descargas != 1 {
		t.Fatalf("descargas en la primera vuelta: %d", *descargas)
	}

	// La segunda vuelta es la que importa. Un rechazo que no se recuerda es un
	// bucle mas lento: ni siquiera debe volver a bajarselo.
	checkAndUpdate()
	if *descargas != 1 {
		t.Fatalf("se lo ha vuelto a descargar (%d veces): el rechazo no persiste", *descargas)
	}
	if b := blockedReason(); b == "" {
		t.Fatal("el panel no diria nada: desde fuera no se distingue 'al dia' de 'atascado'")
	}
}

// CONTROL: sin esto, lo de arriba lo aprobaria un auto-update que no actualiza
// NUNCA, que es el otro fallo —el que David tiene ahora con -no-update.
func TestUnaActualizacionBuenaSiEntra(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("el artefacto de mentira es un script sh")
	}
	api, descargas := releaseFalso(t, "202609092200", artefacto("202609092200"), true)
	self, reexecd := banco(t, "202609091815", api)

	checkAndUpdate()

	if v := declara(t, self); v != "202609092200" {
		t.Fatalf("la actualizacion buena NO ha entrado: sigue declarando %s", v)
	}
	if *reexecd != self {
		t.Fatalf("no ha re-ejecutado el binario nuevo: %q", *reexecd)
	}
	if *descargas != 1 {
		t.Fatalf("descargas: %d", *descargas)
	}
	if b := blockedReason(); b != "" {
		t.Fatalf("tras actualizarse bien no puede quedar nada marcado: %q", b)
	}
}

// Republicar el release ARREGLADO tiene que funcionar. Si el contador no se
// rearma, el nodo se queda clavado hasta la siguiente version y "republica para
// arreglarlo" es mentira.
func TestRepublicarloArregladoDesatasca(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("el artefacto de mentira es un script sh")
	}
	dir := t.TempDir()
	stateDir = dir
	defer func() { stateDir = "" }()

	// se gastan todos los intentos con el artefacto malo
	for i := 0; i <= maxAttemptsPerVersion; i++ {
		_ = spendAttempt("202609092200", "sha256:malo")
	}
	if err := spendAttempt("202609092200", "sha256:malo"); err == nil {
		t.Fatal("con el artefacto malo tenia que estar cortado")
	}
	// lo republican con otro contenido
	if err := spendAttempt("202609092200", "sha256:bueno"); err != nil {
		t.Fatalf("republicado con otro artefacto tiene que volver a intentarlo: %v", err)
	}
}

// Sin digest publicado no hay forma de reconocer el fichero sin bajarselo. Lo
// que NO puede pasar es que sean infinitas: el contador tiene que acotarlas.
func TestSinDigestLasVueltasSonFinitas(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("el artefacto de mentira es un script sh")
	}
	api, descargas := releaseFalso(t, "202609092200", artefacto("202609091430"), false)
	self, _ := banco(t, "202609091815", api)

	for i := 0; i < 10; i++ {
		checkAndUpdate()
	}
	if v := declara(t, self); v != "202609091815" {
		t.Fatalf("han sustituido el binario que corre: declara %s", v)
	}
	if int(*descargas) > maxAttemptsPerVersion {
		t.Fatalf("%d descargas: el contador no las esta acotando", *descargas)
	}
}
