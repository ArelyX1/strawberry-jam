package main

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/eigerco/strawberry/pkg/log"
	"golang.org/x/net/websocket"
)

type telemetryClient struct {
	url      string
	nodeName string
	version  string
	chain    string
	ws       *websocket.Conn
	done     chan struct{}
}

type telemetryMsg struct {
	ID      int           `json:"id"`
	JSONRPC string        `json:"jsonrpc"`
	Method  string        `json:"method"`
	Params  []interface{} `json:"params"`
}

func startTelemetry(url, nodeName, version, chain string) {
	if url == "" {
		return
	}
	tc := &telemetryClient{
		url:      url,
		nodeName: nodeName,
		version:  version,
		chain:    chain,
		done:     make(chan struct{}),
	}
	go tc.run()
}

func (tc *telemetryClient) run() {
	for {
		err := tc.connect()
		if err != nil {
			log.Internal.Warn().Err(err).Msg("telemetry connection failed, retrying in 30s")
		}
		select {
		case <-tc.done:
			return
		case <-time.After(30 * time.Second):
		}
	}
}

func (tc *telemetryClient) connect() error {
	ws, err := websocket.Dial(tc.url, "", "https://telemetry.polkadot.io")
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer ws.Close()
	tc.ws = ws

	// Send system.connected
	msg := telemetryMsg{
		ID:      1,
		JSONRPC: "2.0",
		Method:  "system.connected",
		Params:  []interface{}{tc.nodeName, tc.version, tc.chain, "", 0, 0, 0},
	}
	b, _ := json.Marshal(msg)
	if err := websocket.Message.Send(ws, string(b)); err != nil {
		return fmt.Errorf("send connected: %w", err)
	}
	log.Internal.Info().Msg("telemetry connected")

	// Keep connection alive
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-tc.done:
			return nil
		case <-ticker.C:
			// Read any incoming messages (or ignore)
			var raw string
			_ = websocket.Message.Receive(ws, &raw)
		}
	}
}
