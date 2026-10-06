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
	"github.com/eigerco/strawberry/internal/crypto"
	"github.com/eigerco/strawberry/internal/crypto/ed25519"
	"github.com/eigerco/strawberry/internal/safrole"
	chainState "github.com/eigerco/strawberry/internal/state"
	"github.com/eigerco/strawberry/internal/validator"
	"github.com/eigerco/strawberry/pkg/db/pebble"
	"github.com/eigerco/strawberry/pkg/devnet"
	"github.com/eigerco/strawberry/pkg/discovery"
	"github.com/eigerco/strawberry/pkg/log"
	"github.com/eigerco/strawberry/pkg/network/node"
	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

var version = "0.1.0-dev"

// genesisPath is the economy this run uses. It is a package variable rather than
// a local because --init-genesis stamps the same economy into the kit it
// writes, and the two have to be the same one.
var genesisPath string

type FullValidatorInfo struct {
	Index      uint   `json:"index"`
	Name       string `json:"name"`
	Seed       string `json:"seed"`
	IP         string `json:"ip"`
	Port       int    `json:"port"`
	Ed25519Pub string `json:"ed25519_pub"`

	// Mesh are the addresses where this validator's peer-to-peer host can be
	// reached, written as libp2p multiaddrs.
	//
	// They are optional and the network works without them, because local
	// discovery and the address book each cover part of it. They exist because
	// two machines on two different networks have to be introduced once, and this
	// is where that first introduction is written down. Any of them being enough
	// is the point: there is no address here that has to keep working.
	Mesh       []string `json:"mesh,omitempty"`
	Ed25519Prv string   `json:"ed25519_private"`
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

// overridePort puts a different port on an address and leaves the host alone.
//
// The host comes back from SplitHostPort without the brackets an IPv6 literal
// needs in an address, and JoinHostPort puts them back, so the round trip is what
// keeps `[::1]:30333` from turning into `[[::1]]:30333`, which no resolver will
// accept and which reads as an address with no port at all.
func overridePort(addr string, port int) (string, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

func (f FullValidatorInfo) ToMetadata() ([]byte, error) {
	return metadataFromAddr(net.JoinHostPort(f.IP, strconv.Itoa(f.Port)))
}

// metadataFromAddr builds a validator metadata key naming where a validator can be
// reached. The address goes in the first eighteen bytes of the key and the rest is
// left zeroed, which is the shape the transport expects to read back.
func metadataFromAddr(hostPort string) ([]byte, error) {
	addr, err := net.ResolveUDPAddr("udp", hostPort)
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
		configFile         string
		validatorIndex     int
		authorCount        int
		flagValidatorCount int
		validatorsFile     string
		fullMesh           bool
		skipMissingAuthors bool
		finalize           bool
		chainSpec          string
		isValidator        bool
		initGenesisDir     string
		meshFlag           string
		nodeName           string
		telemetryURL       string
		portOverride       int
		netConfPath        string
		rpcPort            int
		help               bool
		dataDir            string
		bridgeWallet       string
	)

	flag.StringVar(&configFile, "config", "appconfig.json", "path to config file")
	flag.IntVar(&validatorIndex, "validator-index", -1,
		"which validator from test_validators.json this process is, overriding the config file")
	flag.IntVar(&authorCount, "author-count", 0,
		"how many validators the authorship turn rotates over; 1 makes a lone node write every timeslot, 0 follows the size of the validator set")
	flag.IntVar(&flagValidatorCount, "validator-count", 0,
		"how many validators the network has; 0 takes them from the validator file")
	flag.StringVar(&validatorsFile, "validators-file", "test_validators.json",
		"the validator file: who exists and where to reach them")
	flag.BoolVar(&fullMesh, "full-mesh", false,
		"connect to every configured validator, not only the grid neighbours")
	flag.BoolVar(&skipMissingAuthors, "skip-missing-authors", false,
		"when the designated author of a timeslot does not produce within the grace period, "+
			"the next validator in rotation writes it instead; off by default because a "+
			"production chain should stall honestly rather than attribute a block to the wrong author")
	flag.BoolVar(&finalize, "finalize", false,
		"record blocks as finalized after a fixed depth; off by default because a devnet has nobody to agree with")
	flag.StringVar(&chainSpec, "chain", "dev", "chain specification")
	flag.BoolVar(&isValidator, "validator", false, "run as validator")
	flag.StringVar(&nodeName, "name", "Strawberry-Node", "node name")
	flag.StringVar(&telemetryURL, "telemetry-url", "", "telemetry WebSocket URL")
	flag.IntVar(&portOverride, "port", 0, "override p2p listen port")
	flag.StringVar(&netConfPath, "net-conf", "",
		"where the machines are: this node's own listen address and, under [peers], the address of each validator by index")
	flag.IntVar(&rpcPort, "rpc-port", 9944, "RPC WebSocket and HTTP port")
	flag.StringVar(&genesisPath, "genesis", "genesis/chain-dev.json", "path to the genesis of the PAPU economy")
	flag.StringVar(&dataDir, "data-dir", "", "directory to keep blocks and state in; empty keeps them in memory")
	flag.StringVar(&bridgeWallet, "bridge-wallet", "", "hex Ed25519 seed of the account the node pays faucets from")
	flag.StringVar(&initGenesisDir, "init-genesis", "",
		"write a ready-to-share network kit into this directory (genesis with the current "+
			"genesisTimeslot, a validator set, an appconfig and a one-line launch command per "+
			"node) and exit; copy the directory to every machine, then run the command it prints")
	flag.StringVar(&meshFlag, "mesh", "",
		"mesh addresses for the first contact between networks, as "+
			"index=multiaddr pairs separated by commas; for example "+
			"1=/ip4/203.0.113.4/tcp/40335. They are optional: the local network and "+
			"what the node remembers from earlier runs cover the rest")
	flag.BoolVar(&help, "help", false, "show help")
	flag.Parse()

	if initGenesisDir != "" {
		mesh, err := parseMeshEntries(meshFlag)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if err := writeNetworkKit(initGenesisDir, flagValidatorCount, mesh); err != nil {
			// El logger todavia no esta puesto ahi, asi que un Fatal aqui
			// terminaria con codigo 1 y sin decir por que, que es la peor
			// forma de fallar: un kit que no se escribe parece un kit que no
			// se pidio.
			fmt.Fprintf(os.Stderr, "network kit write failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

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

	// Which validator this process is. The config file carries one, but a file
	// per node is a nuisance to keep in step with the command line, so the flag
	// wins when it is given. Without this every node started without it came up
	// as the same validator, and two processes ended up talking to each other
	// instead of to a peer: same key, same chain, and a test that agreed with
	// itself for the wrong reason.
	if validatorIndex >= 0 {
		appConfig.ValidatorIndex = validatorIndex
	}

	maxuint16 := int(^uint16(0))
	// Out of range is either end, so this is || and not &&: with && a negative
	// index was accepted and then became 65535 when it was cast, which is a
	// validator that does not exist rather than a clear refusal.
	if appConfig.ValidatorIndex < 0 || appConfig.ValidatorIndex > maxuint16 {
		log.Internal.Fatal().
			Msgf("validator index %d out of bounds 0-%d", appConfig.ValidatorIndex, maxuint16)
	}

	index := uint16(appConfig.ValidatorIndex)

	vs, err := loadFullValidatorInfos(validatorsFile)
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
	// Where this node listens comes from the net conf when there is one, and from
	// the validator file when there is not. The file is the same bytes on every
	// machine and the address a machine has today is not, which is why the two are
	// separate: moving a machine is one line in one file, not a regenerated
	// validator file shipped to everybody.
	netConf, err := devnet.LoadNetConf(netConfPath)
	if err != nil {
		log.Internal.Fatal().Err(err).Msg("net conf load failed")
	}
	listen, err := netConf.ListenAddr(index, vs[index].IP, vs[index].Port)
	if err != nil {
		log.Internal.Fatal().Err(err).Msg("net conf listen address failed")
	}
	if portOverride > 0 {
		listen, err = overridePort(listen, portOverride)
		if err != nil {
			log.Internal.Fatal().Str("address", listen).Err(err).Msg("listen address port override failed")
		}
	}
	// The address of every validator, not just this one's, has to go through the
	// net conf before the validator state is built.
	//
	// The node dials its neighbours from the addresses in that state, so an
	// override that is only handed to the runtime below never reaches the dialling:
	// the node binds on the address the conf gave it and then tries to reach
	// everybody at the address the validator file gave them, which is a node that
	// listens where it was told and talks to nobody. Resolving the addresses here,
	// before the state exists, is what makes the conf mean anything.
	listenAddrs, err := netConf.ApplyAddrs(validatorListenAddrs(vs))
	if err != nil {
		log.Internal.Fatal().Err(err).Msg("net conf peer addresses failed")
	}
	if portOverride > 0 {
		// Only this validator's own address. A listen port is the one thing about
		// a machine that changes when it moves: the addresses the other validators
		// are reached on are theirs, not ours, and rewriting them to our port sends
		// every peer to a port where nobody is listening for them.
		//
		// Written over the whole list, a run where each node was given its own port
		// made every validator answer at the port of whichever node was asking.
		// Each node then dialled its own address, found itself, and settled for a
		// conversation with itself: one peer per node, every node talking to itself,
		// and no chain at all. That looked like a mesh that had formed and had not.
		listenAddrs[index], err = overridePort(listenAddrs[index], portOverride)
		if err != nil {
			log.Internal.Fatal().Str("address", listenAddrs[index]).Err(err).Msg("own address port override failed")
		}
	}
	udpAddress, err := net.ResolveUDPAddr("udp", listen)
	if err != nil {
		log.Internal.Fatal().
			Str("address", listen).
			Err(err).
			Msg("address resolve failed")
	}

	// With the port in it, because the first thing anyone asks when a node is not
	// reachable is which port it is on, and "listening on: 127.0.0.1" does not
	// answer that.
	log.Internal.Info().
		Str("address", udpAddress.String()).
		Uint16("validator", index).
		Msg("listening on")

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

		meta, err := metadataFromAddr(listenAddrs[i])
		if err != nil {
			log.Internal.Fatal().
				Int("index", i).
				Str("address", listenAddrs[i]).
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

	// How many validators this network has comes from the validator file, not
	// from a constant compiled into the binary. The constant is the real chain's
	// size, which is nothing like the handful of nodes a devnet actually runs, and
	// taking the validator set from it meant a devnet could only ever be as big
	// as whatever the build tag said. Adding a node to the file is now all it
	// takes to add it to the network.
	validatorCount := len(vs)
	if flagValidatorCount > 0 {
		validatorCount = flagValidatorCount
	}
	if validatorCount > len(vs) {
		log.Internal.Fatal().
			Int("wanted", validatorCount).
			Int("configured", len(vs)).
			Msg("more validators were asked for than the validator file has")
	}

	devValidators, err := devnet.DevValidatorKeys(validatorCount, listenAddrs)
	if err != nil {
		log.Internal.Fatal().Err(err).Msg("validator state build failed")
	}

	// The authorship turn rotates over the validators that are actually here, so
	// a mesh of N nodes has one author per timeslot out of N. Left at the chain's
	// own count, or at one, a node writes a timeslot in a thousand or writes every
	// one of them, and N nodes fork on every slot.
	if authorCount == 0 && validatorCount > 1 {
		authorCount = validatorCount
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

	// The chain is founded at the timeslot the genesis names, so that every node
	// of the network founds it at the same moment. A genesis that does not say is
	// fatal rather than defaulted: a node that dates its own chain when it starts
	// builds a different chain from every other node, and the two then reject each
	// other's blocks. That failure is silent and looks like a network that never
	// syncs, so it is worth refusing to start over.
	if genesis.GenesisTimeslot == 0 {
		log.Internal.Fatal().
			Str("genesis", genesisPath).
			Msg("the genesis has no genesisTimeslot, so this node would found its own chain " +
				"at its own start time and never agree with the rest of the network; " +
				"add genesisTimeslot to the genesis, shared by every node, or run " +
				"scripts/devnet-prepare.sh which writes it into the genesis it generates")
	}
	nodeOpts := []node.NodeOption{node.WithGenesisTimeslot(genesis.GenesisTimeslot)}
	n, err := node.NewNodeWithStore(ctx, udpAddress, vkeys, state, index, kvStore, nodeOpts...)
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
	// Grid-diffusion: when a block is received, re-announce it to this node's
	// grid neighbours so the block reaches the grid hop by hop instead of
	// flooding to every peer.
	n.SetupGridDiffusion()

	// Conectar con los validadores vecinos. Hasta ahora no se llamaba nunca, de
	// modo que el nodo escuchaba y nunca ": conectaba con nadie, y por eso dos
	// nodos en la misma maquina eran dos cadenas calculadas por separado en vez
	// de una red. Se hace en segundo plano y con reintentos porque un vecino
	// puede no estar arrancado todavia, y eso no puede impedir que este nodo
	// sirva su propia cadena.
	go connectToNeighbours(ctx, n)

	// Descubrimiento. El nodo de cadena y el de descubrimiento son dos capas
	// distintas: la cadena habla su propio transporte con los validadores que el
	// genesis fija, y el descubrimiento averigua donde estan las maquinas sin
	// que nadie escriba una direccion a mano. Este es el puente entre ambos, y es
	// lo que permite que dos maquinas en redes distintas formen una sola red
	// sin configuracion previa.
	if len(devValidators) > 1 {
		// La identidad de la malla sale de la clave de validador, no de una
		// generada al azar. Asi cualquier nodo puede calcular el identificador de
		// los demas a partir del genesis sin haber hablado con ellos, que es lo
		// que hace posible que dos maquinas se encuentren sin un tercero que las
		// presente.
		identidad, err := discovery.IdentityFromValidatorKey(vkeys.EdPub)
		if err != nil {
			log.Internal.Warn().Err(err).
				Msg("the mesh identity could not be derived from the validator key, so this " +
					"node will use a generated one: other nodes will have to be told where " +
					"it is, and it will still work on the local network")
		}

		discoveryHost, err := discovery.Start(ctx, discovery.Options{
			DataDir:   discoveryDataDir(dataDir, int(index)),
			Port:      discoveryPortFor(portOverride),
			Relay:     true,
			Identity:  identidad,
			Bootstrap: bootstrapFromValidators(vs),
			Logf: func(format string, args ...any) {
				log.Internal.Info().Msgf(format, args...)
			},
		})
		if err != nil {
			// Sin descubrimiento el nodo todavia puede servir a los validadores que
			// ya estan escritos en el net conf, asi que esto no es fatal. Decirlo
			// alto importa mas que callarlo: si el nodo no encuentra a nadie y no
			// se sabe por que, parece una red muerta.
			log.Internal.Warn().Err(err).
				Msg("peer discovery is not available, so this node will only reach the " +
					"validators written in its net conf; it will not find any others")
		} else {
			defer discoveryHost.Close()
			log.Internal.Info().
				Str("peerID", discoveryHost.ID().String()).
				Msg("peer discovery is running")
			go dialDiscoveredPeers(ctx, n, discoveryHost, udpAddress.String(),
				chainPortsFrom(listenAddrs), func(
					format string, args ...any) {
					log.Internal.Info().Msgf(format, args...)
				})
		}
	}

	// Rellenar los bloques que falten. Solo tiene sentido con pares, asi que
	// espera a que ConnectToNeighbours haya hecho su trabajo: antes de eso el
	// bucle solo encontraria huecos que no puede cerrar.
	go func() {
		for len(n.GetAllPeers()) == 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backfillEvery):
			}
		}
		n.FullMesh = fullMesh
		// Un devnet no tiene consenso, asi que no fija bloques como finalizados: hacerlo
		// por profundidad de generaciones es elegir una rama sin que nadie la haya
		// respaldado, y cuando esa rama pierde el nodo rechaza para siempre todo lo que
		// llega por la que gano. Con la finalizacion apagada la cadena crece y los nodos
		// pueden ponerse de acuerdo en ella.
		n.BlockService.SetFinalization(finalize)
		backfillLoop(ctx, n, n.BlockService)
	}()

	chainName := fmt.Sprintf("Strawberry %s", chainSpec)

	// Say who this node is, out loud. Two nodes that both came up as the same
	// validator look perfectly healthy from the outside: they connect, they
	// announce, and they agree on the tip, because they are the same node
	// talking to itself. Nothing in the log said otherwise, which is how a whole
	// two-validator test went on passing for that reason.
	log.Internal.Info().
		Uint16("validatorIndex", index).
		Str("key", fmt.Sprintf("%x", seed[len(seed)-32:])).
		Str("listening", udpAddress.String()).
		Msg("this node is")

	// Start RPC server (before block producer so it's ready for subscriptions)
	rpcAddr := fmt.Sprintf(":%d", rpcPort)
	rpcSrv := startRPCServer(rpcAddr, nodeName, chainName, version,
		n.BlockService.Store, n.BlockService, n, udpAddress.String())
	rpcSrv.papucoin = newPapucoinHandlers(runtime, rpcSrv)
	// The RPC server is built before the producer knows which validator this
	// node is, so the two values network_map reports are filled in here. The
	// validator set it lists comes from the same file the node was given.
	rpcSrv.rpcAddr = rpcAddr
	rpcSrv.validatorIndex = index
	rpcSrv.validators = vs

	// Start block producer
	startBlockProducer(n.BlockService, runtime, index, uint16(authorCount),
		skipMissingAuthors,
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
// them talking to nobody. The backoff stops growing so a peer that comes up late
// is still picked up, at the cost of a periodic attempt that mostly does
// nothing, which is fine on a dev machine.
//
// Every pass goes through the whole neighbour list rather than stopping once
// there is a peer: the list is short, the peers already held are skipped, and a
// node that stopped looking after the first connection would never build a
// mesh.
func connectToNeighbours(ctx context.Context, n *node.Node) {
	delay := 2 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Se sigue buscando en cada pasada, no solo mientras no haya ninguno.
		//
		// Parar en cuanto hubiera un par valia para dos nodos, donde ese par es el
		// unico que hace falta, y hacia muy mal para una malla: un nodo que se
		// conectaba con el primero que encontraba se quedaba ahi para siempre y
		// nunca marcaba a los demas. Con tres nodos, el segundo solo conocia al
		// primero y el tercero solo al primero tambien, y los dos no se veian
		// entre si. Que un vecino falle no es motivo para no intentar el
		// siguiente, y ConnectToNeighbours ya se salta a quien ya tiene.
		if err := n.ConnectToNeighbours(); err != nil {
			log.Internal.Debug().Err(err).Msg("some neighbours are not reachable yet, will retry")
		}
		if peers := n.GetAllPeers(); len(peers) > 0 {
			log.Internal.Info().Int("peers", len(peers)).Msg("connected to neighbour validators")
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

// bootstrapFromValidators turns the optional mesh addresses in the validator set
// into seeds for the discovery table.
//
// They are the answer to "how does a node on one network learn about a node on
// another the very first time", and the only thing that can answer it: before two
// machines have spoken, nobody knows where the other one is. After that the
// address book takes over, and after a restart nothing has to be written down at
// all.
//
// A malformed address is skipped rather than fatal. One bad line in a hand
// edited file must not stop a node that could otherwise reach nine other
// machines.
func bootstrapFromValidators(vs []FullValidatorInfo) []peer.AddrInfo {
	salida := make([]peer.AddrInfo, 0, len(vs))

	for _, v := range vs {
		if len(v.Mesh) == 0 {
			continue
		}
		pub, err := decodeHex(v.Ed25519Pub)
		if err != nil {
			continue
		}
		id, err := discovery.PeerIDFromValidatorKey(pub)
		if err != nil {
			continue
		}

		addrs := make([]ma.Multiaddr, 0, len(v.Mesh))
		for _, texto := range v.Mesh {
			addr, err := ma.NewMultiaddr(strings.TrimSpace(texto))
			if err != nil {
				log.Internal.Warn().Err(err).
					Str("address", texto).
					Str("validator", v.Name).
					Msg("a mesh address in the validator set does not parse and is being ignored")
				continue
			}
			addrs = append(addrs, addr)
		}
		if len(addrs) == 0 {
			continue
		}
		salida = append(salida, peer.AddrInfo{ID: id, Addrs: addrs})
	}

	if len(salida) > 0 {
		log.Internal.Info().
			Int("seeds", len(salida)).
			Msg("the validator set carries mesh addresses, so this node does not have to be " +
				"told where the others are")
	}
	return salida
}
