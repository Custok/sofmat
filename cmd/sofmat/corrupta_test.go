package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"testing"
)

// Hallazgo de debian-dev (10-09): una descarga CORRUPTA por la red —cuerpo
// >1 MB, HTTP 200, pero bytes distintos al digest publicado— se trata como
// artefacto MALO: rechazo permanente por sha + RefusedTags + intento gastado.
// El siguiente ciclo ni siquiera vuelve a descargar. Una corrupcion de red
// bloquea esa version PARA SIEMPRE.
func TestUnaDescargaCorruptaNoDebeBloquearLaVersionParaSiempre(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("artefacto de mentira = script sh")
	}
	bueno := artefacto("202609100900")
	var n int32
	corrupto := true
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("/asset", func(w http.ResponseWriter, r *http.Request) {
		n++
		if corrupto { // primera vez: la red entrega bytes distintos
			b := append([]byte{}, bueno...)
			b[len(b)/2] ^= 0xFF
			_, _ = w.Write(b)
			return
		}
		_, _ = w.Write(bueno)
	})
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		s := shaDe(t, mustWrite(t, bueno))
		fmt.Fprintf(w, `{"tag_name":"v202609100900","assets":[{"name":%q,"browser_download_url":%q,"digest":"sha256:%s"}]}`,
			assetName(), srv.URL+"/asset", s)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	self, reexecd := banco(t, "202609092339", srv.URL+"/releases/latest")

	checkAndUpdate(false) // ciclo 1: llega corrupto
	if *reexecd != "" || declara(t, self) != "202609092339" {
		t.Fatal("un artefacto corrupto NO puede instalarse (control)")
	}

	corrupto = false      // la red se porta: el siguiente ciclo traeria los bytes buenos
	checkAndUpdate(false) // ciclo 2

	if n < 2 {
		t.Fatalf("tras una corrupcion de RED, el ciclo siguiente tenia que VOLVER A DESCARGAR; no lo hizo (descargas=%d). La version queda bloqueada para siempre por un fallo de red.", n)
	}
	if declara(t, self) != "202609100900" {
		t.Fatalf("con los bytes buenos disponibles, tenia que instalarse; declara %s", declara(t, self))
	}
}

func mustWrite(t *testing.T, b []byte) string {
	t.Helper()
	p := t.TempDir() + "/x"
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
