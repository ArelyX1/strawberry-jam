#!/usr/bin/env bash
# La red entera, de uno a seis nodos, con transacciones y con nodos tirados.
#
# Lo que se comprueba, para cada numero de nodos:
#
#   1. que la cadena arranca sola y los N se ponen de acuerdo
#   2. que una transaccion llega a todos: el mismo bloque, la misma raiz de estado
#      y el mismo saldo de la cuenta que se fundo, en los N nodos
#   3. que al tirar un nodo y levantar otro, sin reiniciar el que se tiro, la red
#      vuelve a ponerse de acuerdo
#   4. lo mismo tirando la mitad y levantando la mitad
#
# El nodo que se tira no se reinicia: se apaga y se levanta otro nuevo en su sitio.
# Reiniciar el mismo con su directorio es otra cosa, y esa esta probada en
# TestANodeResumesTheChainItWasRunning. Aqui lo que se prueba es que la red aguanta
# que un nodo desaparezca y otro ocupe su lugar.
#
#   scripts/devnet-matrix.sh [nodos] [segundos por comprobacion]
set -uo pipefail

NODES="${1:-6}"
LIMITE="${2:-70}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT" || exit 1

BASE_PORT=30333
BASE_RPC=19944
RUN="/tmp/strawberry-matrix"
VALFILE="$RUN/validators.json"
GENFILE="$RUN/genesis.json"
PIDS=()
FALLOS=0
CUENTA="0x0000000000000000000000000000000000001092"

# El faucet lo paga la cuenta puente del propio nodo, y todos tienen que tener la
# misma: cada uno firma el pago con su clave, y si las claves no son la misma el
# pago es distinto en cada nodo, y con el el estado.
BRIDGE="1111111111111111111111111111111111111111111111111111111111111111"

mkdir -p "$RUN"

q() { curl -s -m 4 -X POST -H 'content-type: application/json' \
  -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$2\",\"params\":$3}" "http://localhost:$1" 2>/dev/null; }

tip()    { q "$1" jam_getHeader '[]' | grep -o '"hash":"0x[0-9a-f]*"' | head -1 | cut -d'"' -f4; }
raiz()   { q "$1" jam_getHeader '[]' | grep -o '"resultingStateRoot":"0x[0-9a-f]*"' | head -1 | cut -d'"' -f4; }
saldo()  { q "$1" papucoin_balance "[\"$CUENTA\"]" | grep -o '"raw":"[0-9]*"' | head -1 | cut -d'"' -f4; }
altura() { q "$1" eth_blockNumber '[]' | grep -o '"result":"0x[0-9a-f]*"' | head -1 | cut -d'"' -f4; }

limpia() {
  local p
  for p in "${PIDS[@]:-}"; do [ -n "$p" ] && kill -TERM "$p" 2>/dev/null; done
  sleep 1
  for p in "${PIDS[@]:-}"; do [ -n "$p" ] && kill -KILL "$p" 2>/dev/null; done
  PIDS=()
  rm -rf "$RUN"/n*/ 2>/dev/null
}
trap limpia EXIT

# esperaconsiste: se da tiempo a que la red se ponga de acuerdo. Los nodos se
# separan un momento cuando entra un bloque nuevo, mientras el que no lo escribio
# lo trae y lo ejecuta, y eso se cierra solo. Preguntar una sola vez mediria esa
# ventana; preguntar hasta que se de acuerdo mide lo que importa.
esperaconsiste() {
  local i=0 ref refr malo j
  while [ "$i" -lt "$LIMITE" ]; do
    ref=$(tip $((BASE_RPC)))
    refr=$(raiz $((BASE_RPC)))
    if [ -n "$ref" ] && [ -n "$refr" ]; then
      malo=""
      for j in $(seq 0 $((N - 1))); do
        [ "$(tip $((BASE_RPC + j)))" = "$ref" ] || malo="$malo $j"
        [ "$(raiz $((BASE_RPC + j)))" = "$refr" ] || malo="$malo $j"
      done
      [ -z "$malo" ] && return 0
    fi
    sleep 1
    i=$((i + 1))
  done
  return 1
}

# mismoSaldoEnTodos: la cuenta tiene que verse igual en los N nodos.
mismoSaldoEnTodos() {
  local refsaldo j
  refsaldo=$(saldo $((BASE_RPC)))
  [ -n "$refsaldo" ] || return 1
  for j in $(seq 0 $((N - 1))); do
    [ "$(saldo $((BASE_RPC + j)))" = "$refsaldo" ] || return 1
  done
  return 0
}

