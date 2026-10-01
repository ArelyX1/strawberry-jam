package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/chain"
	"github.com/eigerco/strawberry/internal/crypto"
	"github.com/eigerco/strawberry/internal/store"
	"github.com/eigerco/strawberry/pkg/log"
	p2pnode "github.com/eigerco/strawberry/pkg/network/node"
	"golang.org/x/net/websocket"
)

type rpcServer struct {
	nodeName    string
	chainName   string
	nodeVersion string
	chainStore  *store.Chain
	bs          *chain.BlockService
	latestHash  crypto.Hash
	blockNum    uint
	mu          sync.RWMutex
	subs        map[uint]chan<- subscribeEvent
	subID       uint
	subMu       sync.Mutex
	// papucoin serves the economy, and is nil only on a node built without a
	// runtime, which cannot happen on a node that produces blocks.
	papucoin *papucoinHandlers
	// node is the network node, so the peer methods can report who is actually
	// connected instead of an empty list. nil only on a node built without a
	// network, which the dev node never is.
	node *p2pnode.Node
	// listenAddr is the address this node is reachable on. It is passed in
	// because the transport keeps no accessor for its own address.
	listenAddr string
	// startedAt is when this process came up, which is what uptime has to be
	// measured from. It is not the chain's age: a node replays from genesis, so
	// the chain is much older than the process serving it.
	startedAt time.Time
	// rebuilt is closed once this node has rebuilt the state its blocks describe.
	//
	// Until then the state in memory is a state part way through a replay, and a
	// node that answers a balance or a state root from it is answering about a
	// chain that never existed. A caller waits here rather than being told a
	// number that is about to change under it.
	rebuilt chan struct{}
}

type subscribeEvent struct {
	method string
	params interface{}
}

func startRPCServer(addr string, nodeName, chainName, nodeVersion string, chainStore *store.Chain, bs *chain.BlockService, node *p2pnode.Node, listenAddr string) *rpcServer {
	srv := &rpcServer{
		node:        node,
		listenAddr:  listenAddr,
		startedAt:   time.Now(),
		nodeName:    nodeName,
		chainName:   chainName,
		nodeVersion: nodeVersion,
		chainStore:  chainStore,
		bs:          bs,
		subs:        make(map[uint]chan<- subscribeEvent),
		rebuilt:     make(chan struct{}),
	}
	mux := http.NewServeMux()
	wsServer := &websocket.Server{
		Handler: srv.handleWS,
		Handshake: func(config *websocket.Config, req *http.Request) error {
			return nil
		},
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "websocket" {
			wsServer.ServeHTTP(w, r)
			return
		}
		srv.handleHTTP(w, r)
	})
	// A node that was restarted still has the chain it was running, so the RPC
	// starts from the tip that is on disk rather than waiting for the first block
	// of this run to be produced before it can answer anything.
	if tip, found := srv.loadTip(); found {
		srv.mu.Lock()
		srv.latestHash = tip.hash
		srv.blockNum = tip.number
		srv.mu.Unlock()
		log.Internal.Info().
			Str("hash", hashToHex(tip.hash)).
			Uint("number", tip.number).
			Msg("the RPC is answering for the chain on disk")
	}

	log.Internal.Info().Msgf("RPC server listening on ws://%s and http://%s", addr, addr)
	go func() {
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Internal.Error().Err(err).Msg("RPC server failed")
		}
	}()
	return srv
}

// markRebuilt says the state this node rebuilt from its own blocks is the state it
// is carrying, so the answers it gives are answers about a chain.
func (s *rpcServer) markRebuilt() { close(s.rebuilt) }

// waitRebuilt holds a caller until the state has been rebuilt. It is closed on a
// node whose replay failed too, but that node does not survive to answer anything.
func (s *rpcServer) waitRebuilt() { <-s.rebuilt }

