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

## What happens when a node falls

The mesh holds with half of it down: the survivors stay in agreement and keep
writing. When the missing one comes back, it catches up.

Authorship redundancy exists now, behind a flag. Without `--skip-missing-authors`
the chain still waits for the dead author — that was the old behaviour and it is
still the default. With the flag on, a designated author that has not written a
timeslot within the grace period is skipped: the suplente `(slot+1)%authorCount`
writes that slot itself, carrying its own `BlockAuthorIndex` in the block, so the
seal verifies against its own key and no escrow is needed.

The suplente is deterministic — one node per slot, so several candidates cannot
all write the same slot and fork the chain. A cap of `authorCount` consecutive
skips stops a fully dead mesh from producing nonsense: if that cap is exceeded,
the node logs an error and does not skip; the chain stops honestly rather than
writing blocks nobody can verify.

When the skipped author comes back, `catchUpBehind()` pulls it to the tip and it
resumes writing its own slots. The skip check runs before the chain-behind wait
in the producer loop: the suplente must not wait for a chain that is one slot
behind, because the slot it is waiting on is the one the dead author was
supposed to write. `shouldSkipSlot` still requires `atTipFor`, so the suplente
cannot write on a parent it does not have.

## Block propagation: grid-diffusion first, flood as fallback

Block announcements are sent to the grid neighbours first — the same
deterministic ring the connection grid already builds, so propagation follows
the topology instead of shouting at everyone. If the grid announcement tells
failed peers, the announcer falls back to a full flood for that block. Stored
blocks are re-announced to newly connected peers so a late joiner does not have
to wait for the next slot to hear about what it missed.

Grid-diffusion is installed at startup via `SetupGridDiffusion()`; it does not
change the wire protocol, only the order in which the existing announcement
messages are sent.

## Estado de los binarios

| Sistema | Arquitectura | Estado |
|---|---|---|
| Linux | amd64 | Listo. Un solo fichero, con las librerías Rust dentro |
| Linux | arm64 | Pendiente de compilar |
| macOS | amd64, arm64 | Pendiente de compilar |
| Windows | amd64, arm64 | Pendiente de compilar |

### Dónde están los binarios compilados

Tras `./scripts/build-release.sh linux/amd64` o `./scripts/build-release.sh windows/amd64`:

```
node-go/dist/strawberry-linux-amd64      (51 MB, Linux amd64)
node-go/dist/strawberry-windows-amd64.exe (54 MB, Windows amd64)
```

Son archivos autocontenidos: no necesitan Go, ni cargo, ni nada más. Se copian
tal cual a la otra máquina y se ejecutan. En Linux puede que haya que darles
permiso de ejecución (`chmod +x`). En Windows no hace falta nada.

Para llevar la red a otra PC se copia **el binario más la carpeta del kit**
(`--init-genesis`): `genesis.json`, `validators.json`, `appconfig.json` y los
`net-conf-N.conf`. Sin el kit el nodo no sabe qué red es y se niega a arrancar.

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

- **libp2p completo**: el descubrimiento entre redes distintas funciona ya
  (`5f5f985e`), pero DHT, *hole punching* y relay no están implementados. La red
  se monta con direcciones o con el kit `--init-genesis`.
- **Arranque automático**: no hay unidad de systemd, ni *launchd*, ni tarea
  programada de Windows.
- **Prueba en dos máquinas**: verificado en local, no entre dos equipos
  distintos.
