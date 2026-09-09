package coordinator

import (
	"testing"
	"time"
)

// El problema que resuelve la edad: `refreshLatest` falla en silencio y se queda
// con el valor anterior. Asi que "available coincide con la ultima release" y
// "este nodo lleva horas sin poder hablar con GitHub" producian EL MISMO JSON.
//
// Lo que hay que probar no es que el campo exista: es que un valor viejo y uno
// fresco se puedan DISTINGUIR. Un `age` que devolviera siempre 0 pasaria
// cualquier prueba que solo mire que el campo esta ahi.

func ponerCache(t *testing.T, ver string, cuando time.Time, err string) {
	t.Helper()
	latestMu.Lock()
	viejoV, viejoAt, viejoErr := latestVer, latestAt, latestErr
	latestVer, latestAt, latestErr = ver, cuando, err
	latestMu.Unlock()
	t.Cleanup(func() {
		latestMu.Lock()
		latestVer, latestAt, latestErr = viejoV, viejoAt, viejoErr
		latestMu.Unlock()
	})
}

// "Nunca lo he conseguido" NO es "hace 0 segundos".
func TestSinHaberloConseguidoNuncaLaEdadEsMenosUno(t *testing.T) {
	ponerCache(t, "", time.Time{}, "")
	v, age, _ := latestReleaseWithAge()
	if v != "" {
		t.Fatalf("no deberia haber valor: %q", v)
	}
	if age != -1 {
		t.Fatalf("la edad tiene que ser -1 (no lo se), no %d", age)
	}
}

// Un valor recien traido y uno de hace horas tienen que verse distintos.
func TestUnValorViejoNoSeConfundeConUnoFresco(t *testing.T) {
	ponerCache(t, "202609092243", time.Now(), "")
	_, fresco, _ := latestReleaseWithAge()

	ponerCache(t, "202609092243", time.Now().Add(-4*time.Hour), "sin respuesta de GitHub")
	v, viejo, err := latestReleaseWithAge()

	if v != "202609092243" {
		t.Fatalf("el valor se conserva aunque falle el refresco: %q", v)
	}
	if fresco > 60 {
		t.Fatalf("el recien traido no puede salir viejo: %d s", fresco)
	}
	if viejo < 3*60*60 {
		t.Fatalf("el de hace cuatro horas tiene que salir viejo: %d s", viejo)
	}
	// Y ademas dice POR QUE no se ha refrescado: un valor viejo con el motivo al
	// lado ya no se puede leer como bueno.
	if err == "" {
		t.Fatal("con un fallo de refresco, el motivo tiene que viajar al lado del valor")
	}
}

// CONTROL de que el error se limpia: si no, un fallo antiguo marcaria como
// sospechoso un valor que ya se ha vuelto a traer bien.
func TestTrasUnRefrescoBuenoElErrorSeVa(t *testing.T) {
	ponerCache(t, "202609092243", time.Now().Add(-time.Hour), "GitHub HTTP 500")
	if _, _, err := latestReleaseWithAge(); err == "" {
		t.Fatal("el error tenia que estar ahi antes de limpiarlo")
	}
	// simula lo que hace refreshLatest al terminar bien
	latestMu.Lock()
	latestVer, latestAt, latestErr = "202609092243", time.Now(), ""
	latestMu.Unlock()
	_, age, err := latestReleaseWithAge()
	if err != "" {
		t.Fatalf("tras un refresco bueno no puede quedar error: %q", err)
	}
	if age > 60 {
		t.Fatalf("y la edad tiene que reiniciarse: %d s", age)
	}
}
