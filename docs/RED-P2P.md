# La red, y cómo poner un nodo en marcha

Este documento es lo que hay que leer para levantar la red. Está escrito para
quien va a poner un nodo en su máquina y no quiere leer el código.

## Lo primero: por qué el binario no bastaba

La pregunta natural es "dame un binario y lo ejecuto". El problema es que eso no
era posible, y no por el binario: por el **genesis**.

La cadena se funda en un *timeslot* concreto. Todos los nodos tienen que fundarla
en el mismo. Un nodo sin ese dato fechaba la cadena a su propia hora de
arranque, así que dos máquinas levantadas con los mismos archivos fundaban dos
cadenas distintas: cada bloque que una escribía, la otra lo rechazaba porque no
descendía del bloque "finalizado", y la red no sincronizaba nunca. No había
ningún error visible, solo nodos que arrancaban, parecían sanos y no se
entendían.

Es el fallo más caro que tenía la red, porque se manifests como "no funciona" y
no como "te falta un dato".

Ahora el nodo **se niega a arrancar** y lo dice:

```
the genesis has no genesisTimeslot, so this node would found its own chain at
its own start time and never agree with the rest of the network
```

## Levantar la red: un comando, y luego copiar una carpeta

En la máquina que va a crear la red:

```
./strawberry --init-genesis ./red --validator-count 4
```

Eso escribe una carpeta con cuatro archivos:

| Archivo | Qué es |
|---|---|
| `genesis.json` | La economía y el *timeslot* en que se funda la cadena |
| `validators.json` | Las claves de los validadores: quién existe |
| `appconfig.json` | Ajustes del nodo |
| `LEVANTAR.txt` | Las órdenes exactas para arrancar |

Copias esa carpeta a las demás máquinas. **Es la misma carpeta en todas**: mismo
genesis, mismos validadores.

En cada máquina, un nodo por validador. Para el validador 0:

```
./strawberry \
  --config      red/appconfig.json \
  --net-conf    red/net-conf-0.conf \
  --validators-file red/validators.json \
  --genesis     red/genesis.json \
  --validator-index 0 \
  --rpc-port    29644 \
  --data-dir    datos0
```

El `--net-conf` es lo que hace que los nodos **se encuentren**. Sin él, cada
nodo busca a los demás en el puerto 30334 que dice el archivo de validadores, y
allí no escucha nadie: los cuatro arrancan, todos dicen "vivo", y la malla está
vacía. Esto ya pasó y es la segunda forma más cara de perder tiempo.

## Verificar que están de acuerdo

Que dos nodos tengan bloques no significa que estén de acuerdo. Lo que importa
es la *state root*, que es un checksum del estado completo:

```
curl -s -X POST -H 'content-type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"jam_getHeader","params":[null]}' \
  http://localhost:29644
```

Dos cosas tienen que coincidir en **todos** los nodos: `timeSlotIndex` (dónde
está la cadena) y `resultingStateRoot` (qué estado tiene). Si el root difiere,
dos nodos han aplicado los mismos bloques de forma distinta, y eso es un bug
serio aunque ambos estén "sincronizados".

Comprobar un saldo es la prueba de que una transacción llegó entera:

```
curl -s -X POST -H 'content-type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"papucoin_balance","params":["sdlg..."]}' \
  http://localhost:29644
```

## Transacciones firmadas

`cmd/papusigner` firma items contra el SDK del propio nodo, de modo que no puede
desincronizarse de lo que la cadena acepta:

```
./papusigner -seed <hex> -method transfer -to sdlg... -amount 25 -nonce 1
```

Imprime el item en base64, que es lo que espera `papucoin_submit`.

Dos cosas que costaron tiempo y conviene no volver a descubrir:

- El item se firma **con la clave que produce la dirección del emisor**.
  Firmar como otro es imposible sin reescribir el item, y eso es a propósito.
- El item es `method` / `sender` / `nonce` / `to` / `amount`, y **la firma va
  dentro** del item, no al lado en un sobre. Un sobre aparte se rechaza con
  `item is not a PAPU item`.

## Qué pasa cuando un nodo se cae

La malla se sostiene con la mitad caída: los supervivientes siguen de acuerdo y
siguen escribiendo. Cuando el que falta vuelve, se pone al día.

Lo que **no** hay es redundancia de autoría. El turno de autoría rota por
`timeslot % authorCount == índice`, así que si el nodo que le toca escribir está
apagado, la cadena **espera** a que vuelva. No hay otro que escriba en su lugar.

Esto es una limitación real y hay que decirla con todas las letras: la red
tolera que un nodo se caiga, pero **no continúa avanzando si el que cae es el
autor de ese timeslot**. Un diseño tolerante de verdad necesita o redundancia
de autores o rotación del conjunto de validadores (Safrole), y aquí Safrole está
desactivado: el conjunto de validadores es fijo y está escrito en el archivo.

## Estado de los binarios

| Sistema | Arquitectura | Estado |
|---|---|---|
| Linux | amd64 | Listo. Un solo fichero, con las librerías Rust dentro |
| Linux | arm64 | Pendiente de compilar |
| macOS | amd64, arm64 | Pendiente de compilar |
| Windows | amd64, arm64 | Pendiente de compilar |

El de Linux/amd64 no depende de nada externo salvo la libc: las dos librerías
nativas (bandersnatch y Reed-Solomon) van incrustadas en el binario y se
desembeben en tiempo de ejecución.

Los otros sistemas no compilan hoy porque esas dos librerías existen solo como
`.so` de Linux. Hace falta compilarlas por plataforma, y para eso falta un
*cross-compiler* de C en la máquina que construye, porque la capa FFI usa tipos
de cgo. El trabajo de dejar los *bindings* preparados por plataforma está hecho
(`bandersnatch_windows.go`, `reedsolomon_windows.go`); lo que falta es la
herramienta de construcción y el script que recorra los targets.

## Lo que no está hecho

- **libp2p**: el descubrimiento entre redes distintas (DHT, *hole punching*,
  relay) no está implementado. Hoy la red se monta con direcciones escritas a
  mano. Es el trabajo más grande que queda.
- **Arranque automático**: no hay unidad de systemd, ni *launchd*, ni tarea
  programada de Windows.
- **Prueba en dos máquinas**: verificado en local, no entre dos equipos
  distintos.
