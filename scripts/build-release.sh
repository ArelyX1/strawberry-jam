#!/usr/bin/env bash
#
# Compila el nodo y las dos librerias Rust para un objetivo, y comprueba que lo
# que sale es de la arquitectura correcta.
#
# Cada libreria Rust va incrustada en el binario con go:embed, asi que no hace
# falta un enlazador cruzado para Go: basta con incrustar la libreria del
# objetivo. Para Rust si hace falta, y de ahi cargo-zigbuild con zig, que trae
# su propia cadena de herramientas para C y para el enlazado.
#
# Uso:
#   scripts/build-release.sh linux/amd64
#   scripts/build-release.sh windows/amd64
#   scripts/build-release.sh todos
#
# Lo que no se puede hacer aqui esta escrito en voz alta en vez de fallar en
# silencio: ver la seccion de objetivos no soportados.

set -euo pipefail

cd "$(dirname "$0")/.."

RAIZ=$PWD
SALIDA=${SALIDA:-$RAIZ/dist}

# El Portable GNU de Windows necesita el flag de solo biblioteca dinamica. Sin
# el, el DLL busca sus propias dependencias en el PATH de quien lo ejecute, y
# un nodo que arranca en una maquina de pruebas y no en la del usuario es un
# nodo que no arranca.
BANDERSNATCH_EXT=so
    ERASURECODING_EXT=so
    FLAGS_CARGO=()
    SIMBOLOS_BS="init_ring_size get_ring_size new_secret secret_public ietf_vrf_sign ietf_vrf_verify ietf_vrf_output_hash new_ring_vrf_verifier free_ring_vrf_verifier ring_vrf_verifier_commitment ring_vrf_verifier_verify ring_vrf_output_hash new_ring_vrf_prover free_ring_vrf_prover ring_vrf_prover_sign"
    SIMBOLOS_EC="reed_solomon_encode reed_solomon_decode"

log() { printf '\n== %s\n' "$*"; }

fallar() {
    printf 'ERROR: %s\n' "$*" >&2
    exit 1
}

# La extension de la libreria y el nombre que la incrusta, por objetivo. El
# nombre va dentro del binario, asi que tiene que coincidir con el que espera
# el fichero de ese sistema operativo.
configurar_objetivo() {
    local objetivo=$1
    BANDERSNATCH_LIB=libbandersnatch
    ERASURECODING_LIB=liberasurecoding

    case "$objetivo" in
        linux/amd64)
            BANDERSNATCH_EXT=so
            ERASURECODING_EXT=so
            FLAGS_CARGO=()
            ;;
        linux/arm64)
            BANDERSNATCH_EXT=so
            ERASURECODING_EXT=so
            FLAGS_CARGO=()
            SIMBOLOS_BS="init_ring_size get_ring_size new_secret secret_public ietf_vrf_sign ietf_vrf_verify ietf_vrf_output_hash new_ring_vrf_verifier free_ring_vrf_verifier ring_vrf_verifier_commitment ring_vrf_verifier_verify ring_vrf_output_hash new_ring_vrf_prover free_ring_vrf_prover ring_vrf_prover_sign"
            SIMBOLOS_EC="reed_solomon_encode reed_solomon_decode"
            ;;
        windows/amd64)
            BANDERSNATCH_EXT=dll
            ERASURECODING_EXT=dll
            FLAGS_CARGO=(--no-default-features)
            SIMBOLOS_BS="init_ring_size get_ring_size new_secret secret_public ietf_vrf_sign ietf_vrf_verify ietf_vrf_output_hash new_ring_vrf_verifier free_ring_vrf_verifier ring_vrf_verifier_commitment ring_vrf_verifier_verify ring_vrf_output_hash new_ring_vrf_prover free_ring_vrf_prover ring_vrf_prover_sign"
            SIMBOLOS_EC="reed_solomon_encode reed_solomon_decode"
            ;;
        darwin/amd64|darwin/arm64)
            # En un Mac no se cruza nada: es el nativo, y cargo compila para el
            # anfitrion sinzig. El caso esta para que el script sea el mismo en
            # todas las maquinas y no haya dos listas de objetivos que se
            # separan sin que nadie se entere.
            BANDERSNATCH_EXT=dylib
            ERASURECODING_EXT=dylib
            FLAGS_CARGO=()
            SIMBOLOS_BS="init_ring_size get_ring_size new_secret secret_public ietf_vrf_sign ietf_vrf_verify ietf_vrf_output_hash new_ring_vrf_verifier free_ring_vrf_verifier ring_vrf_verifier_commitment ring_vrf_verifier_verify ring_vrf_output_hash new_ring_vrf_prover free_ring_vrf_prover ring_vrf_prover_sign"
            SIMBOLOS_EC="reed_solomon_encode reed_solomon_decode"
            ;;
        windows/arm64)
            BANDERSNATCH_EXT=dll
            ERASURECODING_EXT=dll
            FLAGS_CARGO=(--no-default-features)
            SIMBOLOS_BS="init_ring_size get_ring_size new_secret secret_public ietf_vrf_sign ietf_vrf_verify ietf_vrf_output_hash new_ring_vrf_verifier free_ring_vrf_verifier ring_vrf_verifier_commitment ring_vrf_verifier_verify ring_vrf_output_hash new_ring_vrf_prover free_ring_vrf_prover ring_vrf_prover_sign"
            SIMBOLOS_EC="reed_solomon_encode reed_solomon_decode"
            ;;
        *)
            return 1
            ;;
    esac
}