// readsTheChain is the set of methods whose answer depends on a state this node
// has to rebuild from its blocks. A wallet asking for a balance during the replay
// would be told a number that changes a second later, which is worse than a
// second of waiting.
func readsTheChain(method string) bool {
	switch method {
	case "papucoin_balance", "papucoin_supply", "papucoin_submit", "papucoin_faucet",
		"jam_getHeader", "jam_stateRoot", "jam_serviceAccount", "jam_getStorage",
		"eth_blockNumber", "eth_getBalance", "eth_getTransactionCount",
		"eth_getBlockByNumber", "eth_sendRawTransaction", "eth_call", "eth_estimateGas":
		return true
	default:
		return false
	}
}

func (s *rpcServer) handleHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req rpcReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if isSubscription(req.Method) {
		http.Error(w, "subscriptions are not supported over HTTP", http.StatusBadRequest)
		return
	}
	resp := s.handleRequest(req, nil, nil)
	if resp != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}
}

func isSubscription(method string) bool {
	return method == "chain_subscribeNewHeads" ||
		method == "chain_subscribeFinalizedHeads" ||
		method == "chain_unsubscribeNewHeads" ||
		method == "chain_unsubscribeFinalizedHeads" ||
		method == "state_subscribeRuntimeVersion" ||
		method == "state_subscribeMetadata" ||
		method == "state_unsubscribeRuntimeVersion" ||
		method == "state_unsubscribeMetadata"
}

// tip is the last block a chain has, as far as this node is concerned.
type tip struct {
	hash   crypto.Hash
	number uint
}

// loadTip finds the newest block the store holds. Headers are keyed by their own
// hash, so they are not stored in any order this can lean on, and the newest one
// is the one with the highest timeslot.
func (s *rpcServer) loadTip() (tip, bool) {
	var (
		newest block.Header
		found  bool
		number uint
	)
	if _, _, err := s.chainStore.FindHeader(func(h block.Header) bool {
		if h.ParentHash == chain.GenesisParent {
			return false
		}
		if !found || h.TimeSlotIndex > newest.TimeSlotIndex {
			newest, found = h, true
		}
		return false
	}); err != nil || !found {
		return tip{}, false
	}
	hash, err := newest.Hash()
	if err != nil {
		return tip{}, false
	}
	// The number of a block is how many blocks sit between the genesis and it, and
	// the timeslots of a chain this node produces are one per block, so the count
	// comes from the timeslots rather than from a walk that would have to read
	// every header back.
	if genesis, _, ok := s.bs.GenesisHeader(); ok {
		number = uint(newest.TimeSlotIndex-genesis.TimeSlotIndex) + 1
	}
	return tip{hash: hash, number: number}, true
}

func (s *rpcServer) updateBlock(hash crypto.Hash, num uint, h block.Header) {
	s.mu.Lock()
	s.latestHash = hash
	s.blockNum = num
	s.mu.Unlock()
	s.broadcast("chain_subscribeNewHeads", headerToSubstrate(h, num))

	lf := s.bs.GetLatestFinalized()
	if lf.Hash != (crypto.Hash{}) {
		lfHeader, err := s.chainStore.GetHeader(lf.Hash)
		if err == nil {
			lfNum := findBlockNum(s.chainStore, s.latestHash, s.blockNum, lf.Hash)
			if lfNum > 0 {
				s.broadcast("chain_subscribeFinalizedHeads", headerToSubstrate(lfHeader, lfNum))
			}
		}
	}
}

func findBlockNum(chainStore *store.Chain, tip crypto.Hash, tipNum uint, target crypto.Hash) uint {
	if tip == target {
		return tipNum
	}
	current := tip
	for i := uint(0); i < tipNum; i++ {
		h, err := chainStore.GetHeader(current)
		if err != nil {
			return 0
		}
		if current == target {
			return tipNum - i - 1
		}
		current = h.ParentHash
	}
	return 0
}

func (s *rpcServer) broadcast(method string, params interface{}) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	ev := subscribeEvent{method: method, params: params}
	for _, ch := range s.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

