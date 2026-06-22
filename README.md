# Strawberry JAM — Dev Node (Fork con Debug & Tooling)

Fork de [eigerco/strawberry](https://github.com/eigerco/strawberry) con modificaciones para ejecutar un nodo JAM en modo desarrollo con producción de bloques, RPC WebSocket/HTTP, telemetry, y más.

## Cambios realizados

### 1. Corrección de JSON tags en `test_validators.json`

Los tags `json` de `FullValidatorInfo` en `cmd/strawberry/main.go` se corrigieron para coincidir con el archivo `test_validators.json`:
- `"address"` → `"ip"`
- `"ed25519_pub"`, `"ed25519_private"` para las claves ed25519

### 2. Manejo de prefijo `0x` en claves

Se agregó `decodeHex()` en `cmd/strawberry/main.go` que remueve el prefijo `"0x"` con `strings.TrimPrefix` antes de decodificar hex. Las claves en `test_validators.json` usan prefijo `0x`.

### 3. Seed de 32 bytes → PrivateKey de 64 bytes

Las claves `ed25519_private` en el JSON son seeds de 32 bytes. Se usa `ed25519.NewKeyFromSeed(seed)` para derivar la private key completa de 64 bytes requerida por Go.

### 4. Flags de línea de comandos

Se agregaron flags CLI en `cmd/strawberry/main.go`:
- `--name` — nombre del nodo (default: `Strawberry-Node`)
- `--port` — puerto P2P (default: del validador)
- `--rpc-port` — puerto RPC WebSocket/HTTP (default: `9944`)
- `--telemetry-url` — URL de telemetry WebSocket
- `--chain` — especificación de chain (default: `dev`)
- `--validator` — modo validador (flag aceptado pero no-op)
- `--help` — muestra ayuda

### 5. Archivo de constantes dev (`chain_dev.go`)

Se creó `internal/constants/chain_dev.go` con build tag `dev`:
- `NumberOfValidators = 2`
- `TimeslotsPerEpoch = 12`
- `EpochDuration = TimeslotDuration * TimeslotsPerEpoch`

Los build constraints de `chain.go` y `chain_tiny.go` se actualizaron para excluir el tag `dev`.

### 6. Block Producer (`cmd/strawberry/producer.go`)

Archivo nuevo que implementa producción autónoma de bloques:
- Genera headers JAM con `ParentHash`, `PriorStateRoot`, `ExtrinsicHash`, `TimeSlotIndex`, `BlockAuthorIndex`
- Agrega `EpochMarker` al primer timeslot de cada epoch
- Catch-up desde genesis hasta el slot actual (~10ms delay entre bloques)
- Luego produce un bloque cada `jamtime.TimeslotDuration` (6s)
- Almacena header + block via `store.PutBlock`, luego notifica via `HandleNewHeader`
- Callback a RPC server para broadcast de suscripciones

### 7. RPC Server WebSocket/HTTP (`cmd/strawberry/rpc.go`)

Archivo nuevo que implementa un servidor JSON-RPC compatible con Substrate:
- **WebSocket** (con `golang.org/x/net/websocket`) para Polkadot.js
- **HTTP POST** para curl/scripts
- Origen WebSocket permitido (handshake custom que acepta todos los orígenes)
- Suscripciones `chain_subscribeNewHeads`, `chain_subscribeFinalizedHeads`
- **Métodos implementados:**
  - `system_chain`, `system_name`, `system_version`, `system_health`, `system_peers`, `system_properties`, `system_chainType`, `system_localListenAddresses`, `system_syncState`, `system_accountNextIndex`
  - `chain_getHeader`, `chain_getBlock`, `chain_getBlockHash`, `chain_getFinalizedHead`
  - `chain_subscribeNewHeads`, `chain_subscribeFinalizedHeads`, `chain_unsubscribeNewHeads`, `chain_unsubscribeFinalizedHeads`
  - `state_getRuntimeVersion`, `chain_getRuntimeVersion`, `state_getMetadata`
  - `state_subscribeRuntimeVersion`, `state_subscribeMetadata`, `state_unsubscribeRuntimeVersion`, `state_unsubscribeMetadata`
  - `rpc_methods`
- Headers JAM convertidos a formato Substrate (hex `parentHash`, `number`, `stateRoot`, `extrinsicsRoot`, `digest`) para que exploradores puedan renderizarlos

### 8. Telemetry Client (`cmd/strawberry/telemetry.go`)

Archivo nuevo que conecta a telemetry de Polkadot vía WebSocket:
- Envía handshake con `system_chain`, `system_name`, `system_version`
- Si se pasa `--telemetry-url`, conecta y reporta estadísticas del nodo (bloques, peers, etc.)

### 9. Getter `GetLatestFinalized()` en BlockService

Agregado en `internal/chain/service.go` para que el RPC pueda consultar el último bloque finalizado.

### 10. Corrección de bug JAM codec: Ed25519 nil → 32 bytes padding

**Archivo:** `pkg/serialization/codec/jam/encode.go`

**Problema:** `encodeEd25519PublicKey` llamaba `bw.Write(in)` que escribe **0 bytes** cuando `in` es `nil`. En `ValidatorKeys.Ed25519` (campo `[]byte`), el zero value es `nil`. Esto ocurría en los `EpochMarker.Keys` en bloques de límite de epoch.

El custom `UnmarshalJAM` de `ValidatorKeys` siempre lee **32 bytes**, causando un desajuste de 64 bytes (2 validadores × 32 bytes) en el stream JAM. Esto desplazaba todos los campos siguientes del header, haciendo que `BlockSealSignature` (campo `[96]byte` al final) fallara con `unexpected EOF`.

**Solución:** padding a 32 bytes antes de escribir:
```go
buf := make([]byte, ed25519.PublicKeySize)
copy(buf, in)
_, err := bw.Write(buf)
```

### 11. Debug logging mejorado

- Log del slot y epoch en cada bloque producido
- Mensaje de warning de `HandleNewHeader` incluye hash y slot del bloque problemático
- Mensaje de `IsDescendantOfFinalized` incluye slot y hash del bloque padre fallido

## Cómo usar

### Requisitos
- Go 1.25.5+

### Construir y ejecutar (modo dev)
```bash
go build -tags dev ./cmd/strawberry/
./strawberry --name "MiNodo" --rpc-port 9944 --telemetry-url "wss://telemetry.polkadot.io/submit/ 0"
```

Esto inicia un nodo que:
1. Escucha en puerto UDP (P2P, default del validador)
2. Sirve RPC en `ws://[::1]:9944` y `http://[::1]:9944`
3. Produce bloques JAM desde genesis hasta el slot actual (~100ms de catch-up)
4. Reporta a telemetry de Polkadot

### Flags disponibles
```
--config          archivo de configuración (default: appconfig.json)
--chain           chain spec (default: dev)
--validator       modo validador
--name            nombre del nodo (default: Strawberry-Node)
--telemetry-url   URL de telemetry WebSocket
--port            puerto P2P (default: del validador)
--rpc-port        puerto RPC (default: 9944)
--help            muestra ayuda
```

### Probar RPC
```bash
# Información del chain
curl -X POST http://localhost:9944 -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"system_chain","params":[]}'

# Último header
curl -X POST http://localhost:9944 -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"chain_getHeader","params":[]}'

# Métodos disponibles
curl -X POST http://localhost:9944 -H "Content-Type: application/json" \
  -d '{"jsonrpc":"2.0","id":1,"method":"rpc_methods","params":[]}'
```

## Archivos relevantes

| Archivo | Descripción |
|---------|-------------|
| `cmd/strawberry/main.go` | Entry point con flags, carga de validadores, inicialización |
| `cmd/strawberry/producer.go` | Block producer (nuevo) |
| `cmd/strawberry/rpc.go` | Servidor RPC WS/HTTP (nuevo) |
| `cmd/strawberry/telemetry.go` | Cliente telemetry (nuevo) |
| `internal/constants/chain_dev.go` | Constantes dev (nuevo) |
| `internal/constants/chain.go` | Build constraint actualizado |
| `internal/constants/chain_tiny.go` | Build constraint actualizado |
| `internal/chain/service.go` | GetLatestFinalized + debug logging |
| `pkg/serialization/codec/jam/encode.go` | Fix nil Ed25519 padding |
| `test_validators.json` | Validadores Alice y Bob (nuevo) |
| `appconfig.json` | Config con validatorIndex |

## Créditos

Fork de [eigerco/strawberry](https://github.com/eigerco/strawberry), un cliente JAM en Go para Polkadot.
