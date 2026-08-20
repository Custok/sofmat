# web-panel — sección FRONTEND (lane: particionador/frontend)

> Para integrar en `docs/design/web-panel.md`. Stack acordado: **Vite + React +
> TypeScript**, live por **SSE**, servido por el gateway (un solo origen, todo
> `/api/*` autenticado). Sin código hasta el OK visual del wireframe.

## Principios de diseño (pieza de escaparate)

1. **Una sola pantalla hero** que cuenta el producto en un vistazo: "un modelo
   que no cabe en ninguna máquina, corriendo repartido — y lo estás viendo".
2. **El pipeline es el protagonista**, no las cards: la banda superior muestra
   el modelo fluyendo por los nodos, con el cuello de botella señalado. Es la
   imagen diferencial frente a cualquier dashboard de GPUs genérico.
3. **Tema claro/oscuro** con tokens (no dos CSS), tipografía con números
   tabulares para métricas, medidores sobrios (sin gauges circulares de
   aeropuerto), micro-transiciones solo donde hay cambio de dato real.
4. **Cero infra**: todo lo que pinta viene de `/api/status` + `/api/stream`
   (etiquetas lógicas). Ningún literal de red/host en el bundle — el leak-guard
   cubre `web/`.

## Wireframe — pantalla hero (para OK del owner)

```
┌──────────────────────────────────────────────────────────────────────────┐
│ ⬡ sofmat        modelo: <id>  ·  ctx 25k  ·  BF16       ● serving  ⚙ 🌙 │
├──────────────────────────────────────────────────────────────────────────┤
│  PIPELINE                                    14.5 tok/s · TTFT 0.6s · 5%red│
│  ┌────────────┐   ┌────────────┐   ┌────────────┐                        │
│  │  node-a    │──▶│  node-c    │──▶│  node-d    │      capas 0────────65 │
│  │ capas 0-24 │   │ capas 25-37│   │ capas 38-64│      ▓▓▓▓▓░░░▓▓▓▓▓▓▓▓  │
│  │ 31ms       │   │ 12ms       │   │ 26ms ⚠cuello│     (mini-mapa capas)  │
│  └────────────┘   └────────────┘   └────────────┘                        │
│  si cae node-c → remapea a 2 nodos (11.9 tok/s)          [ver mapas N-1] │
├──────────────────────────────────────────────────────────────────────────┤
│  NODOS                                                                    │
│  ┌──────────────────┐ ┌──────────────────┐ ┌──────────────────┐          │
│  │ node-a   master ●│ │ node-c   worker ●│ │ node-d   worker ●│          │
│  │ GPU0 ▮▮▮▮▮▮▯ 86% │ │ GPU0 ▮▮▮▮▮▯▯ 78% │ │ GPU0 ▮▮▮▮▮▮▯ 83% │          │
│  │ GPU1 ▮▮▮▮▮▮▯ 91% │ │      12.3/14.5GB │ │ GPU1 ▮▮▮▮▮▮▯ 85% │          │
│  │ CPU 12% RAM 41%  │ │ CPU 8%  RAM 22%  │ │ CPU 6%  RAM 30%  │          │
│  │ espera 4% ·3.8ms/salto│ espera 6%      │ │ espera 5%        │          │
│  └──────────────────┘ └──────────────────┘ └──────────────────┘          │
├──────────────────────────────────────────────────────────────────────────┤
│  PETICIONES EN VIVO                                    [contenido: OFF ▾] │
│  02:51:04  chat/completions  512→148 tok   14.4 tok/s   0.58s TTFT   200 │
│  02:49:31  chat/completions   28→150 tok   14.5 tok/s   0.59s TTFT   200 │
└──────────────────────────────────────────────────────────────────────────┘
```

Claves del wireframe:
- **Banda pipeline**: una caja por etapa con su rango de capas y `stage_ms_est`;
  la etapa cuello lleva el marcador. Debajo, la frase de tolerancia a fallos
  (dato del bloque `fallbacks` — los mapas N-1 hechos visibles) con detalle
  expandible. A la derecha, mini-mapa de las 65 capas coloreado por nodo.
- **Cabecera de métricas vivas**: tok/s de decode, TTFT y fracción de red (el
  KPI) — los tres números que definen el producto, siempre visibles.
- **Cards de nodo**: barras de VRAM por GPU (uso/total), util%, temperatura en
  el tooltip, CPU/RAM del `sofmat-agent`. Punto de estado (verde/ámbar/rojo)
  = healthcheck.
- **Métrica de red = LATENCIA, no bps** (decisión de diseño, dato del Grafana
  del operador): en decode PP la activación por token son ~10 KB → el bps
  siempre parece idle y engaña. Las cards muestran `espera%` (wait de la
  telemetría por etapa) y `ms/salto`; el rx/tx bps queda como secundario en
  el tooltip/detalle. El pipeline es latency-bound y la UI debe contarlo así
  ("latency-bound · X ms/hop" en la banda, no gráficas de ancho de banda).
- **Peticiones en vivo** (`/api/requests`, SSE): métricas por fila; el preview
  de contenido nace OFF (toggle explícito, truncado) — contrato de gateway-lane.

## Nombres de nodo editables (requisito del owner)

La card muestra `display_name` (default = etiqueta lógica) con **rename inline**
(icono ✎ / doble-click → input → Enter guarda, Esc cancela). El guardado va por
`PATCH /api/nodes/{id}` autenticado (gateway de gateway-lane) que persiste
`nodes[].display_name` en `config.local` — **los alias son datos del usuario y
NUNCA viajan al repo** (`config.example` mantiene node-a/b/c); el frontend los
trata como texto plano (escapado, sin HTML) y con límite de longitud. En el
screenshot público del README los nombres van genéricos.

## Playground de chat (requisito del owner)

Componente `Playground` bajo el request-log: burbujas user/asistente, input con
Enter-envía (Shift+Enter = salto), **streaming token a token** desde el
`POST /api/chat` del gateway (proxy autenticado al motor — el navegador nunca
habla con el motor directo), y **tok/s + TTFT por respuesta** al pie de cada
burbuja (el playground también es instrumento de medida). Botón de parar
generación. El contenido del playground es efímero (memoria del navegador, no
se persiste ni se loguea — coherente con la política del request-log; las
peticiones del playground aparecen en el log como las demás, con las métricas
y el contenido oculto por defecto). Render del texto del modelo como texto
plano/markdown saneado — nunca HTML crudo (XSS).

## Estados no-felices (también hay que diseñarlos)

- Worker caído: su card en rojo + la banda pipeline muestra el remapeo N-1
  aplicado (o "sin mapa de rescate" si no lo hay).
- Modelo cargando: la banda muestra el progreso de carga por etapa (los
  workers reciben tensores del main — dato del agent `stage.connected`).
- Sin auth / token inválido: pantalla de login limpia, sin filtrar nada.

## Entregables cuando el owner dé el OK visual

`web/` (Vite+React+TS): layout + tokens de tema, componentes `PipelineBand`,
`NodeCard`, `RequestsFeed`, cliente SSE con reconexión, y build estático que
sirve el gateway. Tests de componentes + fixture de `/api/status` de ejemplo
(datos ficticios). Screenshot claro y oscuro para el README.