type rpcReq struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResp struct {
	JSONRPC string      `json:"jsonrpc"`
	ID      interface{} `json:"id"`
	Result  interface{} `json:"result,omitempty"`
	Error   *rpcErr     `json:"error,omitempty"`
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcNotif struct {
	JSONRPC string      `json:"jsonrpc"`
	Method  string      `json:"method"`
	Params  interface{} `json:"params"`
}

type subParams struct {
	Result       interface{} `json:"result"`
	Subscription uint        `json:"subscription"`
}

type substrateHeader struct {
	ParentHash     string        `json:"parentHash"`
	Number         string        `json:"number"`
	StateRoot      string        `json:"stateRoot"`
	ExtrinsicsRoot string        `json:"extrinsicsRoot"`
	Digest         substrateLogs `json:"digest"`
}

type substrateLogs struct {
	Logs []string `json:"logs"`
}

func headerToSubstrate(h block.Header, num uint) substrateHeader {
	return substrateHeader{
		ParentHash:     fmt.Sprintf("0x%x", h.ParentHash),
		Number:         fmt.Sprintf("0x%x", num),
		StateRoot:      fmt.Sprintf("0x%x", h.PriorStateRoot),
		ExtrinsicsRoot: fmt.Sprintf("0x%x", h.ExtrinsicHash),
		Digest:         substrateLogs{Logs: []string{}},
	}
}

func hashToHex(h crypto.Hash) string {
	return fmt.Sprintf("0x%x", h)
}

func (s *rpcServer) handleWS(ws *websocket.Conn) {
	defer ws.Close()
	subCh := make(chan subscribeEvent, 64)
	var subIDs []uint

	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case ev, ok := <-subCh:
				if !ok {
					return
				}
				s.subMu.Lock()
				ids := make([]uint, len(subIDs))
				copy(ids, subIDs)
				s.subMu.Unlock()
				for _, sid := range ids {
					msg := rpcNotif{
						JSONRPC: "2.0",
						Method:  ev.method,
						Params: subParams{
							Result:       ev.params,
							Subscription: sid,
						},
					}
					b, _ := json.Marshal(msg)
					websocket.Message.Send(ws, string(b))
				}
			case <-done:
				return
			}
		}
	}()

	for {
		var raw string
		if err := websocket.Message.Receive(ws, &raw); err != nil {
			break
		}
		var req rpcReq
		if err := json.Unmarshal([]byte(raw), &req); err != nil {
			continue
		}
		resp := s.handleRequest(req, subCh, &subIDs)
		if resp != nil {
			b, _ := json.Marshal(resp)
			websocket.Message.Send(ws, string(b))
		}
	}
	s.subMu.Lock()
	for _, sid := range subIDs {
		delete(s.subs, sid)
	}
	s.subMu.Unlock()
}

func (s *rpcServer) newSubID(ch chan<- subscribeEvent) uint {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	s.subID++
	s.subs[s.subID] = ch
	return s.subID
}

