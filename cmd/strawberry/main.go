package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/constants"
	"github.com/eigerco/strawberry/internal/crypto"
	"github.com/eigerco/strawberry/internal/crypto/ed25519"
	"github.com/eigerco/strawberry/internal/safrole"
	chainState "github.com/eigerco/strawberry/internal/state"
	"github.com/eigerco/strawberry/internal/validator"
	"github.com/eigerco/strawberry/pkg/db/pebble"
	"github.com/eigerco/strawberry/pkg/devnet"
	"github.com/eigerco/strawberry/pkg/log"
	"github.com/eigerco/strawberry/pkg/network/node"
)

var version = "0.1.0-dev"

type FullValidatorInfo struct {
	Index      uint   `json:"index"`
	Name       string `json:"name"`
	Seed       string `json:"seed"`
	IP         string `json:"ip"`
	Port       int    `json:"port"`
	Ed25519Pub string `json:"ed25519_pub"`
	Ed25519Prv string `json:"ed25519_private"`
}

type AppConfig struct {
	LogLevel       string `json:"loglevel"`
	ValidatorIndex int    `json:"validatorIndex"`
}

func decodeHex(s string) ([]byte, error) {
	return hex.DecodeString(strings.TrimPrefix(s, "0x"))
}

func (f FullValidatorInfo) ToJson() ([]byte, error) {
	pub, prv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, err
	}
	prvStr := hex.EncodeToString(prv)
	pubStr := hex.EncodeToString(pub)
	f.Ed25519Prv = prvStr
	f.Ed25519Pub = pubStr
	return json.MarshalIndent(f, "", "	")
}

func loadConfig(filename string) (*AppConfig, error) {
	appConfig := AppConfig{}

	f, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("config file open failed: %v", err)
	}

	if err := json.NewDecoder(f).Decode(&appConfig); err != nil {
		return nil, fmt.Errorf("umarshalling application config failed: %v", err)
	}

	return &appConfig, nil
}

func loadFullValidatorInfos(filename string) ([]FullValidatorInfo, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("error reading file: %w", err)
	}

	var validators []FullValidatorInfo
	if err := json.NewDecoder(f).Decode(&validators); err != nil {
		return nil, fmt.Errorf("error unmarshaling JSON: %w", err)
	}

	return validators, nil
}

func encodeToBytes(addr *net.UDPAddr) ([]byte, error) {
	if len(addr.IP) != net.IPv6len {
		return nil, fmt.Errorf("not an IPv6 address")
	}

	result := make([]byte, 18)
	copy(result[:16], addr.IP)
	binary.LittleEndian.PutUint16(result[16:], uint16(addr.Port))
	return result, nil
}

func (f FullValidatorInfo) ToMetadata() ([]byte, error) {
	addr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(f.IP, strconv.Itoa(f.Port)))
	if err != nil {
		return nil, err
	}
	addrBytes, err := encodeToBytes(addr)
	if err != nil {
		return nil, err
	}

	result := make([]byte, 128)
	copy(result, addrBytes)

	return result, nil
}

