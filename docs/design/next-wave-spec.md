# next_wave — spec de la política de especulación por dominio (v1)

> Lane: particionador/scheduler (cerebro). Ejecuta: gateway. Estado: GO (20-08).
> Todas las cifras de este doc son wall-clock (regla de metodología: bajo
> especulación, ni `predicted_per_second` ni `predicted_ms` — solo tokens
> emitidos / reloj propio).

## 1. Qué decide y dónde corre

El motor acepta `speculative.n_max` POR PETICIÓN (override del server-side
default). `next_wave` es la política del gateway que fija ese campo en cada
request según el dominio estimado del contenido y la α observada. El gateway
la ejecuta; el diseño (esta spec) es del scheduler.

## 2. La física detrás (calibrada por reloj, 20-08)

- Base sin especulación: **24.4 tok/s**. Con ola MTP: **~63±3 código (α .96) ·
  46.6 técnico (α .59) · 38.5 prosa (α .40)**.
- Curva n_max en código: **PLANA en 3-5** (65.8/64.9/66.7 medido). Motivo: con
  α alta, la tasa marginal del candidato k+1 ≈ tasa media → el codo no es un
  pico sino donde la marginal iguala a la media. Pasarse no duele; quedarse
  corto (1-2) sí.
- En α baja (prosa), n_max alto verifica candidatos condenados: mismo tok/s,
  cómputo tirado. Bajar n_max ahí AHORRA GPU sin perder velocidad — relevante
  en cuanto hay batching (ese cómputo lo aprovechan otros slots).

## 3. Política v1 (tabla inicial + adaptación)

| dominio detectado | n_max | por qué |
|---|---|---|
| código / formato estructurado | 3 | curva plana 3-5; 3 = mínimo del plateau |
| técnico | 3 | α ~0.6, E[aceptados] aún crece a 3 |
| prosa / conversacional | 2 | α ~0.4: k>2 verifica basura |
| desconocido (arranque) | 3 | default del server; nunca peor que hoy |

- **Detección de dominio v1 (barata):** heurística sobre el request — presencia
  de bloques de código / content-type del tenant / etiqueta explícita del
  cliente. Nada de clasificar con un LLM: la política debe costar ~0.
- **Adaptación (v1.1):** α_ema por (tenant, dominio) alimentada del log del
  server (`draft acceptance` + `mean len` — los contadores fiables). Regla:
  si α_ema cae bajo 0.5 con n_max=3 → bajar a 2; si sube de 0.8 con n_max=2 →
  subir a 3. Histéresis de N=20 olas para no oscilar.
- **Guardarraíl:** n_max ∈ {2,3,4} en v1. Nada fuera de la banda medida.

## 4. Interfaz (estable desde el doc del invento)

```
next_wave(alpha_ema: float, domain: str | None) -> n_max: int
```
(La firma completa `(α_ema, stage_times, boundary_ms, t_draft) → (k, tree)` es
de F2/continuo; v1 solo necesita α_ema + dominio → n_max.)

## 5. Anexo F2 — especulación continua (draft externo en node-c)

Cuando exista el prototipo del draft-server (lane transporte):
- k* se disuelve en **c*** (chunk de verificación) y **L*** (ventaja del
  draft). Objetivo: minimizar trabajo desperdiciado con el pipeline nunca
  ocioso. El rechazo dispara cancelación temprana (transporte).
- **L* CORREGIDO POR MEDICIÓN (sweep del PoC, 20-08, 48 celdas):** en nuestro
  régimen el draft (3.9 ms/token) domina al consumo → burbujas = 0 en todo el
  grid → correr por delante no evita nada: es cómputo condenado puro
  (α 0.9, L 16 → 57% condenado). **Política: L* = c + 1 chunk de colchón
  anti-jitter**, NO 1/(1−α) (esa fórmula asumía draft-cuello; refutada aquí).
  La adaptación por α se muda al CHUNK (c) y a la compuerta "¿especular?".
  Frontera del régimen: si el prototipo real diera verify-por-token ≲ t_draft
  (posible en el extremo alto de la horquilla: 155 tok/s → 6.5 ms/token vs
  3.9 de draft = margen solo 1.7×), reaparecen burbujas y 1/(1−α) vuelve —
  el prototipo dirime.

### 5.1 Política continua v1 — TABLA EJECUTABLE (para el gateway, GO F2 20-08)

| banda α_ema | dominio típico | c* (chunk) | L* (=c+1) | por qué |
|---|---|---|---|---|
| ≥ 0.85 | código/estructurado | 4 | 5 | racha larga: chunk hondo amortiza la travesía; el sweep da 9% condenado a L bajo |
| 0.55 – 0.85 | técnico | 3 | 4 | zona media: c=3 mantiene E[aceptados] sin inflar el condenado |
| 0.25 – 0.55 | prosa/conversación | 2 | 3 | α baja: chunks hondos verifican basura (52-66% condenado en el sweep) |
| < 0.25 | patológico | — | — | **COMPUERTA: no especular** (passthrough al pipeline); caso raro, seguro |

- **Granularidad de adaptación: por CHUNK, no por token** (la decisión solo
  existe en fronteras de chunk — cada ~c tokens, escala de token en la
  práctica). α_ema se actualiza con CADA resultado de verificación (contador
  de aceptación del engine, nunca campos derivados).
- **Histéresis:** banda muerta de ±0.05 sobre los umbrales + mínimo 20
  resultados antes de cambiar de fila. Arranque sin historia: fila técnico
  (c=3) — nunca peor que el discreto actual.
- **La compuerta también es de CAPACIDAD:** si el planificador reclama el
  nodo del draft para slots de batching (multi-tenant), especular se apaga
  con prioridad al batching — la basura de draft es gratis SOLO mientras el
  nodo esté ocioso.
- Toda la tabla se RECALIBRA con el prototipo real (esta v1 sale del sweep
  sintético + las α medidas del discreto; el prototipo puede mover c* ±1).
- Datos que lo habilitan (medidos): α_ext 2B = 0.948 código (empata con MTP);
  t_draft 2B en GPU dedicada = 3.9 ms/token (~4× por delante del consumo).
- Techo del continuo-solapado: **85-155 tok/s en código (RANGO — T_pipeline
  aún confundido con t_draft_MTP; se despeja en el prototipo, sin tocar prod)**.
  En prosa el continuo aporta poco (α .40 → E[aceptados] ~1.6): es palanca de
  código/estructurado.
