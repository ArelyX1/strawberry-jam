#!/usr/bin/env bash
# Prepara una red repartida en varias maquinas.
#
#   scripts/devnet-prepare.sh 10.0.0.1 10.0.0.2 10.0.0.3 10.0.0.4
#
# Escribe los tres ficheros que tienen que ser IGUALES en todas las maquinas, y
# dice que arrancar en cada una. Los tres se copian tal cual; no se regeneran donde
# se levanta cada nodo.
#
#   validators.json   una clave y una direccion por validador
#   genesis.json      el punto de partida de la cadena
#   nodos.txt         el indice, la direccion y la orden de arranque de cada uno
#
# Por que el genesis se escribe una vez y se reparte: dice en que timeslot empieza
# la cadena, y sale del reloj de quien lo escribe. Si cada maquina lo calcula por su
# cuenta y los relojes no coinciden al segundo, cada una funda una cadena distinta
# y los nodos no se ven nunca. No sale un error, sale una cadena vacia, y es de las
# cosas que mas cuesta encontrar. Leyendolo del reloj una sola vez y repartiendo el
# fichero, el problema no puede ocurrir.
#
# Las claves tambien se reparten. Un validador es quien es por su clave, no por su
# sitio: un nodo que se cambia de maquina sigue siendo el mismo nodo.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

OUT="multi"
BASE_PORT="${BASE_PORT:-30333}"
DIR="${1:-}"

if [ -z "$DIR" ]; then
  echo "uso: scripts/devnet-prepare.sh <salida> [direccion...]" >&2
  echo "  ejemplo: scripts/devnet-prepare.sh multi 10.0.0.1 10.0.0.2 10.0.0.3" >&2
  exit 1
fi
shift

NODES=("$@")
CUENTOS="${#NODES[@]}"
if [ "$CUENTOS" -lt 1 ]; then
  echo "devnet-prepare: hay que decir al menos una direccion" >&2
  exit 1
fi

mkdir -p "$DIR"

# Las direcciones van todas con el mismo puerto: una maquina, un nodo, un puerto.
ADDRS=""
for n in "${NODES[@]}"; do
  if [ -n "$ADDRS" ]; then ADDRS="$ADDRS,"; fi
  ADDRS="$ADDRS$n:$BASE_PORT"
done

echo "preparando $CUENTOS nodo(s) en $DIR"
GOMAXPROCS=1 go run ./scripts/gen-validators -out "$DIR/validators.json" -count "$CUENTOS" -addrs "$ADDRS"

# El genesis, leido del reloj una sola vez.
python3 - "$DIR/genesis.json" <<'PY'
import datetime, json, sys, time
g = json.load(open("genesis/chain-dev.json"))
epoch = int(datetime.datetime(2025,1,1,12,0,0,tzinfo=datetime.timezone.utc).timestamp())
g["genesisTimeslot"] = int(time.time() - epoch) // g["timeslotSecs"]
json.dump(g, open(sys.argv[1], "w"), indent=4)
print("genesis en el timeslot", g["genesisTimeslot"])
PY

# La orden de arranque de cada maquina.
BIN="$(pwd)/strawberry"
[ -x "$BIN" ] || { echo "devnet-prepare: falta $BIN; compilalo antes (go build -o strawberry ./cmd/strawberry/)" >&2; exit 1; }

: > "$DIR/nodos.txt"
i=0
for n in "${NODES[@]}"; do
  rpc=$((19944 + i))
  echo "nodo $i en $n  (puerto $BASE_PORT, rpc $rpc)" >> "$DIR/nodos.txt"
  echo "  $BIN --name mesh-$i --validator-index $i \\" >> "$DIR/nodos.txt"
  echo "    --validators-file validators.json --author-count $CUENTOS --full-mesh \\" >> "$DIR/nodos.txt"
  echo "    --genesis genesis.json --rpc-port $rpc --port $BASE_PORT \\" >> "$DIR/nodos.txt"
  echo "    --data-dir datos-$i" >> "$DIR/nodos.txt"
  echo "" >> "$DIR/nodos.txt"
  i=$((i + 1))
done

cat <<FIN

Listo. Copia $DIR/validators.json, $DIR/genesis.json y el binario a TODAS las maquinas,
y arranca cada nodo con su linea de $DIR/nodos.txt.

Lo que tiene que ser igual en todas, y no volver a generar:
  - genesis.json   el timeslot de partida sale del reloj de quien lo escribio
  - validators.json  las claves: un validador es su clave, no su sitio

Lo que cambia en cada maquina:
  - el indice del validador (--validator-index), que es el nodo que es
  - el puerto y el rpc, que solo pueden coincidir dentro de una misma maquina

Se puede levantar en cualquier orden: un nodo que llega tarde busca la cadena por
la malla cuando aparece, no antes.
FIN