package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/achmadss/hanse/internal/config"
)

// startTestGateway starts a Gateway on a loopback port the operating system
// chooses, and stops it when the test ends.
func startTestGateway(t *testing.T) *gateway {
	t.Helper()
	dir := configFixture(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	g, err := startGateway(ctx, filepath.Join(dir, "gateway.yaml"), startOptions{
		listen: "127.0.0.1:0",
		stderr: io.Discard,
	})
	if err != nil {
		cancel()
		t.Fatalf("start the Gateway: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-g.Done():
			if err != nil {
				t.Errorf("shutdown: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the Gateway did not stop")
		}
	})
	return g
}

// probeURL performs one unauthenticated GET request and returns the status
// and body.
func probeURL(t *testing.T, url string) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s: %v", url, err)
	}
	return response.StatusCode, string(body)
}

// TestStartGatewayRefusesInvalidConfig requires every configuration problem
// to stop the server before it opens a listener, exactly as --check-config
// reports it.
func TestStartGatewayRefusesInvalidConfig(t *testing.T) {
	dir := configFixture(t, "timeuouts:\n  operation: 5m\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g, err := startGateway(ctx, filepath.Join(dir, "gateway.yaml"), startOptions{
		listen: "127.0.0.1:0",
		stderr: io.Discard,
	})
	if err == nil {
		cancel()
		if err := <-g.Done(); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		t.Fatal("the Gateway started with an invalid configuration")
	}
	if !strings.Contains(err.Error(), "configuration problem(s)") {
		t.Errorf("error = %q, want it to name the configuration problems", err)
	}
	if !strings.Contains(err.Error(), filepath.Join(dir, "gateway.yaml")) {
		t.Errorf("error = %q, want it to name the configuration file", err)
	}
	if !strings.Contains(err.Error(), "field timeuouts not found") {
		t.Errorf("error = %q, want the unknown-field problem", err)
	}
}

// TestStartGatewayRefusesNonLoopbackPlainHTTP requires the transport gate:
// no TLS and no allow_insecure_http means loopback only.
func TestStartGatewayRefusesNonLoopbackPlainHTTP(t *testing.T) {
	dir := t.TempDir()
	writeConfigTree(t, dir, "listen: 0.0.0.0:8443\npublic_url: http://192.0.2.10:8443\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g, err := startGateway(ctx, filepath.Join(dir, "gateway.yaml"), startOptions{stderr: io.Discard})
	if err == nil {
		cancel()
		if err := <-g.Done(); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		t.Fatal("the Gateway listened on a non-loopback address without TLS")
	}
	if !strings.Contains(err.Error(), "must be a loopback address") {
		t.Errorf("error = %q, want the loopback rule from the configuration check", err)
	}
}

// TestCheckTransport covers the transport gate directly, including the
// settings that make a non-loopback address acceptable.
func TestCheckTransport(t *testing.T) {
	tls := &config.GatewayTLS{CertFile: "/etc/hanse/tls/cert.pem", KeyFile: "/etc/hanse/tls/key.pem"}
	cases := []struct {
		name    string
		listen  string
		cfg     config.GatewayConfig
		wantErr bool
	}{
		{name: "loopback", listen: "127.0.0.1:8443"},
		{name: "loopback IPv6", listen: "[::1]:8443"},
		{name: "localhost", listen: "localhost:8443"},
		{name: "non-loopback refused", listen: "0.0.0.0:8443", wantErr: true},
		{name: "empty host refused", listen: ":8443", wantErr: true},
		{name: "private address refused", listen: "192.0.2.10:8443", wantErr: true},
		{name: "allow_insecure_http", listen: "0.0.0.0:8443", cfg: config.GatewayConfig{AllowInsecureHTTP: true}},
		{name: "TLS", listen: "0.0.0.0:8443", cfg: config.GatewayConfig{TLS: tls}},
		{name: "bad address", listen: "not-an-address", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkTransport(tc.listen, tc.cfg)
			if tc.wantErr && err == nil {
				t.Fatalf("checkTransport(%q) = nil, want an error", tc.listen)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("checkTransport(%q) = %v, want nil", tc.listen, err)
			}
		})
	}
}

// TestHealthAndReadinessAreUnauthenticated requires both probes to answer
// without a token, while the versioned API keeps its authentication.
func TestHealthAndReadinessAreUnauthenticated(t *testing.T) {
	g := startTestGateway(t)
	base := "http://" + g.Addr

	if status, body := probeURL(t, base+"/healthz"); status != http.StatusOK || body != "ok\n" {
		t.Errorf("GET /healthz = %d %q, want 200 %q", status, body, "ok\n")
	}
	if status, body := probeURL(t, base+"/readyz"); status != http.StatusOK || body != "ready\n" {
		t.Errorf("GET /readyz = %d %q, want 200 %q", status, body, "ready\n")
	}
	if status, _ := probeURL(t, base+"/v1/version"); status != http.StatusUnauthorized {
		t.Errorf("GET /v1/version = %d, want %d", status, http.StatusUnauthorized)
	}
}

