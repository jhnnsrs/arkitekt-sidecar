package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/joho/godotenv"
	"tailscale.com/tsnet"
)

// MockDialer implements the Dialer interface
type MockDialer struct {
	DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)
}

func (m *MockDialer) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	return m.DialFunc(ctx, network, addr)
}

// MockRoundTripper implements http.RoundTripper
type MockRoundTripper struct {
	RoundTripFunc func(req *http.Request) (*http.Response, error)
}

func (m *MockRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return m.RoundTripFunc(req)
}

func TestHandleHTTP(t *testing.T) {
	mockRT := &MockRoundTripper{
		RoundTripFunc: func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader("OK from Tailscale")),
				Header:     make(http.Header),
			}, nil
		},
	}

	proxy := &TailscaleProxy{
		Transport: mockRT,
	}

	req := httptest.NewRequest("GET", "http://example.com/foo", nil)
	w := httptest.NewRecorder()

	proxy.handleHTTP(w, req)

	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	if string(body) != "OK from Tailscale" {
		t.Errorf("Expected body 'OK from Tailscale', got '%s'", string(body))
	}
}

// MockHijackRecorder to test CONNECT
type MockHijackRecorder struct {
	*httptest.ResponseRecorder
	ClientConn net.Conn
	ServerConn net.Conn
}

func (m *MockHijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return m.ClientConn, bufio.NewReadWriter(bufio.NewReader(m.ClientConn), bufio.NewWriter(m.ClientConn)), nil
}

// skipOnCI skips the test if running on GitHub Actions
func skipOnCI(t *testing.T) {
	if os.Getenv("GITHUB_ACTIONS") == "true" || os.Getenv("CI") == "true" {
		t.Skip("Skipping integration test on CI")
	}
}

// loadTestEnv loads environment variables from .env file
func loadTestEnv(t *testing.T) (coordServer, authKey, testServer string) {
	if err := godotenv.Load(); err != nil {
		t.Fatalf("Failed to load .env file: %v", err)
	}

	coordServer = strings.Trim(os.Getenv("TEST_COORD_SERVER"), "\" ")
	authKey = strings.Trim(os.Getenv("TEST_AUTH_KEY"), "\" ")
	testServer = strings.Trim(os.Getenv("TEST_SERVER"), "\" ")

	if coordServer == "" || authKey == "" || testServer == "" {
		t.Fatal("TEST_COORD_SERVER, TEST_AUTH_KEY, and TEST_SERVER must be set in .env")
	}

	return coordServer, authKey, testServer
}

