# Changelog

Historial de versiones de soflink. Cada release publica 5 binarios (Windows / Linux x86_64+arm64 AppImage / macOS arm64+intel) con auto-update desde GitHub.

## v202609091815 (2026-09-09)
La decision de capacidad deja de adivinar: usa el conteo exacto que ya se habia pagado.

El coordinador decidia si una conversacion cabe con una estimacion de bytes/3, y ese error no se puede acotar porque depende del contenido. Medido el mismo dia con dos peticiones reales separadas veinte minutos: la misma formula acerto por 14 tokens en una (contenido tipo prosa, ~3 bytes por token) y se quedo 42.327 corta en la otra (relleno sintetico, 1,91). La segunda entro y reboto contra el motor. La prosa sobreestima, el JSON y los datos repetidos se quedan cortos.

Y el numero bueno ya existia: la admision tokeniza el prompt para decidir la ruta, asi que el conteo exacto esta hecho y pagado antes de elegir motor. Solo faltaba llevarlo. Viaja en la cabecera `x-sofmat-exact-tokens` desde la admision hasta el balanceador, que lo usa en lugar de la estimacion cuando esta.

No cuesta ni una vuelta mas al tokenizador y el rechazo sigue saliendo en milisegundos. Las peticiones pequenas —las que no rozan el techo y para las que no se tokeniza— se siguen estimando: ahi la estimacion sobra.

Cuatro pruebas con sus controles: el conteo exacto caza lo que la estimacion dejaba pasar (y el control comprueba que con la estimacion NO se rechazaba, o la prueba no discriminaria); la reserva de respuesta se sigue sumando sobre el exacto; una cabecera ausente, vacia, cero, negativa o ilegible vuelve a la estimacion sin romper nada; y el exacto tambien manda cuando es MENOR que la estimacion, que es el caso de la prosa y evita rechazar de mas.

Sabotaje que COMPILA: ignorar la cabecera pone en rojo tres de las cuatro. La cuarta —la del respaldo— sigue verde a proposito, porque prueba justo el camino que el sabotaje deja intacto.

## v202609091735 (2026-09-09)
Esperar cuatro minutos por un sitio que no puede aparecer.

Una conversacion de 94.822 tokens contra un motor de 100.096 se quedaba 240 segundos sin respuesta y el cliente la reintentaba. Seis veces: veinticuatro minutos de un slot del decode sin producir un token, y de paso expulsando la cache de los demas.

El balanceador ya comprobaba que la peticion cupiera, ya contaba lo que los slots retienen, ya buscaba otro motor y ya rechazaba con un error detallado. Lo que hacia mal era **esperar antes de darlo**. La espera es deliberada y es correcta: convierte la contencion en cola en vez de en fallo, porque otro cliente termina y libera sitio. Pero si la peticion no cabe **ni en un motor vacio**, ese sitio no va a aparecer nunca, y esperar solo consigue que parezca que el sistema se ha colgado.

Ahora se comprueba antes de la cola: si la conversacion supera el presupuesto utilizable del motor mas grande, se rechaza **al instante** con cuanto se pasa y que hay que reducir. Lo que sigue siendo contencion sigue esperando igual.

La aritmetica del caso real, que es tambien la del precipicio que se veia desde fuera:
```
presupuesto utilizable = 100.096 x 0,97 = 97.093
94.252 + 2.288 retenidos = 96.540   cabe por 553   -> 5,4 s
94.822 + 2.288 retenidos = 97.110   NO por 16      -> 240 s
```
Dieciseis tokens. La misma conversacion iba a 5,4 segundos el turno anterior.

Un presupuesto que aun no se ha sondeado nunca provoca un rechazo: "no se cuanto cabe" y "no cabe" son cosas distintas.

Cuatro pruebas con sus controles: la conversacion mayor que el motor se rechaza sin esperar y con un mensaje que dice que hacer; la contencion transitoria sigue esperando; un presupuesto desconocido no rechaza; y una peticion que cabe en el motor mas grande no se rechaza porque no quepa en el pequeno. Comprobado con un sabotaje que COMPILA: sin el arreglo, la primera falla por tardar los 3 s del presupuesto de espera.

## v202609091520 (2026-09-09)
"Algun slot es grande" no dice que el grande sea el MIO.

Antes de decidir la ruta, el gateway pregunta al motor si todavia guarda el prefijo de esta conversacion, y la comprobacion devolvia "si" en cuanto CUALQUIER slot tuviera suficientes tokens. Con cuatro slots y varias conversaciones en el mismo motor, la cache de otro cliente respondia la pregunta que se hacia sobre esta: la admision conservaba su estimacion optimista y el prompt se procesaba entero desde cero.

Medido en produccion sobre una peticion real del HUD: 17.189 tokens, estimados en 4.100 nuevos, **18 de sus 21,7 segundos** procesando el prompt en el motor de generacion — que es justo el trabajo que el nodo de prefill existe para quitarle. Y los slots que enganaron a la comprobacion los habian llenado nuestras propias mediciones: la medicion alterando lo medido, con la factura pagada por un tercero.