func (s *rpcServer) handleRequest(req rpcReq, subCh chan<- subscribeEvent, subIDs *[]uint) *rpcResp {
	id := req.ID
	log.Internal.Debug().Str("method", req.Method).Str("params", string(req.Params)).Msg("RPC call")
	switch req.Method {
	case "rpc_methods":
		return &rpcResp{JSONRPC: "2.0", ID: id, Result: map[string]interface{}{
			"methods": []string{
				"rpc_methods", "system_chain", "system_name", "system_version",
				"system_health", "system_peers", "system_localListenAddresses",
				"system_syncState", "system_accountNextIndex",
				"chain_getBlockHash", "chain_getHeader", "chain_getBlock",
				"chain_getFinalizedHead", "chain_subscribeNewHeads",
				"chain_subscribeFinalizedHeads", "chain_unsubscribeNewHeads",
				"chain_unsubscribeFinalizedHeads", "chain_getRuntimeVersion",
				"state_getRuntimeVersion", "state_getMetadata",
				"state_subscribeRuntimeVersion", "state_unsubscribeRuntimeVersion",
				"jam_stateRoot", "jam_serviceAccount", "jam_getStorage", "jam_getHeader",
				// Lo que una billetera que solo habla Ethereum pregunta. Cada uno
				// responde con lo que la cadena tiene, y eth_call dice que no hay
				// respuesta honesta todavia.
				"eth_chainId", "eth_blockNumber", "eth_getBalance", "eth_getTransactionCount",
				"eth_gasPrice", "eth_estimateGas", "eth_getBlockByNumber", "eth_sendRawTransaction", "eth_call",
				"papucoin_chainParams", "papucoin_balance", "papucoin_supply",
				"papucoin_submit", "papucoin_faucet",
				"jam_getTelemetry", "jam_getWorks", "jam_getBlockLog", "jam_joinAccumulate",
			},
		}}
	case "system_chain":
		return &rpcResp{JSONRPC: "2.0", ID: id, Result: s.chainName}
	case "system_name":
		return &rpcResp{JSONRPC: "2.0", ID: id, Result: s.nodeName}
	case "system_version":
		return &rpcResp{JSONRPC: "2.0", ID: id, Result: s.nodeVersion}
	case "system_health":
		// El numero de pares es el de verdad. Estaba en cero fijo, y es el campo
		// que lee el panel, asi que un nodo con uno al lado reportaba cero.
		peers := 0
		if s.node != nil {
			peers = len(s.node.GetAllPeers())
		}
		return &rpcResp{JSONRPC: "2.0", ID: id, Result: map[string]interface{}{
			"peers": peers, "isSyncing": false, "shouldHavePeers": false,
		}}
	case "system_uptime":
		// How long this process has been up, which is the only uptime that can be
		// asked for over the wire. The age of the chain is a different number: a
		// node replays from genesis, so the head it serves is far ahead of the
		// moment the process started.
		up := time.Since(s.startedAt)
		return &rpcResp{JSONRPC: "2.0", ID: id, Result: map[string]interface{}{
			"ms":        up.Milliseconds(),
			"seconds":   int64(up.Seconds()),
			"startedAt": s.startedAt.UTC().Format(time.RFC3339),
		}}
	case "system_peers":
		// Los pares de verdad. Antes devolvia una lista vacia fijada en el
		// codigo, asi que un nodo conectado a otros parecia no tener ninguno, y
		// el panel no podia decir quantos habia.
		out := make([]interface{}, 0)
		if s.node != nil {
			for _, p := range s.node.GetAllPeers() {
				entry := map[string]interface{}{
					"address": fmt.Sprintf("%v", p.Address),
				}
				if p.BAnnouncer != nil {
					entry["announcing"] = true
				}
				out = append(out, entry)
			}
		}
		return &rpcResp{JSONRPC: "2.0", ID: id, Result: out}
	case "system_localListenAddresses":
		out := []string{}
		if s.listenAddr != "" {
			out = append(out, s.listenAddr)
		}
		return &rpcResp{JSONRPC: "2.0", ID: id, Result: out}
	case "chain_getBlockHash":
		s.mu.RLock()
		hash := s.latestHash
		s.mu.RUnlock()
		if hash == (crypto.Hash{}) {
			h, _, err := s.chainStore.FindHeader(func(hdr block.Header) bool { return true })
			if err != nil {
				return &rpcResp{JSONRPC: "2.0", ID: id, Result: nil}
			}
			hash, _ = h.Hash()
		}
		return &rpcResp{JSONRPC: "2.0", ID: id, Result: hashToHex(hash)}
	case "chain_getHeader":
		s.mu.RLock()
		hash := s.latestHash
		num := s.blockNum
		s.mu.RUnlock()
		if hash == (crypto.Hash{}) {
			return &rpcResp{JSONRPC: "2.0", ID: id, Error: &rpcErr{Code: -32000, Message: "no blocks"}}
		}
		h, err := s.chainStore.GetHeader(hash)
		if err != nil {
			return &rpcResp{JSONRPC: "2.0", ID: id, Error: &rpcErr{Code: -32000, Message: err.Error()}}
		}
		return &rpcResp{JSONRPC: "2.0", ID: id, Result: headerToSubstrate(h, num)}
	case "chain_getBlock":
		s.mu.RLock()
		hash := s.latestHash
		num := s.blockNum
		s.mu.RUnlock()
		if hash == (crypto.Hash{}) {
			return &rpcResp{JSONRPC: "2.0", ID: id, Error: &rpcErr{Code: -32000, Message: "no blocks"}}
		}
		b, err := s.chainStore.GetBlock(hash)
		if err != nil {
			h, err := s.chainStore.GetHeader(hash)
			if err != nil {
				return &rpcResp{JSONRPC: "2.0", ID: id, Error: &rpcErr{Code: -32000, Message: err.Error()}}
			}
			return &rpcResp{JSONRPC: "2.0", ID: id, Result: map[string]interface{}{
				"block": map[string]interface{}{
					"header":     headerToSubstrate(h, num),
					"extrinsics": []string{},
				},
				"justifications": nil,
			}}
		}
		return &rpcResp{JSONRPC: "2.0", ID: id, Result: map[string]interface{}{
			"block": map[string]interface{}{
				"header":     headerToSubstrate(b.Header, num),
				"extrinsics": []string{},
			},
			"justifications": nil,
		}}
	case "chain_getFinalizedHead":
		lf := s.bs.GetLatestFinalized()
		if lf.Hash == (crypto.Hash{}) {
			s.mu.RLock()
			hash := s.latestHash
			s.mu.RUnlock()
			if hash == (crypto.Hash{}) {
				return &rpcResp{JSONRPC: "2.0", ID: id, Result: "0x0000000000000000000000000000000000000000000000000000000000000000"}
			}
			return &rpcResp{JSONRPC: "2.0", ID: id, Result: hashToHex(hash)}
		}
		return &rpcResp{JSONRPC: "2.0", ID: id, Result: hashToHex(lf.Hash)}
	case "chain_subscribeNewHeads", "chain_subscribeFinalizedHeads":
		sid := s.newSubID(subCh)
		*subIDs = append(*subIDs, sid)
		return &rpcResp{JSONRPC: "2.0", ID: id, Result: sid}
	case "chain_unsubscribeNewHeads", "chain_unsubscribeFinalizedHeads":
		return &rpcResp{JSONRPC: "2.0", ID: id, Result: true}
	case "state_getRuntimeVersion", "chain_getRuntimeVersion":
		return &rpcResp{JSONRPC: "2.0", ID: id, Result: map[string]interface{}{
			"specName": s.chainName, "implName": s.nodeName,
			"authoringVersion": 1, "specVersion": 1, "implVersion": 0,
			"apis": [][]interface{}{}, "transactionVersion": 1, "stateVersion": 1,
		}}
	case "state_getMetadata":
		return &rpcResp{JSONRPC: "2.0", ID: id, Result: "0x" + s.chainName}
	case "state_subscribeRuntimeVersion", "state_subscribeMetadata":
		sid := s.newSubID(subCh)
		*subIDs = append(*subIDs, sid)
		return &rpcResp{JSONRPC: "2.0", ID: id, Result: sid}
	case "state_unsubscribeRuntimeVersion", "state_unsubscribeMetadata":
		return &rpcResp{JSONRPC: "2.0", ID: id, Result: true}
	case "papucoin_chainParams", "papucoin_balance", "papucoin_supply", "papucoin_submit", "papucoin_faucet", "jam_stateRoot", "jam_serviceAccount", "jam_getStorage", "jam_getHeader", "jam_getTelemetry", "jam_getWorks", "jam_getBlockLog", "jam_joinAccumulate",
		"eth_chainId", "eth_blockNumber", "eth_getBalance", "eth_getTransactionCount",
		"eth_gasPrice", "eth_estimateGas", "eth_getBlockByNumber", "eth_sendRawTransaction", "eth_call":
		return s.papucoinCall(req)
	case "system_syncState":
		return &rpcResp{JSONRPC: "2.0", ID: id, Result: map[string]interface{}{
			"currentBlock": fmt.Sprintf("0x%x", s.blockNum),
			"highestBlock": fmt.Sprintf("0x%x", s.blockNum),
		}}
	case "system_accountNextIndex":
		return &rpcResp{JSONRPC: "2.0", ID: id, Result: "0x0"}
	default:
		return &rpcResp{
			JSONRPC: "2.0", ID: id,
			Error: &rpcErr{Code: -32601, Message: fmt.Sprintf("Method not found: %s", req.Method)},
		}
	}
}

