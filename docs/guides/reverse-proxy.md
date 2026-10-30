# Run the Gateway behind a TLS reverse proxy

The Gateway can serve TLS itself, but a reverse proxy usually owns the
certificate, the public host name, and the rate limits. This guide shows a
Caddy configuration that proxies every Gateway route, including the node
agent's WebSocket connection, over HTTPS.

The values here are placeholders: `gateway.example.com` for the host name
and `192.0.2.10` for the Gateway host.

## 1. Put the Gateway on a loopback address

The proxy and the Gateway run on the same host, so the Gateway listens on a
loopback address and lets Caddy terminate TLS
(docs/configuration.md sections 3 and 15):

```yaml
# gateway.yaml
listen: 127.0.0.1:8443
allow_insecure_http: false
```

A loopback `listen` address needs no `allow_insecure_http`. Set that flag
only when the Gateway must listen on a non-loopback address without TLS,
which this guide does not do.

The agent protocol requires `https` URLs; an agent may use `http` only for a
loopback host in tests (docs/agent-protocol.md section 1).

## 2. Proxy the Gateway with Caddy

Caddy obtains and renews the certificate for the host name automatically.
WebSocket upgrades need no separate route or header: a `reverse_proxy` in
Caddy 2 forwards the `Connection: Upgrade` and `Upgrade: websocket` headers
of `GET /v1/agent/connect`, and keeps the connection open for as long as
both sides use it.

```caddyfile
gateway.example.com {
	encode zstd gzip

	reverse_proxy 127.0.0.1:8443 {
		# Do not buffer responses: the agent WebSocket and the
		# operation log stream must reach the client as they are written.
		flush_interval -1
	}
}
```

That single block covers all of these routes:

| Route | Transport | Used by |
|---|---|---|
| `GET /v1/version`, `/v1/capabilities`, `/v1/nodes`, `/v1/operations` | HTTPS + JSON | Clients and discovery |
| `POST /v1/operations`, `/v1/operations/{id}/cancel` | HTTPS + JSON | Clients |
| `POST /v1/agent/join` | HTTPS + JSON | Enrollment |
| `GET /v1/agent/connect` | WebSocket over HTTPS | The agent connection |
| `GET /v1/agent/bundles/{hash}` | HTTPS + gzip | Handler download |

`POST /v1/agent/join`, `GET /v1/agent/connect`, and
`GET /v1/agent/bundles/{hash}` authenticate with a join token or a node
credential, not with a user API token. The Gateway therefore serves them
outside the user-token middleware; the proxy must not add authentication of
its own in front of them, or the agent cannot connect.

### Certificate trusted only by the nodes

For a private deployment, Caddy's internal CA avoids a public certificate:

```caddyfile
gateway.example.com {
	tls internal
	reverse_proxy 127.0.0.1:8443 {
		flush_interval -1
	}
}
```

Every node must then trust Caddy's root certificate. Copy
`/var/lib/caddy/.local/share/caddy/pki/authorities/local/root.crt` to the
node and install it in the system trust store before `hanse-node join`.

## 3. Enroll and connect

On the Gateway host, print a join token for a node that `nodes.yaml`
already lists:

```console
$ hanse-gateway node enroll node-a
```

On the node, use the public HTTPS URL:

```console
$ hanse-node join https://gateway.example.com hjt_<token>
```

The agent stores the returned `hnc_` credential and connects to
`wss://gateway.example.com/v1/agent/connect`. Caddy sees an ordinary
`GET` request with an `Upgrade` header and proxies it to the Gateway.

## 4. Verify the proxy

Check the discovery endpoint through the proxy:

```console
$ curl --fail --silent https://gateway.example.com/v1/version
```

Check the agent route without a credential. The Gateway answers `401`
before the upgrade, which proves the route reaches the Gateway:

```console
$ curl --silent --output /dev/null --write-out '%{http_code}\n' \
    https://gateway.example.com/v1/agent/connect
401
```

Watch the Caddy log while the agent connects. A successful WebSocket shows
status `101` for `GET /v1/agent/connect`:

```console
$ journalctl -u caddy --follow
```

## 5. Timeouts and long connections

The agent connection is long-lived, so the proxy and any middlebox in front
of it must keep idle connections open longer than the agent's heartbeat
interval. The Gateway sends its heartbeat settings in `welcome`
(docs/agent-protocol.md section 3.3). With the defaults, an idle connection
carries a message every 15 seconds, which is well inside Caddy's defaults.

Do not put a proxy in front of the Gateway that buffers the whole response
or that resets a connection after a fixed short time. The agent reconnects
with backoff, but a reset during a run starts the Gateway's reconnect wait
(docs/agent-protocol.md section 8.1).