func main() {
	var (
		configFile   string
		chainSpec    string
		isValidator  bool
		nodeName     string
		telemetryURL string
		portOverride int
		rpcPort      int
		help         bool
		genesisPath  string
		dataDir      string
		bridgeWallet string
	)

	flag.StringVar(&configFile, "config", "appconfig.json", "path to config file")
	flag.StringVar(&chainSpec, "chain", "dev", "chain specification")
	flag.BoolVar(&isValidator, "validator", false, "run as validator")
	flag.StringVar(&nodeName, "name", "Strawberry-Node", "node name")
	flag.StringVar(&telemetryURL, "telemetry-url", "", "telemetry WebSocket URL")
	flag.IntVar(&portOverride, "port", 0, "override p2p listen port")
	flag.IntVar(&rpcPort, "rpc-port", 9944, "RPC WebSocket and HTTP port")
	flag.StringVar(&genesisPath, "genesis", "genesis/chain-dev.json", "path to the genesis of the PAPU economy")
	flag.StringVar(&dataDir, "data-dir", "", "directory to keep blocks and state in; empty keeps them in memory")
	flag.StringVar(&bridgeWallet, "bridge-wallet", "", "hex Ed25519 seed of the account the node pays faucets from")
	flag.BoolVar(&help, "help", false, "show help")
	flag.Parse()

	if help {
		flag.Usage()
		return
	}

	ctx := context.Background()

	appConfig, err := loadConfig(configFile)
	if err != nil {
		panic("application config load failed:" + err.Error())
	}

	loglevel, err := log.ParseLogLevel(appConfig.LogLevel)
	if err != nil {
		panic("log level parsing failed: " + err.Error())
	}

	opts := log.Options{LogLevel: loglevel}
	log.Init(opts)

	maxuint16 := int(^uint16(0))
	if appConfig.ValidatorIndex < 0 && appConfig.ValidatorIndex > maxuint16 {
		log.Internal.Fatal().
			Msgf("validator index %d out of bounds 0-%d", appConfig.ValidatorIndex, maxuint16)
	}

	index := uint16(appConfig.ValidatorIndex)

	vs, err := loadFullValidatorInfos("test_validators.json")
	if err != nil {
		log.Internal.Fatal().
			Err(err).
			Msg("loading validator configuration failed")
	}

	if int(index) >= len(vs) {
		log.Internal.Fatal().
			Uint16("index", index).
			Msg("validator configuration index out of bounds")
	}
	address := vs[index].IP
	port := vs[index].Port
	if portOverride > 0 {
		port = portOverride
	}
	udpAddress, err := net.ResolveUDPAddr("udp", net.JoinHostPort(address, strconv.Itoa(port)))
	if err != nil {
		log.Internal.Fatal().
			Str("address", address).
			Int("port", port).
			Err(err).
			Msg("address resolve failed")
	}

	log.Internal.Info().
		Msgf("listening on: %v", address)

	seed, err := decodeHex(vs[index].Ed25519Prv)
	if err != nil {
		log.Internal.Fatal().
			Err(err).
			Msg("own private key decode failed")
	}
	pub, err := decodeHex(vs[index].Ed25519Pub)
	if err != nil {
		log.Internal.Fatal().
			Err(err).
			Msg("own public key decode failed")
	}

	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := ed25519.PublicKey(pub)
	vkeys := validator.ValidatorKeys{
		EdPrv: privateKey,
		EdPub: publicKey,
	}
	validatorsData := safrole.ValidatorsData{}

	for i, k := range vs {
		pub, err := decodeHex(k.Ed25519Pub)
		if err != nil {
			log.Internal.Fatal().
				Int("index", i).
				Err(err).
				Msg("validator public key decode failed")
		}

		meta, err := k.ToMetadata()
		if err != nil {
			log.Internal.Fatal().
				Int("index", i).
				Err(err).
				Msg("validator metadata decode failed")
		}

		vk := crypto.ValidatorKey{
			Ed25519:  ed25519.PublicKey(pub),
			Metadata: crypto.MetadataKey(meta),
		}
		validatorsData[i] = vk
	}

	vstate := validator.ValidatorState{
		CurrentValidators:  validatorsData,
		ArchivedValidators: validatorsData,
		QueuedValidators:   validatorsData,
	}

	state := chainState.State{
		ValidatorState: vstate,
	}

	// The runtime owns the state of the chain: the services that exist, the PAPU
	// balances and the state root. The node is handed a copy of the state it has
	// at startup, and the runtime is what advances it from block to block.
	genesis, err := devnet.LoadGenesis(genesisPath)
	if err != nil {
		log.Internal.Fatal().Str("genesis", genesisPath).Err(err).Msg("genesis load failed")
	}

	devValidators, err := devnet.DevValidatorKeys(constants.NumberOfValidators, validatorListenAddrs(vs))
	if err != nil {
		log.Internal.Fatal().Err(err).Msg("validator state build failed")
	}

	kvStore, err := openStore(dataDir)
	if err != nil {
		log.Internal.Fatal().Str("dataDir", dataDir).Err(err).Msg("database open failed")
	}

	runtime, err := devnet.New(devnet.Options{
		Genesis:    genesis,
		Validators: devValidators,
		TrieDB:     kvStore,
		BridgeKey:  bridgeKey(bridgeWallet),
		Logf: func(format string, args ...any) {
			log.Internal.Info().Msgf(format, args...)
		},
	})
	if err != nil {
		log.Internal.Fatal().Err(err).Msg("runtime build failed")
	}

	n, err := node.NewNodeWithStore(ctx, udpAddress, vkeys, state, index, kvStore)
	if err != nil {
		log.Internal.Fatal().
			Err(err).
			Msg("node creation failed")
	}
	err = n.Start()
	if err != nil {
		log.Internal.Fatal().
			Err(err).
			Msg("node start failed")
	}

	// Conectar con los validadores vecinos. Hasta ahora no se llamaba nunca, de
	// modo que el nodo escuchaba y nunca ": conectaba con nadie, y por eso dos
	// nodos en la misma maquina eran dos cadenas calculadas por separado en vez
	// de una red. Se hace en segundo plano y con reintentos porque un vecino
	// puede no estar arrancado todavia, y eso no puede impedir que este nodo
	// sirva su propia cadena.
	go connectToNeighbours(ctx, n)

	chainName := fmt.Sprintf("Strawberry %s", chainSpec)

	// Start RPC server (before block producer so it's ready for subscriptions)
	rpcAddr := fmt.Sprintf(":%d", rpcPort)
	rpcSrv := startRPCServer(rpcAddr, nodeName, chainName, version,
		n.BlockService.Store, n.BlockService, n, udpAddress.String())
	rpcSrv.papucoin = newPapucoinHandlers(runtime, rpcSrv)

	// Start block producer
	startBlockProducer(n.BlockService, runtime, index,
		func(hash crypto.Hash, num uint, h block.Header) {
			rpcSrv.updateBlock(hash, num, h)
		}, rpcSrv.markRebuilt, n, ctx)

	// Connect telemetry
	startTelemetry(telemetryURL, nodeName, version, chainName)

	select {}
}

