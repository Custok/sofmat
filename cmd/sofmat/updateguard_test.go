package main

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// falsoBinario escribe algo ejecutable que responde a "version" diciendo lo que
// se le pida. Es el artefacto rancio del 07-09 reproducido en pequeno.
func falsoBinario(t *testing.T, dir, nombre, declara string) string {
	t.Helper()
	p := filepath.Join(dir, nombre)
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho \"soflink "+declara+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func shaDe(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// EL BUG DEL 07-09: el release dice una version y el binario declara otra. Sin
// esta comprobacion se instalaba, arrancaba diciendo la vieja, volvia a verse
// desactualizado y se actualizaba otra vez. 281 arranques.
func TestArtefactoMalEtiquetadoNoSeInstalaYNoSeReintenta(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("el falso binario es un script sh")
	}
	dir := t.TempDir()
	stateDir = dir
	defer func() { stateDir = "" }()

	art := falsoBinario(t, dir, "soflink-rancio", "202609091430")

	err := admitArtifact(art, "202609091815", "")
	if err == nil {
		t.Fatal("un artefacto que declara otra version NO puede admitirse")
	}
	if !strings.Contains(err.Error(), "MAL ETIQUETADO") {
		t.Fatalf("el motivo tiene que decir que esta mal etiquetado: %v", err)
	}

	// y esto es lo que corta el bucle: el rechazo SOBREVIVE, asi que el segundo
	// intento ni siquiera llega a ejecutarlo
	st := loadUpdateState()
	if _, ok := st.Refused[shaDe(t, art)]; !ok {
		t.Fatal("el rechazo no se ha guardado: al reiniciar volveria a intentarlo = el bucle")
	}
	err2 := admitArtifact(art, "202609091815", "")
	if err2 == nil || !strings.Contains(err2.Error(), "ya rechazado") {
		t.Fatalf("el segundo intento debia cortarse por el rechazo guardado: %v", err2)
	}
}

// CONTROL: si el artefacto SI declara lo que promete el release, se admite. Sin
// esto, la prueba de arriba pasaria con un admitArtifact que dijera que no a todo.
func TestArtefactoBienEtiquetadoSeAdmite(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("el falso binario es un script sh")
	}
	dir := t.TempDir()
	stateDir = dir
	defer func() { stateDir = "" }()

	art := falsoBinario(t, dir, "soflink-bueno", "202609091815")
	if err := admitArtifact(art, "202609091815", ""); err != nil {
		t.Fatalf("un artefacto correcto tiene que admitirse: %v", err)
	}
}

// El digest publicado, cuando lo hay, se comprueba ANTES de ejecutar nada.
func TestDigestQueNoCuadraSeRechaza(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("el falso binario es un script sh")
	}
	dir := t.TempDir()
	stateDir = dir
	defer func() { stateDir = "" }()

	art := falsoBinario(t, dir, "soflink-otro", "202609091815")
	err := admitArtifact(art, "202609091815", "sha256:"+strings.Repeat("00", 32))
	if err == nil {
		t.Fatal("un digest que no cuadra tiene que rechazarse")
	}
	if _, ok := loadUpdateState().Refused[shaDe(t, art)]; !ok {
		t.Fatal("tambien ese rechazo debe persistir")
	}
}

// El cinturon: aunque algo se escape a las comprobaciones, las vueltas son finitas.
func TestElContadorDeIntentosCorta(t *testing.T) {
	dir := t.TempDir()
	stateDir = dir
	defer func() { stateDir = "" }()

	for i := 1; i <= maxAttemptsPerVersion; i++ {
		if err := spendAttempt("202609091815", ""); err != nil {
			t.Fatalf("el intento %d debia permitirse: %v", i, err)
		}
	}
	if err := spendAttempt("202609091815", ""); err == nil {
		t.Fatalf("el intento %d debia cortarse", maxAttemptsPerVersion+1)
	}
	// y cuando la version se instala de verdad, lo anotado deja de contar
	clearAttempts("202609091815")
	if err := spendAttempt("202609091815", ""); err != nil {
		t.Fatalf("tras instalarse, el contador debia estar limpio: %v", err)
	}
}

// CONTROL del contador: una version distinta no hereda los intentos de otra.
func TestLosIntentosSonPorVersion(t *testing.T) {
	dir := t.TempDir()
	stateDir = dir
	defer func() { stateDir = "" }()

	for i := 0; i <= maxAttemptsPerVersion; i++ {
		_ = spendAttempt("202609091815", "")
	}
	if err := spendAttempt("202609100900", ""); err != nil {
		t.Fatalf("otra version no puede heredar los intentos gastados: %v", err)
	}
}
