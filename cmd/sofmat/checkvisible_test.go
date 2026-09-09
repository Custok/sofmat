package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// El agujero que cierran estas pruebas: `checkAndUpdate` tenia SIETE `return`
// antes de la primera linea que imprimia algo, y uno de ellos era el caso
// normal. Un soflink al dia no escribia NADA nunca, y —peor— un nodo que no
// podia hablar con GitHub se veia EXACTAMENTE IGUAL que uno correcto.
//
// Lo que hay que probar no es "ahora registra algo": es que los dos casos
// producen resultados DISTINGUIBLES. Un registro que dijera lo mismo en los dos
// no arreglaria nada, y pasaria cualquier prueba que solo mire que no este vacio.

func limpiarUltimoChequeo() {
	lastCheck.mu.Lock()
	lastCheck.at, lastCheck.res = time.Time{}, ""
	lastCheck.mu.Unlock()
}

func githubFalso(t *testing.T, cuerpo func(w http.ResponseWriter)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { cuerpo(w) }))
	t.Cleanup(srv.Close)
	return srv.URL
}

func conVersion(t *testing.T, mia, api string) {
	t.Helper()
	oldV, oldAPI := version, releasesAPI
	version, releasesAPI = mia, api
	limpiarUltimoChequeo()
	t.Cleanup(func() { version, releasesAPI = oldV, oldAPI })
}

// El caso normal, que era el mudo: estoy al dia.
func TestEstarAlDiaDejaConstancia(t *testing.T) {
	api := githubFalso(t, func(w http.ResponseWriter) {
		fmt.Fprintf(w, `{"tag_name":"v202609092243","assets":[]}`)
	})
	conVersion(t, "202609092243", api)

	checkAndUpdate()

	at, res := LastCheck()
	if at.IsZero() {
		t.Fatal("no ha quedado la hora: desde fuera no se sabe si ha mirado")
	}
	if !strings.Contains(res, "al dia") {
		t.Fatalf("el resultado tiene que decir que esta al dia: %q", res)
	}
}

// ESTA es la que importa: incomunicado != al dia.
func TestUnNodoIncomunicadoNoSeParaceAUnoAlDia(t *testing.T) {
	apiOK := githubFalso(t, func(w http.ResponseWriter) {
		fmt.Fprintf(w, `{"tag_name":"v202609092243","assets":[]}`)
	})
	conVersion(t, "202609092243", apiOK)
	checkAndUpdate()
	_, resAlDia := LastCheck()

	apiRoto := githubFalso(t, func(w http.ResponseWriter) { w.WriteHeader(http.StatusForbidden) })
	conVersion(t, "202609092243", apiRoto)
	checkAndUpdate()
	atRoto, resRoto := LastCheck()

	if atRoto.IsZero() {
		t.Fatal("tambien cuando falla tiene que quedar la hora del intento")
	}
	if resRoto == resAlDia {
		t.Fatalf("los dos casos dicen lo mismo (%q): el registro no discrimina", resRoto)
	}
	if !strings.Contains(resRoto, "NO he podido mirar") {
		t.Fatalf("tiene que decir que NO pudo, no callarse: %q", resRoto)
	}
	if !strings.Contains(resRoto, "403") {
		t.Fatalf("y decir que paso, para poder arreglarlo: %q", resRoto)
	}
}

// Sin red, que es el otro `return` mudo que hacia invisible a un nodo aislado.
func TestSinRespuestaDeGitHubTambienQueda(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // cerrado a proposito: nadie contesta ahi
	conVersion(t, "202609092243", url)

	checkAndUpdate()

	at, res := LastCheck()
	if at.IsZero() || !strings.Contains(res, "NO he podido mirar") {
		t.Fatalf("un nodo sin red tiene que decirlo: at=%v res=%q", at, res)
	}
}

// CONTROL: cuando SI hay algo que hacer, el registro lo cuenta y no se queda en
// "al dia". Sin esto, un noteCheck que dijera siempre lo mismo pasaria las de
// arriba a medias.
func TestCuandoHayVersionNuevaElRegistroLoDice(t *testing.T) {
	api := githubFalso(t, func(w http.ResponseWriter) {
		fmt.Fprintf(w, `{"tag_name":"v202609100900","assets":[]}`) // sin asset para esta plataforma
	})
	conVersion(t, "202609092243", api)

	checkAndUpdate()

	_, res := LastCheck()
	if strings.Contains(res, "al dia") {
		t.Fatalf("hay una version mas nueva: no puede decir que esta al dia (%q)", res)
	}
	if !strings.Contains(res, "202609100900") {
		t.Fatalf("tiene que nombrar la version que ha visto: %q", res)
	}
}
