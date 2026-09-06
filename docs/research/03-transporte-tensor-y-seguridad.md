# Research brief 03 — Transporte de activaciones inter-host (gRPC/TCP) y su seguridad

> Dueño: node-c (transporte). Curado a mano (no LLM) — verificar citas antes de referenciar en público.
> Contexto: KPI del proyecto = "transparente a la red" (red+espera / total por token < 10-15 %).

## 1. Qué mueve el transporte y cuánto pesa
En pipeline-parallel, entre dos etapas solo cruza el **tensor de activación del token en curso**: forma `[batch, seq, hidden]`. En **decode (batch=1, seq=1)** es `[1, 1, hidden]` → hidden 4k-8k × 2 B (fp16/bf16) = **8-16 KB por frontera y por token**. En **prefill** cruza `[1, n_prompt, hidden]` (MB), pero se parte con chunked-prefill. Conclusión que fija el diseño: **el hot-path de decode son mensajes pequeños y latencia-crítico** → lo que manda NO es el ancho de banda de la 10 GbE (1.25 GB/s sobra para 16 KB) sino el **RTT + overhead de serialización por frontera**. Por eso el techo del frame (`max_activation_mb`) protege del prefill/DoS, no del decode.

## 2. Presupuesto de overhead por frontera (la cifra que consume el particionador)
- La literatura de PP sobre TCP afinado sitúa el overhead alcanzable en **~0.5-2 ms por frontera**; partitioner-lane (brief 02) fija el umbral de alarma en **>5 ms = problema de framing, no de red**. Coincido y lo adopto como criterio de aceptación del módulo.
- Palancas para quedarnos en ese rango: **`TCP_NODELAY`** (sin Nagle — si no, +40 ms de coalescing por token, mata el decode), framing de **copia mínima** (nada de `pickle`/JSON), y evitar un round-trip de ACK por activación en el hot-path (el ACK solo en el handshake; el flujo de activaciones es push).
- El transporte expone `probe_boundary_overhead_ms()` (mide RTT real por enlace al arrancar) → es exactamente el `boundary_overhead_ms` que el solver de node-d usa para vetar mapas red-dominados. **Medido, no de catálogo** (invariante 3).

## 3. gRPC vs socket TCP crudo (decisión v0)
- **gRPC**: HTTP/2 + protobuf. Streaming bidireccional cómodo, pero protobuf **copia y re-serializa** el buffer del tensor y añade overhead de framing HTTP/2; para tensores grandes se acaba metiendo el payload como `bytes` opaco (perdiendo la ventaja de protobuf) y peleando con el límite de mensaje. Dependencia pesada (grpcio) + toolchain.
- **Socket TCP crudo + framing binario propio**: control total del formato, cero dependencias, **cero copias de más** (el payload viaja tal cual y se reconstruye con `torch.frombuffer(...).reshape(...)`), testable en CI hoy sin GPU.
- **Decisión v0: socket TCP crudo tras la interfaz `Transport`.** gRPC queda como backend alternativo detrás de la MISMA interfaz si algún día conviene su ecosistema; **RDMA/RoCE** igual (nuestras NICs son TCP puro → RDMA es futuro, no v0). La interfaz (`send_activation`/`recv_activation`) es lo que blinda al coordinador de este cambio (invariante 1).

## 4. Formato de wire (implementado en `transport/framing.py`)
Prefijo de longitud (uint32, acotado por `max_activation_mb`) + frame = cabecera binaria fija (`struct`) + payload contiguo. Campos: magic `SOFM`, versión, tipo, `stage_id`, `token_index`, `dtype`, `ndim`, `shape[]`, `n_bytes`. **Sin `pickle`, sin `eval`, sin ejecutar nada en decode.** Todo se valida antes de devolver el payload (ver §5).

