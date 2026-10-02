#!/usr/bin/env bash
# Levanta una malla de N validadores y comprueba que se sincronizan.
#
#   scripts/devnet-mesh.sh [N]
#
# Los nodos se van levantando de uno en uno y despues de cada uno se comprueba
# que el recien llegado se ha sincronizado con los que ya estaban. Asi se ve en
# que momento se rompe, en vez de mirar al final una malla que quizá estar mal desde
# el principio.
#
# Lo que se comprueba en cada etapa:
#
#   - el nodo nuevo ve a todos los demas, y cada uno de los anteriores lo ve a el
#   - todos los nodos nombran el mismo bloque
#   - todos los nodos tienen la misma raiz de estado
#
# La raiz de estado no puede compararse en un instante cualquiera: el nodo que
# autoro el bloque del tip ya lo ejecuto y el otro todavia no, asi que estan un
# paso distintos de la misma cadena durante unos segundos despues de cada bloque.
# Por eso se prueba varias veces y se da por buena la cadena en cuanto coinciden,
# que es lo que significa que el estado va bien.
#
# Los PIDs se guardan y se matan uno a uno. Nada de pkill -f: un patron amplio
# tambien coincide con el shell que lo ejecuta.
set -uo pipefail

N="${1:-3}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT" || exit 1

BIN="./strawberry"
[ -x "$BIN" ] || { echo "falta $BIN; compílalo con: go build -tags dev -o strawberry ./cmd/strawberry/" >&2; exit 1; }

BASE_PORT=30333
BASE_RPC=19944
RUN="/tmp/strawberry-mesh"
# El fichero de validadores se genera para esta ejecucion con exactamente N
# entradas. Asi el numero de nodos no esta escrito en ningun sitio del binario ni
# del repositorio: N nodos es un fichero con N validadores. El conjunto de
# validadores es un parametro de la cadena y por eso lo fijan todos los nodos
# igual, leyendo el mismo fichero; lo que cambia de una ejecucion a otra es
# cuantos hay, no lo que cada nodo cree que hay.
VALFILE="$RUN/validators-$N.json"
# El genesis se comparte y se fecha una sola vez para toda la malla. El bloque de
# genesis es el padre de todo, asi que dos nodos que lo fundaran en momentos
# distintos estarian en dos cadenas distintas desde el primer bloque, y ninguno
# aceptaria nunca un bloque del otro. Por eso el fichero lleva un
# genesisTimeslot y todos los nodos de la ejecucion leen el mismo.
GENFILE="$RUN/genesis-$N.json"
PIDS=()
NODES=()

mkdir -p "$RUN"

# Se borra antes de generar: el generador conserva lo que ya habia en el
# fichero, y asi una ejecucion anterior con otra convencion no se queda pegada.
rm -f "$VALFILE"
if ! GOMAXPROCS=1 go run ./scripts/gen-validators -out "$VALFILE" -count "$N" -port "$BASE_PORT" >/dev/null; then
  echo "no se pudo generar el fichero de validadores para $N nodos" >&2
  exit 1
fi
rm -f "$GENFILE"
python3 - "$GENFILE" "$N" <<'PYGEN'
import datetime, json, sys, time
out, n = sys.argv[1], sys.argv[2]
g = json.load(open("genesis/chain-dev.json"))
# Un momento concreto, el mismo para todos los nodos de la ejecucion, y un poco
# antes del presente para que al levantarse tengan timeslots que producir en
# lugar de tener que alcanzar al presente desde el genesis.
#
# El timeslot no es la cuenta de segundos desde epoch: la cadena cuenta desde su
# propia epoca, asi que hay que restarla antes de dividir. Sin esa resta el
# genesis queda fechado decades en el futuro y cada nodo se duerme hasta ahi.
epoch = int(datetime.datetime(2025, 1, 1, 12, 0, 0,
                              tzinfo=datetime.timezone.utc).timestamp())
g["genesisTimeslot"] = (int(time.time()) - epoch) // g["timeslotSecs"] - 3
json.dump(g, open(out, "w"), indent=4)
PYGEN
echo "fichero de validadores: $VALFILE ($N entradas)"
echo "genesis compartido:    $GENFILE"

q() { # q <rpcPort> <method> [params]
  curl -s -m 4 -X POST -H 'content-type: application/json' \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$2\",\"params\":${3:-[]}}" \
    "http://localhost:$1" 2>/dev/null
}
tip()  { q "$1" jam_getHeader | grep -o '"hash":"0x[0-9a-f]*"' | cut -d'"' -f4; }
root() { q "$1" jam_getHeader | grep -o '"resultingStateRoot":"0x[0-9a-f]*"' | cut -d'"' -f4; }
height() { q "$1" eth_blockNumber | grep -o '0x[0-9a-f]*' | head -1; }
peers() { q "$1" system_peers | grep -o '"peerId"\|"address"' | wc -l; }

para() {
  local p
  for p in "${PIDS[@]:-}"; do
    [ -n "$p" ] && kill -TERM "$p" 2>/dev/null
  done
  sleep 1
  for p in "${PIDS[@]:-}"; do
    [ -n "$p" ] && kill -KILL "$p" 2>/dev/null
  done
  PIDS=()
}
trap para EXIT