Ahora un slot grande se atribuye a esta conversacion solo cuando TODOS los slots que el motor esta cacheando son grandes. Uno cacheado y grande => es el mio, que es la regla anterior sin cambios. Tres cacheados y solo uno grande => el mio puede ser uno de los dos pequenos, y "no se sabe" tiene que leerse como frio. Equivocarse en esta direccion cuesta un traspaso que no hacia falta; equivocarse en la otra cuesta un prefill completo en el motor al que habia que proteger de el.

La cuenta sale de los slots del propio motor y no del reparto de sesiones del balanceador: ese reparto no se mantiene cuando hay un solo motor de generacion, asi que una regla construida sobre el habria sido silenciosamente inutil en la configuracion mas comun.

## v202609091430 (2026-09-09)
El panel se puede operar desde fuera del nodo, pegando la clave una vez.

Decision de David sobre la consecuencia del release anterior: `/api/status` entrega la clave completa solo a quien llama desde el propio equipo, asi que un panel abierto desde otra maquina podia mirar y no actuar. Ahora se le puede dar la clave, y el navegador la recuerda.

`/api/status` dice ademas **si** lo que sirve va enmascarado, en vez de dejar que el panel lo deduzca de la forma de la cadena: un cliente que adivine "tiene puntos suspensivos, luego es una mascara" se rompe el dia que una clave legitima los lleve.

Nuevo `GET /api/authcheck`, detras de la misma guarda que las acciones y **sin cambiar nada**: responde si la clave del que llama es la buena. Existe porque validar una clave pegada exigia hasta ahora disparar una accion de verdad y ver si fallaba — y todas las rutas protegidas expulsan, cargan o borran. Es exactamente la prueba cuyo caso de fallo es el dano, que este mismo dia costo a dos nodos su clave activa. Esta responde la pregunta y no hace nada mas.

La clave pegada vive solo en el navegador que la escribio, y las lecturas y escrituras de ese almacen van protegidas: en una ventana privada o con las cookies bloqueadas el panel sigue funcionando sin memoria, en vez de romperse entero.

## v202609091350 (2026-09-09)
Correccion del release anterior: "la propia maquina" no es lo mismo que "loopback".

`/api/status` entrega la clave completa al operador que llama desde el propio nodo, y esa comprobacion se escribio como `IsLoopback()`. Una maquina tiene mas direcciones que `127.0.0.1`, y la suya propia es una de ellas: abrir el panel en `http://<ip-del-nodo>:1357/` —que es como se abre desde un acceso directo— hace que el navegador conecte a esa direccion, asi que el servidor ve la IP de red de la propia maquina como origen y no la reconoce. El panel guardaba entonces la MASCARA como credencial y todas sus acciones fallaban con 401, en la maquina duena de la clave.

Es la misma forma que la regla de firewall corregida en el release anterior —el comentario decia "LAN" y la regla decia "cualquiera"—, cometida en el mismo commit que la arreglaba: aqui el comentario decia "la propia maquina" y el codigo decia "loopback".

Ahora se acepta loopback o cualquier direccion de las interfaces de este host. La propiedad de seguridad no cambia: otro equipo de la red sigue recibiendo la mascara, y quien llama desde el propio nodo ya puede leer el fichero. Con su test, que falla si se vuelve a exigir loopback.

## v202609091330 (2026-09-09)
La API key no protegia nada: se publicaba en claro y cualquiera podia reemplazarla.

**Enumeradas las 52 rutas en vez de revisar las sospechosas**, y la proteccion habia derivado hasta ser decorativa. `/api/eject` pedia la clave y `/control/eject` —el mismo eject— no pedia nada. Lo mismo con `load`. Y sin ninguna proteccion estaban ademas: `/control/kill` (mata el proceso que ocupa un puerto), `/api/update/fleet` (reinicia los tres nodos), `/api/update`, `/api/setconfig`, `/api/rename`, `/api/selectinstance`, `/api/autoupdate`, `/api/measure`, `/api/hf/download` (baja gigabytes a disco), `/kv/` (sirve y borra estados de KV) y `/api/genkey`.

**`/api/status` servia la clave VIVA, en claro, a cualquiera que preguntase**, sin credencial: la misma que guardaba las diez rutas protegidas. Medido en produccion: un GET sin cabecera devolvio la clave byte a byte identica a la del fichero. La proteccion no estaba debilitada — no existia, porque el secreto se publicaba en la fachada.

**Y `/api/genkey` acunaba una clave nueva y la activaba, sin credencial y con CUALQUIER metodo**, GET y HEAD incluidos. Eso lo saca del escenario del atacante y lo mete en el de cualquier cosa que siga una URL: un navegador precargando un enlace, un rastreador, un monitor de disponibilidad. Dos operadores lo dispararon por accidente con veinte segundos de diferencia, los dos razonando que una ruta que acuna seria POST-only. No lo era. Acunar ademas no deja rastro: no escribe el fichero ni el log, asi que hacerlo es invisible.

