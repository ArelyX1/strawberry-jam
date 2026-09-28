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

## Detalles que no son evidentes

- **El allocator** es un bump sobre un arena estatica en `.data`, no en `.bss`:
  el linker no incluye BSS en el blob y el guest segea con page fault.
- **Las llamadas al host** no pueden asumir su indice. El linker numera los
  imports por el orden de primera llamada; la cadena traduce el indice al id
  canonico leyendo la tabla de imports del blob.
- **`mustBeSigned`** es camelCase en el JSON. Sin el `rename` de serde, el campo
  nunca se parsea, queda `false`, y la verificacion de firma se salta entera.
