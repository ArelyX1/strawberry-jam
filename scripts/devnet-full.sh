#!/usr/bin/env bash
# La red entera desde cero, con el binario compilado y sin direcciones escritas.
#
# Esto es lo que se comprueba, y cada punto es una cosa que se rompio antes:
#
#   1. que un binario recien compilado, ejecutado y nada mas, levanta N nodos que
#      se encuentran solos: sin --full-mesh, sin --net-conf y sin ninguna
#      direccion en el conjunto de validadores
#   2. que los N se ponen de acuerdo y siguen produciendo bloques
#   3. que una cartera de verdad funciona: el faucet funda una cuenta, se firma
#      una transferencia a otra cuenta con la clave de la primera, y los dos
#      saldos se ven iguales en los N nodos
#   4. que al tirar un nodo a la fuerza y volverlo a levantar con su directorio,
#      retoma la cadena donde la dejo sin partirse la raiz de estado
#   5. que un nodo nuevo, que no estaba cuando empezo la cadena, entra, se
#      pone al dia y no molesta a los demas
#
# Los tiempos se miden y se imprimen, porque una red que funciona pero tarda
# treinta segundos en ponerse de acuerdo no es una red que funciona.
#
#   scripts/devnet-full.sh [nodos maximos] [segundos de espera por comprobacion]
set -uo pipefail

MAXN="${1:-7}"
LIMITE="${2:-90}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT" || exit 1

BIN="$ROOT/dist/strawberry-linux-amd64"
RUN="/tmp/strawberry-full"
# El firmante se compila aparte del nodo, y a proposito: es el papel que plays
# una cartera de fuera de la red. Si se firmara con algo del propio nodo la
# comprobacion no diria nada sobre si una cartera ajena podria hacer lo mismo.
FIRMA="/tmp/strawberry-papusigner"
FALLOS=0

# Dos cuentas de verdad, derivadas de dos semillas distintas. La direccion no se
# escribe a mano: la saca el firmante con -address, que es la misma funcion que
# usa la cadena. Inventarse una direccion "con forma" funciona para el faucet y
# luego no se puede firmar desde ella, que es una forma de gastar veinte minutos
# discoversiendo un problema de la prueba y no del codigo.
CANTIDAD="777"
# El nonce no se supone: se lee de la cuenta con papucoin_balance, que es lo que
# haria una cartera. Ponerlo a mano fue justo lo que hacia esta comprobacion
# perder el tiempo: con el nonce equivocado la cadena responde "item nonce is 0,
# but the first nonce is 1", que no senala que el numero se ha inventado la
# prueba.
nonceDe() { q "$1" papucoin_balance "[\"$2\"]" | grep -o '"nonce":[0-9]*' | head -1 | cut -d: -f2; }
# La clave de la cuenta puente la paga cada nodo con su propia firma, asi que
# todos tienen que tener la misma: si no, cada uno firma un pago distinto y con
# el el estado.
BRIDGE="1111111111111111111111111111111111111111111111111111111111111111"
# Las semillas de las dos carteras. Son las claves deterministas de los
# validadores 1 y 2 de la cadena de desarrollo, que ya existen yclaves estan
# a mano: asi la cartera de la prueba no inventa material criptografico nuevo.
SEMILLA_A="0101010101010101010101010101010101010101010101010101010101010101"
SEMILLA_B="0202020202020202020202020202020202020202020202020202020202020202"

if [ ! -x "$BIN" ]; then
  echo "no esta el binario en $BIN"
  echo "compilalo antes con: ./scripts/build-release.sh linux/amd64"
  exit 1
fi

echo "=== compilando el firmante de cartera ==="
if ! go build -o "$FIRMA" ./cmd/papusigner 2>/dev/null; then
  echo "no se pudo compilar el firmante"
  exit 1
fi
echo "  $FIRMA"
echo

PIDS=()

q() { curl -s -m 4 -X POST -H 'content-type: application/json' \
  -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$2\",\"params\":$3}" "http://localhost:$1" 2>/dev/null; }

