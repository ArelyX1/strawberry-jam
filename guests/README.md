# Guest de PAPU

La economia PAPU como guest polkavm, compilada a un blob que el PVM ejecuta.
`papucoin.pol` es el resultado; el codigo esta aqui para poder reconstruirlo.

## Reconstruir

Hace falta el nightly que soporta el target de polkavm y el polkatool de
nuestro fork, que es quien sabe enlazar para ReviveV1.

El target y el `build-std` van en la linea de comandos y no en un
`.cargo/config.toml`. No es una preferencia: un `.cargo/config.toml` se hereda
hacia abajo y no se puede anular, de modo que el crate que hay en `core/` se
buildaba para polkavm al probar sus tests y Cargo se quejaba de que el target no
tiene biblioteca estandar. Puesto en la linea de comandos, cada comando ve solo
lo que necesita.

```sh
cargo +nightly-2025-05-10 build --release \
  --target riscv64emac-unknown-none-polkavm.json -Zbuild-std=core,alloc
../../references/polkavm/target/release/polkatool link -i revive_v1 \
  -o papucoin.pol target/riscv64emac-unknown-none-polkavm/release/papucoin-guest
```

## Probar

La mitad de este codigo no necesita el PVM para nada, y eso es lo que permite
probarla. Un test corre en esta maquina, con la biblioteca estandar de verdad:

```sh
cd core && cargo +nightly-2025-05-10 test
```

O desde la raiz de `node-go`, `make test-guest`.

El blob va con su suma: `papucoin.pol.sha256`, que la CI comprueba. El enlace se
puede reconstruir igual bit a bit, asi que si la suma no cuadra es que el blob
commiteado no sale del codigo que tiene al lado.

Cuando todo estaba dentro del binario del guest no habia forma de ejecutar un
test contra el, y dos supuestos suyos estaban mal sin que nada lo delatara: el
nonce que escribia era de dieciseis bytes y el que leia aceptaba ocho como
mucho, con lo que una cuenta no podia pasar de su primer numero; y el nonce
Ethereum que anunciaba en cada reporte no se comparaba con nada.

## Por que el enlace necesita `-i revive_v1`

El PVM de la cadena decodifica `ecalli` como el opcode 10, que es lo que
ReviveV1 emite. Las ISAs mas nuevas codifican el mismo mnemonico con otros
bytes, y un blob enlazado con ellas se detiene en la primera llamada al host.

## Estructura

`src/main.rs` tiene la economia: refine produce el reporte, accumulate lo aplica
sobre el storage del servicio. Un solo export sirve para las dos fases; el byte
NUL final en los argumentos es lo que las distingue.

`core/src/crypto.rs` tiene las direcciones de cadena (base58 + blake2b-256) y la
verificacion de firmas Ed25519. `core/src/evm.rs` tiene lo que hace falta para
aceptar una transaccion de una wallet que solo habla Ethereum: RLP, keccak y la
decodificacion de la transaccion tipada.

Esas dos estan en un crate aparte porque no dependen de la maquina. El binario
del guest depende de el, no lo contiene: lo que necesita del PVM son las llamadas
al host y el `exit`, y eso se queda en `main.rs`.

**El guest no verifica la firma secp256k1.** `k256::ecrecover` compila pero
entra en panic en no_std: la aritmetica de curva necesita algo que un programa
sin libreria estandar no tiene. La verifica el host, que ya tenia una
implementacion probada, y pasa el remitente en `evmSender`. Confiar en el host
no es un agujero: el host es la cadena, y este programa es codigo que la cadena
eligio ejecutar.

## Detalles que no son evidentes

- **El allocator** es un bump sobre un arena estatica en `.data`, no en `.bss`:
  el linker no incluye BSS en el blob y el guest segea con page fault.
- **Las llamadas al host** no pueden asumir su indice. El linker numera los
  imports por el orden de primera llamada; la cadena traduce el indice al id
  canonico leyendo la tabla de imports del blob.
- **`mustBeSigned`** es camelCase en el JSON. Sin el `rename` de serde, el campo
  nunca se parsea, queda `false`, y la verificacion de firma se salta entera:
  una firma manipulada pasaba.
- **El chain id lo dice la cadena**, no el guest. Esta en el storage que escribe
  el seed. Una cadena que no dice cual es acepta nada relayed, que es lo seguro.
- **Las tres formas de transaccion.** Legacy, EIP-2930 y EIP-1559 tienen los
  campos en sitios distintos, y el destino y el valor cambian de indice segun
  cual sea. Que emita una u otra es decision de la libreria del wallet, no de la
  cadena: un guest que solo entienda una convierte una transferencia legitima en
  un rechazo.
- **El seed lo escribe el guest.** Issuer, simbolo, saldos iniciales y chain id.
  Sin el, el issuer arranca en cero y la cadena no puede pagar un faucet.
