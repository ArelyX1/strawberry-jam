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
  de modo que una cadena entera se reportaba con un bloque todavia ausente.
  Eso era tambien el `stillMissing: 1` que aparecia en el log.

### A3: elegir tip. A medias

La eleccion de tip y la adopcion de cadena estan escritas, con pruebas, y los dos
nodos terminan en el mismo bloque. Lo que no esta es el estado: A adopta el tip de
B y su raiz de estado no coincide con la que B tiene en ese bloque. Adopta la
cadena sin reconstruir el estado que esa cadena describe.

**Lo mas importante que habia en este camino: los dos nodos eran el mismo
validador.** `appconfig.json` lleva `validatorIndex`, los dos procesos lo leian y
los dos salian como Bob, de modo que cada uno se marcaba a si mismo. Todo lo
anterior parecia funcionar: conectaban, se anunciaban y coincidian en el tip,
porque producian la misma cadena por separado. A1, dado por verificado, no
verificaba nada de la red. Ahora hay `--validator-index` y `--author-count`, y el
nodo dice quien es al arrancar, para que esto no vuelva a pasar en silencio.

Lo demas, por cuanto dolia:

- **Un hueco no es un error de protocolo.** `receiveLoop` cerraba el stream ante
  cualquier fallo al procesar un mensaje, y `processAnnouncement` propagaba el
  error de "faltan ancestros" como si fuera grave. Para un nodo que va atrasado
  ese es su estado normal, no un fallo: es justo lo que el relleno de huecos
  existe para arreglar. A consequence: A recibia un anuncio y luego silencio
  para siempre. Ahora sigue escuchando.
- **El relleno se paraba en la cabecera que ya tenia.** `gapHashes` buscaba el
  primer padre cuya cabecera conociera y ahi cortaba. Un nodo atrasado suele
  tener las cabeceras de un tramo entero y ninguno de los bloques, porque las
  cabeceras viajan en los anuncios y los bloques hay que pedirlos por separado.
  Pedia entonces exactamente los dos bloques que ya estaba esperando, una y otra
  vez. Ahora baja mientras falte el bloque, no la cabecera.
- **Anunciar a un stream muertoReported exito.** `sendLoop` salia al fallar una
  escritura y dejaba el announcer en pie, con la conexion viva.
  `SendAnnouncement` seguia encolando en un canal que nadie leia y devolvia
  error nil, asi que el nodo creia que habia avisado a todo el mundo de cada
  bloque que escribia. Ahora el bucle que muere se lleva el announcer consigo.
- **El announcer sobrevivia a la conexion.** El handler de anuncios vive todo el
  proceso y se reparte entre todas las conexiones, asi que guardaba el announcer
  de una conexion ya cerrada. La regla de "gana el stream de id mayor" solo tiene
  sentido dentro de una conexion; entre dos conexiones los ids no significan
  nada, y el par que volvia era cerrado con codigo 0. Ahora se compara la
  conexion y no su salud, porque un peer que reinicia se reconnecta antes de que
  se note la caida.
- **No habia regla de autoria.** Todos los validadores escribian en todos los
  slots, y dos bloques para un mismo slot son una bifurcacion, no una fusion.
  Ahora el turno rota entre los validadores y el que no es autor ejecuta el
  bloque del otro. Y tiene que esperar hasta que el timeslot acaba: esperar tres
  segundos era esperar menos de lo que el autor tarda en escribir, asi que el
  retraso era fijo.
- `appConfig.ValidatorIndex < 0 && > maxuint16` deberia ser `||`. Con `&&` un
  indice negativo pasaba el chequeo y salia como 65535 al castearlo a uint16.

Con `--author-count 1` un nodo solo escribe todos los slots, que es el modo de un
solo nodo y lo que usan el smoke test y las pruebas de reinicio. El valor por
defecto es la constante de la cadena.

### La conexion: dos validadores, dos conexiones, y cada una cerrando la de la otra

Con los dos nodos de verdad en marcha, ninguno se enteraba de nada: los dos
fallaban al abrir el stream de anuncio con `Application error 0x0 (remote)`, o
sea que la conexion que cada uno tenia la habia cerrado el otro. Sin anuncios no
hay cadena compartida, y sin cadena compartida no hay nada que sincronizar.

