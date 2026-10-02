#!/usr/bin/env bash
# N nodos, sin tocar ninguno, y se mira si se mantienen de acuerdo.
#
# La comprobacion de la malla pregunta si coinciden en algun momento. Eso no dice
# si se quedan de acuerdo: si derivan solos, coincidiran al principio y luego no.
# Aqui se pregunta al reves, y se dice el primer momento en que dejan de coincidir.
#
#   scripts/devnet-deriva.sh [N] [segundos]
set -uo pipefail

N="${1:-5}"
SEG="${2:-150}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT" || exit 1

BASE_PORT=30333
BASE_RPC=19944
RUN="/tmp/strawberry-mesh"
VALFILE="$RUN/validators-$N.json"
GENFILE="$RUN/genesis-$N.json"
NODES=(); PIDS=()

mkdir -p "$RUN"

q() { local p="${3:-[]}"; curl -s -m "${RPC_TIMEOUT:-8}" -X POST -H 'content-type: application/json' \
  -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$2\",\"params\":$p}" "http://localhost:$1" 2>/dev/null; }
tip()  { q "$1" jam_getHeader | grep -o '"hash":"0x[0-9a-f]*"' | cut -d'"' -f4; }
root() { q "$1" jam_getHeader | grep -o '"resultingStateRoot":"0x[0-9a-f]*"' | cut -d'"' -f4; }
altura() { q "$1" eth_blockNumber | grep -o '0x[0-9a-f]*' | head -1; }

limpia() { local p; for p in "${PIDS[@]:-}"; do [ -n "$p" ] && kill -TERM "$p" 2>/dev/null; done; sleep 1
           for p in "${PIDS[@]:-}"; do [ -n "$p" ] && kill -KILL "$p" 2>/dev/null; done
           rm -rf "$RUN"/n*/ 2>/dev/null; }
trap limpia EXIT

rm -f "$VALFILE" "$GENFILE"
GOMAXPROCS=1 go run ./scripts/gen-validators -out "$VALFILE" -count "$N" -port "$BASE_PORT" >/dev/null || exit 1
python3 - "$GENFILE" <<'PY'
import datetime, json, sys, time
g = json.load(open("genesis/chain-dev.json"))
epoch = int(datetime.datetime(2025,1,1,12,0,0,tzinfo=datetime.timezone.utc).timestamp())
g["genesisTimeslot"] = int(time.time() - epoch) // g["timeslotSecs"]
json.dump(g, open(sys.argv[1], "w"), indent=4)
PY

i=0
while [ "$i" -lt "$N" ]; do
  NODES+=("$i")
  dir="$RUN/n$i"; rm -rf "$dir"; mkdir -p "$dir"
  GOMAXPROCS=1 ./strawberry --name "mesh-$i" --validator-index "$i" \
    --validators-file "$VALFILE" --author-count "$N" --full-mesh \
    --genesis "$GENFILE" --rpc-port $((BASE_RPC + i)) --port $((BASE_PORT + i)) \
    --data-dir "$dir" > "$RUN/n$i.log" 2>&1 &
  PIDS+=("$!")
  i=$((i + 1))
done
echo "$N nodos en marcha, sin tocar ninguno. Mirando ${SEG}s."

# Se espera a que la cadena exista, y luego se mira si aguanta.
espero=0
while [ "$espero" -lt 60 ]; do
  [ -n "$(tip $((BASE_RPC)))" ] && break
  sleep 1; espero=$((espero + 1))
done

antes=""; coincide=0; deriva=""
i=0
while [ "$i" -lt "$SEG" ]; do
  ref=$(tip $((BASE_RPC)))
  if [ -n "$ref" ]; then
    ref_r=$(root $((BASE_RPC)))
    malos=""
    for j in "${NODES[@]}"; do
      if [ "$(tip $((BASE_RPC + j)))" != "$ref" ] || [ "$(root $((BASE_RPC + j)))" != "$ref_r" ]; then
        malos="$malos $j"
      fi
    done
    if [ -z "$malos" ]; then
      coincide=$((coincide + 1))
      deriva=""
    elif [ -z "$deriva" ]; then
      deriva="muestra $i: altura $(altura $((BASE_RPC))); se separan:$malos"
      echo "$deriva"
    fi
  fi
  sleep 1
  i=$((i + 1))
done

echo
echo "muestras de acuerdo: $coincide de $SEG"
if [ -n "$deriva" ]; then
  echo "PRIMERA DERIVA: $deriva"
  echo "alturas y bloques ahora:"
  for j in "${NODES[@]}"; do
    echo "  mesh-$j altura $(altura $((BASE_RPC + j)))  tip $(tip $((BASE_RPC + j)))"
    echo "           raiz $(root $((BASE_RPC + j)))"
  done
else
  echo "sin deriva: los $N nodos se mantuvieron de acuerdo"
fi
limpia
