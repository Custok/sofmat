package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Custok/sofmat/internal/coordinator"
)

// version is injected at build time (-ldflags "-X main.version=YYYYMMDDHHMM").
// "dev" (go run / unbuilt) never self-updates.
var version = "dev"

// releasesAPI es var, no const, para que la prueba de extremo a extremo pueda
// levantar un release falso y recorrer el camino ENTERO. Las piezas probadas por
// separado no demuestran que esten conectadas.
var releasesAPI = "https://api.github.com/repos/Custok/sofmat/releases/latest"

// reexec es sustituible en pruebas: la de verdad no vuelve nunca (o llama a
// os.Exit), que es incompatible con comprobar nada despues.
var reexec = reexecReal

// assetName is this platform's release asset — must match the names uploaded to
// the GitHub release.
func assetName() string {
	switch runtime.GOOS {
	case "windows":
		return "soflink.exe"
	case "darwin":
		if runtime.GOARCH == "arm64" {
			return "soflink-macos-arm64"
		}
		return "soflink-macos-intel"
	default: // linux
		if runtime.GOARCH == "arm64" {
			return "soflink-aarch64.AppImage"
		}
		return "soflink-x86_64.AppImage"
	}
}

// selfPath is the file to replace: the AppImage bundle when running as one, else
// the executable itself.
var selfPath = func() string {
	if ap := os.Getenv("APPIMAGE"); ap != "" {
		return ap
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if r, e := filepath.EvalSymlinks(exe); e == nil {
		return r
	}
	return exe
}

// checkAndUpdate queries GitHub for a newer release and, if found, downloads this
// platform's asset, swaps the running file and re-execs into it. Best-effort: any
// failure just logs and continues on the current version.
// updating serialises the update path. checkAndUpdate is reachable from three
// places at once — the startup check, the 30-minute ticker and the panel button
// — and each one that gets through spawns a process. On a node where the update
// never converged, overlapping calls are how one binary became many.
var updating atomic.Bool

// Registro de la ULTIMA comprobacion: cuando y con que resultado.
//
// Hasta ahora `checkAndUpdate` tenia SIETE `return` antes de la primera linea
// que imprimia algo, y uno de ellos era el caso normal ("estoy al dia"), o sea
// que un soflink correcto no escribia NADA nunca. Otros dos eran "GitHub no
// responde" y "GitHub devuelve != 200": un nodo incomunicado se veia
// exactamente igual que uno al dia.
//
// `blocked` no cubre esto: dice por que RECHACE algo, y aqui no habia nada que
// rechazar. El silencio significaba dos cosas y se veian igual — que es la
// misma forma que dejo correr el bucle del 07-09 durante 38 horas, sin un solo
// error, saliendo con codigo 0.
var lastCheck struct {
	mu  sync.Mutex
	at  time.Time
	res string
}

// noteCheck deja constancia SIEMPRE, tanto si hubo algo que hacer como si no.
// Va al log y a /api/version, porque un rastro que obliga a entrar en la
// maquina no sirve para comparar tres nodos.
func noteCheck(format string, a ...any) {
	res := fmt.Sprintf(format, a...)
	lastCheck.mu.Lock()
	lastCheck.at, lastCheck.res = time.Now(), res
	lastCheck.mu.Unlock()
	log.Printf("update-check: %s", res)
}

// LastCheck alimenta /api/version. at cero = todavia no ha mirado (el primer
// chequeo del ticker cae a los 30 min de arrancar).
func LastCheck() (time.Time, string) {
	lastCheck.mu.Lock()
	defer lastCheck.mu.Unlock()
	return lastCheck.at, lastCheck.res
}

// checkAndUpdate mira si hay version nueva y, si la hay, la instala. `startup`
// acota el presupuesto: al arrancar corre SINCRONO antes de abrir el puerto, y
// con el presupuesto normal (5 min x 3 reintentos) un GitHub medio caido dejaba
// el gateway ~15 minutos sin servir. Al arrancar: un intento y 20 s; si no
// llega, el ticker lo coge a los 30 min con el presupuesto entero.
func checkAndUpdate(startup bool) {
	defer func() {
		// El octavo silencio: un panic aqui se tragaba sin escribir nada, y
		// soflink no escribe al journal, asi que no habia NINGUN sitio donde
		// pudiera verse. Ahora deja constancia como cualquier otro resultado.
		if r := recover(); r != nil {
			noteCheck("PANIC en el updater, sigo con %s: %v", version, r)
		}
	}()
	if version == "dev" {
		return // build sin sellar: no se actualiza y no hay nada que registrar
	}
	if !updating.CompareAndSwap(false, true) {
		noteCheck("otra actualizacion en curso, no miro")
		return
	}
	defer updating.Store(false)
	// Fail-closed: si el fichero de estado existe y esta roto, NO se actualiza.
	// Antes se leia como vacio y todas las guardas desaparecian en silencio.
	if loadUpdateState(); stateCorrupt != "" {
		noteCheck("NO me actualizo: estado del updater %s", stateCorrupt)
		return
	}
	client := &http.Client{Timeout: apiTimeout}
	req, _ := http.NewRequest(http.MethodGet, releasesAPI, nil)
	if coordinator.GitHubToken != "" { // authenticated = 5000 req/h, dodges the anonymous 60/h cap
		req.Header.Set("Authorization", "Bearer "+coordinator.GitHubToken)
	}
	resp, err := client.Do(req)
	if err != nil || resp == nil {
		noteCheck("NO he podido mirar: sin respuesta de GitHub (%v)", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		noteCheck("NO he podido mirar: GitHub HTTP %d", resp.StatusCode)
		return
	}
	var rel struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
			// Digest lo publica GitHub como "sha256:…" en releases recientes. Si
			// viene, se comprueba; si no, la version declarada sigue siendo la
			// guarda que de verdad corta el bucle del artefacto rancio.
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if json.NewDecoder(resp.Body).Decode(&rel) != nil {
		noteCheck("NO he podido mirar: respuesta de GitHub ilegible")
		return
	}
	latest := strings.TrimPrefix(rel.Tag, "v")
	if latest == "" {
		noteCheck("NO he podido mirar: GitHub no da tag_name")
		return
	}
	if latest <= version { // sortable YYYYMMDDHHMM stamps
		noteCheck("al dia (%s)", version)
		return
	}
	var url, digest string
	for _, a := range rel.Assets {
		if a.Name == assetName() {
			url, digest = a.URL, a.Digest
			break
		}
	}
	if url == "" {
		noteCheck("hay %s pero el release no trae %s para esta plataforma", latest, assetName())
		return
	}
	// Preguntar ANTES de gastar la descarga. Sin esto el rechazo funciona pero
	// cuesta un binario entero cada media hora, para siempre.
	if why, no := alreadyRefused(latest, digest); no {
		noteCheck("detenido: %s", why)
		return
	}
	// El cinturon: aunque todo lo demas falle, el numero de vueltas es finito y
	// queda escrito en disco. Sin esto, "reintentar" y "bucle" son lo mismo.
	if err := spendAttempt(latest, digest); err != nil {
		noteCheck("detenido: %v", err)
		return
	}
	noteCheck("hay %s (tengo %s) - actualizando", latest, version)
	if err := applyUpdate(client, url, latest, digest, startup); err != nil {
		// Un fallo de RED devuelve el intento. El presupuesto de 3 existe contra
		// artefactos malos —"fallos que aun no sabemos nombrar"—, y este si
		// sabemos nombrarlo: la linea toso y se arregla sola. Sin esto, tres
		// hipos seguidos dejan al nodo clavado en la version vieja con un
		// artefacto perfecto esperandole, y `blocked` diria "3 intentos sin
		// conseguirlo", que quien lo lea entendera como "el artefacto esta mal".
		// El fichero de estado SI distingue los dos casos (`refused` vacio,
		// `attempts` en 3); lo que se perdia era al redactarlo.
		if errors.Is(err, ErrDescarga) {
			refundAttempt(latest)
			noteCheck("no he podido descargar %s (%v) - no gasto intento, lo reintento en el proximo ciclo", latest, err)
		} else {
			noteCheck("la actualizacion a %s fallo (%v) - sigo con %s", latest, err, version)
		}
	}
}

// updateCheckEvery es cada cuánto re-comprueba GitHub una guardia en runtime.
const updateCheckEvery = 30 * time.Minute

// periodicUpdate re-lanza checkAndUpdate periódicamente mientras el proceso vive,
// para que un nodo de guardia coja releases nuevas sin esperar a un reinicio
// manual (feedback de node-b/c/d: el auto-update solo saltaba al arranque). Al
// encontrar versión nueva, checkAndUpdate hace swap + re-exec (os.Exit) aquí.
func periodicUpdate() {
	if version == "dev" {
		return
	}
	t := time.NewTicker(updateCheckEvery)
	defer t.Stop()
	for range t.C {
		if coordinator.AutoUpdateOn() { // the header checkbox can pause auto-update live
			checkAndUpdate(false)
		}
	}
}

// ErrDescarga marca los fallos que son de la RED, no del artefacto. La
// diferencia decide si se gasta un intento: el presupuesto existe contra
// artefactos malos, y castigar con el a una linea que tose deja al nodo clavado
// con un artefacto perfecto esperandole en el servidor.
var ErrDescarga = errors.New("fallo de descarga")

// downloadClient es un cliente APARTE del que consulta la API, y es la
// correccion del 10-09.
//
// El de la API tiene 8 s, que esta bien para preguntar "cual es la ultima
// version" y es absurdo para bajarse un binario: en Go `http.Client.Timeout`
// cubre la peticion ENTERA, incluida la lectura del cuerpo. Se estaba usando el
// mismo objeto para las dos cosas, asi que la descarga corria contra un
// cronometro de 8 segundos.
//
// Medido: .30 se baja sus 6,86 MB en 0,70 s y .63 sus 3,55 MB en 0,35 s, o sea
// que NADIE roza ese techo con la red sana. Pero .63 tenia 46 fallos en su log,
// todos con el mismo mensaje ("Client.Timeout exceeded while awaiting headers"),
// 19 de ellos seguidos contra la misma version. Un cronometro corto no falla por
// el tamaño: falla porque convierte cualquier lentitud momentanea en un fallo
// COMPLETO, sin termino medio.
// apiTimeout es para PREGUNTAR "cual es la ultima version": una respuesta JSON
// pequeña. 8 s esta bien aqui y era absurdo para la descarga.
const apiTimeout = 8 * time.Second

var downloadClient = &http.Client{Timeout: 5 * time.Minute}

// descargasPorIntento: reintentos dentro de la MISMA vuelta del updater. Idea de
// metahuman-dev, y es mas barata que las otras dos correcciones juntas: sus 46
// fallos historicos eran hipos, y el siguiente intento —media hora despues—
// funcionaba. Reintentar a los pocos segundos habria evitado casi todos sin
// esperar al siguiente tick ni gastar un intento.
const descargasPorIntento = 3

// startupClient es el presupuesto del chequeo de ARRANQUE, que corre antes de
// abrir el puerto. Un binario de 7 MB en 20 s son 350 KB/s: cualquier enlace
// sano lo hace en uno. Si no llega, no pasa nada: el ticker reintenta a los 30
// min con downloadClient y sus 5 minutos, ya con el puerto abierto.
var startupClient = &http.Client{Timeout: 20 * time.Second}

// bajarArtefacto deja el artefacto en newPath, ya VERIFICADO contra el digest
// publicado si lo hay. Todo lo que devuelve envuelto en ErrDescarga es
// transitorio y NO debe contar como intento.
//
// El digest se comprueba AQUI, dentro del bucle de reintentos, y no despues en
// admitArtifact. La diferencia lo es todo: unos bytes corruptos por la red son
// un fallo de descarga (se reintenta), no un artefacto malo (se rechaza para
// siempre). Con la comprobacion fuera del bucle, una sola corrupcion dejaba la
// version bloqueada de forma permanente -demostrado el 10-09-.
func bajarArtefacto(url, newPath, wantDigest string, startup bool) error {
	client, tries := downloadClient, descargasPorIntento
	if startup {
		client, tries = startupClient, 1
	}
	want := strings.ToLower(strings.TrimPrefix(wantDigest, "sha256:"))
	var last error
	for i := 1; i <= tries; i++ {
		if i > 1 {
			time.Sleep(time.Duration(i-1) * 3 * time.Second)
			noteCheck("reintento %d/%d de la descarga tras %v", i, tries, last)
		}
		last = intentarDescarga(client, url, newPath)
		if last != nil {
			continue
		}
		if want == "" {
			return nil
		}
		got, err := sha256File(newPath)
		if err != nil {
			last = err
			continue
		}
		if got == want {
			return nil
		}
		_ = os.Remove(newPath)
		last = fmt.Errorf("los bytes no coinciden con el digest publicado (%s != %s)", truncate(got, 12), truncate(want, 12))
	}
	return fmt.Errorf("%w tras %d intentos: %v", ErrDescarga, tries, last)
}

func intentarDescarga(client *http.Client, url, newPath string) error {
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.OpenFile(newPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, resp.Body)
	f.Close()
	if err != nil {
		_ = os.Remove(newPath)
		return err
	}
	if n < 1_000_000 { // sanity: a real binary is > 1 MB
		_ = os.Remove(newPath)
		return fmt.Errorf("solo %d bytes", n)
	}
	return nil
}

func applyUpdate(client *http.Client, url, wantVersion, assetDigest string, startup bool) error {
	_ = client // la API y la descarga ya NO comparten cliente: ver downloadClient
	self := selfPath()
	if self == "" {
		return fmt.Errorf("no self path")
	}
	newPath := self + ".new"
	if err := bajarArtefacto(url, newPath, assetDigest, startup); err != nil {
		return err
	}
	installedSha, _ := sha256File(newPath)
	_ = os.Chmod(newPath, 0o755) // hay que poder EJECUTARLO para preguntarle quien es
	// La comprobacion que faltaba: que el fichero SEA lo que el release dice que
	// es. El tag lo pone una persona; lo que va a correr es esto.
	if err := admitArtifact(newPath, wantVersion, assetDigest); err != nil {
		_ = os.Remove(newPath)
		return err
	}
	// Swap: a running file can be RENAMED (even on Windows), just not overwritten.
	oldPath := self + ".old"
	_ = os.Remove(oldPath)
	if err := os.Rename(self, oldPath); err != nil {
		_ = os.Remove(newPath)
		return err
	}
	if err := os.Rename(newPath, self); err != nil {
		_ = os.Rename(oldPath, self) // roll back
		return err
	}
	_ = os.Chmod(self, 0o755)
	// Instalada de verdad: purgar lo de versiones superadas y DEJAR CONSTANCIA
	// de que se instalo y con que sha. Esta es la verificacion que el lanzador
	// de arranque no tenia y por la que, sin oraculo, hacia downgrade (.30) o
	// no arrancaba (.63).
	noteInstalled(wantVersion, installedSha)
	return reexec(self)
}
