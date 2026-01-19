package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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


