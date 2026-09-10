package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Hallazgo de debian-dev (10-09, medido en .51): noteInstalled corre en el
// binario VIEJO que hace la instalacion, asi que el primer binario con la
// feature llega sin `installed` escrito y el lanzador no tiene ancla. Al
// arrancar, el binario que corre ES el instalado y lo deja escrito el mismo.

func bancoBootstrap(t *testing.T) (dir string) {
	t.Helper()
	dir = t.TempDir()
	self := filepath.Join(dir, "soflink")
	if err := os.WriteFile(self, []byte("soy-el-binario-que-corre"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldSelf, oldV := selfPath, version
	selfPath = func() string { return self }
	version = "202609101250"
	stateDir = dir
	t.Cleanup(func() { selfPath, version, stateDir, stateCorrupt = oldSelf, oldV, "", "" })
	return dir
}

func TestAlArrancarSinEstadoQuedaEscritoLoInstalado(t *testing.T) {
	bancoBootstrap(t)
	bootstrapInstalled()
	st := loadUpdateState()
	want, _ := sha256File(selfPath())
	if st.Installed.Version != "202609101250" || st.Installed.Sha != want || st.Installed.At == "" {
		t.Fatalf("tras arrancar tiene que estar escrito quien soy: %+v (sha esperado %s)", st.Installed, want[:12])
	}
}

// Idempotente: un segundo arranque con lo mismo no reescribe el fichero.
func TestElBootstrapNoReescribeSiYaEstaBien(t *testing.T) {
	dir := bancoBootstrap(t)
	bootstrapInstalled()
	p := filepath.Join(dir, "soflink-update-state.json")
	fi1, _ := os.Stat(p)
	// tocamos el mtime hacia atras para poder detectar una reescritura
	old := fi1.ModTime().Add(-60e9)
	_ = os.Chtimes(p, old, old)
	bootstrapInstalled()
	fi2, _ := os.Stat(p)
	if !fi2.ModTime().Equal(old) {
		t.Fatal("con todo igual, el bootstrap no debe reescribir el estado")
	}
}

// CONTROL: si lo escrito es de OTRO binario (version o sha distintos), se
// actualiza. Sin esto, la de arriba pasaria con un bootstrap que nunca escribe.
func TestElBootstrapCorrigeUnInstalledAjeno(t *testing.T) {
	bancoBootstrap(t)
	st := loadUpdateState()
	st.Installed.Version, st.Installed.Sha = "202609090000", "0000"
	saveUpdateState(st)
	bootstrapInstalled()
	st = loadUpdateState()
	want, _ := sha256File(selfPath())
	if st.Installed.Version != "202609101250" || st.Installed.Sha != want {
		t.Fatalf("tenia que corregirse a lo que corre: %+v", st.Installed)
	}
}

// Fail-closed tambien aqui: con el estado corrupto, el bootstrap NO reescribe
// encima (perderia lo que hubiera).
func TestElBootstrapNoPisaUnEstadoCorrupto(t *testing.T) {
	dir := bancoBootstrap(t)
	p := filepath.Join(dir, "soflink-update-state.json")
	_ = os.WriteFile(p, []byte("{{{"), 0o644)
	bootstrapInstalled()
	b, _ := os.ReadFile(p)
	if string(b) != "{{{" {
		t.Fatal("un estado ilegible no se pisa: hay que mirarlo, no borrarlo")
	}
}
