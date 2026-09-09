package main

// Guardas del auto-update. Los tres agujeros que produjeron el bucle del 07-09
// —281 arranques, 272 reinicios de servicio, ~940 MB en 24 h, saliendo con
// codigo 0 cada vez— estaban aqui y no en los envoltorios de cada nodo:
//
//  1. no se comprobaba que el binario descargado DECLARASE la version que el
//     release promete. Con un artefacto mal etiquetado (release vX con un
//     binario que dice vY) el proceso nuevo arranca diciendo vY, vuelve a ver
//     vX > vY y se actualiza otra vez. Para siempre.
//  2. no habia contador: nada cortaba ese bucle. El candado que habia solo
//     evitaba dos updates SIMULTANEOS, no repetidos.
//  3. no se verificaba integridad: el unico control era "pesa mas de 1 MB".
//
// La cura del (1) tiene que SOBREVIVIR AL REINICIO, porque el bucle se cierra
// justamente reiniciando: por eso el rechazo se guarda en disco por sha256 del
// artefacto. Y se levanta solo cuando el sha cambia —es decir, cuando alguien
// republica— no cuando pasa el tiempo: reintentar cada media hora un artefacto
// que esta mal es el mismo bucle, mas lento.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// maxAttemptsPerVersion es cuantas veces se intenta la MISMA version destino
// antes de dejarlo. Con el rechazo por sha ya no deberia hacer falta, pero es el
// cinturon: cubre los fallos que aun no sabemos nombrar.
const maxAttemptsPerVersion = 3

type updateState struct {
	// Refused: sha256 del artefacto -> por que se rechazo. Sobrevive al reinicio.
	Refused map[string]string `json:"refused"`
	// Attempts: version destino -> intentos gastados.
	Attempts map[string]int `json:"attempts"`
	// Digests: version destino -> digest del artefacto con el que se gastaron
	// esos intentos. Si el release se REPUBLICA con otro artefacto, el contador
	// vuelve a cero: si no, "republicar para arreglarlo" no funcionaria y el
	// nodo se quedaria clavado hasta la siguiente version. El cinturon tiene que
	// cortar los bucles, no dejar tirado a quien ya lo ha arreglado.
	Digests map[string]string `json:"digests"`
	// RefusedTags: version destino -> digest PUBLICADO del artefacto que se
	// rechazo. Es lo unico que se conoce ANTES de descargar, y por eso es lo que
	// evita volver a bajarse el mismo fichero malo cada media hora: el rechazo
	// por sha llega tarde —ya has gastado la descarga—. Eso eran los ~940 MB en
	// 24 h del 07-09. Republicar cambia el digest y lo desbloquea solo.
	RefusedTags map[string]string `json:"refused_tags"`
}

// stateDir permite a las pruebas apuntar el estado a un directorio temporal.
// Vacio = junto al binario, que es donde tiene que estar en produccion para
// sobrevivir al reinicio.
var stateDir string

func updateStatePath() string {
	if stateDir != "" {
		return filepath.Join(stateDir, "soflink-update-state.json")
	}
	self := selfPath()
	if self == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(self), "soflink-update-state.json")
}

func loadUpdateState() updateState {
	st := updateState{
		Refused:     map[string]string{},
		Attempts:    map[string]int{},
		Digests:     map[string]string{},
		RefusedTags: map[string]string{},
	}
	p := updateStatePath()
	if p == "" {
		return st
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return st
	}
	_ = json.Unmarshal(b, &st)
	if st.Refused == nil {
		st.Refused = map[string]string{}
	}
	if st.Attempts == nil {
		st.Attempts = map[string]int{}
	}
	if st.Digests == nil {
		st.Digests = map[string]string{}
	}
	if st.RefusedTags == nil {
		st.RefusedTags = map[string]string{}
	}
	return st
}