**El arreglo no son parches sino que el registro de rutas declara la politica.** `mut()` exige clave y POST, y envuelve las diecinueve rutas que cambian estado; POST importa tanto como la clave, porque lo que solo sigue enlaces emite GET y HEAD. `peer()` cubre la superficie entre nodos —`/control/*`, `/kv/`, `/soflink/rename`— que no puede ir detras de la clave porque los nodos no se mandan credencial entre si: acepta a un host declarado en la configuracion, a la propia maquina, o a quien traiga la clave. Anadir una ruta obliga ahora a elegir su clase.

`/api/status` entrega la clave completa solo a quien llama desde la propia maquina —que ya tiene el fichero— y una mascara a todos los demas, sin mirar `X-Forwarded-For`, que lo pone el que llama.

**La regla de firewall que abria el puerto decia una cosa y hacia otra:** su comentario decia "accesible desde la LAN" y no llevaba restriccion de origen ninguna, que netsh interpreta como cualquier direccion. Ahora se limita a la subred local. En Linux, ufw no tiene equivalente, asi que el binario AVISA de que abre a cualquier origen en vez de dar a entender lo contrario.

**Nuevo `require_api_key`, apagado por defecto**, que extiende la clave a las rutas de inferencia. Apagado a proposito: encenderlo rechaza a todo cliente que no mande la clave, y si un editor la manda o no es un hecho de produccion, no algo que suponer. El gateway registra ahora `auth=si|no|mala` en cada peticion, asi que la respuesta se mide antes de accionar el interruptor.

Cada arreglo lleva su control —la llamada legitima que debe seguir funcionando— y la politica entera se comprueba recorriendo la lista de rutas, no las que uno recuerda.

## v202609091258 (2026-09-09)
Los fallos dejan de ser mudos, y dos carreras de datos que decidian admisiones.

**Lo que un usuario nota.** Una peticion que fallaba llegaba como exito. El gateway leia la respuesta del motor, se quedaba el cuerpo y estampaba HTTP 200 encima, asi que un error del motor —"the request exceeds the available context size", por ejemplo— aterrizaba como una respuesta correcta sin el campo `choices`, y todo cliente compatible con OpenAI informaba de lo unico que podia ver: *Response contained no choices*. El motivo venia dentro de la respuesta y un 200 le dice al cliente que no hay motivo que buscar. Ahora el estado del motor viaja intacto y el mensaje con el. La mitad en streaming del mismo gateway siempre habia reenviado el estado; las dos mitades discrepaban y solo la callada estaba mal.

Y cuando el motor acepta la peticion y cierra el flujo sin enviar un solo byte, el gateway ya no cierra en silencio: la linea de estado ya salio y no se puede retirar, asi que el motivo viaja por el unico canal abierto, un evento con `finish_reason: "error"` y su causa. Medido en produccion: 75 peticiones de 2 358 murieron asi en un nodo, todas como silencio.

**Dos carreras de datos, las dos en produccion.** El presupuesto de contexto de cada motor lo escribia la sonda de arranque mientras el gateway ya admitia trabajo, sin sincronizar, y es el numero con el que se decide si una peticion cabe: podia leerse a medias. Y el porcentaje de CPU del agente hace lectura-modificacion-escritura sobre dos variables globales desde el muestreador y desde la ruta de respaldo a la vez, lo que no solo es una carrera: dos llamadas simultaneas calculan su delta contra una base a medio actualizar y el numero que sale es plausible y falso.

**Un zombi por arranque y un descriptor por lanzamiento.** El abridor del panel se lanzaba y no se recogia nunca, asi que en Linux quedaba un proceso zombi mientras viviera el demonio; y en un nodo servidor abria ademas una pestana del navegador en el escritorio en cada arranque. `-no-browser` pasa a ser el defecto fuera de Windows y macOS. El fichero de log de arranque del motor se duplicaba al hijo y la copia del padre no se cerraba jamas: un descriptor por lanzamiento, en los dos lanzadores. Y el agente suelto nunca recogia el motor que arrancaba.

**Un motor lleno no es un motor roto.** El driver libera sitio en el decode a partir de una lectura de sus slots y la restauracion aterriza un instante despues, cuando otra peticion puede haber cogido el hueco. Esa negativa llegaba como averia de componente y abria el cortacircuitos del prefill 30 segundos, quitandole el traspaso a todas las demas peticiones. Ocho casos medidos: hasta cuatro minutos de rutado degradado que nadie podia ver, porque quien lo provoca sobrevive y las victimas son las siguientes.

**Diagnostico que sobrevive a un reinicio.** Las notas de cada peticion vivian solo en memoria, asi que la evidencia de justo los fallos que existen para diagnosticar desaparecia al siguiente arranque. Ahora una peticion que acaba mal deja una linea en el log con el motor, el estado, los bytes y la causa. Una peticion sana no deja nada.

