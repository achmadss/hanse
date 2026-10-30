package auth

import (
	"strings"
	"testing"
)

func TestCheckTransport(t *testing.T) {
	cases := []struct {
		name        string
		listen      string
		useTLS      bool
		allowHTTP   bool
		wantFailure bool
		wantMessage string
	}{
		{"loopback plain http", "127.0.0.1:8443", false, false, false, ""},
		{"localhost plain http", "localhost:8443", false, false, false, ""},
		{"ipv6 loopback plain http", "[::1]:8443", false, false, false, ""},
		{"any interface plain http", ":8443", false, false, true, "not a loopback address"},
		{"wildcard ip plain http", "0.0.0.0:8443", false, false, true, "not a loopback address"},
		{"lan ip plain http", "192.0.2.10:8443", false, false, true, "not a loopback address"},
		{"lan ip with allow_insecure_http", "192.0.2.10:8443", false, true, false, ""},
		{"lan ip with tls", "192.0.2.10:8443", true, false, false, ""},
		{"missing port", "127.0.0.1", false, false, true, "host:port"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := CheckTransport(c.listen, c.useTLS, c.allowHTTP)
			if !c.wantFailure {
				if err != nil {
					t.Errorf("CheckTransport: %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Errorf("want an error")
				return
			}
			if !strings.Contains(err.Error(), c.wantMessage) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), c.wantMessage)
			}
			if strings.Contains(err.Error(), "192.0.2.") {
				// The address in the message is placeholder only; this
				// line records that the message names the address, which
				// is safe. It never names a token: the gate deals with
				// addresses, not credentials.
				t.Logf("message names the placeholder address: %v", err)
			}
		})
	}
}

// TestCheckTransportRefusesPlainHTTPOnNonLoopback is the acceptance
// test: by default, a non-loopback plain-HTTP listener is refused.
func TestCheckTransportRefusesPlainHTTPOnNonLoopback(t *testing.T) {
	err := CheckTransport("192.0.2.10:8443", false, false)
	if err == nil {
		t.Fatalf("plain HTTP on a non-loopback address: want a refusal")
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Errorf("error = %q, want the loopback refusal wording", err.Error())
	}
}
