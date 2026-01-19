package main

import (
	"context"
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

func main() {
	var (
		authKey     string
		controlURL  string
		hostname    string
		port        string
		stateDir    string
		mode        string
	)

	flag.StringVar(&authKey, "authkey", "", "Tailscale Auth Key")
	flag.StringVar(&controlURL, "coordserver", "", "Coordination Server URL")
	flag.StringVar(&hostname, "hostname", "ts-proxy", "Hostname in the Tailnet")
	flag.StringVar(&port, "port", "8080", "Port to listen on")
	flag.StringVar(&stateDir, "statedir", "", "State directory (defaults to current working directory)")
	flag.StringVar(&mode, "mode", "http", "Proxy mode: 'http' or 'socks5'")
	flag.Parse()

	fmt.Printf("Arkitekt Sidecar %s\n", version)

	// 1. Setup State Directory (prevents re-login on restart)
	if stateDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			log.Fatalf("!!! Failed to get current working directory: %v", err)
		}
		stateDir = cwd
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
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
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	
	if _, err := s.Up(ctx); err != nil {
		log.Fatalf("!!! Failed to connect to Tailnet: %v", err)
	}
	fmt.Println(">>> Tailscale is Online!")

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
		log.Fatal(http.ListenAndServe(addr, proxy))

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
			log.Fatalf("!!! Failed to create SOCKS5 server: %v", err)
		}
		log.Fatal(socks5Server.ListenAndServe("tcp", addr))

	default:
		log.Fatalf("!!! Unknown mode '%s'. Use 'http' or 'socks5'", mode)
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