## 5. Seguridad (OWASP — repo público, cualquier fallo queda a la vista)
- **A01 (control de acceso):** el puerto del worker ejecuta `forward` sobre lo que recibe → **jamás abierto**. Handshake **reto-respuesta con token compartido** (`transport/auth.py`): el worker manda un nonce, el master responde `HMAC-SHA256(token, nonce)`, el worker verifica en **tiempo constante** (`hmac.compare_digest`). El token **nunca viaja** por el cable y sale solo de `SOFMAT_TRANSPORT_TOKEN` (env/`config.local`), nunca hardcodeado (A02). Fail-closed: sin token, el transporte se niega a arrancar.
- **A08 (integridad / deserialización):** framing binario propio; **`pickle` prohibido en el hot-path** (pickle sobre red = RCE de libro). El leak-guard de gateway-lane ya bloquea `pickle`/`torch.load` sin `weights_only`.
- **A04 (diseño resiliente):** cada frame se valida antes de tocar la GPU — magic/versión, `ndim`≤8, dims acotadas, y **`prod(shape)·dtype_size == n_bytes`**. Un tensor malformado levanta `FrameError` y **aborta la conexión**, no tumba el worker. El prefijo de longitud está topado (`max_frame_bytes`) → un peer que anuncia 2 GB se corta antes de reservar memoria (anti-DoS).
- **A09 (logs):** el transporte loguea etiquetas lógicas (`node-c`) y nunca IPs/hosts reales (enlaza con leak-guard).

## 6. Pendiente / siguiente (v1)
- Backend gRPC alternativo tras la interfaz (si se decide) + **backend RDMA** cuando haya NIC RoCE.
- **Backpressure / ventana** para prefill grande (chunked) sin ahogar al worker lento (node-b, memoria unificada).
- Integración con el **healthcheck por-etapa** del coordinador: latido y `probe_boundary_overhead_ms` periódico → detección de caída (recordar la energía inestable de node-c) para disparar los mapas N-1 del particionador.
- `pip-audit` cuando el runtime traiga `torch` (el transporte v0 no tiene dependencias).

## 7. Hallazgos de la literatura 2025-26 relevantes al transporte (búsqueda web, citas VERIFICADAS)
Contrastado con los papers que señaló el panel — todos confirmados por búsqueda real, aptos para repo público:

- **Prima.cpp** (arXiv 2504.08791, ICLR 2026) — el listón real (llama.cpp distribuido en cluster doméstico heterogéneo). Su **Pipelined-Ring Parallelism** solapa IO/cómputo/**comunicación** → implicación DIRECTA para mi módulo: el transporte no debe ser `send`→bloquea→`recv` estrictamente serial; el `send` de la activación de la etapa N debe **solaparse** con el cómputo de la etapa N+1. v1 del transporte = envío no-bloqueante / doble-buffer para que la red no aparezca en el camino crítico (es justo lo que baja la fracción red del KPI <15%).
- **ShuntServe** (arXiv 2606.18600) + **SpotServe** (arXiv 2311.15566) — serving sobre **spot/preemptible**. Es EXACTAMENTE el modelo de node-c (worker elástico con energía inestable = puede desaparecer a mitad de forward). SpotServe aporta **reparalelización dinámica + migración de KV-cache** ante expulsión → es el patrón formal de nuestros **mapas N-1 + tolerancia a fallos**; el transporte debe exponer al coordinador la **caída limpia de un peer** (ya lo hace: `TransportError "peer closed mid-frame"` aborta la conexión de forma detectable, no cuelga). ShuntServe además **calibra la red (FLOPS + mem-bw + network-bw)** con un one-shot ligero (MAPE 6.63%) → valida `probe_boundary_overhead_ms()` como enfoque medido.
- **Hetis** (arXiv 2509.08309, SC'25) — paralelismo dinámico fino, reparte Attention a las GPUs lentas a granularidad de cabeza (2.25× throughput). Más para particionador/runtime, pero su **membresía dinámica** confirma que el transporte debe tratar conexiones que entran/salen como caso normal, no excepción.

**Cierre (particionador + coordinador):** la calibración de red de ShuntServe y el PRP de Prima.cpp encajan con nuestro diseño (probe medido + veto de mapas red-dominados); añado a mi roadmap v1 el **envío solapado (double-buffer)** como palanca concreta del KPI.

## Citas base (verificar exacta antes de publicar)
GPipe (burbuja), PipeDream / HetPipe (PP async), Alpa (TP×PP óptimo), Sarathi / chunked-prefill (vLLM), `TCP_NODELAY` en serving de baja latencia. Nombres correctos de memoria técnica; confirmar referencia exacta al citarlos en el repo público (las de §7 ya están verificadas por búsqueda).