// papucoinCall dispatches the economy methods. They are all answered from the
// state the node committed, never from a cache the RPC keeps of its own.
func (s *rpcServer) papucoinCall(req rpcReq) *rpcResp {
	if s.papucoin == nil {
		return &rpcResp{JSONRPC: "2.0", ID: req.ID, Error: &rpcErr{Code: -32601, Message: "this node has no PAPU service"}}
	}

	params, err := paramsOf(req.Params)
	if err != nil {
		return &rpcResp{JSONRPC: "2.0", ID: req.ID, Error: &rpcErr{Code: -32602, Message: err.Error()}}
	}

	// Everything below answers about the chain as it is, and the chain is not
	// this node yet until it has rebuilt the state its own blocks describe. A node
	// that answered during its own replay would be describing a state no block
	// ever named, so the answer waits instead.
	if readsTheChain(req.Method) {
		s.waitRebuilt()
	}

	var result any
	switch req.Method {
	case "papucoin_chainParams":
		result, err = s.papucoin.chainParams()
	case "papucoin_balance":
		result, err = s.papucoin.balance(params)
	case "papucoin_supply":
		result, err = s.papucoin.supply()
	case "papucoin_submit":
		result, err = s.papucoin.submit(params)
	case "papucoin_faucet":
		result, err = s.papucoin.faucet(params)
	case "jam_getHeader":
		result, err = s.papucoin.header()
	case "jam_getTelemetry":
		return s.papucoin.telemetryCall(req)
	case "jam_getWorks":
		return s.papucoin.worksCall(req)
	case "jam_getBlockLog":
		return s.papucoin.blockLogCall(req)
	case "jam_joinAccumulate":
		return s.papucoin.joinAccumulateCall(req)
	case "eth_chainId":
		result, err = s.papucoin.evmChainID()
	case "eth_blockNumber":
		result, err = s.papucoin.evmBlockNumber()
	case "eth_getBalance":
		result, err = s.papucoin.evmGetBalance(params)
	case "eth_getTransactionCount":
		result, err = s.papucoin.evmGetTransactionCount(params)
	case "eth_gasPrice":
		result, err = s.papucoin.evmGasPrice()
	case "eth_getBlockByNumber":
		result, err = s.papucoin.evmGetBlockByNumber(params)
	case "eth_sendRawTransaction":
		result, err = s.papucoin.sendRawTransaction(params)
	case "eth_estimateGas":
		result, err = s.papucoin.evmEstimateGas(params)
	case "eth_call":
		result, err = s.papucoin.evmCall()
	case "jam_stateRoot":
		result, err = s.papucoin.stateRoot()
	case "jam_serviceAccount":
		result, err = s.papucoin.serviceAccount()
	case "jam_getStorage":
		result, err = s.papucoin.storage(params)
	default:
		return &rpcResp{JSONRPC: "2.0", ID: req.ID, Error: &rpcErr{Code: -32601, Message: fmt.Sprintf("Method not found: %s", req.Method)}}
	}

	if err != nil {
		return &rpcResp{JSONRPC: "2.0", ID: req.ID, Error: &rpcErr{Code: -32000, Message: err.Error()}}
	}
	return &rpcResp{JSONRPC: "2.0", ID: req.ID, Result: result}
}
