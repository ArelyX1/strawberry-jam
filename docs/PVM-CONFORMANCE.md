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
