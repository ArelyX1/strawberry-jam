package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// handleWeb is what / becomes when the node mounts a web panel (--www-dir).
// The RPC port then serves the page itself: the web lives in the network, so
// opening any node's address gives the panel, and everything is same-origin
// because this node also proxies /rpc to the peers it discovered. A device
// only needs a browser (and MetaMask for /connect); it does not run a node.
func (s *rpcServer) handleWeb(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if r.Method == http.MethodPost {
		if path == "/rpc" {
			s.handleRPCProxy(w, r)
			return
		}
		if path == "/" {
			// The RPC proper still answers here: a wallet pointed at the node
			// calls this address directly.
			s.handleHTTP(w, r)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if path == "/nodes" {
		s.serveNodes(w, r)
		return
	}
	// static: / serves index.html, /connect its index, /_astro the assets
	http.FileServer(http.Dir(s.wwwRoot)).ServeHTTP(w, r)
}

// serveNodes is what the panel asks to know its seed. A node hosts a single
// seed: itself, at the origin the browser came from. The panel discovers the
// rest through network_map.
func (s *rpcServer) serveNodes(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if host == "" {
		host = "127.0.0.1"
	}
	// El Host puede traer tambien el puerto; eso ya es lo que el navegador uso
	// para llegar aqui, asi que es la URL correcta para el mismo origen.
	url := "http://" + host
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"nodes":   []map[string]string{{"id": "0", "url": url}},
		"default": "0",
		"source":  "node",
		"scanned": "",
	})
}

// handleRPCProxy reenvia /rpc a donde diga ?node=: una URL completa (un peer
// que el panel descubrio con network_map), o este mismo nodo. Sin parametro,
// va a este nodo. Todo sale server-side, asi el navegador solo habla con el
// origen que le sirvio la pagina.
func (s *rpcServer) handleRPCProxy(w http.ResponseWriter, r *http.Request) {
	which := r.URL.Query().Get("node")
	target, self := s.proxyTarget(which)
	if self {
		// Este mismo nodo: se responde sin pasar por un segundo HTTP.
		s.handleHTTP(w, r)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.serverUnreachable(w, target, "no se pudo leer la peticion: "+err.Error())
		return
	}
	req, err := http.NewRequest(r.Method, target, strings.NewReader(string(body)))
	if err != nil {
		s.serverUnreachable(w, target, err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		s.serverUnreachable(w, target, err.Error())
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// proxyTarget resuelve ?node= hacia una URL o hacia este nodo. Los indices son
// de este nodo (es el unico que el nodo conoce): "0", vacio, o cualquier
// numero van a el. Una URL completa va ahi.
func (s *rpcServer) proxyTarget(which string) (target string, self bool) {
	if which == "" || which == "0" || (which[0] >= '0' && which[0] <= '9') {
		return s.selfURL(), true
	}
	if strings.HasPrefix(which, "http://") || strings.HasPrefix(which, "https://") {
		return which, false
	}
	return s.selfURL(), true
}

// selfURL is this node's own RPC, reachable from itself. The addr is ":9944";
// on loopback that means 127.0.0.1.
func (s *rpcServer) selfURL() string {
	port := s.rpcPort()
	if port <= 0 {
		port = 9944
	}
	return fmt.Sprintf("http://127.0.0.1:%d/", port)
}

// serverUnreachable escribe el mismo cuerpo que el proxy del panel escribia
// cuando no habia nadie escuchando, para que la pagina lea "apagado".
func (s *rpcServer) serverUnreachable(w http.ResponseWriter, target, why string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusServiceUnavailable)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"error":   map[string]interface{}{"code": -32000, "message": fmt.Sprintf("nodo inaccesible en %s: %s", target, why)},
	})
}