arranca() { # arranca <indice>
  local i="$1" net=$((BASE_PORT + i)) rpc=$((BASE_RPC + i))
  local dir="$RUN/n$i"
  rm -rf "$dir"; mkdir -p "$dir"
  # validator-index dice cual es este nodo, author-count cuantos hay en la malla:
  # el turno de escribir rota entre ellos, y por eso un nodo solo no puede hacer
  # lo mismo que un nodo en una malla de tres.
  GOMAXPROCS=1 "$BIN" --name "mesh-$i" --validator-index "$i" \
    --validators-file "$VALFILE" --author-count "$N" --full-mesh \
    --genesis "$GENFILE" \
    --rpc-port "$rpc" --port "$net" --data-dir "$dir" \
    > "$RUN/n$i.log" 2>&1 &
  PIDS+=("$!")
  NODES+=("$i")
  echo "  levantado mesh-$i  validador $i  red $net  rpc $rpc"
}

espera_rpc() { # espera_rpc <indice> <segundos>
  local rpc=$((BASE_RPC + $1)) t="$2" i=0
  while [ "$i" -lt "$((t * 2))" ]; do
    [ -n "$(q "$rpc" eth_blockNumber)" ] && return 0
    sleep 0.5; i=$((i + 1))
  done
  return 1
}

# espera a que la cadena y el estado coincidan entre todos los nodos vivos.
# Se prueba varias veces porque la ejecucion va un paso detras en cuanto al autor
# del bloque del tip, y hay que caer en un momento en el que hayan convergido.
convergen() { # convergen <etiqueta> <segundos>
  local etiqueta="$1" t="$2" i=0 mejor=0
  while [ "$i" -lt "$((t * 2))" ]; do
    local tips=0 roots=0 n=${#NODES[@]} j ref
    ref=$(tip $((BASE_RPC + ${NODES[0]})))
    [ -z "$ref" ] && { sleep 0.5; i=$((i + 1)); continue; }
    local coinciden=1
    for j in "${NODES[@]}"; do
      [ "$(tip $((BASE_RPC + j)))" = "$ref" ] || coinciden=0
    done
    if [ "$coinciden" = "1" ]; then
      tips=1
      local r ref_r
      ref_r=$(root $((BASE_RPC + ${NODES[0]})))
      local coinciden_r=1
      for j in "${NODES[@]}"; do
        [ "$(root $((BASE_RPC + j)))" = "$ref_r" ] || coinciden_r=0
      done
      [ "$coinciden_r" = "1" ] && roots=1
    fi
    if [ "$tips" = "1" ] && [ "$roots" = "1" ]; then
      echo "  OK $etiqueta: los ${#NODES[@]} nodos comparten bloque y raiz de estado"
      return 0
    fi
    sleep 0.5; i=$((i + 1))
  done
  echo "  FALLO $etiqueta: tras ${t}s los nodos no coinciden"
  for j in "${NODES[@]}"; do
    echo "    mesh-$j  altura $(height $((BASE_RPC + j)))  tip $(tip $((BASE_RPC + j)))"
    echo "              raiz $(root $((BASE_RPC + j)))  pares $(peers $((BASE_RPC + j)))"
  done
  return 1
}

comprueba_malla() { # comprueba_malla <etiqueta>
  local esperado=$(( ${#NODES[@]} - 1 )) fallo=0 j
  for j in "${NODES[@]}"; do
    local p; p=$(peers $((BASE_RPC + j)))
    if [ "$p" -lt "$esperado" ]; then
      echo "  FALLO: mesh-$j ve $p pares y deberia ver $esperado"
      fallo=1
    fi
  done
  [ "$fallo" = "0" ] && echo "  OK malla: cada nodo ve $esperado pares"
  return "$fallo"
}

echo "== malla de $N validadores =="
fallos=0
i=0
hasta=0
i2=0
while [ "$i" -lt "$N" ]; do
  echo
  if [ "$i" = "0" ]; then
    echo "-- nodo 1 en solitario --"
    arranca 0
    espera_rpc 0 25 || { echo "  FALLO: el primer nodo no abrio su RPC"; exit 1; }
    # Con N validadores en el turno este nodo escribe uno de cada N slots, asi
    # que un solo nodo tarda N timeslots en escribir el primero. Se le da margen
    # para que produzca algo antes de que se una el siguiente.
    hasta=$((N * 8)); i2=0
    while [ "$i2" -lt "$hasta" ]; do
      [ "$(height $((BASE_RPC + 0)))" != "0x0" ] && break
      sleep 1; i2=$((i2 + 1))
    done
    echo "  altura $(height $((BASE_RPC + 0)))  escribiendo solo, que es lo que tiene que hacer"
  else
    echo
    echo "-- se une mesh-$i a los $i que ya estaban --"
    arranca "$i"
    espera_rpc "$i" 25 || { echo "  FALLO: mesh-$i no abrio su RPC"; exit 1; }
    # Un nodo recien llegado tiene que alcanzar la cadena comun. Se le da margen
    # para que la complete, y luego se exige que los $((i+1)) coincidan.
    convergen "nodo $i.sync" 75 || fallos=$((fallos + 1))
    comprueba_malla "nodo $i.malla" || fallos=$((fallos + 1))
  fi
  i=$((i + 1))
done

echo
if [ "$fallos" = "0" ]; then
  echo "VEREDICTO: los $N nodos forman una malla sincronizada."
else
  echo "VEREDICTO: $fallos comprobaciones fallaron."
fi
para
exit "$fallos"
