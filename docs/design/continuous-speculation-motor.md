# Continuous speculation (F2) — el motor: draft-siempre-por-delante + transporte

> Section owner: transport/motor lane. Feeds the synthesis in `docs/design/low-latency-invention.md` (F2).
> Anonymous node labels only; no infra values. Todos los números son wall-clock (`completion_tokens/reloj`), NUNCA `predicted_per_second` (la especulación lo confunde).

## De dónde partimos (medido, con reloj)
La ola MTP DISCRETA (`--spec-type draft-mtp`, el cabezal nextn co-entrenado como draft) da,
sobre el pipeline distribuido, baseline honesto:
- **código ~63 tok/s (α 0.96, ×2.6) · técnico ~47 (×1.9) · prosa ~38 (×1.6)** vs ~24 sin ola.
- `n_max` es PLANO pasado ~3 (3≈4≈5): el cuello no es la profundidad de ola, es la **travesía
  de verificación** + el **draft serial** del MTP (~12 ms/token, encadenado en la MISMA GPU que
  la verificación → no solapa).

## El reframe de F2
El muro de la ola discreta es que **draft y verificación se serializan en el mismo hardware.**
Continuous speculation = **el draft corre SIEMPRE por delante, en hardware SEPARADO**, de modo
que la ola t+1 se drafta MIENTRAS el pipeline verifica la ola t → el coste del draft **desaparece
del camino** (queda escondido tras la travesía de verificación). El pipeline pasa de pagar
`T_verif + k·t_draft` por ola a pagar solo `T_verif`.

## Por qué en el nodo libre (no el MTP integrado) — con datos
- El MTP integrado vive DENTRO del modelo agrupado → su draft se serializa en la GPU de la cola.
  No se puede "sacar" a otro nodo.
- Un **draft externo pequeño en el nodo libre** SÍ solapa. Medido en el nodo libre (GPU dedicada):
  **255 tok/s = 3.9 ms/token** — **~4× más rápido que el consumo del pipeline** (~15.9 ms/token
  efectivo). → el draft se mantiene HOLGADAMENTE por delante; nunca es el cuello.
- **α NO es el riesgo** (era la duda): el draft externo pequeño empata al MTP co-entrenado en
  aceptación — **código 0.948 vs 0.98 · prosa 0.389 vs 0.394** (medido, contador de aceptación).
  La ventaja del nodo-libre es el SOLAPE (esconder los 3.9 ms/token), no una α mejor.
- Coste explícito: recuperamos el nodo libre OCUPADO como draft-server, a cambio de la capacidad
  de solapar. Compatibilidad obligatoria: header-check GGUF (vocab de la sub-familia) antes de
  stagear — regla firme del proyecto.

## Las dos perillas (del scheduler; el motor las consume)
En continuo la "ola discreta" se disuelve en dos knobs adaptativos por α_ema:
- **c\*** — tamaño de chunk de verificación (cuántos candidatos por micro-inyección). Argmin
  contra coste de OCUPACIÓN, no de travesía completa.
- **L\*** — ventana de ventaja del draft (cuánto corre por delante antes de pausar). Todo lo
  drafteado más allá del primer rechazo es basura → `L* ≈ 1/(1−α) + colchón` (código L*≈8-10,
  prosa L*≈2-3).
El objetivo cambia de "minimizar coste por ola" a **minimizar fracción de trabajo desperdiciado
con el pipeline nunca ocioso.**

## Lo que aporta el MOTOR (mi lane)
1. **Transporte de olas solapado** (`BufferedSender`, ya implementado y aparcado): streamear la
   ola t a verificar MIENTRAS el nodo libre drafta t+1. Aquí el overlap SÍ paga (en la ola
   discreta valía solo +12% porque el cuello era cómputo; aquí esconde el draft entero).
2. **Transporte de CANCELACIÓN (nuevo, la pieza crítica):** al primer rechazo, todo el draft en
   vuelo más allá del token rechazado es basura → **el mensaje de cancelación debe ADELANTAR al
   trabajo en vuelo** (fuera de banda, prioritario sobre el stream de candidatos). Sin esto, el
   nodo libre malgasta drafteando una rama condenada. Es la diferencia entre continuo eficiente y
   continuo que desperdicia.
3. **Coreografía draft↔verificación↔rollback** (con el scheduler): árbol de candidatos +
   rollback al re-muestrear en el rechazo. Lossless (salida idéntica a decode no-especulativo).

## Techo honesto (medir antes de prometer)
- Con el draft escondido, **techo ≈ travesía de verificación pura = T_pipeline/E[aceptados].**
- **Horquilla actual: 85-155 tok/s en código** — ancha porque `T_pipeline` y `t_draft_MTP` están
  aún CONFUNDIDOS en el modelo (solo observamos su suma por ola). **Se cierra GRATIS en el
  prototipo** (el draft externo corre allí por reloj → despeja T_pipeline sin tocar prod).
- **NO se promete ×4.** Realista **~×2-2.5 sobre el baseline ~63** en código; **en prosa el
  continuo aporta poco** (α 0.40 → racha ~1.6, el techo de verificación queda cerca del actual).
  Es una palanca de **código/estructurado**, no de prosa.
- **Build de semanas** (transporte de cancelación + árbol/rollback + coreografía), NO un flag.

## Orden de trabajo (medir → decidir → construir)
1. Baseline wall-clock ✔ (~63/47/38, ×2.5-2.7).
2. α_ext ✔ (empatada) · t_draft_ext ✔ (3.9 ms) → **GO al diseño node-c.**
3. Prototipo mínimo del draft-server en el nodo libre → **cierra la horquilla del techo** (despeja
   T_pipeline) ANTES de comprometer el build completo.
4. Si el techo despejado justifica las semanas → transporte de cancelación + coreografía + árbol.
   Si no (techo cerca de banda baja ~85 y el baseline ya en ~63) → el continuo es marginal y se
   deja documentado, sin construir. **El prototipo decide, no la promesa.**

## No-goals / abierto
- No prometer números de techo hasta despejar `T_pipeline` en el prototipo.
- El nodo libre como draft-server compite con su uso como pool-de-VRAM / prefill desagregado —
  el planificador de roles (solver) arbitra cuál rol toca según la carga.
