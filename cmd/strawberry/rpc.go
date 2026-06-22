package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/chain"
	"github.com/eigerco/strawberry/internal/crypto"
	"github.com/eigerco/strawberry/internal/store"
	"github.com/eigerco/strawberry/pkg/log"
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
}

type subscribeEvent struct {
	method string
	params interface{}
}

func startRPCServer(addr string, nodeName, chainName, nodeVersion string, chainStore *store.Chain, bs *chain.BlockService) *rpcServer {
	srv := &rpcServer{
		nodeName:    nodeName,
		chainName:   chainName,
		nodeVersion: nodeVersion,
		chainStore:  chainStore,
		bs:          bs,
		subs:        make(map[uint]chan<- subscribeEvent),
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
	log.Internal.Info().Msgf("RPC server listening on ws://%s and http://%s", addr, addr)
	go func() {
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Internal.Error().Err(err).Msg("RPC server failed")
		}
	}()
	return srv
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
			},
		}}
	case "system_chain":
		return &rpcResp{JSONRPC: "2.0", ID: id, Result: s.chainName}
	case "system_name":
		return &rpcResp{JSONRPC: "2.0", ID: id, Result: s.nodeName}
	case "system_version":
		return &rpcResp{JSONRPC: "2.0", ID: id, Result: s.nodeVersion}
	case "system_health":
		return &rpcResp{JSONRPC: "2.0", ID: id, Result: map[string]interface{}{
			"peers": 0, "isSyncing": false, "shouldHavePeers": false,
		}}
	case "system_peers":
		return &rpcResp{JSONRPC: "2.0", ID: id, Result: []interface{}{}}
	case "system_localListenAddresses":
		return &rpcResp{JSONRPC: "2.0", ID: id, Result: []string{}}
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