levanta() {
  local idx="$1"
  local dir="$RUN/n$idx"
  rm -rf "$dir"; mkdir -p "$dir"
  GOMAXPROCS=1 ./strawberry --name "mesh-$idx" --validator-index "$idx" \
    --validators-file "$VALFILE" --author-count "$N" --full-mesh \
    --genesis "$GENFILE" --bridge-wallet "$BRIDGE" \
    --rpc-port $((BASE_RPC + idx)) --port $((BASE_PORT + idx)) \
    --data-dir "$dir" > "$RUN/n$idx.log" 2>&1 &
  PIDS+=("$!")
}

tira() {
  local idx="$1"
  local p="${PIDS[$1]}"
  [ -n "$p" ] && kill -TERM "$p" 2>/dev/null
  PIDS[$1]=""
  rm -rf "$RUN/n$idx"
}

ok()    { echo "  OK    $1"; }
falla() { echo "  FALLA $1"; FALLOS=$((FALLOS + 1)); }

rm -f "$VALFILE" "$GENFILE"
GOMAXPROCS=1 go run ./scripts/gen-validators -out "$VALFILE" -count "$NODES" -port "$BASE_PORT" >/dev/null || exit 1
python3 - "$GENFILE" <<'PY'
import datetime, json, sys, time
g = json.load(open("genesis/chain-dev.json"))
epoch = int(datetime.datetime(2025,1,1,12,0,0,tzinfo=datetime.timezone.utc).timestamp())
g["genesisTimeslot"] = int(time.time() - epoch) // g["timeslotSecs"]
json.dump(g, open(sys.argv[1], "w"), indent=4)
PY

echo "=== transaccion y tolerancia a caidas, de 1 a $NODES nodos ==="
N=1
while [ "$N" -le "$NODES" ]; do
  echo "-- $N nodo(s) --"
  limpia

  i=0
  while [ "$i" -lt "$N" ]; do levanta "$i"; i=$((i + 1)); done
  # Un nodo solo puede buscar la cadena cuando los demas ya estan.
  sleep 8

  if esperaconsiste; then
    ok "los $N se ponen de acuerdo"
  else
    falla "los $N no se ponen de acuerdo al arrancar"
  fi

  # Una transaccion de verdad: el faucet paga a una cuenta que no existia y el
  # trabajo viaja dentro de un bloque hasta todos los nodos. El faucet solo se
  # paga una vez por cuenta, y aqui una sola vez en toda la red.
  q $((BASE_RPC)) papucoin_faucet "[\"$CUENTA\"]" >/dev/null

  llego=0
  s=0
  while [ "$s" -lt "$LIMITE" ]; do
    v=$(saldo $((BASE_RPC)))
    if [ -n "$v" ] && [ "$v" != "0" ]; then llego=1; break; fi
    sleep 1
    s=$((s + 1))
  done

  if [ "$llego" = "1" ]; then
    if mismoSaldoEnTodos; then
      ok "la transaccion llega a los $N con el mismo saldo ($(saldo $((BASE_RPC))))"
    else
      falla "la transaccion llega distinta a cada nodo"
    fi
  else
    falla "la transaccion no llego a ninguno"
  fi

  # Tirar la mitad y levantar la mitad. El que se tira no vuelve: se apaga, y otro
  # nodo ocupa su sitio desde cero, sin su directorio y sin su estado.
  if [ "$N" -ge 2 ]; then
    mitad=$((N / 2))
    j=0
    while [ "$j" -lt "$mitad" ]; do tira "$j"; j=$((j + 1)); done
    echo "  (tirados $mitad de $N, y otros $mitad levantados de cero)"
    j=0
    while [ "$j" -lt "$mitad" ]; do levanta "$j"; j=$((j + 1)); done
    sleep 8

    if esperaconsiste; then
      ok "se vuelve a poner de acuerdo con $mitad nodos nuevos"
    else
      falla "no se vuelve a poner de acuerdo tras $mitad caidas"
    fi

    if mismoSaldoEnTodos; then
      ok "el saldo es el mismo en todos tras las caidas ($(saldo $((BASE_RPC))))"
    else
      falla "el saldo no es el mismo en todos tras las caidas"
    fi
  fi

  N=$((N + 1))
done

limpia
echo
if [ "$FALLOS" -eq 0 ]; then
  echo "VEREDICTO: todo correcto."
else
  echo "VEREDICTO: $FALLOS comprobaciones fallaron."
fi
exit "$FALLOS"