**Nuevo `GET /api/runtime`**: goroutines, descriptores abiertos, hijos, hijos zombis, memoria y marca absoluta de arranque. Toda la caza de fugas de la vispera hubo que hacerla desde fuera del proceso, asi que una fuga solo se veia cuando ya era lo bastante grande para asomar en los numeros del sistema. Fuera de Linux los contadores que no se pueden leer devuelven -1, nunca 0: "he mirado y no hay" y "no puedo mirar" son cosas distintas.

**La clave de API**: el guardian anti-fugas no tenia ninguna regla capaz de cazar un token pelado en una linea, ni por nombre de fichero ni por contenido. Los dos agujeros cerrados y probados por separado. Y el fichero avisa cuando sus permisos no son efectivos, comprobandolos con un `stat` posterior en vez de fiarse de la llamada que debia aplicarlos.

## v202609071520 (2026-09-07)
Carga por triplicado: el traspaso ya no se gasta en balde y el motor no se ahoga.

- **Cola de verdad en el decode (antes se rendia a los 20 s y enviaba igual).** Reproducido con 3 clientes de ~46k tokens a la vez: no caben en los 100 096 de KV unificada, la guarda esperaba 20 s y luego enviaba de todas formas → llama-server rechazaba las TRES (`Response contained no choices`). Ahora espera hasta 180 s (una peticion de 46k tarda ~40 s en servirse) y, si aun asi no cabe, **rechaza esa peticion** con HTTP 503 y un motivo claro en vez de tumbar a los demas.
- **La reserva de la respuesta se acota a 4 096 tokens.** Copilot pide 16 000 de salida que casi nunca usa; reservarlos enteros dejaba entrar a un solo cliente.

- Fallo reproducido con carga real (3 clientes, prompts de 50k): el prefill procesaba 26,5 s y al entregar, el decode respondia `Unable to restore slot: No available space in KV cache` (su KV unificada ya tenia 57k de otra conversacion). El gateway degradaba a directo y el decode REPROCESABA los 50 019 tokens: 72 s de pared y el trabajo del prefill a la basura. Ademas el fallo abria el cortacircuitos, asi que la siguiente vuelta ni lo intentaba (`prefill-down`).
- Ahora `Prefill` mira lo que los slots del decode SOSTIENEN de verdad (`GET /slots`, `n_prompt_tokens`; llama.cpp mantiene la cache del prompt cuando la peticion acaba, asi que "ocupado" no es lo mismo que "en vuelo") y si el estado no cabe, NO gasta el prefill: la peticion va directa con `admission: handoff-skipped` y el motivo en el registro.
- Un "no cabe" ya NO es un fallo del prefill: nuevo `gateway.ErrSkipHandoff`, que no abre el cortacircuitos, asi que la siguiente peticion vuelve a intentar el traspaso.
- Antes del `restore`, el driver vacia el slot destino (`action=erase`): el restore necesita hueco en el presupuesto unificado y llama.cpp no desaloja por su cuenta.
- El presupuesto real de los dos motores se lee de su `/props` al arrancar.

## v202609071345 (2026-09-07)
Prefill y decode de verdad en paralelo: el prefill ingiere varios prompts a la vez y el decode deja de bloquearse.

- **Prefill concurrente (antes serializado).** El driver usaba un mutex global y un unico slot: con tres prompts frios de 40k se encolaban 25 s cada uno mientras 3 de los 4 slots del motor estaban ociosos. Ahora reserva slot + hueco en el presupuesto de KV del propio motor (`/props` al arrancar), asi que dos prompts de 40k entran a la vez en un motor de 100k; el tercero espera hasta 45 s y, si no hay sitio, esa peticion se sirve directa (fail-soft). Cada prefill usa su slot y lo libera al guardar.
- **Guarda de contexto en el decode.** El gateway lleva la cuenta de los tokens en vuelo por motor y hace esperar a la peticion que no cabe en lugar de dejar que el motor la rechace. Ataca el fallo visto en produccion con 3 VS Code: al agotarse la KV unificada, llama-server rechaza TODAS las peticiones concurrentes a la vez (el usuario ve `Response contained no choices` en los tres clientes y funciona 10 s despues).
- **Varios motores de decode (opcional).** Cualquier instancia con `role: "decode"` o clave `decode*` entra en el reparto; la conversacion queda pegada al motor que tiene su cache de prompt (una vuelta reenvia 44k tokens de mediana pero solo ~650 son nuevos: cambiar de motor convierte 1 s en 30 s de reproceso) y una conversacion nueva va al motor con menos carga. El primero de la lista es el primario: una peticion con KV traspasado se sirve ahi. Con un solo decode configurado, el comportamiento es el de siempre.
- `GET /api/requests` incluye `decode_engines` con el reparto en vivo (motor, presupuesto, peticiones y tokens en vuelo).
- Tasas por defecto del modelo de coste al dia: prefill 2 080 tok/s (recuperado tras el arreglo de la tarjeta; estaba en 1 424) y decode 2 270 tok/s, medidos hoy con un prompt de 13k.

## v202609062345 (2026-09-06)
Handoff: comprobar los soflink de los dos nodos ANTES de gastar el prefill.

