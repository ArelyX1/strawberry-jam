# Conformidad del PVM: dónde estamos

Estado: **16 de 30 vectores de `accumulate` fallan.** Todos por la misma razón,
y no es la que parecía.

Este documento existe para que nadie tenga que repetir la investigación.

## Lo que sí funciona

El PVM real corre. `guests/papucoin.pol` prepara con `entry=36083`, ejecuta y
consume `529` gas, y la cadena avanza con el guest como servicio de la economía.
Eso no es simulado.

Las seis invocaciones de `InvokeWholeProgram` del código usan el entry point que
declara el propio blob y traducen los ecalls por el import table. Asumir un
offset fijo, empezaba a ejecutar en medio de lo que el linker puso primero.

## Lo que falla

Los 16 no fallan todos por lo mismo, y conviene separarlos:

| Clase | Vectores | Qué difiere |
| --- | --- | --- |
| Coste del PVM | 14 | Solo `accumulate_gas_used` |
| Expulsión de servicio | 2 | Mucho más: `Balance`, `CodeHash`, `PreimageLookup`, `totalNumberOfItems`, `totalNumberOfOctets`, el KV global… |

O sea que hay **dos bugs distintos** y abajo solo se investiga el primero, que es
el grande. El segundo —`transfer_for_ejected_service` y `work_for_ejected_service`
— es un problema de expulsión de servicios, no de metering, y está sin tocar.

Sobre la clase grande, el caso representativo es:

| | Gas |
| --- | --- |
| Referencia | `4364` |
| Nuestro PVM | `65` |

Estado anterior a este trabajo: `0` gas, porque `ParseBlob` rechazaba el código
de los vectores. Ahora ejecuta, consume `65`, y para. Ese `65` es el problema:
son unas 32 instrucciones y luego `panic: explicit trap`.

## Lo que ya se descartó

Esto es lo importante, porque son las cosas que **no** son:

- **No es el entry point mal calculado.** Se barrieron todas las entradas
  candidatas y ninguna corre más de `90` gas. Ninguna se acerca a `4364`.
- **No es `ParseBlob`.** El framing de los vectores cuadra exactamente:
  `11 + 7360 + 20 + 4 + 28261 = 35656`, que es el tamaño exacto del código. El
  programa está bien partido.
- **No es `Deblob`.** Sobre el code de un vector devuelve `code=24792`,
  `bitmask=24792`, `jt=182`: una estructura válida y consistente.
- **No es un contenedor polkavm.** Se probó el preimage completo y con offsets
  0, 40, 64, 71 y 72: ninguno es un contenedor.
- **No son los metadatos.** Los metadatos del vector son texto legible
  (`test-service`, `6.1.27`, `Parity Technologies`), no llevan entry point. El
  `service` de la cuenta tampoco lo lleva.

## La pista que queda

Con entry `0`, el programa hace `panic: explicit trap` en la primera instrucción.

El primer byte del code deblobeado es `0x28`, que en polkavm es una instrucción
de bloque básico: `TrapIfGeU`. Los registros arrancan a cero, y `0 >= 0` es
verdad, así que la trampa salta en el instante.

Eso significa que **la instrucción 0 no puede ser el punto de entrada** de este
programa. Y como el código no trae entry point en ninguna parte que se haya
podido localizar, ahí está el problema: para el framing A.38 asumimos entry `0`,
y este programa no está pensado para entrar ahí.

Queda una hipótesis que no se ha podido comprobar: que estos vectores no usen el
framing A.38 sino algún otro punto de entrada que la referencia conoce y nosotros
no, y que al asumir `0` el programa entre por el principio y muera.

## Cómo retomarlo

1. Comparar instrucción a instrucción contra **`jam-pvm-common`** (la
   implementación de referencia en Rust que usa jamtestnet) y los vectores de
   **`w3f/jamtestvectors`**. Ahí está la regla exacta de coste por instrucción.
2. Comprobar si estos vectores son de la versión del graypaper que implementa
   este nodo. Un desajuste de versión explicaría el `4364` sin ningún bug de
   código.
3. Descartado lo de arriba, mirar si nuestro decodificador de instrucciones
   está desalineado un byte: la instrucción 0 leída como `TrapIfGeU` es
   sospechosa.

Un aviso sobre el método: **no barrer todas las entradas reejecutando el
programa.** El código reparsea el blob en cada invocación, así que un barrido de
30.000 entradas es O(n²) y tumba la máquina. Si hace falta, instrumentar el PVM
para recorrer el code array en memoria, sin reparsear.

## El otro bug: expulsión de servicios

