package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"time"
	"github.com/armon/go-socks5"
	"tailscale.com/tsnet"
)

var (
	version = "dev"
)

// Magic words for IPC signaling to parent process
// These can be parsed by a governing process (e.g., Python script) to track state
const (
	SignalStarting      = "@@SIDECAR:STARTING@@"
	SignalConnecting    = "@@SIDECAR:CONNECTING@@"
	SignalConnected     = "@@SIDECAR:CONNECTED@@"
	SignalListening     = "@@SIDECAR:LISTENING@@"
	SignalReady         = "@@SIDECAR:READY@@"
	SignalError         = "@@SIDECAR:ERROR@@"
	SignalShutdown      = "@@SIDECAR:SHUTDOWN@@"
	SignalAuthRequired  = "@@SIDECAR:AUTH_REQUIRED@@"
)

// signal emits a magic word signal for IPC
func signal(sig string, details ...string) {
	if len(details) > 0 {
		fmt.Printf("%s %s\n", sig, details[0])
	} else {
		fmt.Println(sig)
	}
}

func main() {
	var (
		authKey     string
		controlURL  string
		hostname    string
		port        string
		stateDir    string
		mode        string
		statusPort  string
	)

	flag.StringVar(&authKey, "authkey", "", "Tailscale Auth Key")
	flag.StringVar(&controlURL, "coordserver", "", "Coordination Server URL")
	flag.StringVar(&hostname, "hostname", "ts-proxy", "Hostname in the Tailnet")
	flag.StringVar(&port, "port", "8080", "Port to listen on")
	flag.StringVar(&stateDir, "statedir", "", "State directory (defaults to current working directory)")
	flag.StringVar(&mode, "mode", "http", "Proxy mode: 'http' or 'socks5'")
	flag.StringVar(&statusPort, "statusport", "", "Port for status API (disabled if empty)")
	flag.Parse()

	fmt.Printf("Arkitekt Sidecar %s\n", version)
	signal(SignalStarting, version)

	// 1. Setup State Directory (prevents re-login on restart)
	if stateDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			signal(SignalError, fmt.Sprintf("failed to get cwd: %v", err))
			log.Fatalf("!!! Failed to get current working directory: %v", err)
		}
		stateDir = cwd
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		signal(SignalError, fmt.Sprintf("failed to create state dir: %v", err))
		log.Fatalf("!!! Failed to create state directory: %v", err)
	}

	// 2. Configure the embedded Tailscale Node
	s := &tsnet.Server{
		Hostname:   hostname,
		AuthKey:    authKey,
		ControlURL: controlURL,
		Dir:        stateDir,
		Logf: func(format string, args ...any) {
			// Uncomment to see verbose Tailscale logs
			// fmt.Fprintf(os.Stderr, "[Tailscale] "+format+"\n", args...)
		},
	}
	defer s.Close()

	// Wait for the node to come online
	fmt.Printf(">>> Starting Tailscale Node '%s'...\n", hostname)
	signal(SignalConnecting, hostname)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	
	status, err := s.Up(ctx)
	if err != nil {
		signal(SignalError, fmt.Sprintf("tailnet connection failed: %v", err))
		log.Fatalf("!!! Failed to connect to Tailnet: %v", err)
	}
	fmt.Println(">>> Tailscale is Online!")
	signal(SignalConnected, fmt.Sprintf("ips=%v", status.TailscaleIPs))

	// Start status API if enabled
	if statusPort != "" {
		go startStatusServer(s, statusPort)
	}

	// 3. Create the Proxy Handler
	// We create a custom HTTP transport that uses the Tailscale Dialer
	tsTransport := &http.Transport{
		DialContext: s.Dial, // <--- THE MAGIC: Dials via Tailscale
	}

	proxy := &TailscaleProxy{
		Dialer:    s,
		Transport: tsTransport,
	}

	// 4. Start the Server based on mode
	addr := fmt.Sprintf("127.0.0.1:%s", port)

	switch mode {
	case "http":
		fmt.Printf(">>> HTTP Proxy listening on %s\n", addr)
		fmt.Printf(">>> Configure your apps to use HTTP Proxy: %s\n", addr)
		signal(SignalListening, fmt.Sprintf("mode=http addr=%s", addr))
		signal(SignalReady, fmt.Sprintf("http://%s", addr))
		if err := http.ListenAndServe(addr, proxy); err != nil {
			signal(SignalError, fmt.Sprintf("http server failed: %v", err))
			log.Fatal(err)
		}

	case "socks5":
		fmt.Printf(">>> SOCKS5 Proxy listening on %s\n", addr)
		fmt.Printf(">>> Configure your apps to use SOCKS5 Proxy: %s\n", addr)

		// Create SOCKS5 server with Tailscale dialer
		conf := &socks5.Config{
			Dial: func(ctx context.Context, network, addr string) (net.Conn, error) {
				fmt.Printf("[SOCKS5] Dialing %s via Tailscale\n", addr)
				return s.Dial(ctx, network, addr)
			},
		}
		socks5Server, err := socks5.New(conf)
		if err != nil {
			signal(SignalError, fmt.Sprintf("socks5 server creation failed: %v", err))
			log.Fatalf("!!! Failed to create SOCKS5 server: %v", err)
		}
		signal(SignalListening, fmt.Sprintf("mode=socks5 addr=%s", addr))
		signal(SignalReady, fmt.Sprintf("socks5://%s", addr))
		if err := socks5Server.ListenAndServe("tcp", addr); err != nil {
			signal(SignalError, fmt.Sprintf("socks5 server failed: %v", err))
			log.Fatal(err)
		}

	default:
		signal(SignalError, fmt.Sprintf("unknown mode: %s", mode))
		log.Fatalf("!!! Unknown mode '%s'. Use 'http' or 'socks5'", mode)
	}
}