# sha2-asm se niega a compilar para Windows con un error, no con un aviso, asi
# que la feature asm se apaga ahi. El resultado es el mismo: lo comprueba
# scripts/comprobar-bandersnatch.sh, que compara los bytes de las dos
# compilaciones.
# Para el objetivo del anfitrion se usa cargo a secas, que ya sabe compilarlo.
# Para cualquier otro hace falta cargo-zigbuild, que aporta la cadena de C y el
# enlazador para el destino. cargo-zigbuild deduce el objetivo de zig del triple
# de Rust, asi que no admite flags de zig propias.
construir_rust() {
    local triple=$1
    local comando=(cargo)
    local anfitrion
    anfitrion=$(rustc -vV | awk '/^host:/ {print $2}')

    if [ "$triple" != "$anfitrion" ]; then
        if ! command -v cargo-zigbuild >/dev/null; then
            fallar "para $triple hace falta cargo-zigbuild, y no esta instalado:
                cargo install cargo-zigbuild
            y zig, que se descarga de https://ziglang.org/download/"
        fi
        comando=(cargo-zigbuild)
    fi

    log "bandersnatch para $triple"
    ( cd "$RAIZ" && "${comando[@]}" build --release --lib \
        "${FLAGS_CARGO[@]}" --manifest-path=bandersnatch/Cargo.toml \
        --target "$triple" )

    log "erasurecoding para $triple"
    ( cd "$RAIZ" && "${comando[@]}" build --release --lib \
        --manifest-path=erasurecoding/Cargo.toml \
        --target "$triple" )
}

# Fija RUTA_BS y RUTA_EC: donde va la libreria de cada crate dentro del arbol de
# Go. Vive aparte porque los necesitan dos funciones distintas, incrustar() para
# escribir y la comprobacion para leer, y duplicar el nombre en las dos seria
# una forma de que se desincronicen sin que nada falle.
destinos() {
    RUTA_BS="internal/crypto/bandersnatch/lib/${BANDERSNATCH_LIB}.${BANDERSNATCH_EXT}"
    RUTA_EC="internal/erasurecoding/reedsolomon/lib/${ERASURECODING_LIB}.${ERASURECODING_EXT}"
}

# Copia la libreria del objetivo donde la incrusta el binario. El nombre es el
# que espera el fichero de ese sistema, no el que produce Rust, porque son
# nombres distintos para la misma cosa.
incrustar() {
    local triple=$1

    destinos
    mkdir -p internal/crypto/bandersnatch/lib internal/erasurecoding/reedsolomon/lib

    cp "$(encontrar_lib bandersnatch "$BANDERSNATCH_LIB" "$BANDERSNATCH_EXT" "$triple")" "$RUTA_BS"
    cp "$(encontrar_lib erasurecoding "$ERASURECODING_LIB" "$ERASURECODING_EXT" "$triple")" "$RUTA_EC"

    printf '  incrustada %s\n' "$RUTA_BS"
    printf '  incrustada %s\n' "$RUTA_EC"
}

