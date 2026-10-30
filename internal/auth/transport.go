package auth

import (
	"fmt"
	"net"
	"strings"
)

// CheckTransport decides whether the Gateway may listen on the address
// without TLS: a non-loopback plain-HTTP listener is refused unless
// allow_insecure_http is true (architecture.md §15; the config checks
// the same rule at validation, this gate is the runtime backstop the
// server calls right before listen). useTLS means a certificate and
// key are configured and the listener will serve TLS. The returned
// error is a plain configuration error; it names the address only.
func CheckTransport(listen string, useTLS, allowInsecureHTTP bool) error {
	if useTLS || allowInsecureHTTP {
		return nil
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return fmt.Errorf("listen %s: is not a host:port address", listen)
	}
	if !isLoopbackHost(host) {
		return fmt.Errorf("listen %s: is not a loopback address: without TLS the Gateway listens only on a loopback address; set tls.cert_file and tls.key_file, or set allow_insecure_http", listen)
	}
	return nil
}

// isLoopbackHost reports whether host is a loopback address: the name
// localhost, or an IP in the loopback range. An empty host listens on
// every interface, so it is not loopback.
func isLoopbackHost(host string) bool {
	host = strings.Trim(host, "[]")
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