`transfer_for_ejected_service-1.json` y `work_for_ejected_service-2.json` fallan
por una razón que no tiene nada que ver con el gas. Cuando un servicio es
expulsado por no alcanzar su umbral de equilibrio, su KV y su account se borran,
y el estado posterior no cuadra: `Balance`, `CodeHash`, `PreimageLookup`,
`CreationTimeslot`, `MostRecentAccumulationTimeslot`, `ParentService`,
`totalNumberOfItems`, `totalNumberOfOctets` y el KV global salen todos distintos.

Son 14 campos en un solo vector, lo que apunta a que la expulsión no se está
aplicando en absoluto (o se aplica al servicio equivocado), no a un error
aritmético. Es independiente del metering y conviene atacarlo por separado:
mientras el PVM gas no cuadre, estos dos vectores ya dan una prueba limpia de que el
resto de la transición está bien.

## Referencias

- `https://github.com/davxy/jam-conformance` — donde se coordinan este tipo de
  discrepancias entre implementaciones.
- `https://github.com/w3f/jamtestvectors` — los vectores.
- `jam-pvm-common` — la implementación de referencia.
- `https://playground.jamcha.in/` — para comparar traces a mano.

## Hallazgo confirmado: dos bugs de framing A.38 (corregidos)

Los vectores de expulsión fallaban por dos motivos que no eran de gas ni de
lógica de estado, sino de decodificación del programa. Los dos están corregidos
y cubiertos por `internal/pvm/program_framing_test.go`.

**1. `|c|` no es un entero compacto.** En el framing A.38 el tamaño del código es
`E4(|c|)`, un entero fijo de cuatro bytes (eq. A.38 v0.7.2), no un entero
compacto. `ParseBlob` lo leía como compacto, así que el primer byte de la
longitud se interpretaba como etiqueta: `0xb8` daba `46` en vez de `123320`, el
programa se rechazaba con `code size mismatch`, y `InvokePVM` convertía ese
fallo de framing en un accumulate vacío sin ejecutar nada. El servicio no
expulsaba a nadie porque nunca llegó a arrancar.

**2. `z` se perdía al rearmar el programa.** `GuestProgram` guardaba el tamaño de
pila del blob pero no el número de páginas de heap iniciales, y `Code()` pasaba
`0` a `ToA38`. El PVM se levantaba con `z=0`, de modo que el heap acababa en
`0x31000` cuando el programa necesita `0x33000`, y el guest moría en su primer
acceso con `page fault inaccessible memory: address=204800`. `z` forma parte de
la imagen de memoria del programa, no es un parámetro que el host pueda elegir,
así que ahora viaja en `GuestProgram.InitialHeapPages`.

Con ambos arreglos el blob de bootstrap (`d1b097b4...`) se encuadra como
`|o|=13600, |w|=40, z=2, s=8192, |c|=123320`, el programa corre ~3050 gas en vez
de morir en 65, y aparecen llamadas a `fetch` y `log` reales.

### El bug de fondo: el punto de entrada de accumulate

Los tres vectores de expulsión seguían fallando, y el síntoma engañaba: el gas
salía siempre en `3007` para los catorce vectores, mientras lo esperado iba de
`4364` a `26117`. Un número constante para catorce entradas con trabajo distinto
significa que el PVM estaba ejecutando siempre lo mismo, sin mirar el work
report. La hipótesis fácil —que faltaba la tabla de costes por instrucción— era
falsa, y comprobarlo fue lo que destapó la causa real.

Contra el gray paper v0.7.2 (`gavofyork/graypaper`, tag `v0.7.2`):

- **ϱ∆ vale `1` para las 139 instrucciones.** No hay tabla que rellenar: la
  tabla del apéndice es uniforme. Nuestro modelo de 1 gas por instrucción ya
  era correcto.
- **Cada host call cuesta `10`**, vía `gascounter' = gascounter - 10` en el
  context mutator `F`. Nuestro bloque `const` con iota repetido ya daba `10`
  para todas. También correcto.

El gas no estaba mal medido: el guest no estaba haciendo el trabajo.

**ΨR y ΨA no entran en el mismo sitio.** El refine arranca en `ι = 0` (eq. B.8)
y el accumulate en `ι = 5` (eq. B.9). Como el framing A.38 no trae tabla de
exports, el punto de entrada lo decide la fase. Nosotros pasábamos el mismo
`guest.Entry` (=`0`) a los dos, así que accumulate entraba por el preámbulo en
vez de por su trampolín de entrada, el guest recorría su inicialización y
terminaba por `explicit trap` sin llegar nunca a `Transfer` ni a `Eject`.

Ahora `pvm.AccumulateEntryPoint` fija el `5`, y solo se aplica al framing A.38:
un contenedor polkavm sigue usando el offset que declara su tabla de exports.

### Efecto