La causa es que dos nodos que arrancan juntos se llaman los dos, asi que cada uno
acaba con dos conexiones al mismo par y tiene que tirar una. Como se tirara no
importaba mientras los dos nodos fueran el mismo validador, porque entonces no
habia dos conexiones que cruzar. Con dos validadores si importa, y las dos
reglas que se probaron estaban mal:

- **Gana la mas nueva.** Las dos conexiones llegan en orden opuesto a los dos
  nodos, asi que A se queda con la segunda que vio y B con la segunda que vio, y
  esas son conexiones distintas. Cada uno cerraba la que el otro tenia.
- **Gana la primera.** Cuando ambos llaman a la vez, la primera que ve cada uno es
  la que el mismo ha llamado, asi que pasa lo mismo: A se queda con la suya y B
  con la suya, y cada uno acaba de cerrar la del otro.

Lo que si es el mismo en los dos extremos son los dos puertos de la conexion, y
no en el orden en que aparecen: una conexion que B llamo a A se ve en A como
`(30333, 51000)` y en B como `(51000, 30333)`, con local y remoto cambiados de
sitio. Compararlos como llegan deja a cada nodo comparando numeros distintos
sobre las mismas dos conexiones, que es exactamente la discrepancia que se
pretendia evitar. Ordenando el par antes de comparar, los dos eligen la misma y
cada uno conserva una conexion viva que el otro no esta cerrando. El test que
fija esto tiene que modelar el cambio de local y remoto, porque un test que usa
el mismo par en los dos extremos pasa con el bug dentro.

Con esto los dos nodos comparten cadena: 36 de 40 muestras con el mismo tip,
antes 0 de 40.


### N nodos, y una malla de verdad

El numero de nodos ya no esta escrito en ningun sitio. `scripts/devnet-mesh.sh N`
genera un fichero de validadores con N entradas, levanta los nodos de uno en uno y
comprueba despues de cada uno que el recien llegado se ha sincronizado con los que
ya estaban, y que la malla esta completa.

Lo que hacia falta para eso, y estaba atado a dos validadores:

- **El conjunto de validadores es un parametro de la cadena, y por eso no puede
  ser dinamico por proceso.** Dos nodos que discrepan de el estan en cadenas
  distintas, asi que lo fijan todos por igual leyendo el mismo fichero. Lo que si
  cambia de una ejecucion a otra es cuantos hay, y eso lo decide el fichero. El
  techo del dev estaba en 2, y el conjunto es un array de tamano fijo, asi que un
  tercer validador era un indice fuera de rango. Ahora el techo es 6 y el
  fichero dice cuantos se usan.
- **El turno de escribir rota entre los que hay.** `--author-count` lo pone el
  que lanza la malla, y con N nodos cada nodo escribe uno de cada N slots. Con 1
  significa que ese nodo escribe todos, que es el modo de un solo nodo.
- **`--full-mesh`**: el grid del protocolo solo hace vecinos a los validadores que
  comparten fila o columna, y un grid es cuadrado, asi que con tres validadores en
  una cadena que admite seis el grid es de dos por dos y el tercero solo se
  alcanza a traves del primero. Eso es la definicion del protocolo y no se cambia,
  pero significa que el grid no es una malla, y una red local de unos pocos nodos
  quiere que cada uno llegue a todos los demos directamente.

Tres bugs que solo aparecen con mas de dos nodos:

- **Un vecino inalcanzable paraba el bucle entero.** `ConnectToNeighbours` hacia
  `return` en el primer fallo, con lo que un solo vecino con una direccion que no
  salia impedia marcar a los demas y el nodo se quedaba sin pares. Y el grid
  entrega como vecinos las entradas del conjunto que no se rellenaron, que son
  claves vacias sin direccion: por eso el nodo se quedaba a cero. Ahora se sigue
  con el resto y se dice al final cuales no se alcanzaron.
- **En cuanto un nodo tenia un par, dejaba de buscar los demas.** El bucle hacia
  `continue` mientras tuviera alguno, asi que un nodo que se conectaba con el
  primero que encontraba se quedaba ahi para siempre. Con dos nodos no se notaba,
  porque ese par era el unico que hacia falta; con tres, el segundo solo conocia al
  primero y el tercero tambien, y no se veian entre si.