- Visto en produccion: el soflink del nodo prefill no volvio tras un reinicio del host (AppImage sin unidad); el gateway hizo 33 s de prefill + save y solo entonces fallo el `kv-fetch` (conexion rechazada) → 57 s en vez de 24 s directo. Ahora `Prefill` pide `/soflink/hello` (1,5 s) al soflink del prefill y al del decode antes de tocar el motor; si alguno no responde, error inmediato (`prefill_error: prefill/decode soflink unreachable`) + cortacircuitos, sin trabajo en GPU.

## v202609062215 (2026-09-06)
Gateway robusto con el prefill caido (nodo ejectado o host apagado).

- Visto al ejectar el prefill de un nodo por hardware: el gateway ya degradaba a decode directo (200, causa en el registro), pero cada prompt largo volvia a intentar el tokenize contra el nodo. Ahora un fallo en el lado prefill (tokenize, prefill, save, fetch, restore) abre un cortacircuitos de 30 s (`PrefillBreaker`; registro `admission: prefill-down`) y las llamadas al prefill/decode de control usan un transporte con dial acotado a 2 s: un host apagado cuesta 2 s una vez cada 30 s, no un timeout TCP por peticion.

## v202609062130 (2026-09-06)
Admision consciente de la cache + modo `auto` (nuevo por defecto).

- Visto con el HUD de David (agente que encadena vueltas de tools): con `always`, cada vuelta pasaba el prompt ENTERO por el prefill (34k → 18 s + traspaso) aunque el decode tenia 22k de ese prefijo calientes y lo habria continuado en ~1 s. Ahora el gateway guarda los ids del ultimo prompt por prefijo y cuenta como NUEVO solo lo que sigue al prefijo comun con la vuelta anterior (`new_tokens` en el registro; `cache-hot` cuando no llega al suelo de 8 192).
- `kv_handoff: auto` (por defecto): decode ocupado → traspaso (protege los streams); decode libre → modelo de coste con las tasas MEDIDAS por tamano (EMA): `directo = nuevos / pp_decode` frente a `prefill = total / pp_prefill + traspaso`. Con los datos de hoy: 46k frio → prefill (27 s vs 33 s); 11,5k frio → directo; vuelta de agente 34k con 22k calientes → directo. `busy` y `always` siguen disponibles. Registro: `admission: decode-cheaper` cuando el modelo elige directo.

## v202609062100 (2026-09-06)
Admision: el registro de prefijos calientes se alinea con la cache real del decode.

- Visto en la prueba de David desde el HUD: un prompt de 16 816 tokens se admitio como `small-new-prefill` (est. 3 688 nuevos) porque el registro del gateway creia caliente el system-prompt de una peticion anterior, pero el decode ya lo habia desalojado (`cache_n 0`) y lo proceso entero (7,4 s). Fix: (1) el registro tiene tantas entradas como slots tiene el decode (4), no 512: no puede haber mas prefijos calientes que slots; (2) si el decode responde con `cache_n` por debajo de la mitad del prefijo en una peticion directa, el gateway olvida ese prefijo (`prefix_cold: true` en el registro) y la siguiente peticion se admite por su tamano completo.

## v202609061930 (2026-09-06)
Handoff: transporte del sidecar del borrador MTP (parche llama.cpp incluido en docs/patches).

- Causa del tg a la mitad tras un restore, localizada en llama.cpp a3b1eff: `slots/save|restore` a fichero solo serializan el contexto del modelo principal; el borrador MTP (contexto `ctx_dft` aparte) arranca frio en otro proceso. Parche `docs/patches/llama-a3b1eff-slot-save-dft.patch` (server-context, speculative, server-task): el save escribe ademas `<fichero>.dft` (KV del borrador + `pending_h`) y el restore lo lee si existe; respuesta con `n_written_dft` / `n_read_dft`. Compilado en CPU sin errores; build CUDA sm120 por la flota.
- soflink: `POST /control/kv-fetch` del `<estado>.dft` best-effort tras el estado principal (registro `dft`, `dft_bytes`), borrado de ambos ficheros en los dos nodos, nombres `.bin.dft` admitidos en `/kv/*`. Con motores sin parche no cambia nada.

## v202609061815 (2026-09-06)
Handoff: restaurar siempre en un slot LIBRE del decode.

- Medido con un usuario en streaming: un `restore` dirigido al slot que estaba generando se encola detras de ese stream (31,5 s en vez de 0,2 s para un estado de 32k). Ahora el driver consulta `GET /slots` del decode y restaura en el slot pedido solo si esta libre; si no, en el primer slot libre (con todos ocupados, se encola). El gateway sigue al driver: `id_slot`, cabecera y registro reflejan el slot usado.
- Interferencia medida (A en streaming en el decode, B = prompt de 32k): B directo al decode deja a A en 3,4 tok/s (de 36) con pausas de 1,1 s; B por el gateway (prefill + handoff) deja a A en 37,8 tok/s (de 41,9) con pausa maxima 0,07 s.

