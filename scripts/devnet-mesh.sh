#!/usr/bin/env bash
# Levanta una malla de N validadores y comprueba que se sincronizan.
#
#   scripts/devnet-mesh.sh [N]
#
# Los N nodos se levantan a la vez, y no es un detalle. En JAM cada timeslot tiene
# exactamente un autor, y si ese autor no esta en marcha ese timeslot no lo escribe
# nadie. Montar la red de uno en uno choca de frente con esa regla: los timeslots de
# los validadores que aun no habian llegado se quedaban sin escribir, y una malla
# asi se quedaba parada o se dividia, porque no habia manera de que los nodos se
# pusieran de acuerdo en quien escribia cada hueco. Con el turno fijo repartido
# entre N y los N en marcha, cada timeslot tiene su autor y la cadena avanza sola.
#
# El genesis se fecha en el presente por lo mismo: las timeslots anteriores a la
# fundacion no los ha escrito nadie, y con el turno repartido cada validator tiene
# que esperar a que le toque en vez de escribir un pasado que no es suyo.
#
# Lo que se comprueba:
#
#   1. la cadena arranca sola y los N nodos comparten bloque y raiz de estado
#   2. la malla esta completa: cada nodo ve a los otros N-1
#   3. uno se cae, la cadena sigue sin el, y al volver se pone al dia
#
# La raiz de estado no se puede comparar en un instante cualquiera: el nodo que
# autoro el bloque del tip ya lo ejecuto y el otro todavia no, asi que estan un
# paso distintos de la misma cadena durante unos segundos despues de cada bloque.
# Por eso se prueba varias veces y la cadena se da por buena en cuanto coinciden.
#
# Los PIDs se guardan y se matan uno a uno. Nada de pkill -f: un patron amplio
# tambien coincide con el shell que lo ejecuta, y ha matado procesos que no eran
# los nodos.
set -uo pipefail

N="${1:-3}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT" || exit 1

BIN="./strawberry"
[ -x "$BIN" ] || { echo "falta $BIN; compílalo con: go build -tags dev -o strawberry ./cmd/strawberry/" >&2; exit 1; }

BASE_PORT=30333
BASE_RPC=19944
RUN="/tmp/strawberry-mesh"
VALFILE="$RUN/validators-$N.json"
GENFILE="$RUN/genesis-$N.json"

NODES=()   # indices de validador, en orden
PIDS=()    # pid de cada nodo, en el mismo orden

mkdir -p "$RUN"

q() { # q <rpcPort> <method> [params]
  local params="${3:-[]}"
  curl -s -m "${RPC_TIMEOUT:-8}" -X POST -H 'content-type: application/json' \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$2\",\"params\":$params}" \
    "http://localhost:$1" 2>/dev/null
}
tip()    { q "$1" jam_getHeader | grep -o '"hash":"0x[0-9a-f]*"' | cut -d'"' -f4; }
root()   { q "$1" jam_getHeader | grep -o '"resultingStateRoot":"0x[0-9a-f]*"' | cut -d'"' -f4; }
height() { q "$1" eth_blockNumber | grep -o '0x[0-9a-f]*' | head -1; }
peers()  { q "$1" system_peers | grep -o '"peerId"\|"address"' | wc -l; }
vivos()  { local p; for p in "${PIDS[@]:-}"; do [ -n "$p" ] && kill -0 "$p" 2>/dev/null && return 0; done; return 1; }

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

limpia() { rm -rf "$RUN"/n*/ 2>/dev/null; }

# arranca <indice-de-en-la-lista>
arranca() {
  local pos="$1"
  local i="${NODES[$pos]}"
  local net=$((BASE_PORT + i))
  local rpc=$((BASE_RPC + i))
  local dir="$RUN/n$i"
  rm -rf "$dir"; mkdir -p "$dir"
  GOMAXPROCS=1 "$BIN" --name "mesh-$i" --validator-index "$i" \
    --validators-file "$VALFILE" --author-count "$N" --full-mesh \
    --genesis "$GENFILE" --rpc-port "$rpc" --port "$net" --data-dir "$dir" \
    > "$RUN/n$i.log" 2>&1 &
  PIDS[$pos]="$!"
}

# mata <posicion> y espera a que el proceso se vaya de verdad
mata() {
  # En dos pasos y no en uno: `local` declara los nombres antes de asignarlos, asi
  # que en la misma linea $pos todavia no existe y con `set -u` eso es un error.
  local pos="$1"
  local p="${PIDS[$pos]}"
  local i
  [ -z "$p" ] && return 0
  kill -TERM "$p" 2>/dev/null
  for i in $(seq 1 20); do
    kill -0 "$p" 2>/dev/null || break
    sleep 0.5
  done
  kill -0 "$p" 2>/dev/null && kill -KILL "$p" 2>/dev/null
  sleep 1
  PIDS[$pos]=""
}

espera_rpc() { # espera_rpc <segundos>
  local t="$1" i=0 pos todos
  while [ "$i" -lt "$((t * 2))" ]; do
    todos=1
    for pos in "${!NODES[@]}"; do
      [ -n "${PIDS[$pos]}" ] || continue
      [ -n "$(q $((BASE_RPC + ${NODES[$pos]})) eth_blockNumber)" ] || todos=0
    done
    [ "$todos" = "1" ] && return 0
    sleep 0.5; i=$((i + 1))
  done
  return 1
}