Los 30 vectores de `TestAccumulate` pasan, en `tiny` y en `full`. Con ello caen
también los traces de estado y preimages que llevaban tiempo en rojo
(`TestTraceStorage`, `TestTraceStorageLight`, `TestTracePreimages`,
`TestTracePreimagesLight`: 606 subtests, antes en fallo).

En total: tres arreglos —dos de framing (`E4(|c|)` y `z`) y uno de punto de
entrada—, cero regresiones en los 36 paquetes de pruebas unitarias y los 88 del
SDK, y la integración completa en verde tanto en `tiny` como en `full`.

### Pendiente: `TestTraceFuzzy`

Los 205 casos del fuzzer se lanzan todos en paralelo (`t.Parallel()` en cada
subtest), y eso es lo que satura la máquina: 205 subtests contendiendo a la vez,
no el coste de los vectores. Limitando el paralelismo a 2 con `GOMAXPROCS=2`, los
primeros lotes (casos `00000000`–`00000309`) pasan en unos segundos sin fallos.

No se ha ejecutado el conjunto completo porque quedan casos de coste patológico
que no terminan en un plazo razonable, y agotarlos aquí no aporta tanto como
mantener la máquina usable. Queda como verificación pendiente de una máquina
dedicada (`-parallel 2`, sin límite de tiempo).

### Arreglado: `allocatePages`

`allocatePages` (`internal/pvm/common.go`) comparaba un índice de página
absoluto contra `len(rw.data)`, que es relativo a `rw.address`. Las dos
magnitudes no son comparables: el resultado era sobreasignar `rw.address` bytes de
más, y `rw.end` se quedaba describiendo la extensión anterior, así que
`GetAccess` y `SetAccess` seguían marcando como inaccesibles las páginas que
`sbrk` acababa de entregar al huésped.

Ahora convierte a relativo explícitamente y actualiza `rw.end`, que es la
autoridad sobre dónde acaba el segmento escribible. Cubierto por
`internal/pvm/memory_allocate_test.go`, cuyo primer test falla si se restaura la
versión anterior.

### Arreglado: `Eject` expulsaba antes de tiempo

`Eject` comparaba `y < t − D` con ambos operandos en `uint32`. Para todo
`t < D` la resta se envuelve a un valor enorme, así que la guarda se cumplía y
el hijo era expulsado, con su saldo traspasado al padre, miles de timeslots antes
de lo debido. Los dos hermanos en `Forget` ya casteaban; esta rama no.

Con `D = 19_200` en producción y timeslots 0-100 en los vectores de conformidad
(`D = 32`), los slots 0-31 quedan afectados.

El test `eject` de `accumulate_functions_test.go` pasaba `t=200` esperando `OK`,
lo cual solo era alcanzable por el underflow: `10 < 200 − 19_200` es falso, así
que una guarda correcta responde `HUH`. Corregido el timeslot del vector y
añadida cobertura en `internal/pvm/host_call/eject_expunge_test.go`, que verifica
el caso negativo (`HUH`) y el positivo (`OK`) para que el primero no pueda pasar
por otra causa.

### Arreglado: longitudes de guestreserveadas sin cota

Varias host calls dimensionaban su buffer directamente con un registro del
guest: `make([]byte, regs[R9])` en `Read`, `Write`, `Log`, `Peek`, `Poke`,
`Machine` y `Provide`, y `make([]byte, regs[R8])` en el retorno por `halt`.

Esto no es un bug de corrección sino uno fatal. Comprobado en aislamiento:
`make([]byte, 1<<40)` produce `fatal error: runtime: out of memory` vía
`runtime.throw`, que **no** es un panic y por tanto no lo captura ningún
`recover`; el proceso muere. Y las host calls se ejecutan desde
`InvokeHostCall`, fuera del `recover` de `InvokeBasic`, que ya ha retornado cuando
el cuerpo de la llamada empieza a correr. Un solo `ecalli` tumbaba el nodo.

Se añadió `Memory.HasAccess`, que valida el rango sin reservar, y todos esos
sitios comprueban el rango antes de dimensionar. El resultado observable no
cambia — una rango inaccesible ya devolvía panic — pero la reserva solo ocurre
para rangos que caben de verdad en memoria.

Cubierto por `internal/pvm/host_call/guest_sized_alloc_test.go`. Verificado
revirtiendo el guard de `Read`: el proceso muere con `fatal error: runtime: out
of memory` y el test falla.

De paso, `Log` ya no se traga el error de lectura: registraba el fallo con
`log.VM.Error()` y continuaba, devolviendo éxito sobre memoria a la que el guest
no tenía acceso, mientras el resto de host calls convierten eso en panic.

## Red entre nodos