- **Adoptar un bloque no es un reinicio, pero se trataba como uno.**
  `FinishRebuild` olvidaba los numeros que el nodo habia repartido, y eso esta bien
  para un nodo que acaba de arrancar, cuyo estado sale de los bloques que
  reproduce. En vivo no: el nodo que adopta el bloque de otro sigue siendo el
  nodo que lleva la cadena, y el numero que le toca a la siguiente operacion se
  cuenta desde ahi. Borrarlos hacia que dos nodos con el mismo bloque escribieran
  operaciones distintas sobre la misma cadena, y por eso las raices de estado de un
  bloque salian distintas en cada nodo. Ahora `FinishRebuild` solo termina la
  repeticion y el olvido es explicito, con `ForgetHandedOut`, en el reinicio.

Comprobado con 3 nodos: los tres comparten bloque, la cadena avanza, y cada nodo
ve a los otros dos.

### La causa de fondo: cada nodo fundaba su propia cadena

El bloque de genesis se fechaba con `jamtime.Now()` al arrancar, con el razonamiento
de que un nodo solo no tiene a nadie con quien acordar la fecha. Para una red eso
significa que el nodo 0 funda su cadena en T0 y el nodo 1 en T1: mismo bloque de
genesis en la forma, distinto timeslot, distinto hash. Son dos cadenas desde el
primer bloque, y el genesis es el padre de todo, asi que ningun bloque que uno
escribiera descendia del genesis del otro. Toda cabecera que llegara por el otro
lado se rechazaba por no descender del bloque finalizado, y los dos no se ponian de
acuerdo en nada. No era un problema de red ni de finalizacion: era que no estaban
en la misma cadena.

Ahora el genesis lleva `genesisTimeslot` en su fichero, y todos los nodos de una
red lo leen. El script de malla escribe uno por ejecucion con un momento concreto
y un par de timeslots de margen, y se lo pasa a todos. Un nodo cuyo genesis no lo
diga sigue fechando el suyo al arrancar, que es lo que le vale a uno solo.

Con esto dos nodos comparten bloque **y raiz de estado**.

### Un lote por anuncio, y alcanzar la cadena al ritmo del reloj

Dos cosas mas, y con ellas una malla de tres nodos queda sincronizada: mismo bloque
y misma raiz de estado en los tres, comprobado mas de una vez.

**Un bloque por anuncio era poco.** El autor de un timeslot escribe su bloque al
empezar ese timeslot, y quien tiene que construir encima lo quiere al empezar el
siguiente. Pidiendo un solo bloque por anuncio, el seguidor iba un bloque atras
siempre y el autor acababa esperando un bloque que no llegaba. Se pide el tramo
detras del bloque anunciado, que es para lo que el rango del protocolo existe.

**Alcanzar la cadena no puede esperar al turno de uno.** Un nodo solo ejecutaba en
los timeslots que no le tocaba escribir, que en una malla de N es uno de cada N: un
nodo que entraba en una malla de cinco solo podia ejecutar un bloque cada treinta
segundos, y alcanzar una cadena diez bloques por delante le llevaba cinco minutos.
Un nodo que va atrasado no esta participando en su propio timeslot de todas formas,
asi que ejecuta lo que tiene y alcanza al ritmo del reloj. Cuando esta a la altura
no hace nada.

Y dos fallos propios de esta sesion, por si quedan:

- Alcanzar la cadena estaba puesto como un caso del mismo `switch` que el turno, de
  modo que un nodo que acababa de alcanzar se comia el resto del timeslot: si el
  timeslot que le tocaba a el llegaba mientras iba atrasado, alcanzaba, daba por
  hecho que su turno estaba hecho y no escribia el bloque. Nadie mas lo escribe
  porque hay un autor por timeslot, y ahi se paraba la cadena.
- Reintentar sin esperar, con una linea por intento, escribio cientos de megabytes
  de log en un par de minutos y lleno el disco. El reintento espera ahora y lo dice
  una vez por vez.

### El modelo: turno fijo y los N nodos a la vez

Se eligio el modelo seguro: el turno de escribir se reparte entre N validadores y
los N se levantan a la vez.

No es una preferencia de estilo. En JAM cada timeslot tiene exactamente un autor, y
si ese autor no esta en marcha ese timeslot no lo escribe nadie. Montar la red de
uno en uno choca de frente con esa regla, y se notaba: los timeslots de los
validadores que aun no habian llegado se quedaban sin escribir y una malla asi se
quedaba parada o se dividia, porque no habia manera de que los nodos se pusieran de
acuerdo en quien escribia cada hueco. Con el turno repartido y todos en marcha, cada
timeslot tiene su autor y la cadena avanza sola.

