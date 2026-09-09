package main

// keyPerms answers ONE question about the key sidecar: is it actually
// unreadable by anyone else, right now, on disk?
//
// It exists because os.WriteFile(..., 0o600) on Windows applies NO ACL and
// returns NO error. An operator there reads "guardada en .soflink-apikey" and
// believes the file is protected; it is world-readable. That is worse than an
// unprotected file — it is an unprotected file with a message saying otherwise.
//
// So this checks the RESULT, never the return code of the call that was
// supposed to produce it: it stats the file back and reads the mode it really
// has. On Windows, where the mode carries no such meaning, it says so plainly
// instead of inventing an answer.

import (
	"fmt"
	"os"
	"runtime"
)

// keyPermWarning returns "" when the file is genuinely private, and the exact
// warning to print otherwise.
func keyPermWarning(path string) string {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Sprintf("AVISO: no se pudo comprobar los permisos de %s (%v)", path, err)
	}
	if runtime.GOOS == "windows" {
		return "AVISO: en Windows el modo 0600 NO aplica ACL y no da error: " +
			"el fichero puede ser legible por otros usuarios de la maquina. " +
			"Cerrar con: icacls \"" + path + "\" /inheritance:r /grant:r \"%USERNAME%\":F"
	}
	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		return fmt.Sprintf("AVISO: %s tiene permisos %04o (legible por otros); deberia ser 0600", path, mode)
	}
	return ""
}
