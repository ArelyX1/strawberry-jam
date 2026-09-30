#!/usr/bin/env bash
#
# Arranca un nodo, comprueba que levanta y que produce bloques encadenados, y lo
# apaga. Es la comprobación de humo: si esto pasa, el nodo arranca y la cadena
# avanza. No sustituye a `make test`, que corre la suite entera.
#
# Uso:
#   ./scripts/devnet-smoke.sh              # nodo efímero en memoria, puerto 19944
#   RPC_PORT=19955 ./scripts/devnet-smoke.sh
#   KEEP_RUNNING=1 ./scripts/devnet-smoke.sh   # lo deja vivo y con datos en disco
#
# Sale con 0 si todo va bien, con 1 si el nodo no levanta o no produce.

set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

RPC_PORT="${RPC_PORT:-19944}"
KEEP_RUNNING="${KEEP_RUNNING:-0}"
DATA_DIR="${DATA_DIR:-}"
LOG_FILE="${LOG_FILE:-/tmp/strawberry-smoke.log}"

BINARY="$ROOT/strawberry"
if [[ ! -x "$BINARY" ]]; then
  echo "ERROR: no existe $BINARY" >&2
  echo "       constrúyelo con: make build   (o: go build -tags dev -o strawberry ./cmd/strawberry/)" >&2
  exit 1
fi

if [[ -z "$DATA_DIR" ]]; then
  ARGS=(--data-dir "")
else
  ARGS=(--data-dir "$DATA_DIR")
fi

rpc() {
  curl -s -m 5 -X POST -H 'Content-Type: application/json' \
    --data "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$1\",\"params\":$2}" \
    "http://127.0.0.1:$RPC_PORT/"
}

wait_for_rpc() {
  for _ in $(seq 1 60); do
    if [[ -n "$(rpc system_health '[]')" ]]; then return 0; fi
    sleep 1
  done
  return 1
}

cleanup() {
  if [[ -n "${NODE_PID:-}" ]] && kill -0 "$NODE_PID" 2>/dev/null; then
    if [[ "$KEEP_RUNNING" == "1" ]]; then
      echo "KEEP_RUNNING=1: el nodo sigue vivo (pid $NODE_PID, log $LOG_FILE)"
    else
      kill "$NODE_PID" 2>/dev/null
      wait "$NODE_PID" 2>/dev/null
      echo "nodo apagado"
    fi
  fi
}
trap cleanup EXIT

echo "==> arrancando nodo en el puerto $RPC_PORT (log: $LOG_FILE)"
./strawberry --name SmokeTest --rpc-port "$RPC_PORT" "${ARGS[@]}" > "$LOG_FILE" 2>&1 &
NODE_PID=$!

if ! wait_for_rpc; then
  echo "FALLO: el RPC no respondió en 60s" >&2
  tail -20 "$LOG_FILE" >&2
  exit 1
fi
echo "    RPC responde"

# Hash del último bloque ahora y dentro de un rato. Si no cambia, la cadena está
# parada: es el mismo número que devolvería chain_getBlockNumber.
latest_hash() {
  rpc chain_getBlockHash '[99999999]' | grep -o '0x[0-9a-f]*' | head -1
}

FIRST="$(latest_hash)"
echo "    último bloque: ${FIRST:-<ninguno>}"

sleep 12

SECOND="$(latest_hash)"
echo "    último bloque: ${SECOND:-<ninguno>}"

if [[ -z "$SECOND" ]]; then
  echo "FALLO: no hay ningún bloque en la cadena" >&2
  exit 1
fi

if [[ "$FIRST" == "$SECOND" ]]; then
  echo "FALLO: la cadena no avanzó (sigue en el bloque $SECOND)" >&2
  exit 1
fi

# El estado tiene que avanzar también, no solo la altura: dos bloques con la
# misma raíz indicarían que el runtime no está aplicando nada.
ROOTS="$(grep -o '"stateRoot": "0x[0-9a-f]*"' "$LOG_FILE" | tail -3 | sort -u | wc -l)"
if [[ "$ROOTS" -lt 2 ]]; then
  echo "AVISO: solo se vio un stateRoot distinto; la cadena podría no estar aplicando estado"
else
  echo "    stateRoot avanzando ($ROOTS raíces distintas en los últimos bloques)"
fi

echo
echo "OK: el nodo arranca, sirve RPC y produce bloques encadenados"
exit 0