// TestIntegrationTailscaleConnection tests that we can connect to the Tailscale network
func TestIntegrationTailscaleConnection(t *testing.T) {
	skipOnCI(t)

	coordServer, authKey, _ := loadTestEnv(t)

	// Create temporary state directory for test
	stateDir, err := os.MkdirTemp("", "tsnet-test-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(stateDir)

	s := &tsnet.Server{
		Hostname:   "test-integration",
		AuthKey:    authKey,
		ControlURL: coordServer,
		Dir:        stateDir,
		Logf:       func(format string, args ...any) {},
	}
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	status, err := s.Up(ctx)
	if err != nil {
		t.Fatalf("Failed to connect to Tailnet: %v", err)
	}

	if status.BackendState != "Running" {
		t.Errorf("Expected BackendState 'Running', got '%s'", status.BackendState)
	}

	t.Logf("Successfully connected to Tailnet with IP: %v", status.TailscaleIPs)
}

// TestIntegrationDialServer tests that we can dial a server on the Tailnet
func TestIntegrationDialServer(t *testing.T) {
	skipOnCI(t)

	coordServer, authKey, testServer := loadTestEnv(t)

	// Create temporary state directory for test
	stateDir, err := os.MkdirTemp("", "tsnet-test-dial-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(stateDir)

	s := &tsnet.Server{
		Hostname:   "test-dial",
		AuthKey:    authKey,
		ControlURL: coordServer,
		Dir:        stateDir,
		Logf:       func(format string, args ...any) {},
	}
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := s.Up(ctx); err != nil {
		t.Fatalf("Failed to connect to Tailnet: %v", err)
	}

	// Try to dial the test server
	dialCtx, dialCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer dialCancel()

	conn, err := s.Dial(dialCtx, "tcp", fmt.Sprintf("%s:80", testServer))
	if err != nil {
		t.Fatalf("Failed to dial %s:80 via Tailscale: %v", testServer, err)
	}
	defer conn.Close()

	t.Logf("Successfully dialed %s via Tailscale", testServer)
}

// TestIntegrationHTTPProxy tests the HTTP proxy functionality against the test server
func TestIntegrationHTTPProxy(t *testing.T) {
	skipOnCI(t)

	coordServer, authKey, testServer := loadTestEnv(t)

	// Create temporary state directory for test
	stateDir, err := os.MkdirTemp("", "tsnet-test-proxy-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(stateDir)

	s := &tsnet.Server{
		Hostname:   "test-proxy",
		AuthKey:    authKey,
		ControlURL: coordServer,
		Dir:        stateDir,
		Logf:       func(format string, args ...any) {},
	}
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := s.Up(ctx); err != nil {
		t.Fatalf("Failed to connect to Tailnet: %v", err)
	}

	// Create the proxy with Tailscale transport
	tsTransport := &http.Transport{
		DialContext: s.Dial,
	}

	proxy := &TailscaleProxy{
		Dialer:    s,
		Transport: tsTransport,
	}

	// Start proxy server
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()

	// Create a client that uses our proxy
	proxyClient := &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(mustParseURL(proxyServer.URL)),
		},
		Timeout: 30 * time.Second,
	}

	// Make a request through the proxy to the test server
	resp, err := proxyClient.Get(fmt.Sprintf("http://%s/", testServer))
	if err != nil {
		t.Fatalf("Failed to make request through proxy: %v", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	t.Logf("Response from %s: status=%d, body length=%d", testServer, resp.StatusCode, len(body))

	if resp.StatusCode >= 500 {
		t.Errorf("Expected successful response, got status %d", resp.StatusCode)
	}
}

func mustParseURL(rawURL string) *url.URL {
	u, err := url.Parse(rawURL)
	if err != nil {
		panic(err)
	}
	return u
}

// --- STATUS API TESTS ---

func TestPeerStatusJSON(t *testing.T) {
	peer := PeerStatus{
		Name:          "test-node.tailnet.ts.net",
		HostName:      "test-node",
		TailscaleIPs:  []string{"100.64.0.1", "fd7a:115c:a1e0::1"},
		Online:        true,
		Direct:        true,
		RelayedVia:    "",
		CurAddr:       "192.168.1.100:41641",
		RxBytes:       12345,
		TxBytes:       67890,
		LastSeen:      "2026-01-19T20:30:00Z",
		LastHandshake: "2026-01-19T20:29:55Z",
	}

	data, err := json.Marshal(peer)
	if err != nil {
		t.Fatalf("Failed to marshal PeerStatus: %v", err)
	}

	var decoded PeerStatus
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal PeerStatus: %v", err)
	}

	if decoded.Name != peer.Name {
		t.Errorf("Expected Name '%s', got '%s'", peer.Name, decoded.Name)
	}
	if decoded.Direct != peer.Direct {
		t.Errorf("Expected Direct %v, got %v", peer.Direct, decoded.Direct)
	}
	if len(decoded.TailscaleIPs) != 2 {
		t.Errorf("Expected 2 IPs, got %d", len(decoded.TailscaleIPs))
	}
}

func TestStatusResponseJSON(t *testing.T) {
	response := StatusResponse{
		Self: PeerStatus{
			Name:         "my-proxy.tailnet.ts.net",
			HostName:     "my-proxy",
			TailscaleIPs: []string{"100.64.0.5"},
			Online:       true,
		},
		Peers: []PeerStatus{
			{
				Name:         "peer1.tailnet.ts.net",
				HostName:     "peer1",
				TailscaleIPs: []string{"100.64.0.10"},
				Online:       true,
				Direct:       true,
				CurAddr:      "10.0.0.50:41641",
			},
			{
				Name:         "peer2.tailnet.ts.net",
				HostName:     "peer2",
				TailscaleIPs: []string{"100.64.0.20"},
				Online:       true,
				Direct:       false,
				RelayedVia:   "nyc",
			},
		},
		BackendState: "Running",
	}

	data, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("Failed to marshal StatusResponse: %v", err)
	}

	var decoded StatusResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Failed to unmarshal StatusResponse: %v", err)
	}

	if decoded.BackendState != "Running" {
		t.Errorf("Expected BackendState 'Running', got '%s'", decoded.BackendState)
	}
	if decoded.Self.HostName != "my-proxy" {
		t.Errorf("Expected Self.HostName 'my-proxy', got '%s'", decoded.Self.HostName)
	}
	if len(decoded.Peers) != 2 {
		t.Errorf("Expected 2 peers, got %d", len(decoded.Peers))
	}

	// Check direct vs relayed
	if !decoded.Peers[0].Direct {
		t.Error("Expected peer1 to be direct")
	}
	if decoded.Peers[1].Direct {
		t.Error("Expected peer2 to NOT be direct")
	}
	if decoded.Peers[1].RelayedVia != "nyc" {
		t.Errorf("Expected peer2 RelayedVia 'nyc', got '%s'", decoded.Peers[1].RelayedVia)
	}
}