// TestGracefulShutdown requires the Gateway to stop accepting connections
// and report a clean exit when its context is cancelled.
func TestGracefulShutdown(t *testing.T) {
	dir := configFixture(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	g, err := startGateway(ctx, filepath.Join(dir, "gateway.yaml"), startOptions{
		listen: "127.0.0.1:0",
		stderr: io.Discard,
	})
	if err != nil {
		cancel()
		t.Fatalf("start the Gateway: %v", err)
	}
	base := "http://" + g.Addr
	if status, _ := probeURL(t, base+"/healthz"); status != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want %d before shutdown", status, http.StatusOK)
	}

	cancel()
	select {
	case err := <-g.Done():
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the Gateway did not stop")
	}

	// The listener is closed, but the port may still accept a queued
	// connection briefly, so the check is a request with a short timeout.
	client := &http.Client{Timeout: 2 * time.Second}
	if response, err := client.Get(base + "/healthz"); err == nil {
		_ = response.Body.Close()
		t.Error("the Gateway still answers after shutdown")
	}
	if _, err := net.DialTimeout("tcp", g.Addr, time.Second); err == nil {
		t.Error("the listen address still accepts connections after shutdown")
	}
}

// writeConfigTree writes a minimal Gateway configuration with the given
// extra gateway.yaml lines. Placeholder addresses only: 192.0.2.0/24 is
// reserved for documentation.
func writeConfigTree(t *testing.T, dir, extra string) {
	t.Helper()
	gw := "version: 1\nnode: gateway\npaths:\n" +
		"  nodes: nodes.yaml\n  routes: routes.yaml\n" +
		"  capabilities:\n    - " + dir + "/capabilities\n" +
		"  credentials_dir: " + dir + "/credentials\n  state_dir: " + dir + "/state\n" +
		extra
	nodes := "version: 1\nnodes:\n  gateway:\n    os: linux\n    arch: amd64\n"
	routes := "version: 1\ncapabilities: {}\n"
	for name, content := range map[string]string{
		"gateway.yaml": gw,
		"nodes.yaml":   nodes,
		"routes.yaml":  routes,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	for _, sub := range []string{"capabilities", "credentials", "state"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatalf("create %s: %v", sub, err)
		}
	}
}

// TestStartGatewayServesTLS requires the Gateway to serve HTTPS when the
// configuration names a certificate, and to load it before it reports ready.
func TestStartGatewayServesTLS(t *testing.T) {
	dir := t.TempDir()
	writeConfigTree(t, dir, "listen: 127.0.0.1:8443\n"+
		"public_url: https://gateway.example.com\n"+
		"tls:\n  cert_file: "+dir+"/tls/cert.pem\n  key_file: "+dir+"/tls/key.pem\n")
	writeSelfSignedCertificate(t, filepath.Join(dir, "tls"), "127.0.0.1")

	ctx, cancel := context.WithCancel(context.Background())
	g, err := startGateway(ctx, filepath.Join(dir, "gateway.yaml"), startOptions{
		listen: "127.0.0.1:0",
		stderr: io.Discard,
	})
	if err != nil {
		cancel()
		t.Fatalf("start the Gateway: %v", err)
	}
	defer func() {
		cancel()
		if err := <-g.Done(); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	}()

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			// The certificate is self-signed for this test only.
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	response, err := client.Get("https://" + g.Addr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz over TLS: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read /healthz: %v", err)
	}
	if response.StatusCode != http.StatusOK || string(body) != "ok\n" {
		t.Errorf("GET /healthz = %d %q, want 200 %q", response.StatusCode, body, "ok\n")
	}
}

// TestStartGatewayRefusesABadCertificate requires the certificate to load
// before the Gateway reports ready, so a corrupt key never starts a server.
func TestStartGatewayRefusesABadCertificate(t *testing.T) {
	dir := t.TempDir()
	writeConfigTree(t, dir, "listen: 127.0.0.1:8443\n"+
		"public_url: https://gateway.example.com\n"+
		"tls:\n  cert_file: "+dir+"/tls/cert.pem\n  key_file: "+dir+"/tls/key.pem\n")
	if err := os.MkdirAll(filepath.Join(dir, "tls"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cert.pem", "key.pem"} {
		if err := os.WriteFile(filepath.Join(dir, "tls", name), []byte("not a PEM"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g, err := startGateway(ctx, filepath.Join(dir, "gateway.yaml"), startOptions{listen: "127.0.0.1:0", stderr: io.Discard})
	if err == nil {
		cancel()
		if err := <-g.Done(); err != nil {
			t.Fatalf("shutdown: %v", err)
		}
		t.Fatal("the Gateway started with a corrupt certificate")
	}
	if !strings.Contains(err.Error(), "TLS certificate") {
		t.Errorf("error = %q, want it to name the TLS certificate", err)
	}
}

// writeSelfSignedCertificate writes a certificate and key for host into
// dir, for the TLS tests only.
func writeSelfSignedCertificate(t *testing.T, dir, host string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: host},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		IPAddresses:           []net.IP{net.ParseIP(host)},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(filepath.Join(dir, "cert.pem"), certPEM, 0o600); err != nil {
		t.Fatalf("write certificate: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "key.pem"), keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
}
