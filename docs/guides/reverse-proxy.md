# Run the Gateway behind a TLS reverse proxy

The Gateway can serve TLS itself, but a reverse proxy usually owns the
certificate, the public host name, and the rate limits. This guide shows a
Caddy configuration that proxies every Gateway route, including the node
agent's WebSocket connection, over HTTPS.

The values here are placeholders: `gateway.example.com` for the host name
and `192.0.2.10` for the Gateway host.

The guide covers both Gateway layouts:

- **Embedded agent** (`embedded_agent: true`, the default). The Gateway runs
  the host node's `exec` handlers itself, and no node agent connects for that
  ID.
- **Host node agent** (`embedded_agent: false`, such as for the Gateway in a
  container). The host runs `hanse-node` for the node that `gateway.yaml`'s
  `node` names. Its agent connects through the proxy, or through a published
  loopback port, like every other node agent.

## 1. Put the Gateway behind the proxy

Choose one of the three layouts below.

### Proxy on the same host: loopback

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
which this layout does not do.

### Proxy on another host: a private address

When the proxy runs on another machine, the Gateway must accept plain HTTP
on a network address. That is one case where `allow_insecure_http: true`
is correct:

```yaml
# gateway.yaml
listen: 192.0.2.10:8443
allow_insecure_http: true
public_url: https://gateway.example.com
```

Restrict the listener to the proxy, so no other host can reach the plain-HTTP
port. With a firewall that allows only the proxy's address:

```console
$ sudo nft add rule inet filter input tcp dport 8443 \
    ip saddr != 192.0.2.11 drop
```

Keep `allow_insecure_http: true` on that private, proxy-only network, or on
the bridge listener below, and nowhere else. A Gateway that listens on a
public address without TLS hands credentials and arguments to anyone on the
path, because the node credential and the API token travel in plain text.

### Proxy in a container on the same host

When the proxy runs in a container on the Gateway host, a loopback listener
does not work: the container has its own network namespace, so `127.0.0.1`
inside it is the container, not the host. The Gateway instead listens on a
host address that the container can reach, which is not a loopback address,
so this layout needs `allow_insecure_http: true`:

```yaml
# gateway.yaml
listen: 192.0.2.10:8443
allow_insecure_http: true
public_url: https://gateway.example.com
```

`192.0.2.10` stands for the host's address on the network the proxy container
uses. On the default bridge, that is the address of `docker0`, which
`ip -4 addr show docker0` prints. The address stays assigned while `docker0`
is DOWN, so the command works even when no container uses that network. On a
user-defined network, such as the one Compose creates by default, use the
network's gateway address instead, which `docker network inspect <network>`
reports. A container reaches the host on that address. Do not use `0.0.0.0`,
which also accepts traffic on every other interface.

The proxy container then reaches the Gateway at that address, so Caddy's
upstream is the address rather than `127.0.0.1`:

```caddyfile
gateway.example.com {
	encode zstd gzip

	reverse_proxy 192.0.2.10:8443 {
		flush_interval -1
	}
}
```

The address exists only while Docker runs. On a host that starts the
Gateway under systemd, order the units so the Gateway starts after
`docker.service`; otherwise it can bind before the address exists, or refuse
to start:

```ini
# /etc/systemd/system/hanse-gateway.service.d/docker.conf
[Unit]
After=docker.service
Wants=docker.service
```

```console
$ sudo systemctl daemon-reload
$ sudo systemctl restart hanse-gateway
```

Keep `allow_insecure_http: true` only while the listener accepts the proxy's
subnet, and restrict the port to it. Allow port 8443 only from the subnet of
every network the proxy container joins, and drop it from everywhere else.
A Compose container usually sits on a user-defined network, whose subnet is
not `docker0`'s, so find each subnet with:

```console
$ docker network inspect <network> --format '{{range .IPAM.Config}}{{.Subnet}}{{end}}'
```

Then add the allow and drop rules with the host's own firewall tool. The
following nftables command assumes an existing `inet filter` table with an
`input` chain, and `192.0.2.0/24` for the proxy network's subnet; repeat the
rule for each subnet the proxy uses:

```console
$ sudo nft add rule inet filter input tcp dport 8443 \
    ip saddr != 192.0.2.0/24 drop
```

As with the private address above, keep the plain-HTTP port off every other
interface. The node credential and the API token travel in plain text, so a
public listener without TLS exposes both.

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
| `GET /healthz`, `GET /readyz` | HTTPS + text | Health probes |
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

`GET /healthz` and `GET /readyz` also need no token. A proxy health check can
use them, but they must not be cached: the Gateway answers them with
`Cache-Control: no-store`. See
[Health and readiness endpoints](health.md).

### The proxy must pass WebSocket connections

The node agent is reachable only through
`GET /v1/agent/connect`, and that route is a WebSocket upgrade on HTTPS. Any
proxy in front of the Gateway must:

- forward `Connection: Upgrade` and `Upgrade: websocket` for
  `/v1/agent/connect`,
- keep the connection open for as long as both sides use it, and
- stream bytes both ways without buffering the response.

Caddy 2 does all three by default. Other proxies need an explicit setting; an
nginx location, for example, needs the upgrade header map:

```nginx
location = /v1/agent/connect {
    proxy_pass http://127.0.0.1:8443;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
    proxy_read_timeout 1h;
    proxy_buffering off;
}
```

A proxy that strips the upgrade headers answers the agent with an ordinary
`200` or `400` response instead of `101`, and the node never comes online.
See [Verify the proxy](#4-verify-the-proxy).

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

With `embedded_agent: false`, enroll the machine that runs the Gateway like
any other node. List it in `nodes.yaml` and `routes.yaml`, then run the same
two commands on that host. Its agent can use the public URL as well, so the
proxy must stay reachable from the Gateway host. A host that shares the
network with the Gateway can use the published loopback port instead; see the
[Docker guide](docker.md#connect-to-the-gateway).

With `embedded_agent: true`, enrolling the node that `gateway.yaml`'s `node`
names fails with the `forbidden` code: the embedded agent is already that
node's executor. Set `embedded_agent: false` when the host should run a node
agent.

## 4. Verify the proxy

Check the discovery endpoint through the proxy:

```console
$ curl --fail --silent https://gateway.example.com/v1/version
```

Check the probes. Both need no token and answer through the proxy:

```console
$ curl --fail --silent https://gateway.example.com/healthz
ok
$ curl --fail --silent https://gateway.example.com/readyz
ready
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