func saveUpdateState(st updateState) {
	p := updateStatePath()
	if p == "" {
		return
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(p, b, 0o644)
}

// sha256File es el identificador con el que se rechaza un artefacto: no la
// version que dice tener (que es justo lo que puede estar mal) ni su nombre.
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

var versionLine = regexp.MustCompile(`(?m)^\s*soflink\s+(\S+)\s*$`)

// declaredVersion PREGUNTA al binario descargado que version es. Es la
// comprobacion que faltaba: el tag del release es una etiqueta que pone una
// persona, y lo que va a correr es el fichero.
func declaredVersion(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").CombinedOutput()
	if err != nil && len(out) == 0 {
		return "", fmt.Errorf("no responde a 'version': %w", err)
	}
	if m := versionLine.FindStringSubmatch(string(out)); m != nil {
		return m[1], nil
	}
	// tolerante: alguna build puede imprimir solo el numero
	s := strings.TrimSpace(string(out))
	if s != "" && !strings.ContainsAny(s, " \n") {
		return s, nil
	}
	return "", fmt.Errorf("no entiendo su respuesta a 'version': %q", truncate(s, 120))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// admitArtifact decide si el artefacto recien descargado puede sustituir al que
// corre. Devuelve nil solo si se ha comprobado QUE ES lo que dice ser.
//
// Orden deliberado: primero el rechazo persistido (no gastar nada en algo ya
// descartado), luego la integridad si el release publica digest, y por ultimo
// preguntarle la version, que es lo unico que distingue un artefacto rancio.
func admitArtifact(newPath, wantVersion, assetDigest string) error {
	sum, err := sha256File(newPath)
	if err != nil {
		return fmt.Errorf("no puedo calcular el sha del artefacto: %w", err)
	}
	st := loadUpdateState()
	if why, bad := st.Refused[sum]; bad {
		return fmt.Errorf("artefacto ya rechazado antes (%s): no reintento hasta que se republique con otro contenido", why)
	}
	if assetDigest != "" {
		want := strings.TrimPrefix(assetDigest, "sha256:")
		if !strings.EqualFold(want, sum) {
			st.Refused[sum] = fmt.Sprintf("sha no coincide con el digest publicado (%s)", truncate(want, 16))
			saveUpdateState(st)
			noteRefusedTag(wantVersion, assetDigest)
			return fmt.Errorf("el fichero descargado no coincide con el digest del release")
		}
	}
	got, err := declaredVersion(newPath)
	if err != nil {
		return fmt.Errorf("no he podido preguntarle su version: %w", err)
	}
	if got != wantVersion {
		st.Refused[sum] = fmt.Sprintf("el release dice %s y el binario declara %s", wantVersion, got)
		saveUpdateState(st)
		noteRefusedTag(wantVersion, assetDigest)
		return fmt.Errorf("ARTEFACTO MAL ETIQUETADO: el release %s trae un binario que declara %s; "+
			"no lo instalo y no volvere a intentarlo hasta que se republique (hay que rehacer el release)",
			wantVersion, got)
	}
	return nil
}

// alreadyRefused se pregunta ANTES de descargar nada. Solo puede responder que
// si cuando el release publica digest —que es lo normal: GitHub lo pone en todos
// los assets—. Sin digest no hay forma de saber que es el mismo fichero sin
// bajarselo, y ahi el que acota las vueltas es el contador de intentos.
func alreadyRefused(target, digest string) (string, bool) {
	if digest == "" {
		return "", false
	}
	st := loadUpdateState()
	if st.RefusedTags[target] == digest {
		return fmt.Sprintf("el artefacto publicado para %s ya se rechazo y no ha cambiado", target), true
	}
	return "", false
}

// noteRefusedTag ancla el rechazo al digest publicado para no volver a gastar la
// descarga. Se llama junto al rechazo por sha, no en su lugar: el sha protege
// aunque el mismo contenido reaparezca bajo otra etiqueta.
func noteRefusedTag(target, digest string) {
	if digest == "" || target == "" {
		return
	}
	st := loadUpdateState()
	st.RefusedTags[target] = digest
	saveUpdateState(st)
}

// spendAttempt gasta un intento para esta version destino. Es el cinturon del
// punto 2: aunque algo se escape a las comprobaciones, el numero de vueltas es
// finito y queda escrito.
func spendAttempt(target, digest string) error {
	st := loadUpdateState()
	if digest != "" && st.Digests[target] != digest {
		// artefacto distinto para la misma version = alguien lo ha republicado:
		// evidencia nueva, presupuesto nuevo.
		st.Attempts[target] = 0
		st.Digests[target] = digest
	}
	n := st.Attempts[target] + 1
	if n > maxAttemptsPerVersion {
		return fmt.Errorf("ya he intentado actualizar a %s %d veces sin conseguirlo: no insisto", target, n-1)
	}
	st.Attempts[target] = n
	saveUpdateState(st)
	return nil
}

// blockedReason resume, en una linea para el panel, por que el auto-update no
// avanza. Un fallo que solo existe en un log es un fallo que nadie ve: David
// desactivo el auto-update precisamente porque desde fuera no se distinguia
// "al dia" de "dando vueltas".
func blockedReason() string {
	st := loadUpdateState()
	for target, n := range st.Attempts {
		if n >= maxAttemptsPerVersion {
			return fmt.Sprintf("%d intentos fallidos hacia %s; esperando a que se republique", n, target)
		}
	}
	for target, dig := range st.RefusedTags {
		_ = dig
		return fmt.Sprintf("el artefacto publicado para %s esta mal: no lo instalo hasta que se republique", target)
	}
	if n := len(st.Refused); n > 0 {
		return fmt.Sprintf("%d artefacto(s) rechazado(s) por no declarar la version que prometian", n)
	}
	return ""
}

// clearAttempts se llama cuando una version se instala de verdad: lo que quedo
// escrito de los intentos fallidos deja de importar.
func clearAttempts(target string) {
	st := loadUpdateState()
	if _, ok := st.Attempts[target]; !ok {
		return
	}
	delete(st.Attempts, target)
	delete(st.Digests, target)
	delete(st.RefusedTags, target)
	saveUpdateState(st)
}