### Hecho: los nodos se conectan y se anuncian (A1)

Antes de esto un nodo escuchaba y no `: se conectaba con nadie. Dos nodos en
la misma maquina eran dos cadenas calculadas por separado, no una red.
`ConnectToNeighbours()` y `AnnounceBlock()` ya existian en el arbol y no los
llamaba nadie.

Ahora `main.go` marca con los validadores vecinos al arrancar, con reintentos
porque en local el otro nodo suele estar todavia levantandose, y el productor
anuncia cada bloque que escribe. `system_peers` y el campo `peers` de
`system_health` devuelven los pares de verdad; antes devolvian una lista vacia
y un cero fijos en el codigo, asi que un nodo con uno al lado reportaba que no
tenia ninguno. Ese `peers` es el campo que lee el panel.

Verificado con dos nodos: se ven, anuncian sin un solo fallo y se quedan en el
mismo tip. El modo de un solo nodo sigue igual, con el smoke test y la
conformidad en verde.

### Lo que faltaba y hacia que fallara

Tres cosas, y las tres hubo que encontrarlas mirando el error de verdad:

1. **El bucle de reconexion destruia el anunciador.** `ConnectToPeer` rechaza
   marcar un par que ya existe, pero compara por direccion, y el par se guarda
   con el puerto efimero de la conexion, no con el que se marco. Asi que nunca
   coincidia: el bucle reconectaba cada 30s y cada reconexion cerraba el par
   anterior con su stream de anuncio puesto. Todos los bloques siguientes
   fallaban con `context canceled`.

2. **El contexto del anuncio no se podia cancelar.** Cancelar el contexto por
   bloque dejaba muerto el stream del anunciador para siempre, porque el
   anotador queda ligado al contexto que lo creo. El anuncio va ahora en su
   propia goroutine con el contexto del nodo.

3. **La deduplicacion era por direccion en vez de por clave de validador.** Dos
   nodos que arrancan a la vez se marcan el uno al otro, se crean dos conexiones
   y la segunda reemplaza a la primera, tirando el stream. Con la clave
   Ed25519 del vecino, un vecino ya conectado se deja tranquilo.

### Hecho: rellenar los bloques que faltan (A2)

Anunciar un bloque y tenerlo son cosas distintas. Al announced una cabecera que
este nodo no puede recorrer, el recorrido que comprueba que desciende de lo
finalizado se para en el primer padre que no tiene, y la cabecera se descartaba
como si fuera invalida. Un nodo que se quedaba atras no se ponia al dia por
mucho que esperase.

Ahora el hueco tiene nombre propio: `ErrMissingAncestors` distingue "no puedo
comprobarlo porque me faltan los bloques" de "esto no es mio y hay que tirarlo",
que antes llegaban como el mismo fallo generico. La cabecera se guarda como
agarrador de lo que falta, se piden los bloques por el protocolo CE 128 en
lotes de 32, y cuando el hueco se cierra lo que esperaba se coloca solo.

Verificado con dos nodos: B arranca cuando A ya tiene historia y trae 13 bloques
de A. Una sola vez, sin bucle de peticiones.

Dos cosas salieron por el camino y estan corregidas:

- **El relleno no encontraba el hueco.** Recorria las hojas, pero una cabecera
  que no se pudo colocar nunca llega a ser hoja, asi que un nodo atrasado no
  tenia por donde empezar. Ahora las cabeceras pendientes se miran primero: son
  las unicas que saben que falta algo.
- **Pedia bloques que ya tenia.** Anadia la hoja sin mirar si el bloque estaba,
  de modo que una cadena entera se|reportaba| con un bloque todavia ausente.
  Eso era tambien el `stillMissing: 1` que aparecia en el log.

### Pendiente

Rellenar el almacen de bloques no es alcanzar ahi. Un nodo pide los bloques que
le faltan y los guarda, pero su productor sigue escribiendo su propia cadena sin
mirar la del otro, asi que todavia no elige tip. Eso es A3. Tampoco se ejecuta
la cadena ajena: reproducirla antes de decidir cual es la canonica seria
ejecutar una rama que despues se puede abandonar.

Sigue en pie lo de la autoria: los dos nodos se atribuyen los mismos bloques,
porque cada uno produce los suyos sin coordinarse y la cadena sale identica. La
verificacion por indice existe, pero no hay regla que decida quien firma cada
timeslot.

Nota sobre `blockAuthorIndex`: hoy los dos nodos se atribuyen los mismos
bloques, porque cada uno produce los suyos sin coordinarse y la cadena resulta
identica. La autorizacion por indice existe y se verifica contra el conjunto de
validadores, pero no hay ninguna regla que decida quien debe firmar cada
timeslot.
