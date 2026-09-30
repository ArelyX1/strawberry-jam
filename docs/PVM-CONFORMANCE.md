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