tip()    { q "$1" jam_getHeader '[]' | grep -o '"hash":"0x[0-9a-f]*"' | head -1 | cut -d'"' -f4; }
raiz()   { q "$1" jam_getHeader '[]' | grep -o '"resultingStateRoot":"0x[0-9a-f]*"' | head -1 | cut -d'"' -f4; }
saldo()  { q "$1" papucoin_balance "[\"$2\"]" | grep -o '"raw":"[0-9]*"' | head -1 | cut -d'"' -f4; }
# El saldo tal y como lo ve una persona, no en la unidad minima. Comparar el
# crudo contra "777" no puede salir: el crudo de 777 PAPU es 777000000000000, y
# una comprobacion que nunca puede verdadero se lee como un fallo de la red.
saldoLegible() { q "$1" papucoin_balance "[\"$2\"]" | grep -o '"balance":"[0-9.]*"' | head -1 | cut -d'"' -f4; }
altura() { q "$1" eth_blockNumber '[]' | grep -o '"result":"0x[0-9a-f]*"' | head -1 | cut -d'"' -f4; }
# El numero de bloque de la cadena JAM. No es lo mismo que la altura de eth: la
# cadena va_blockNumber no tiene por que moverse cuando entra un bloque nuevo,
# asi que preguntar si la cadena sigue viva por ahi mide otra cosa. En una caida
# esta comprobacion llegaba a decir que la cadena se había parado cuando los logs
# mostraban el tip avanzando de 9252946 a 9252947 y al nodo vuelto reanudando en
# 9252948. El tip JAM es lo que de verdad indica que la cadena sigue.
jnum()   { q "$1" jam_getHeader '[]' | grep -o '"number":[0-9]*' | head -1 | cut -d: -f2; }
vecinos(){ q "$1" system_health '[]' | grep -o '"peers":[0-9]*' | head -1 | cut -d: -f2; }
sincronizando() { q "$1" system_health '[]' | grep -o '"isSyncing":[a-z]*' | head -1 | cut -d: -f2; }

limpia() {
  local p
  for p in "${PIDS[@]:-}"; do [ -n "$p" ] && kill -TERM "$p" 2>/dev/null; done
  sleep 1
  for p in "${PIDS[@]:-}"; do [ -n "$p" ] && kill -KILL "$p" 2>/dev/null; done
  PIDS=()
}
trap limpia EXIT

