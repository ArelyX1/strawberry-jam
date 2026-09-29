# Guest de PAPU

La economia PAPU como guest polkavm, compilada a un blob que el PVM ejecuta.
`papucoin.pol` es el resultado; el codigo esta aqui para poder reconstruirlo.

## Reconstruir

Hace falta el nightly que soporta el target de polkavm y el polkatool de
nuestro fork, que es quien sabe enlazar para ReviveV1.

```sh
cargo +nightly-2025-05-10 build --release
../../references/polkavm/target/release/polkatool link -i revive_v1 \
  -o papucoin.pol target/riscv64emac-unknown-none-polkavm/release/papucoin-guest
```

## Por que el enlace necesita `-i revive_v1`

El PVM de la cadena decodifica `ecalli` como el opcode 10, que es lo que
ReviveV1 emite. Las ISAs mas nuevas codifican el mismo mnemonico con otros
bytes, y un blob enlazado con ellas se detiene en la primera llamada al host.

## Estructura

`src/main.rs` tiene la economia: refine produce el reporte, accumulate lo aplica
sobre el storage del servicio. Un solo export sirve para las dos fases; el byte
NUL final en los argumentos es lo que las distingue.

`src/crypto.rs` tiene las direcciones de cadena (base58 + blake2b-256) y la
verificacion de firmas Ed25519.

`src/evm.rs` tiene lo que hace falta para aceptar una transaccion de una wallet
que solo habla Ethereum: RLP, keccak y la decodificacion de la transaccion
tipada 0x02.

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
- **El seed lo escribe el guest.** Issuer, simbolo, saldos iniciales y chain id.
  Sin el, el issuer arranca en cero y la cadena no puede pagar un faucet.
