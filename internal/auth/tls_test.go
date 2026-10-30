package auth

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// certFixture generates a fresh self-signed certificate at test time,
// so no key material is ever committed to the repository.
func certFixture(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "gateway.example.com"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	dir := t.TempDir()
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

// TestTLSConfigServesTLS performs a real TLS handshake against a
// listener built from the produced configuration: the certificate and
// key gateway.yaml names must actually serve TLS.
func TestTLSConfigServesTLS(t *testing.T) {
	certFile, keyFile := certFixture(t)
	cfg, err := TLSConfig(certFile, keyFile)
	if err != nil {
		t.Fatalf("TLSConfig: %v", err)
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %#x, want TLS 1.2", cfg.MinVersion)
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("certificates = %d, want 1", len(cfg.Certificates))
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		tlsConn := tls.NewListener(ln, cfg)
		for {
			conn, err := tlsConn.Accept()
			if err != nil {
				return
			}
			_, _ = io.Copy(io.Discard, conn)
			_ = conn.Close()
		}
	}()
	conn, err := tls.DialWithDialer(
		&net.Dialer{Timeout: 5 * time.Second},
		"tcp", ln.Addr().String(),
		&tls.Config{InsecureSkipVerify: true}, // the certificate is self-signed; the handshake itself is the assertion
	)
	if err != nil {
		t.Fatalf("TLS handshake against the generated config: %v", err)
	}
	defer func() { _ = conn.Close() }()
	state := conn.ConnectionState()
	if state.Version < tls.VersionTLS12 {
		t.Errorf("negotiated version = %#x, want at least TLS 1.2", state.Version)
	}
}

func TestTLSConfigMissingFiles(t *testing.T) {
	if _, err := TLSConfig(filepath.Join(t.TempDir(), "no.pem"), filepath.Join(t.TempDir(), "no.key")); err == nil {
		t.Errorf("TLSConfig with missing files: want an error")
	}
}