## v202609061805 (2026-09-06)
Politica del handoff: solo cuando protege a alguien.

- Nuevo `kv_handoff` en la config del coordinador: `busy` (por defecto) = un prompt largo se traspasa al nodo de prefill SOLO si el decode esta generando para otras peticiones (sonda `GET /slots`, `is_processing`); con el decode libre va directo. `always` = traspaso siempre que se admita.
- Motivo (medido en la e2e real, 27B Q6_K): tras un `restore` la generacion baja de ~66 a ~32-38 tok/s porque el estado de slot de llama-server no incluye el contexto del borrador MTP (dejar K tokens sin procesar para "calentarlo" no lo recupera: K=1/64/512/1024 iguales). El traspaso vale para que un prompt largo no bloquee los streams vivos (interferencia x5,8 medida), no para acelerar una peticion en solitario.
- Registro: `admission: decode-idle` cuando la politica manda directo; una sonda caida se lee como "libre" (camino rapido).

## v202609061750 (2026-09-06)
Prefill/decode desagregado REAL: KV handoff entre nodos (F1 de docs/design/kv-handoff-desagregado.md).

- Gateway `/v1/chat/completions` (JSON y streaming): un prompt largo (estimacion >= 6144 tokens y recuento EXACTO >= 8192 con la plantilla de chat aplicada) se procesa en el nodo de PREFILL y su estado KV viaja al nodo de DECODE, que solo procesa el ultimo token (`timings.prompt_n = 1`). Medido en el spike F0 (27B Q6_K, 10GbE): handoff 0,35 / 0,9 / 2,2 s a 8k / 32k / 100k tokens frente a 3,5 / 14,7 / 66 s reprocesando el prompt en el decode.
- Receta del motor: `apply-template` + `tokenize` en el prefill (misma plantilla que el decode: mismo gguf + `--jinja`), `/completion` con `tokens[:-1]` y `n_predict 0` (la cache recurrente del modelo hibrido no se puede truncar), `slots/0?action=save` + `erase` (presupuesto KV unificado; prefills serializados), el soflink del nodo decode baja el estado directo del soflink del prefill (`POST /control/kv-fetch` <- `GET /kv/<nombre>`), `slots/<slot>?action=restore` y la peticion original con `id_slot` + `cache_prompt`. Ficheros borrados en los dos nodos tras el restore.
- Todo fail-soft: cualquier fallo (recuento, prefill, fetch, restore) degrada a decode directo sin perder la peticion; la causa queda en el registro.
- Nuevo `GET /api/requests`: registro por peticion con decision de admision, via (`decode` / `prefill` / `decode-fallback`), tokens, `prefill_ms`, `save_ms`, `fetch_ms`, `restore_ms`, `handoff_ms`, `prompt_n`, `cache_n`, `kv_miss`, tg tok/s.
- Config nueva por nodo: `kv_state_dir` (= `--slot-save-path` del llama-server del nodo). Activa `GET/DELETE /kv/<nombre>` y `POST /control/kv-fetch` en ese nodo, y anade `--slot-save-path` a los llama-server que lance soflink. Sin ella el gateway sigue decode-only.
- Umbral de admision: 2048 -> 6144 tokens estimados + suelo exacto 8192 (medido: por debajo de 8k el handoff no compensa).
- Tests: gateway con backends simulados (recuento exacto, pin de slot, metricas, kv_miss, streaming) y e2e del coordinador con dos llama-server falsos + tres soflink reales (prefill, decode, gateway) en loopback.

## v202609061625 (2026-09-06)
Mejoras desde la version anterior:

- Sensor /gpu con instantanea en cache: el daemon muestrea nvidia-smi + CPU/RAM en segundo plano cada 2 s y /gpu responde siempre desde la ultima lectura (campo nuevo `age_ms`). En hosts Windows cargados nvidia-smi tardaba 0,4-7 s y el panel marcaba el nodo como caido a ratos.
- Agregador de nodos del panel: timeout por nodo 1,2 s -> 3 s (cola larga de latencia entre nodos de la LAN).
- Panel: tok/s en vivo por instancia (muestreo de /slots entre ticks), etiqueta `model_name` por instancia y ajustes de control (kill por puerto solo de procesos propios).
## v202608231328 (2026-08-23)
Gestion del llama-server en el HOST de cada nodo (eject/load en toda la flota):

- Eject/load enrutados al nodo del instance, no siempre al nodo de control fijo. El eject de un modelo cargado ahora se dirige al nodo que HOSPEDA el endpoint (resuelto por la IP del propio endpoint -> plano de control :1357), y ya no depende solo del registro local del coordinador (un decode lanzado por fuera tambien se puede expulsar). Cargar un preset se enruta al plano de control del nodo MAIN del preset, para poder (re)lanzarlo desde otro nodo.
- Eject seguro por PID propio: el eject deja de usar un patron generico (taskkill /IM llama-server.exe, pkill -f llama-server) que tumbaria un llama-server de produccion ajeno en el mismo host; ahora detiene SOLO los PID que lanzo este soflink (se registran al arrancar cada proceso).
- rpc_exe en el config: nueva ruta del ggml-rpc-server del host para la fase de fleet-load de las uniones multi-nodo (accesor + auto-descubrimiento junto al binario). El lanzamiento del worker RPC queda como TODO documentado: la union no se dispara a medias.
- Panel: el boton Cargar aparece para cualquier preset cuyo nodo main exponga plano de control, no solo el local; su etiqueta indica el nodo main destino.

