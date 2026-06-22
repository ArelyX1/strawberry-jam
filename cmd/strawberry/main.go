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

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/crypto"
	"github.com/eigerco/strawberry/internal/crypto/ed25519"
	"github.com/eigerco/strawberry/internal/safrole"
	chainState "github.com/eigerco/strawberry/internal/state"
	"github.com/eigerco/strawberry/internal/validator"
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
	)

	flag.StringVar(&configFile, "config", "appconfig.json", "path to config file")
	flag.StringVar(&chainSpec, "chain", "dev", "chain specification")
	flag.BoolVar(&isValidator, "validator", false, "run as validator")
	flag.StringVar(&nodeName, "name", "Strawberry-Node", "node name")
	flag.StringVar(&telemetryURL, "telemetry-url", "", "telemetry WebSocket URL")
	flag.IntVar(&portOverride, "port", 0, "override p2p listen port")
	flag.IntVar(&rpcPort, "rpc-port", 9944, "RPC WebSocket port")
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

	n, err := node.NewNode(ctx, udpAddress, vkeys, state, index)
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

	chainName := fmt.Sprintf("Strawberry %s", chainSpec)

	// Start RPC server (before block producer so it's ready for subscriptions)
	rpcAddr := fmt.Sprintf(":%d", rpcPort)
	rpcSrv := startRPCServer(rpcAddr, nodeName, chainName, version,
		n.BlockService.Store, n.BlockService)

	// Start block producer
	startBlockProducer(n.BlockService, index,
		func(hash crypto.Hash, num uint, h block.Header) {
			rpcSrv.updateBlock(hash, num, h)
		})

	// Connect telemetry
	startTelemetry(telemetryURL, nodeName, version, chainName)

	select {}
}