Por lo mismo el genesis se fecha **en el presente**: las timeslots anteriores a la
fundacion no las ha escrito nadie, y con el turno repartido cada validator espera a
que le toque en vez de escribir un pasado que no es suyo.

Tres cosas mas que hacia falta:

- **El padre de un bloque es el ultimo bloque escrito**, no el del timeslot
  inmediatamente anterior. Exigir el segundo hacia que la cadena no arrancara
  nunca: un timeslot se queda sin escribir cuando el validador de su turno no esta,
  y entre dos bloques escritos puede haber un hueco de varios timeslots sin que la
  cadena este mala. Esperar un bloque que nadie va a escribir es esperar para
  siempre.
- **Sin rafaga de arranque.** Al arrancar, un nodo rellenaba de golpe todas las
  timeslots que se habia perdido, sin mirar de quien eran. Un nodo que entraba en
  una malla de cinco escribio veintiocho bloques mientras sus pares escribian tres
  cada uno, y dos de ellos acabaron en cadenas distintas. El pasado ya lo ha escrito
  quien le tocaba.
- **No escribir un timeslot que ya tiene bloque.** Dos nodos que discrepan un
  momento sobre de quien es el turno escriben los dos, y dos bloques para un
  timeslot son una bifurcacion de la que la cadena no vuelve.

Comprobado con 3 nodos: la cadena arranca sola, los tres comparten bloque y raiz de
estado, la malla esta completa, uno se cae, la cadena sigue sin el y al volver se
sincroniza.

### Un timeslot sin bloque no se puede inventar

El replay recorria **todos** los timeslots hasta la punta, y los que no tenian
bloque los ejecutaba como si no hubieran liquidado nada. Eso no es una forma lenta
de alcanzar la cadena: es un estado distinto.

Un nodo que va atrasado ejecuta de una vez todo lo que le ha llegado, con lo que un
timeslot sin bloque lo ejecutaba vacío mientras otro nodo que si tenia los bloques
lo ejecutaba de verdad. Los dos llegaban a estados distintos y publicaban el mismo
bloque encima de cada uno: mismo bloque, dos raices.

Ahora el replay **para en el hueco**. Lo que paso en un timeslot que nadie ha
descrito no se puede suponer, asi que el nodo ejecuta lo que puede demostrar y
espera a lo demas, que llega despues y se recoge en la pasada siguiente. Parar
tampoco es una forma de no alcanzar nunca: un tramo sin huecos se recorre entero, y
eso tambien esta fijado por un test.

Con eso, dos validadores que se van turnando la autoria dan la misma raiz en cada
timeslot, un tramo con huecos se detiene en el hueco dejando el estado que el autor
tenia en ese punto, y un tramo entero se ejecuta de punta a punta. Los tres casos
tienen test.

### La deriva, medida con los nodos en paz

Nada de reinicios, nada de nodos caidos: cinco nodos en marcha y mirandolos durante
150 segundos. La comprobacion de la malla pregunta si coinciden en algun momento, y
eso no dice si se quedan de acuerdo. Preguntado al reves:

```
muestra  0: altura 0x2; se separan: 3
muestra 31: altura 0x8; se separan: 1 2 3 4
muestra 47: altura 0xb; se separan: 4
muestra 58: altura 0xc; se separan: 1
```

Se separan desde el principio y se separan otra vez. No es del reinicio: es el
regimen.

Y con la traza de replay puesta, el dato que lo explica:

```
STRAWBERRY_TRACE_REPLAY=1   ->  cero lineas en los cinco nodos
peticiones de bloque        ->  0
backfill                    ->  0
anuncios procesados         ->  0
```

`bp.replay` **no se llama ni una vez**. Los nodos ejecutan sus propios bloques y
nunca los de los demas. Y a la vez los cinco nombran el mismo bloque, o sea que las
**cabeceras si se propagan y los cuerpos no**.

De ahi la forma exacta del fallo: mismo bloque, distintas raices. Cada nodo ha
ejecutado lo suyo y ha adoptado la cabecera de los demas sin ejecutar el bloque que
nombra. El estado que anuncia es el suyo, no el de la cadena que publica.

Y el motivo por el que la cadena aun asi avanza y los tips concuerdan es que la
comprobacion de la punta dice siempre "la punta va un timeslot por detras", que es
lo que hace un nodo que solo ve su propia cadena: el siguiente bloque es suyo y
nadie le ha traido el de al lado.