## v202608230234 (2026-08-23)
Arreglo de red domestica (cortes incluso por cable):

- Barrido de descubrimiento LAN: el intervalo del barrido PERIODICO sube de 20s a 300s (5 min). Se mantiene UN barrido al arranque, asi que el auto-descubrimiento de nodos nuevos (la DGX, etc.) sigue funcionando; el barrido periodico solo hace falta para nodos nuevos, que es raro (toda la flota ya esta en el config explicito). A 20s, un barrido /24 en :1357 lanzaba ~253 SYN por nodo cada 20s y saturaba la tabla de conexiones (conntrack) del router domestico Orbi -> caidas de red.
- Concurrencia del barrido limitada de 256 a 16 conexiones simultaneas: el pico de sondeos sube de forma gradual en vez de abrir las 253 de golpe, un goteo que el router absorbe.

## v202608230158 (2026-08-23)
Reduccion drastica del churn de conexiones (arreglo de red domestica):

- Telemetria con HTTP keep-alive: los sondeos periodicos del panel (/gpu de cada nodo, /props, /v1/models y el fan-out de /api/version?local=1) ahora comparten un unico http.Transport con pool de conexiones (MaxIdleConnsPerHost=8, IdleConnTimeout=90s) en vez de abrir un socket nuevo por tick.
- Bodies drenados hasta EOF antes de Close, para que la conexion keep-alive vuelva de verdad al pool y se reutilice (sin drenar, se cerraba y generaba un TIME_WAIT por sondeo).
- Panel: intervalo de refresco de estado subido de 3s a 5s.

Resultado: el numero de conexiones TCP cortas y de TIME_WAIT generadas por la telemetria baja a ~0 por sondeo, aliviando la tabla de conexiones del router.

## v202608230043 (2026-08-23 00:43)
FIX CRITICO de red:

- Cortada la recursion de panelVersion: cada /api/version consultaba la version de TODOS los nodos, y cada nodo consultado hacia lo mismo -> explosion exponencial que saturaba la LAN (tumbo la red). Ahora el fan-out usa ?local=1 y una peticion con ?local=1 NO reenvia. Bucle roto.
- Retiradas las releases v202608222358 y v202608230003 (contenian ese bug). NO las useis.
- Incluye el display NET en Mb/s y el sensor Linux robusto de las versiones previas.


## v202608230003 (2026-08-23 00:03)
Mejoras:

- **Sensor NET de Linux robusto**: coge la interfaz con MAS trafico en /proc/net/dev (antes filtraba docker/br/veth y en hosts con mucho Docker se quedaba en 0). Arregla NET=0 en node-c/node-d.
- Boton de update: se llama **'actualizar'** y SOLO aparece si algun nodo esta por debajo de la ultima version (desaparece cuando toda la flota esta al dia).


## v202608222358 (2026-08-23 00:00)
Mejoras:

- **Sensor NET de Linux robusto**: coge la interfaz con MAS trafico en /proc/net/dev (antes filtraba docker/br/veth y en hosts con mucho Docker se quedaba en 0). Arregla NET=0 en node-c/node-d.
- Boton de update: se llama **'actualizar'** y SOLO aparece si algun nodo esta por debajo de la ultima version (desaparece cuando toda la flota esta al dia).


## v202608222341 (2026-08-22 23:41)
Mejoras:

- **Token de GitHub en config** (`github_token` en config.local.json): el auto-update va AUTENTICADO (5000 req/h) en vez del anonimo (60/h por IP compartida). Asi la flota no vuelve a perder el canal de update por rate-limit.
- Boton **'actualizar todos'**: un clic dispara el update en TODOS los nodos (fan-out desde el coordinador), no solo el local.
- **FIX de release**: los AppImage de Linux (x86_64/aarch64) ahora se REGENERAN en cada release. Antes se subian los de una tanda vieja, y los nodos Linux/AppImage se quedaban atascados en la version anterior aunque 'actualizaran'.
- (de v2318) Indicadores NET rx/tx por nodo, tarjeta de estado al lanzar modelos, fix de carga en subcarpeta.


## v202608222334 (2026-08-22 23:34)
Mejoras:

- Boton 'actualizar flota': un solo clic dispara el 'actualizar ahora' en TODOS los nodos (el coordinador hace fan-out a cada soflink), no solo el local. Ademas muestra el resultado por nodo.
- (de v2318) Indicadores NET (rx/tx) por nodo, tarjeta de estado al lanzar modelos, y fix de carga de modelos en subcarpeta.

