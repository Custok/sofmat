# Changelog

Historial de versiones de soflink. Cada release publica 5 binarios (Windows / Linux x86_64+arm64 AppImage / macOS arm64+intel) con auto-update desde GitHub.

## v202608221852 (2026-08-22)
Mejoras desde la version anterior:

- Autoactualizador en RUNTIME: el binario re-comprueba GitHub cada 30 min mientras corre (antes solo al arranque), asi las guardias persistentes cogen releases nuevas sin reiniciar a mano. Al encontrar version nueva: swap del binario + re-exec.
- Lanzado con feedback real: tras arrancar el proceso, el panel sondea /health de la instancia y muestra 'cargando modelo... (Ns)' -> 'modelo CARGADO y sirviendo (Ns)' o 'no cargo en 180s'. Antes ponia 'lanzado' aunque el modelo fallara al cargar.


## v202608221833 (2026-08-22)
Mejoras desde la version anterior:

- Catalogo GGUF-only: el catalogo de descarga solo lista repos GGUF (filter=gguf); los repos NVFP4/AWQ/MLX ya no aparecen (su listado de cuantizaciones salia vacio).
- Descargas con velocidad + ETA: la seccion Descargas muestra en vivo MB/s y ETA ademas de % y tamano.
- Borrar modelos: nuevo boton en Modelos locales que elimina el .gguf del disco, con confirmacion y endurecido contra path-traversal (solo .gguf del models dir, requiere API key).