func TestStatusResponseDirectDetection(t *testing.T) {
	tests := []struct {
		name       string
		curAddr    string
		relay      string
		wantDirect bool
	}{
		{
			name:       "direct connection with address",
			curAddr:    "192.168.1.100:41641",
			relay:      "",
			wantDirect: true,
		},
		{
			name:       "relayed connection",
			curAddr:    "",
			relay:      "nyc",
			wantDirect: false,
		},
		{
			name:       "relayed with addr (edge case)",
			curAddr:    "10.0.0.1:41641",
			relay:      "fra",
			wantDirect: false,
		},
		{
			name:       "no address no relay (offline)",
			curAddr:    "",
			relay:      "",
			wantDirect: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Simulate the logic from startStatusServer
			isDirect := tc.curAddr != "" && tc.relay == ""
			if isDirect != tc.wantDirect {
				t.Errorf("Expected direct=%v, got %v", tc.wantDirect, isDirect)
			}
		})
	}
}

// TestIntegrationStatusAPI tests the status API with a real Tailscale connection
func TestIntegrationStatusAPI(t *testing.T) {
	skipOnCI(t)

	coordServer, authKey, _ := loadTestEnv(t)

	// Create temporary state directory for test
	stateDir, err := os.MkdirTemp("", "tsnet-test-status-*")
	if err != nil {
		t.Fatalf("Failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(stateDir)

	s := &tsnet.Server{
		Hostname:   "test-status-api",
		AuthKey:    authKey,
		ControlURL: coordServer,
		Dir:        stateDir,
		Logf:       func(format string, args ...any) {},
	}
	defer s.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := s.Up(ctx); err != nil {
		t.Fatalf("Failed to connect to Tailnet: %v", err)
	}

	// Start status server on a random port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Failed to create listener: %v", err)
	}
	statusAddr := listener.Addr().String()
	listener.Close()

	// Extract port from address
	_, port, _ := net.SplitHostPort(statusAddr)

	// Start status server in background
	go startStatusServer(s, port)

	// Give the server time to start
	time.Sleep(100 * time.Millisecond)

	// Test /health endpoint
	healthResp, err := http.Get(fmt.Sprintf("http://%s/health", statusAddr))
	if err != nil {
		t.Fatalf("Failed to call /health: %v", err)
	}
	defer healthResp.Body.Close()

	if healthResp.StatusCode != http.StatusOK {
		t.Errorf("Expected health status 200, got %d", healthResp.StatusCode)
	}

	// Test /status endpoint
	statusResp, err := http.Get(fmt.Sprintf("http://%s/status", statusAddr))
	if err != nil {
		t.Fatalf("Failed to call /status: %v", err)
	}
	defer statusResp.Body.Close()

	if statusResp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", statusResp.StatusCode)
	}

	var response StatusResponse
	if err := json.NewDecoder(statusResp.Body).Decode(&response); err != nil {
		t.Fatalf("Failed to decode status response: %v", err)
	}

	// Validate response
	if response.BackendState != "Running" {
		t.Errorf("Expected BackendState 'Running', got '%s'", response.BackendState)
	}

	if response.Self.HostName != "test-status-api" {
		t.Errorf("Expected Self.HostName 'test-status-api', got '%s'", response.Self.HostName)
	}

	if len(response.Self.TailscaleIPs) == 0 {
		t.Error("Expected at least one Tailscale IP for self")
	}

	t.Logf("Status API response: BackendState=%s, Self=%s, Peers=%d",
		response.BackendState, response.Self.HostName, len(response.Peers))

	// Log peer connection details
	for _, peer := range response.Peers {
		connType := "relayed"
		if peer.Direct {
			connType = "direct"
		}
		t.Logf("  Peer: %s (%s) - %s", peer.HostName, peer.TailscaleIPs, connType)
	}
}