### Lo que esto significa para lo que falta

Lo que se pide es que todo converja y que haya pruebas con transacciones de uno a
seis nodos, tirando nodos y levantando otros. Nada de eso puede pasar mientras los
nodos no ejecuten los bloques ajenos: un nodo con la cadena correcta y el estado
equivocado no esta sincronizado, y una transaccion comprobada sobre ese estado
comprobaria el estado equivocado.

El caminho esta acotado y son tres cosas, en este orden:

1. **Que llegue el cuerpo del bloque.** Un anuncio trae una cabecera; el cuerpo se
   pide aparte, y esas peticiones son cero. Es el CE 128 que ya existe y que la
   malla no esta usando.
2. **Que el nodo lo ejecute.** Con el cuerpo en el store, `executeUpTo` tiene lo
   que necesita y `bp.replay` entraria en juego, que es justo lo que hoy no
   ocurre nunca.
3. **Que la comprobacion mida el bloque y no el instante.** Dos nodos con un autor
   por timeslot estan un paso distintos casi siempre, asi que comparar sus raices
   vivas mide la ventana y no el estado. Hace falta poder preguntar por la raiz que
   un nodo tiene para un bloque concreto.

`scripts/devnet-deriva.sh N` deja la primera de estas medida, que es como se
llego aqui, y es la forma de comprobar cada uno de los tres pasos por separado.

### Lo que queda, ahora con lo que ya esta arreglado

La deriva que medi esta arreglada: cinco nodos sin tocar ninguno pasan de 0 de 100
muestras de acuerdo a 99 de 100. Lo que queda son dos cosas, y ninguna es de la
cadena.

**El faucet se queda colgado.** `papucoin_faucet` llega al nodo y no vuelve nunca:

```
DEBUG | message: "RPC call" | method: "papucoin_faucet"
... y despues, nada. Ni el faucet, ni system_health, ni jam_getHeader.
```

La llamada se registra y el manejador no termina. Como el servidor atiende de uno en
uno, el resto del RPC se queda sin respuesta con el nodoProduces bloques normal,
que es la parte incomoda: el nodo esta bien y el RPC esta muerto.

Se sabe de donde sale: sin clave puente contesta enseguida con

```
the node has no bridge key, so it cannot pay for a payout
```

o sea que la llamada entra, pide la clave, la encuentra, y ahi es donde se queda.
`Runtime.Faucet` firma el pago y lo mete en la cola, y pedir el estado de la cola
despues (`s.runtime.Pending()`) es lo que no termina. Es la parte de la
transaccion, no la de la cadena.

**Seis nodos no caben en esta maquina.** Con seis, la cadena arranca y hay muestras
de acuerdo, pero el RPC no contesta: seis nodos con `GOMAXPROCS=1` saturan la
maquina y el `curl` de cuatro segundos expira antes de que el nodo conteste. Es un
limite de la caja de pruebas, no del protocolo, y se ve subiendo el margen del
`curl` o dejando los nodos por tandas.

### La matriz de pruebas

`scripts/devnet-matrix.sh [nodos] [segundos]` recorre de uno a seis nodos y, en
cada numero:

1. que la cadena arranque sola y los N se pongan de acuerdo
2. que una transaccion llegue a todos, con el mismo bloque, la misma raiz y el
   mismo saldo
3. que al tirar la mitad de los nodos y levantar otros tantos de cero, sin
   reiniciar los que se tiraron, la red vuelva a ponerse de acuerdo
4. que el saldo sea el mismo en todos despues de las caidas

El punto 3 es el que pedia que un nodo que cae se apague y se cree otro nuevo para
saltarse el fallo, y no se reinicie el mismo: `tira` mata el proceso y borra su
directorio, y `levanta` arranca uno nuevo desde cero. Que un nodo vuelva con su
directorio es otra prueba, y esa la cubre `TestANodeResumesTheChainItWasRunning`.

Los pasos 2 y 4 dependen del faucet, asi que hoy fallan por lo de arriba y no por
la cadena. Los pasos 1 y 3 son los que se pueden pasar, y el 1 ya se pasa.

### Cuantos validadores caben, y donde estaba el limite de verdad

