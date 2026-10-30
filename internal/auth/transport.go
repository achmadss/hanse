package auth

import (
	"fmt"
	"net"

	"github.com/achmadss/hanse/internal/config"
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
	if !config.IsLoopbackHost(host) {
		return fmt.Errorf("listen %s: is not a loopback address: without TLS the Gateway listens only on a loopback address; set tls.cert_file and tls.key_file, or set allow_insecure_http", listen)
	}
	return nil
}