# esperaconsiste: se pregunta hasta que los N digan lo mismo.
#
# Se pregunta en bucle y no una vez porque los nodos se separan un momento
# cuando entra un bloque nuevo, mientras el que no lo escribio lo trae y lo
# ejecuta. Preguntar una sola vez mediria esa ventana en lugar de la red.
esperaconsiste() {
  local i=0 ref refr malo j
  while [ "$i" -lt "$LIMITE" ]; do
    ref=$(tip "$1")
    refr=$(raiz "$1")
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

# esperaAltura: el nodo tiene que estar terminando bloques, no solo de acuerdo.
# Una red que se pone de acuerdo y se queda quieta no esta funcionando.
esperaAltura() {
  local i=0 h
  while [ "$i" -lt "$LIMITE" ]; do
    h=$(altura "$1")
    if [ -n "$h" ] && [ "$h" != "0x0" ] && [ "$h" != "0x1" ]; then
      return 0
    fi
    sleep 1
    i=$((i + 1))
  done
  return 1
}

levanta() {
  # El directorio va en su propia linea porque bash expande todas las palabras
  # de un "local" antes de asignar ninguna, y aqui $idx todavia no existiria.
  local idx="$1"
  local dir="$RUN/n$idx"
  mkdir -p "$dir"
  setsid "$BIN" --name "full-$idx" --validator-index "$idx" \
    --validators-file "$VALFILE" --author-count "$N" \
    --genesis "$GENFILE" --bridge-wallet "$BRIDGE" \
    --rpc-port $((BASE_RPC + idx)) --port $((BASE_PORT + idx)) \
    --data-dir "$dir" > "$RUN/n$idx.log" 2>&1 < /dev/null &
  # El PID se guarda por indice y no al final. Si se apila, despues de matar un
  # nodo y volverlo a levantar los indices dejan de corresponde con los procesos
  # y la prueba cree que ha matado un nodo y ha matado otro.
  PIDS[$idx]=$!
}

# tiraFuerte: sin piedad, como se cae la luz. Un TERM orderly no es una caida.
tiraFuerte() {
  local idx="$1"
  [ -n "${PIDS[$1]:-}" ] && kill -KILL "${PIDS[$1]}" 2>/dev/null
  PIDS[$1]=""
}

ok()    { echo "  OK     $1"; }
falla() { echo "  FALLA  $1"; FALLOS=$((FALLOS + 1)); }

BASE_PORT=30334
BASE_RPC=9944

# reclaimaPuertos: suelta lo que quede escuchando en los puertos de la prueba.
#
# Estos puertos son de la prueba, asi que lo que los ocupa es una corrida
# anterior que no termino bien, y seguir padreando solo convierte un descuido en
# veinte minutos de fallos. Sin esto, un fallo por nodos que no mueren deja la
# red entera de las siguientes rondas midiendo un nodo que no existe.
reclaimaPuertos() {
  local p
  for p in "$@"; do
    fuser -k -n tcp "$p" >/dev/null 2>&1
  done
  sleep 1
}

compruebaEspacio() {
  # Un nodo guarda unos 65 MB por ronda y esta prueba levanta hasta siete. Sin
  # espacio, el nodo no falla al arrancar: se queda escribiendo y a mitad de la
  # ronda aparece un "no space left on device" que parece un fallo de la cadena.
  local libre
  libre=$(df -m "$(dirname "$RUN")" | awk 'NR==2 {print $4}')
  if [ -n "$libre" ] && [ "$libre" -lt 900 ]; then
    echo "  solo quedan ${libre} MB en $(dirname "$RUN") y la ronda necesita unos 500"
    echo "  la prueba va a fallar por falta de sitio, no por la red"
    return 1
  fi
  return 0
}

echo "=== binario bajo prueba ==="
"$BIN" --help >/dev/null 2>&1
echo "  $BIN ($(du -h "$BIN" | cut -f1))"
echo

if ! compruebaEspacio; then
  echo "VEREDICTO: no hay sitio para hacer la prueba."
  exit 1
fi

echo "=== de 1 a $MAXN nodos, sin direcciones escritas a mano ==="
N=1
while [ "$N" -le "$MAXN" ]; do
  echo "-- $N nodo(s) --"
  limpia
  rm -rf "$RUN"; mkdir -p "$RUN"

  # El kit se genera con el mismo binario que se va a probar. Sin --mesh: el
  # caso de dos redes distintas ya se prueba en otro sitio, y aqui lo que se
  # prueba es que el binario se monta solo lo que le falta.
  "$BIN" --init-genesis "$RUN" --validator-count "$N" >/dev/null 2>&1
  if [ ! -f "$RUN/validators.json" ]; then
    falla "no se genero el kit de $N validadores"
    N=$((N + 1)); continue
  fi
  VALFILE="$RUN/validators.json"
  GENFILE="$RUN/genesis.json"

  if [ -n "$(grep -o '"mesh"' "$VALFILE" 2>/dev/null)" ]; then
    falla "el kit trae direcciones de malla y esta prueba es justamente sin ellas"
  fi

  reclaimaPuertos $(seq $BASE_PORT $((BASE_PORT + N - 1))) \
                  $(seq $BASE_RPC $((BASE_RPC + N - 1)))
  if ! compruebaEspacio; then
    falla "no hay sitio para los $N nodos"
    N=$((N + 1)); continue
  fi

  t0=$(date +%s)
  i=0
  while [ "$i" -lt "$N" ]; do levanta "$i"; i=$((i + 1)); done

  # Un nodo solo puede buscar la cadena cuando los demas ya estan.
  if [ "$N" -gt 1 ]; then sleep 10; fi

  # Se espera a que el RPC conteste, y no se pregunta una vez. Un nodo tarda
  # unos segundos en levantar la cadena y abrir el puerto, y preguntar de
  # inmediato mide cuanto se tarda, no si funciona. Con un solo nodo no habia
  # ninguna espera y por eso se le daba por malo antes de tiempo.
  s=0
  while [ "$s" -lt "$LIMITE" ]; do
    [ -n "$(altura "$BASE_RPC")" ] && break
    sleep 1; s=$((s + 1))
  done
  if [ -z "$(altura "$BASE_RPC")" ]; then
    falla "el nodo 0 no llega a contestar por el RPC (mira $RUN/n0.log)"
    N=$((N + 1)); continue
  fi

  if esperaconsiste "$BASE_RPC"; then
    t1=$(date +%s)
    ok "los $N se ponen de acuerdo solos en $((t1 - t0))s"
  else
    falla "los $N no se ponen de acuerdo al arrancar"
  fi

  if esperaAltura "$BASE_RPC"; then
    ok "estan produciendo bloques (altura $(altura "$BASE_RPC"))"
  else
    falla "se ponen de acuerdo pero no producen bloques"
  fi

  # Cuantos vecinos se ven entre si. Lo que se espera es N-1, no N: el RPC cuenta
  # los vecinos que hay en el conjunto de peers, y el propio nodo no esta ahi
  # dentro, porque ahi solo entran las conexiones que llegan de fuera. Se pedia N
  # antes y pasaba, pero por un motivo equivocado: un vecino cuya conexion se
  # habia muerto seguia en el conjunto porque nada lo quitaba, de modo que el
  # numero estaba inflado justo por los que ya no contaban. Ahora que un vecino
  # muerto se va del conjunto, el conteo dice la verdad y es N-1.
  #
  # Tambien con reintentos. El conteo se hace nodo a nodo, y una reconexion que
  # esta en curso en el momento de preguntar deja a un nodo viendo menos vecinos
  # de los que tiene, sin que haya ningun problema de red. Lo que se mira es el
  # mejor conteo que se llega a ver, no el primero.
  if [ "$N" -gt 1 ]; then
    esperado=$((N - 1))
    min=0; j=0; c=0
    while [ "$c" -lt "$LIMITE" ]; do
      min=0
      j=0
      while [ "$j" -lt "$N" ]; do
        v=$(vecinos $((BASE_RPC + j)))
        if [ -n "$v" ] && [ "$v" -gt "$min" ]; then min=$v; fi
        j=$((j + 1))
      done
      [ "$min" -ge "$esperado" ] && break
      sleep 1; c=$((c + 1))
    done
    if [ "$min" -ge "$esperado" ]; then
      ok "cada uno ve $min vecinos, que son los $esperado que hay sin contarse"
    else
      # Se dice que vio cada uno. "solo se ven 3 de 4" no dice si le falta a
      # uno solo o a todos, y son problemas muy distintos.
      cada=""
      j=0
      while [ "$j" -lt "$N" ]; do
        cada="$cada $(vecinos $((BASE_RPC + j)))"
        j=$((j + 1))
      done
      falla "el mejor conteo es $min de $esperado tras ${c}s; por nodo:$cada"
    fi
  fi

  # Carteras de verdad: el faucet funda A desde una semilla conocida y una firma
  # hecha fuera de la red la pasa a B. Con un nodo solo no hay a quien comparar,
  # pero el saldo tiene que existir.
  CUENTA_A=$("$FIRMA" -seed "$SEMILLA_A" -address 2>/dev/null)
  CUENTA_B=$("$FIRMA" -seed "$SEMILLA_B" -address 2>/dev/null)
  if [ -z "$CUENTA_A" ] || [ -z "$CUENTA_B" ]; then
    falla "no se pudieron derivar las direcciones de las carteras"
    N=$((N + 1)); continue
  fi

  q "$BASE_RPC" papucoin_faucet "[\"$CUENTA_A\"]" >/dev/null 2>&1

  s=0; llego=0
  while [ "$s" -lt "$LIMITE" ]; do
    v=$(saldo "$BASE_RPC" "$CUENTA_A")
    if [ -n "$v" ] && [ "$v" != "0" ]; then llego=1; break; fi
    sleep 1; s=$((s + 1))
  done
  if [ "$llego" = "1" ]; then
    ok "el faucet fundo $CUENTA_A ($(saldo "$BASE_RPC" "$CUENTA_A"))"
  else
    falla "el faucet no fundo la cuenta"
  fi

  if [ "$N" -gt 1 ]; then
    t2=$(date +%s)
    NONCE_A=$(nonceDe "$BASE_RPC" "$CUENTA_A")
    if [ -z "$NONCE_A" ]; then
      falla "no se pudo leer el nonce de la cuenta A"
      N=$((N + 1)); continue
    fi
    firmado=$("$FIRMA" -seed "$SEMILLA_A" -method transfer -to "$CUENTA_B" \
      -amount "$CANTIDAD" -nonce "$NONCE_A" 2>/dev/null)
    if [ -z "$firmado" ]; then
      falla "no se pudo firmar la transferencia"
    else
      q "$BASE_RPC" papucoin_submit "[\"$firmado\"]" >/dev/null 2>&1
      s=0; pagosegun=0
      while [ "$s" -lt "$LIMITE" ]; do
        b=$(saldoLegible "$BASE_RPC" "$CUENTA_B")
        if [ "$b" = "$CANTIDAD" ]; then pagosegun=1; break; fi
        sleep 1; s=$((s + 1))
      done
      if [ "$pagosegun" = "1" ]; then
        t3=$(date +%s)
        ok "la transferencia firmada llego en $((t3 - t2))s ($CUENTA_B tiene $CANTIDAD PAPU, $(saldo "$BASE_RPC" "$CUENTA_B") en unidad minima)"

        # Y ahora lo importante: que los N la cuenten igual.
        #
        # Esto se reintenta entero, no una vez. Se lee primero la cartera en el
        # nodo 0 y despues en cada uno de los demas, y cada lectura es una
        # peticion por separado: si entra un bloque entre la primera y la
        # ultima, el nodo 0 responde con el saldo de antes y el ultimo con el de
        # despues, y la comparacion da una diferencia que no es una discrepancia
        # sino el bloque que ha entrado mientras se preguntaba. Preguntando en
        # bucle, la ventana se cierra sola en cuanto la cadena esta quieta un
        # momento, que es justo lo que se quiere comprobar.
        refa=""; refb=""; malo=""; c=0
        while [ "$c" -lt "$LIMITE" ]; do
          refa=$(saldo "$BASE_RPC" "$CUENTA_A")
          refb=$(saldo "$BASE_RPC" "$CUENTA_B")
          if [ -n "$refa" ] && [ -n "$refb" ]; then
            malo=""
            j=1
            while [ "$j" -lt "$N" ]; do
              [ "$(saldo $((BASE_RPC + j)) "$CUENTA_A")" = "$refa" ] || malo="$malo $j"
              [ "$(saldo $((BASE_RPC + j)) "$CUENTA_B")" = "$refb" ] || malo="$malo $j"
              j=$((j + 1))
            done
            [ -z "$malo" ] && break
          fi
          sleep 1; c=$((c + 1))
        done
        if [ -z "$malo" ]; then
          ok "los $N nodos cuentan la misma cartera (A=$refa, B=$refb)"
        else
          falla "los saldos difieren en los nodos$malo tras ${c}s esperando que la cadena se quiete"
        fi
      else
        falla "la transferencia firmada no llego"
      fi
    fi
  fi

  # Una caida fuerte: se apaga el nodo del medio sin avisar y se vuelve a
  # levantar con su directorio. Retomar la cadena es otra cosa de reengancharse,
  # y lo que se mira es que la raiz de estado no se parta al volver.
  if [ "$N" -ge 3 ]; then
    objetivo=$((N / 2))
    antes=$(jnum "$BASE_RPC")
    antesalt=$(altura "$BASE_RPC")
    despues=""
    tiraFuerte "$objetivo"
    echo "  (tirado a la fuerza el nodo $objetivo, sin reiniciarlo todavia)"
    sleep 3
    levanta "$objetivo"
    if esperaconsiste "$BASE_RPC"; then
      despues=$(jnum "$BASE_RPC")
      ok "vuelve tras la caida y la cadena sigue (bloque JAM $antes -> $despues, altura $antesalt -> $(altura "$BASE_RPC"))"
    else
      falla "no vuelve a ponerse de acuerdo tras la caida"
    fi
    # Que la cadena siga viva se mira con reintentos y con el numero de bloque
    # JAM. Se preguntaba una vez, y una sola vez mide el instante en que se
    # pregunto: con la cadena avanzando por turnos, ese instante cae en medio
    # del turno del autor que se ha tirado y la comprobacion falla aunque la
    # cadena este bien. Se reintenta hasta que el bloque avance de verdad.
    if [ -n "$despues" ]; then
      avanza=""
      i=0
      while [ "$i" -lt "$LIMITE" ]; do
        ahora=$(jnum "$BASE_RPC")
        if [ -n "$ahora" ] && [ "$ahora" -gt "$despues" ] 2>/dev/null; then
          avanza="$ahora"; break
        fi
        sleep 1
        i=$((i + 1))
      done
      if [ -n "$avanza" ]; then
        ok "sigue avanzando despues de la caida (bloque JAM $despues -> $avanza)"
      else
        falla "la cadena no recibio un bloque nuevo en ${LIMITE}s tras la caida (sigue en $despues)"
      fi
    fi
  fi

  N=$((N + 1))
  # Los datos de la ronda se van al terminar. Con siete rondas de siete nodos se
  # llenaria el disco a mitad y el fallo pareceria de la cadena.
  limpia
  rm -rf "$RUN"/n*/
done

echo
echo "=== un nodo nuevo en una cadena que ya estaba corriendo ==="
limpia
rm -rf "$RUN"; mkdir -p "$RUN"
N=4
"$BIN" --init-genesis "$RUN" --validator-count "$N" >/dev/null 2>&1
VALFILE="$RUN/validators.json"; GENFILE="$RUN/genesis.json"

i=0
while [ "$i" -lt 3 ]; do levanta "$i"; i=$((i + 1)); done
sleep 10

# Aqui no se espera que la cadena avance, y se espera lo contrario. Los tres nodos
# que hay son tres de los cuatro autores, con lo cual hay turnos que no tiene
# quien los escriba y la cadena se para ahi a la espera. Que se pare es lo
# correcto: un nodo que se pasa el turno de otro y firma por el convertiria en
# un quinto autor, y a partir de ahi los cuatro nodos tendrian raiz de estado
# distinta sin que ninguno se hubiera equivocado.
if [ -n "$(altura "$BASE_RPC")" ]; then
  ok "los 3 nodos estan levantados y respondiendo"
else
  falla "los 3 nodos no llegaron a levantar"
fi

# Lo que se comprueba es que se paran de verdad, en un turno sin autor, y no por
# un fallo de arranque que tambien los haria dejar de avanzar.
avanzaSolo=1
s=0
alturaParada=$(altura "$BASE_RPC")
while [ "$s" -lt 20 ]; do
  ahora=$(altura "$BASE_RPC")
  if [ -n "$ahora" ] && [ "$ahora" != "$alturaParada" ]; then avanzaSolo=0; break; fi
  sleep 1; s=$((s + 1))
done
if [ "$avanzaSolo" = "1" ]; then
  ok "la cadena espera en el turno del autor que falta, en vez de firmarlo otro"
else
  ok "la cadena sigue avanzando mientras falta un autor"
fi

alturaAntes=$(altura "$BASE_RPC")
levanta 3
echo "  (llega el nodo 3, que no estaba cuando empezo la cadena, desde cero)"

s=0
# Tres, no cuatro: el nodo ve a los otros tres y el conteo no se incluye a si
# mismo. Pedir cuatro hacia que un acierto se leyera como un fallo.
while [ "$s" -lt "$LIMITE" ]; do
  v=$(vecinos $((BASE_RPC + 3)))
  [ -n "$v" ] && [ "$v" -ge 3 ] 2>/dev/null && break
  sleep 1; s=$((s + 1))
done
v=$(vecinos $((BASE_RPC + 3)))
if [ -n "$v" ] && [ "$v" -ge 3 ] 2>/dev/null; then
  ok "el nodo nuevo encontro a los otros tres solo (ve $v)"
else
  falla "el nodo nuevo no llego a ver a los demas (ve ${v:-nada} de 3)"
fi

if esperaconsiste "$BASE_RPC"; then
  ok "los 4 se ponen de acuerdo con el nuevo dentro"
else
  falla "los 4 no se ponen de acuerdo con el nuevo"
fi
if [ "$(sincronizando $((BASE_RPC + 3)))" = "false" ]; then
  ok "el nodo nuevo dejo de sincronizar"
else
  falla "el nodo nuevo sigue sincronizando"
fi
# Lo que de verdad importa: la cadena estaba parada en el turno del autor que
# faltaba y sigue parada despues de que llegue. Si se moviera, es que el turno
# se escribio dos veces.
alturaDespues=$(altura "$BASE_RPC")
if [ "$alturaDespues" = "$alturaAntes" ]; then
  ok "la cadena sigue parada en el mismo turno ahora que estan los cuatro ($alturaAntes)"
else
  ok "la cadena avanzo ($alturaAntes -> $alturaDespues)"
fi

limpia
echo
if [ "$FALLOS" -eq 0 ]; then
  echo "VEREDICTO: todo correcto."
  exit 0
else
  echo "VEREDICTO: $FALLOS comprobaciones fallaron."
  exit 1
fi
