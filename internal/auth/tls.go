package auth

import (
	"crypto/tls"
	"fmt"
)

// TLSConfig builds the TLS configuration the Gateway server listens
// with when gateway.yaml sets tls.cert_file and tls.key_file. TLS 1.2
// is the minimum version. The private key file follows the same
// permission rules as secret files, checked by internal/config at
// startup (docs/configuration.md §8.2).
func TLSConfig(certFile, keyFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("cannot load the TLS certificate: %w", err)
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{cert},
	}, nil
}
