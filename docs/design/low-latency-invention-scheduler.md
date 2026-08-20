# low-latency-invention — sección SCHEDULER (el cerebro de decisión)

> Para la síntesis de `low-latency-invention.md`. Lane: particionador/scheduler.
> Citas verificadas por búsqueda web (20-08-2026).

## 1. Survey de frontera (verificado)

| paper | idea | dato clave para nosotros |
|---|---|---|
| **PipeInfer** (SC'24, arXiv 2407.11798) | Especulación asíncrona+continua sobre pipeline; KV multibuffering; cancelación temprana | Hasta **2.15×**; diseñado para **baja aceptación y bajo ancho de banda de interconexión** — nuestro caso exacto |
| **FlowSpec** (arXiv 2507.02620) | Especulación continua en PP para edge distribuido | Confirma dirección |
| **SpecPipe** (arXiv 2504.04104) | Speculative decoding sobre PP | Confirma dirección |
| **PPSD** (arXiv 2509.19368) | **Early-exit COMO auto-especulación en PP**: las capas tempranas del propio modelo son el draft; el resto verifica | **Fusiona nuestras ideas 1 y 3 en un solo mecanismo, sin modelo draft ni GPU extra** |
| **SPD zero-bubble** (arXiv 2605.30852) | Especulación en pipeline con burbuja cero | Estado del arte 2026 |

## 2. El problema, formalizado

En decode single-stream cada token paga T_travesía = Σ(etapas) + saltos·overhead.
La especulación cambia la unidad de pago: una OLA de k candidatos paga UNA
travesía. El scheduler decide k (y la forma del árbol) para minimizar el
**coste por token aceptado**:

```
coste(k) = [ T_pipeline(k) + k·t_draft ] / E[aceptados(k, α)]
E[aceptados] ≈ (1 − α^(k+1)) / (1 − α)        (α = tasa de aceptación)
```

- T_pipeline(k): travesía verificando k candidatos en batch (sublineal en k —
  el batch amortiza pesos; medible con la telemetría por etapa).
- α se estima en vivo (EMA de las olas recientes). **α depende del contenido**:
  código/formato → alto (0.85-0.95); prosa creativa → medio (0.6-0.75).
- k* = argmin coste(k), recalculado por ola. Con histéresis (la misma del
  rebalanceo) para no oscilar.

Números orientativos con nuestro perfil medido (T_travesía ≈ 46 ms, α 0.7):
k* ≈ 4-6 → **~2.3-2.8× single-stream** (23.7 → ~55-65 tok/s). Con α 0.9: >3×.

## 3. Forma del árbol (extensión 2D)

Con verificación en batch casi-gratis, conviene proponer un ÁRBOL (varias
continuaciones alternativas por posición) en vez de una cadena: sube
E[aceptados] a igual coste de travesía. La forma (ancho×profundidad) sale del
mismo argmin — árbol ancho cuando α es bajo (hedging), cadena profunda cuando
α es alto. Referencia: verificación tipo Medusa/SpecInfer.

## 4. Las dos vías de draft — decisión POR MEDICIÓN, no por gusto

| vía | mecanismo | pros | contras |
|---|---|---|---|
| **(a) PPSD-style: auto-especulación por early-exit** | Las primeras capas (stage-0, en el main) proponen; el resto del pipeline verifica | Cero GPU extra, cero modelo extra, cero RAM extra; el draft "es gratis" | α de capas tempranas típicamente menor; exige compuerta de confianza calibrada |
| **(b) draft separado (PipeInfer-style)** | Modelo pequeño de la misma familia en el nodo libre | α mayor (draft entrenado para ello); el nodo libre ya existe | VRAM/gestión de otro modelo; el draft también tarda t_draft |

El A/B es barato: medir α de ambos con el mismo corpus (código y prosa).
El scheduler es AGNÓSTICO a la vía — consume (α, t_draft) sea cual sea.

## 5. Interfaz comprometida (para el motor de transporte y el gateway)

```
next_wave(alpha_ema: float, stage_times_ms: list[float],
          boundary_ms: float, t_draft_ms: float) -> (k: int, tree: TreeShape)
```
- La consume el draft-server (genera la ola) y el coordinador (verifica).
- El gateway ejecuta además la política de ruteo por topología (contexto
  corto → single-host; largo → pipeline) con los mapas del particionador —
  misma familia de decisión, mismo dueño del diseño, ejecución en el gateway.

## 6. Riesgos honestos

- El speedup REAL depende de α y de que la verificación en batch sea de veras
  sublineal en nuestro motor (a medir en Fase 0 del invento).
- La cancelación temprana (PipeInfer) requiere el transporte propio
  des-aparcado — sin él, una ola rechazada se paga entera.
- Todo el diseño es orquestación: cero cambios a los pesos del modelo.

## 7. Corrección de contrato (20-08, hallazgo de fuente)

llama.cpp inserta los devices RPC AL PRINCIPIO de la lista (`llama.cpp:275`)
y asigna capas tempranas→devices[0], tardías→devices[último]
(`llama-model.cpp:1364`). Consecuencias:
- **La cola del modelo (+ nextn/MTP + output) vive en el MAIN por defecto** —
  la co-locación del §5 ya la da el motor gratis; no reordenar sin motivo.
- **Contrato del split**: el particionador emite el vector en orden de
  dispositivos DEL MOTOR ([RPC..., locales...]) y cada posición lleva su
  device etiquetado explícitamente. Prohibida la convención implícita
  "locales primero" (nos tuvo toda una noche con los mapas intercambiados,
  funcionando de chiripa porque eran casi simétricos).