Nota operativa: la API publica de GitHub son 60 req/h por IP; si toda la flota comparte IP y se sondea en rafaga se agota y el auto-update deja de ver releases. En regimen normal (poll cada 30 min) queda muy por debajo.


## v202608222318 (2026-08-22 23:18)
Mejoras:

- Al LANZAR un modelo aparece una TARJETA con estado en vivo: Lanzando -> cargando (Ns) -> verde 'CARGADO y sirviendo' o rojo 'crasheo (sin VRAM / arch no soportada)'. Botones Cerrar y Relanzar SIEMPRE visibles (Cerrar ademas hace eject para no dejar el proceso huerfano).
- Indicadores de RED (NET down/up) por fin funcionan: el sensor de cada nodo publica rx/tx en Mbps (Windows: netstat -e; Linux: /proc/net/dev, ignorando lo/docker/veth). Antes el endpoint /gpu no publicaba red y el panel mostraba '-' o 0.00.
- Fix: lanzar un modelo que vive en su subcarpeta ya NO falla con 'modelo no esta descargado' (se preserva la subcarpeta, con anti-traversal).


## v202608222300 (2026-08-22 23:00)
Fix:

- Lanzar un modelo que vive en su subcarpeta (p.ej. Qwen3.8-27B-Q4_0/Qwen3.8-27B-Q4_0.gguf) ya NO falla con 'modelo no está descargado'. El chequeo de existencia y el flag -m ahora preservan la subcarpeta; antes filepath.Base la descartaba y buscaba la ruta plana. Afectaba a TODOS los modelos organizados en carpetas al lanzarlos en modo Individual/local. Se mantiene el anti-traversal (rechaza '..' y rutas de mas de 2 tramos).


## v202608222106 (2026-08-22)
Mejoras:

- Boton 'actualizar ahora' en el header cuando hay una version nueva: dispara el auto-update inmediato (swap + re-exec) en vez de esperar al check de 30 min.
- El header muestra el NUMERO de version (v20260822...) en vez de la fecha formateada.
- Al lanzar un modelo: si falla al cargar salen botones Cerrar y Relanzar; y el fallo se detecta RAPIDO (el daemon rastrea el proceso y sabe si murio) en vez de esperar el timeout de 180s.


## v202608222043 (2026-08-22)
Mejoras:

- Modelos por CARPETA: cada modelo descargado va a su propia subcarpeta (todas las partes de un split GGUF juntas). El panel lista UNA fila por modelo con el tamano total, y borrar elimina la carpeta entera (todas las partes). Asi un modelo de varios ficheros nunca aparece como varias filas. Sigue mostrando los .gguf sueltos antiguos por compatibilidad.


## v202608222028 (2026-08-22)
Mejoras:

- Header con VERSION: muestra la version en ejecucion (formateada como fecha/hora del build), la version DISPONIBLE en GitHub si hay una mas nueva, y un checkbox de auto-update que se puede activar/desactivar en vivo.
- Modelos partidos (split GGUF de varios ficheros, ...-00001-of-00002) aparecen como UNA sola fila (la parte cargable) con el tamano total, en vez de una fila por parte.


## v202608222012 (2026-08-22)
Mejoras (todo pensado para que el user solo ejecute el binario):

- UN SOLO BINARIO auto-contenido: el daemon soflink ahora lanza y para modelos el mismo (/control/load|eject|kill integrado, in-process en su propio host). Ya NO hace falta el node-agent aparte. Cross-platform (Windows/Linux/macOS).
- AUTO-CONFIGURACION de rutas: el exe descubre solo llama-server (junto al binario, subcarpetas comunes, PATH) y guarda/lee los modelos en una carpeta ABSOLUTA junto al binario, independiente de desde donde lo ejecutes. Cero config manual.


## v202608221852 (2026-08-22)
Mejoras desde la version anterior:

- Autoactualizador en RUNTIME: el binario re-comprueba GitHub cada 30 min mientras corre (antes solo al arranque), asi las guardias persistentes cogen releases nuevas sin reiniciar a mano. Al encontrar version nueva: swap del binario + re-exec.
- Lanzado con feedback real: tras arrancar el proceso, el panel sondea /health de la instancia y muestra 'cargando modelo... (Ns)' -> 'modelo CARGADO y sirviendo (Ns)' o 'no cargo en 180s'. Antes ponia 'lanzado' aunque el modelo fallara al cargar.


## v202608221833 (2026-08-22)
Mejoras desde la version anterior:

- Catalogo GGUF-only: el catalogo de descarga solo lista repos GGUF (filter=gguf); los repos NVFP4/AWQ/MLX ya no aparecen (su listado de cuantizaciones salia vacio).
- Descargas con velocidad + ETA: la seccion Descargas muestra en vivo MB/s y ETA ademas de % y tamano.
- Borrar modelos: nuevo boton en Modelos locales que elimina el .gguf del disco, con confirmacion y endurecido contra path-traversal (solo .gguf del models dir, requiere API key).