// openStore opens the database the node keeps blocks, headers and state trie
// nodes in. An empty directory keeps them in memory, which is what a throwaway
// run wants.
func openStore(dataDir string) (*pebble.KVStore, error) {
	if dataDir != "" {
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			return nil, err
		}
	}
	return pebble.NewKVStoreAt(dataDir)
}

// bridgeKey reads the seed of the account the node pays faucets from. An empty
// path leaves the node without one, so it cannot mint, which is the safe default
// for a node nobody gave a key to.
func bridgeKey(hexSeed string) ed25519.PrivateKey {
	if hexSeed == "" {
		return nil
	}
	seed, err := hex.DecodeString(strings.TrimPrefix(hexSeed, "0x"))
	if err != nil || len(seed) != ed25519.SeedSize {
		log.Internal.Warn().Str("bridgeWallet", hexSeed).Msg("the bridge wallet is not a 32 byte hex seed, so the node cannot pay faucets")
		return nil
	}
	return ed25519.NewKeyFromSeed(seed)
}

// validatorListenAddrs renders the validator configuration as addresses, so the
// state names where each validator can be reached.
func validatorListenAddrs(vs []FullValidatorInfo) []string {
	addrs := make([]string, 0, len(vs))
	for _, v := range vs {
		addrs = append(addrs, net.JoinHostPort(v.IP, strconv.Itoa(v.Port)))
	}
	return addrs
}

// connectToNeighbours keeps trying to reach the neighbouring validators.
//
// The first attempt waits: in a local dev setup the other node is usually still
// starting when this one is ready, and a single attempt would leave the two of
// them talking to nobody. The backoff stops growing so a peer that comes up
// late is still picked up, at the cost of a periodic attempt that mostly does
// nothing, which is fine for two nodes and for a dev machine.
func connectToNeighbours(ctx context.Context, n *node.Node) {
	delay := 2 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Solo se marca si no hay ninguno. ConnectToPeer rechaza cuando ya
		// existe, pero compara por direccion y el par se guarda con el puerto
		// efimero de la conexion, no con el que se marco: asi que nunca
		// coincidia, el bucle reconectaba cada 30s y cada reconexion cerraba el
		// par anterior con su anunciador, dejando a todos los bloques
		// siguientes con "context canceled" al anunciarse.
		if peers := n.GetAllPeers(); len(peers) > 0 {
			log.Internal.Info().Int("peers", len(peers)).Msg("connected to neighbour validators")
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
			continue
		}

		if err := n.ConnectToNeighbours(); err != nil {
			log.Internal.Debug().Err(err).Msg("no neighbours reachable yet, will retry")
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if delay < 30*time.Second {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
	}
}