# El nombre que produce Rust y el que espera el fichero de Go no son el mismo en
# todos los sistemas: en Windows Rust no pone el prefijo lib, y en Linux y macOS
# si. Se buscan las dos formas en vez de suponer una, porque suponer mal deja el
# binario sin la libreria dentro y el fallo sale al arrancar, en la maquina del
# usuario y no en la de compilar.
encontrar_lib() {
    local crate=$1
    local nombre=$2
    local ext=$3
    local triple=$4
    local corto=${nombre#lib}
    local dir="$crate/target/$triple/release"

    for base in "$nombre" "$corto"; do
        if [ -f "$dir/$base.$ext" ]; then
            printf '%s' "$dir/$base.$ext"
            return 0
        fi
    done

    fallar "no se encuentra $nombre.$ext para $triple en $dir:
    lo que hay es:
$(ls -1 "$dir" 2>/dev/null | grep -E '\.(so|dylib|dll)$' || echo '  (ninguna libreria)')"
}

# Una DLL valida puede no exportar nada. dll de Windows empieza con la tabla de
# exports vacia, y rustc no la rellena al compilar para windows-gnu, de modo que
# se obtiene un fichero con el tamano y el aspecto correctos que no sirve para
# nada. file lo declara PE32+ y el nodo muere en el arranque al buscar la
# primera funcion. Esta comprobacion es la que lo detecta.
exportar_falta() {
    local dll=$1; shift
    local visto
    visto=$(python3 - "$dll" "$@" <<'PY'
import struct, sys
ruta = sys.argv[1]
esperado = sys.argv[2:]
d = open(ruta, 'rb').read()
pe = struct.unpack_from('<I', d, 0x3c)[0]
nsec = struct.unpack_from('<H', d, pe + 6)[0]
optsz = struct.unpack_from('<H', d, pe + 20)[0]
opt = pe + 24
magic = struct.unpack_from('<H', d, opt)[0]
edata_rva, _ = struct.unpack_from('<II', d, opt + (112 if magic == 0x20b else 96))
secs = []
so = opt + optsz
for i in range(nsec):
    b = so + 40 * i
    secs.append((struct.unpack_from('<I', d, b + 12)[0],
                 struct.unpack_from('<I', d, b + 8)[0],
                 struct.unpack_from('<I', d, b + 20)[0]))
def r2o(rva):
    for va, vs, raw in secs:
        if va <= rva < va + max(vs, 1) + 0x1000:
            return raw + (rva - va)
    return None
faltan = list(esperado)
if edata_rva:
    n = r2o(edata_rva)
    nNombres = struct.unpack_from('<I', d, n + 24)[0]
    rNombres = r2o(struct.unpack_from('<I', d, n + 32)[0])
    hay = set()
    for i in range(nNombres):
        o = r2o(struct.unpack_from('<I', d, rNombres + 4 * i)[0])
        if o:
            hay.add(d[o:d.index(b'\0', o)].decode())
    faltan = [e for e in esperado if e not in hay]
print(' '.join(faltan))
PY
)
    if [ -n "$visto" ]; then
        fallar "a $dll le faltan estas funciones: $visto"
    fi
}

construir_un_objetivo() {
    local objetivo=$1
    local goos=${objetivo%/*}
    local goarch=${objetivo#*/}

    configurar_objetivo "$objetivo" || fallar "objetivo no soportado: $objetivo"

    log "objetivo $objetivo"

    # Un nombre de triple de Rust no es un nombre de GOOS/GOARCH, y confundirlos
    # compila para una cosa y nombra el fichero como si fuera otra.
    local triple
    case "$objetivo" in
        linux/amd64)   triple=x86_64-unknown-linux-gnu ;;
        linux/arm64)   triple=aarch64-unknown-linux-gnu ;;
        windows/amd64) triple=x86_64-pc-windows-gnu ;;
        windows/arm64) triple=aarch64-pc-windows-gnu ;;
        darwin/amd64)   triple=x86_64-apple-darwin ;;
        darwin/arm64)   triple=aarch64-apple-darwin ;;
    esac

    if ! rustup target list --installed | grep -qx "$triple"; then
        printf '  instalando el objetivo de Rust %s\n' "$triple"
        rustup target add "$triple"
    fi

    construir_rust "$triple"
    incrustar "$triple"

    local sufijo=""
    [ "$goos" = windows ] && sufijo=".exe"

    mkdir -p "$SALIDA"
    # El objetivo lleva una barra, y en un nombre de fichero se leeria como una
    # carpeta: el resultado era dist/strawberry-windows/amd64.exe, que parece un
    # directorio con un binario dentro y no un binario.
    local etiqueta=${objetivo//\//-}
    local binario="$SALIDA/strawberry-$etiqueta$sufijo"

    destinos
    log "comprobando que las librerias exportan lo que el nodo busca"
    # En Unix los simbolos se ven con nm, en Windows hay que leer la tabla de
    # exports del PE, porque ahi una libreria puede no exportar nada sin que nada
    # falle al compilar.
    case "$objetivo" in
        windows/*)
            exportar_falta "$RUTA_BS" $SIMBOLOS_BS
            exportar_falta "$RUTA_EC" $SIMBOLOS_EC
            ;;
        *)
            for s in $SIMBOLOS_BS; do
                nm -D --defined-only "$RUTA_BS" | grep -q " T $s\$" ||
                    fallar "la libreria de bandersnatch no define $s"
            done
            for s in $SIMBOLOS_EC; do
                nm -D --defined-only "$RUTA_EC" | grep -q " T $s\$" ||
                    fallar "la libreria de erasure coding no define $s"
            done
            ;;
    esac
    printf '  las dos librerias exportan todo lo que el nodo busca\n'

    log "go para $objetivo"
    # CGO apagado a proposito: la unica cosa de C en este repositorio son las dos
    # librerias Rust, y esas van incrustadas, no enlazadas. Con cgo encendido el
    # enlazado busca un compilador cruzado que no hace falta y no hay.
    ( cd "$RAIZ" && GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 \
        go build -trimpath -ldflags="-s -w" -o "$binario" ./cmd/strawberry )

    log "comprobando $binario"
    local tipo
    tipo=$(file -b "$binario")
    printf '  %s\n' "$tipo"

    # Un binario que no es del sistema pedido se instala y luego no arranca, y el
    # aviso llega tarde, en la maquina del usuario.
    case "$objetivo" in
        windows/*)
            grep -q 'PE32' <<<"$tipo" || fallar "el binario de windows no es un PE32: $tipo"
            case "$objetivo" in
                windows/arm64)
                    grep -qE 'ARM64|aarch64' <<<"$tipo" || fallar "el binario no es ARM64: $tipo"
                    ;;
            esac
            ;;
        darwin/*)
            grep -q 'Mach-O' <<<"$tipo" || fallar "el binario de macOS no es un Mach-O: $tipo"
            case "$objetivo" in
                darwin/arm64)
                    grep -q 'arm64' <<<"$tipo" || fallar "el binario no es arm64: $tipo"
                    ;;
            esac
            ;;
        linux/*)
            grep -q 'ELF 64-bit' <<<"$tipo" || fallar "el binario de linux no es un ELF de 64 bits: $tipo"
            case "$objetivo" in
                linux/arm64)
                    grep -q 'ARM aarch64' <<<"$tipo" || fallar "el binario no es aarch64: $tipo"
                    ;;
            esac
            ;;
    esac

    printf '\nlisto: %s (%s)\n' "$binario" "$(du -h "$binario" | cut -f1)"
}

main() {
    case "${1:-}" in
        todos|todas)
            for objetivo in linux/amd64 windows/amd64; do
                construir_un_objetivo "$objetivo"
            done
            printf '\nObjetivos que faltan y por que:\n'
            printf '  linux/arm64  hace falta el objetivo de Rust y ya esta soportado aqui:\n'
            printf '                scripts/build-release.sh linux/arm64\n'
            printf '  darwin/*     un binario de macOS se enlaza contra el SDK y los frameworks\n'
            printf '                de Apple, que solo existen en un Mac. Desde Linux no hay\n'
            printf '                forma de enlazarlos, y un binario sin enlazar no arranca.\n'
            printf '                Se compila en un Mac con:\n'
            printf '                  scripts/build-release.sh darwin/arm64\n'
            ;;
        "")
            printf 'uso: scripts/build-release.sh <objetivo|todos>\n'
            printf 'objetivos: linux/amd64 linux/arm64 windows/amd64 darwin/arm64 darwin/amd64\n'
            exit 2
            ;;
        *)
            construir_un_objetivo "$1"
            ;;
    esac
}

main "$@"
