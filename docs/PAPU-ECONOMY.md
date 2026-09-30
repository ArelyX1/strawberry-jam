# El precio de PAPU

PAPU tiene un precio por transferencia, y ese precio se mueve. No es una tarifa
fija con otro nombre: reacciona a lo que la cadena está haciendo.

## Dónde vive

La regla está escrita una vez, en Go, y una vez en Rust, y las dos tienen que
darla misma:

| Concern | Nativo | Guest |
| --- | --- | --- |
| Constantes y regla | `sdk/papucoin/fee.go` | `guests/src/main.rs` |
| Almacenamiento | key `0x0a` | key `0x0a` |
| Cobro | `sdk/papucoin/service.go` | `guests/src/main.rs` |
| Expuesto por RPC | `cmd/strawberry/rpc_papucoin.go` | — |

Si las constantes viven en dos sitios, tarde o temprano dejan de coincidir y la
cadena tiene dos precios a la vez. Es exactamente el bug que había.

## La regla

El precio se guarda en unidades enteras de PAPU, nunca en decimales: la
aritmética en coma flotante sobre dinero acaba en diferencias de un céntimo que
nadie sabe de dónde salió.

- Suelo: `1_000` (0,000001 PAPU). Nadie regala transferencias.
- Techo: `1_000_000_000`. Un sobrecargo que sube sin límite deja de ser un
  precio y pasa a ser una multa.
- Objetivo de carga: 8 transferencias por bloque.

Dado un bloque con `t` transferencias aceptadas:

| Transferencias | Qué hace |
| --- | --- |
| `t > 8` | Sube × 3/2 |
| `t == 0` | Baja × 2/3 |
| `0 < t <= 8` | Se queda igual |

La banda muerta del medio es a propósito. Un mercado que reacciona a cada
transferencia individual es un mercado que nadie quiere usar: la gente esperaría
al bloque tranquilo en vez de enviar.

Cada ajuste se redondea **hacia abajo**, y el suelo y el techo se aplican
después. Redondear hacia arriba acumularía error en cada bloque y el precio
subiría solo con el tiempo, sin que nadie pagara nada.

## El bloque vacío cuenta

Este es el punto que costó encontrar.

El planificador asigna un servicio a un núcleo **cuando tiene trabajo encolado**.
Si no hay trabajo, no hay acumulación. Y si no hay acumulación, el precio no se
mira. O sea que el precio solo sabía subir.

Eso convierte una racha de carga en un peaje permanente: la cadena se encarece
una vez y se queda cara para siempre, sin que nada en la cadena haya cambiado
que lo justifique. No es un precio, es una multa con memoria.

El arreglo está en `pkg/devnet/runtime.go`: **el servicio de la economía se
ejecuta una vez por timeslot aunque no tenga nada que hacer.** Ese bloque vacío
no es un bloque tirado: es la evidencia de calma que el precio necesita para
volver a su sitio.

El guest hace lo mismo: `accumulate` con la lista de items vacía sigue siendo un
acumulado, y por eso llama a `next_fee(0)`. Cuando el guest retornaba temprano
con el body vacío, el bloque vacío no decía nada, y las dos implementaciones de
la misma cadena acababan con precios distintos.

## Nota sobre "esto solo funciona en local"

No. `cmd/strawberry/main.go` —el binario que se levanta en un servidor— construye
el runtime con `devnet.New(...)` y carga el génesis con `devnet.LoadGenesis(...)`.
**El runtime de `pkg/devnet` es el runtime del servidor**, no un camino de
pruebas aparte. El cambio de arriba está en producción, y está verificado con
ese mismo binario levantando dos cadenas.

Existe además `internal/statetransition`, que sí es otro camino: lo usan
`pkg/conformance` para correr los vectores oficiales de JAM. Ese camino maneja
servicios del spec, no la economía de PAPU, así que el precio no le concierne.
Allí el equivalente ya existía: `AmountOfGasPerServiceId`, el mapa de servicios
que acumulan en cada bloque con un mínimo de gas.

## Qué se cotiza y cuándo se cobra

Refine cotiza, accumulate cobra.

- **Refine** es quien fija `op.Fee` en el reporte del item, porque refine es
  donde una transferencia se acepta y todavía se puede rechazar.
- **Accumulate** cobra exactamente esa cotización, del estado almacenado en
  refine. No vuelve a mirar el precio actual.

Si accumulate consultara el precio en el momento del cobro, el emisor pagaría un
precio que nunca le cotizaron. Con un bloque vacío entre el envío y el cobro, el
pagador pagaría una cifra distinta de la que le dijeron, y esa diferencia saldría
de su bolsillo sin que él pudiera evitarla.

## Verificación

- `sdk/papucoin/service_test.go`: sube con carga, baja con el bloque vacío, se
  queda quieto en la banda, y siempre es fracción de una unidad.
- `pkg/devnet/guest_economy_test.go`, `TestNativeAndGuestChargeTheSamePrice`:
  la misma carga deja nativa y guest en el mismo precio, **incluyendo los
  bloques ociosos del final**. Es el test que habría cazado la divergencia entre
  guest y nativo.
- `cmd/strawberry/node_test.go`: la cotización sobrevive a un reinicio.
- En las dos cadenas levantadas: el precio baja solo en bloques vacíos, y las
  tres formas de transacción EVM (eip1559, eip2930, legacy) se aplican y se
  cobran.

## Un fallo que era del test, no del código

El primer version de `TestNativeAndGuestChargeTheSamePrice` leía "el precio tras
la carga" **después** de los pasos ociosos, así que comparaba el precio consigo
mismo y pasaba siempre. El código estaba bien en esa parte; el test no midía lo
que decía medir. Está aquí anotado porque un test que pasa sin comprobar nada
es peor que no tener test.