convergen() { # convergen <etiqueta> <segundos>
  local etiqueta="$1" t="$2" i=0 pos ref
  while [ "$i" -lt "$((t * 2))" ]; do
    ref=""
    for pos in "${!NODES[@]}"; do
      [ -n "${PIDS[$pos]}" ] || continue
      ref=$(tip $((BASE_RPC + ${NODES[$pos]})))
      [ -n "$ref" ] && break
    done
    if [ -n "$ref" ]; then
      local mismos=1
      for pos in "${!NODES[@]}"; do
        [ -n "${PIDS[$pos]}" ] || continue
        [ "$(tip $((BASE_RPC + ${NODES[$pos]})))" = "$ref" ] || mismos=0
      done
      if [ "$mismos" = "1" ]; then
        local ref_r mismos_r=1
        ref_r=""
        for pos in "${!NODES[@]}"; do
          [ -n "${PIDS[$pos]}" ] || continue
          ref_r=$(root $((BASE_RPC + ${NODES[$pos]})))
          [ -n "$ref_r" ] && break
        done
        for pos in "${!NODES[@]}"; do
          [ -n "${PIDS[$pos]}" ] || continue
          [ "$(root $((BASE_RPC + ${NODES[$pos]})))" = "$ref_r" ] || mismos_r=0
        done
        if [ "$mismos_r" = "1" ]; then
          echo "  OK $etiqueta: los nodos comparten bloque y raiz de estado"
          return 0
        fi
      fi
    fi
    sleep 0.5; i=$((i + 1))
  done
  echo "  FALLO $etiqueta: tras ${t}s los nodos no coinciden"
  local pos
  for pos in "${!NODES[@]}"; do
    [ -n "${PIDS[$pos]}" ] || { echo "    mesh-${NODES[$pos]}  (apagado)"; continue; }
    echo "    mesh-${NODES[$pos]}  altura $(height $((BASE_RPC + ${NODES[$pos]})))  tip $(tip $((BASE_RPC + ${NODES[$pos]})))"
    echo "                 raiz $(root $((BASE_RPC + ${NODES[$pos]})))  pares $(peers $((BASE_RPC + ${NODES[$pos]})))"
  done
  return 1
}

comprueba_malla() {
  local esperado=$((N - 1)) fallo=0 pos
  for pos in "${!NODES[@]}"; do
    [ -n "${PIDS[$pos]}" ] || continue
    local p; p=$(peers $((BASE_RPC + ${NODES[$pos]})))
    if [ "$p" -lt "$esperado" ]; then
      echo "  FALLO: mesh-${NODES[$pos]} ve $p pares y deberia ver $esperado"
      fallo=1
    fi
  done
  [ "$fallo" = "0" ] && echo "  OK malla: cada nodo ve $esperado pares"
  return "$fallo"
}

echo "== malla de $N validadores =="
rm -f "$VALFILE" "$GENFILE"
if ! GOMAXPROCS=1 go run ./scripts/gen-validators -out "$VALFILE" -count "$N" -port "$BASE_PORT" >/dev/null; then
  echo "no se pudo generar el fichero de validadores para $N nodos" >&2
  exit 1
fi
python3 - "$GENFILE" <<'PYGEN'
import datetime, json, sys, time
g = json.load(open("genesis/chain-dev.json"))
epoch = int(datetime.datetime(2025, 1, 1, 12, 0, 0,
                              tzinfo=datetime.timezone.utc).timestamp())
g["genesisTimeslot"] = int(time.time() - epoch) // g["timeslotSecs"]
json.dump(g, open(sys.argv[1], "w"), indent=4)
PYGEN
echo "validadores: $VALFILE ($N entradas)"
echo "genesis:     $GENFILE"

fallos=0
i=0
while [ "$i" -lt "$N" ]; do NODES+=("$i"); i=$((i + 1)); done
i=0
while [ "$i" -lt "$N" ]; do arranca "$i"; i=$((i + 1)); done
echo "levantados $N nodos a la vez, puertos $BASE_PORT-$((BASE_PORT + N - 1))"

if ! espera_rpc 40; then
  echo "  FALLO: no todos los nodos abrieron su RPC"
  exit 1
fi

echo
echo "-- la cadena arranca sola y los nodos se ponen de acuerdo --"
convergen "arranque" $((50 + N * 10)) || fallos=$((fallos + 1))

echo
echo "-- la malla esta completa --"
comprueba_malla || fallos=$((fallos + 1))

echo
echo "-- uno se cae, la cadena sigue sin el, y al volver se pone al dia --"
mata 0
sleep 15
QUEDA=$(tip $((BASE_RPC + 1)))
if [ -n "$QUEDA" ]; then
  echo "  OK la cadena sigue sin mesh-0: $QUEDA"
else
  echo "  FALLO: la cadena se paro sin mesh-0"
  fallos=$((fallos + 1))
fi
arranca 0
if ! espera_rpc 40; then
  echo "  FALLO: mesh-0 no abrio su RPC al volver"
  fallos=$((fallos + 1))
else
  echo "  mesh-0 ha vuelto"
fi
sleep 12
convergen "regreso" $((70 + N * 20)) || fallos=$((fallos + 1))
comprueba_malla || fallos=$((fallos + 1))

echo
if [ "$fallos" = "0" ]; then
  echo "VEREDICTO: los $N nodos forman una malla sincronizada, y el que vuelve se pone al dia."
else
  echo "VEREDICTO: $fallos comprobaciones fallaron."
fi
para
limpia
exit "$fallos"