// --- STATUS API ---

// PeerStatus represents the connection status to a peer
type PeerStatus struct {
	Name           string   `json:"name"`
	HostName       string   `json:"hostname"`
	TailscaleIPs   []string `json:"tailscale_ips"`
	Online         bool     `json:"online"`
	Direct         bool     `json:"direct"`          // true if connection is direct (not relayed)
	RelayedVia     string   `json:"relayed_via"`     // DERP region if relayed
	CurAddr        string   `json:"current_address"` // current endpoint address
	RxBytes        int64    `json:"rx_bytes"`
	TxBytes        int64    `json:"tx_bytes"`
	LastSeen       string   `json:"last_seen"`
	LastHandshake  string   `json:"last_handshake"`
}

// StatusResponse is the full status response
type StatusResponse struct {
	Self       PeerStatus   `json:"self"`
	Peers      []PeerStatus `json:"peers"`
	BackendState string     `json:"backend_state"`
}

func startStatusServer(s *tsnet.Server, port string) {
	mux := http.NewServeMux()
	
	mux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		lc, err := s.LocalClient()
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to get local client: %v", err), http.StatusInternalServerError)
			return
		}

		status, err := lc.Status(r.Context())
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to get status: %v", err), http.StatusInternalServerError)
			return
		}

		response := StatusResponse{
			BackendState: status.BackendState,
		}

		// Self info
		if status.Self != nil {
			ips := make([]string, len(status.Self.TailscaleIPs))
			for i, ip := range status.Self.TailscaleIPs {
				ips[i] = ip.String()
			}
			response.Self = PeerStatus{
				Name:         status.Self.DNSName,
				HostName:     status.Self.HostName,
				TailscaleIPs: ips,
				Online:       status.Self.Online,
			}
		}

		// Peer info
		for _, peer := range status.Peer {
			ips := make([]string, len(peer.TailscaleIPs))
			for i, ip := range peer.TailscaleIPs {
				ips[i] = ip.String()
			}

			// Determine if connection is direct
			// If CurAddr is empty or starts with "127.3." it's relayed through DERP
			isDirect := peer.CurAddr != "" && peer.Relay == ""

			relayedVia := ""
			if peer.Relay != "" {
				relayedVia = peer.Relay
			}

			lastSeen := ""
			if !peer.LastSeen.IsZero() {
				lastSeen = peer.LastSeen.Format(time.RFC3339)
			}

			lastHandshake := ""
			if !peer.LastHandshake.IsZero() {
				lastHandshake = peer.LastHandshake.Format(time.RFC3339)
			}

			response.Peers = append(response.Peers, PeerStatus{
				Name:          peer.DNSName,
				HostName:      peer.HostName,
				TailscaleIPs:  ips,
				Online:        peer.Online,
				Direct:        isDirect,
				RelayedVia:    relayedVia,
				CurAddr:       peer.CurAddr,
				RxBytes:       peer.RxBytes,
				TxBytes:       peer.TxBytes,
				LastSeen:      lastSeen,
				LastHandshake: lastHandshake,
			})
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(response)
	})

	// Simple health check
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	statusAddr := fmt.Sprintf("127.0.0.1:%s", port)
	fmt.Printf(">>> Status API listening on http://%s/status\n", statusAddr)
	if err := http.ListenAndServe(statusAddr, mux); err != nil {
		log.Printf("Status server failed: %v", err)
	}
}

// --- PROXY IMPLEMENTATION ---

type Dialer interface {
	Dial(ctx context.Context, network, addr string) (net.Conn, error)
}

type TailscaleProxy struct {
	Dialer    Dialer
	Transport http.RoundTripper
}

func (p *TailscaleProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Log the request
	fmt.Printf("[%s] %s %s\n", r.RemoteAddr, r.Method, r.URL)

	if r.Method == http.MethodConnect {
		p.handleTunnel(w, r)
	} else {
		p.handleHTTP(w, r)
	}
}

// handleHTTP proxies standard HTTP requests (e.g. GET http://internal-host/...)
func (p *TailscaleProxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	// Construct the upstream request
	// r.RequestURI is technically not allowed to be set in client requests
	r.RequestURI = "" 
	
	// Use the transport that dials via Tailscale
	resp, err := p.Transport.RoundTrip(r)
	if err != nil {
		http.Error(w, fmt.Sprintf("Proxy Error: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy Headers
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	// Copy Body
	io.Copy(w, resp.Body)
}

// handleTunnel proxies HTTPS requests using the CONNECT method
func (p *TailscaleProxy) handleTunnel(w http.ResponseWriter, r *http.Request) {
	// 1. Hijack the connection to get raw TCP access to the client
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Hijacking not supported", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer clientConn.Close()

	// 2. Dial the destination via Tailscale
	targetConn, err := p.Dialer.Dial(context.Background(), "tcp", r.Host)
	if err != nil {
		fmt.Printf("Dial failed: %v\n", err)
		clientConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\n\r\n"))
		return
	}
	defer targetConn.Close()

	// 3. Tell client the tunnel is established
	clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))

	// 4. Pipe data in both directions
	go io.Copy(targetConn, clientConn)
	io.Copy(clientConn, targetConn)
}