Seis no era un limite de la maquina. Era un parametro de la cadena:
`NumberOfValidators` es el tamano de los vectores fijos donde vive el conjunto de
validadores, y ademas entra en la codificacion de la *epoch marker* que viaja en la
cabecera del primer timeslot de cada epoca. Dos nodos que no coincidan en ese numero
estan en cadenas distintas y no pueden entenderse ni por accidente. No puede ser una
opcion de linea de comandos: es parte de como se codifica una cabecera.

De ahi que subirlo de 6 a 32 anada 64 bytes por validador en esa una cabecera por
epoca, y nada mas en ninguna parte. Las pruebas de estado, Safrole y bloque siguen
en verde con 32, y 8 nodos sin tocar convergen.

El generador tenia ademas un segundo limite mas tonto: ocho, porque solo hay ocho
nombres para repartir. A partir de 32 los demas se llaman `validator-N`, y ahora
acepta `-addrs`, una direccion por validador, que es lo que hace falta para escribir
donde esta cada maquina. Con `-addrs` se pueden corregir las direcciones de
validadores que ya estan en el fichero sin tocar las claves: un validador lo
identifica su clave, y moverlo no lo convierte en otro.

### La libreria de Rust se desempaquetaba una vez por proceso

Cada arranque sacaba la libreria a un directorio temporal nuevo y no lo borraba
nunca. Son un par de megabytes, asi que un nodo arrancado y parado unas cuantas
veces dejaba esos mismos megabytes en `/tmp`. En una maquina donde `/tmp` es un
tmpfs de 4 GB, asi se llena la red y todos los nodos mueren en medio de un bloque:

```
pebble: fatal commit error: write .../000008.log: no space left on device
```

Ocurrio con 8 nodos tras 1661 de esos directorios, 3.2 GB de un `/tmp` de 3.6 GB.
Ahora se escribe una vez por maquina bajo un nombre que lleva el hash de sus
bytes, de modo que una red comparte una sola copia, que un binario nuevo nunca carga
una libreria vieja, y que dos nodos arrancando a la vez no encuentran una a medio
escribir: se renombra al entrar. 8 nodos, un directorio, y eran 1661.

### Un extremo de cada par marca, y solo uno

Cuando los dos extremos de un par se llaman mutuamente a la vez hay dos conexiones
por par, y el par tiene que ponerse de acuerdo en cual se queda. Se pusieran de
acuerdo comparando puertos, lo cual en una sola maquina funciona por casualidad:
todas las maquinas escuchan en 30333, luego los puertos de escucha son iguales y lo
que queda son los efimeros, que cada extremo ve en orden distinto. Cada uno se
queda entonces con una conexion distinta. Las dos siguen vivas, la lista de pares
dice dos vecinos, y no se anuncia nada, porque los anuncios bajan por una conexion
mientras el otro extremo escucha en la otra.

Ahora marca un solo extremo de cada par, decidido comparando las dos claves, que
ambos extremos calculan igual. Cinco nodos en loopback no cambian: 99 de 100
muestras, sin deriva al final.

### Lo que falta para varias maquinas: el anunciador

Con cada nodo en una direccion distinta (`127.0.0.1`, `.2`, `.3`, mismo puerto), la
malla se pobla pero un nodo puede quedarse con sus pares y **sin anunciador**: la
lista muestra las conexiones y ninguna tiene `announcing`, y ese nodo deja de
autorar bloques y se queda esperando la base del timeslot.

```
mesh-0 pares={"127.0.0.2:30333"}{"127.0.0.3:30333"}   <- ninguno anunciando
mesh-1 pares={"127.0.0.1:47314","announcing":true}{...}
```

Es decir: los extremos no coinciden en que conexion anuncio. Esto ya pasaba antes
del cambio de marcado (entonces `mesh-0` producia 14 bloques y los otros dos 0), no
lo introdujo el marcado unidireccional, que es neutro en loopback. Queda abierto:
la metadata si hace round-trip correcto con IPv4 (`To16` y `AddrPort.UnmarshalBinary`
se entienden bien), asi que la pista esta en que extremo crea el anunciador y cual
lo recibe, no en como se codifica la direccion.

Mientras tanto, para varias maquinas: `scripts/devnet-prepare.sh` escribe los
validadores y el genesis **una vez** y da una linea de arranque por maquina. Importa
que el genesis sea el mismo fichero byte a byte en todas, porque generarlo en cada
maquina con su propio reloj produce cadenas distintas.
