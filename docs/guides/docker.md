# Run the Gateway in Docker

The repository ships a multi-stage `Dockerfile` that builds a static
`hanse-gateway` binary and runs it as an unprivileged user. This guide shows
the image, the volumes, and the Compose example.

The recommended layout puts the Gateway in a container and runs handlers on
the real host:

- The Gateway container sets no `node` in `gateway.yaml`. It runs no embedded
  agent and no handler in its own process.
- The host runs `hanse-node` as a systemd service, enrolls it with
  `hanse-gateway node enroll`, and connects it to the published loopback port
  or through the TLS proxy.
- The host node is listed in `nodes.yaml` and in `routes.yaml` like any other
  node.

Every value in this guide is a placeholder: `gateway.example.com` for the
public host name, `192.0.2.0/24` for addresses, and `gateway-host` for the
node ID of the machine that runs the container.

The example Compose file is
[`deploy/docker/compose.yaml`](../../deploy/docker/compose.yaml).

## 1. Build the image

Build from the repository root, where the `Dockerfile` is:

```console
$ docker build --tag hanse-gateway:local .
```

The Compose example uses the same `Dockerfile` with `build: { context: ../.. }`
and the local tag `hanse-gateway:local`, so `docker compose up --build` needs
no separate build step. The repository does not publish an image yet; the
Compose file keeps a commented `ghcr.io/achmadss/hanse-gateway:<version>`
line for a future release.

The image has three properties that matter:

| Property | Value |
|---|---|
| Base | Alpine with CA certificates; the binary is static (`CGO_ENABLED=0`). |
| User | `10001:10001`, a dedicated `hanse` account. The container never runs as root. |
| Writable paths | Only `/var/lib/hanse` (and a small `/tmp` tmpfs in the example). |

`docker run` with the image's default command reads
`/etc/hanse/gateway.yaml`.

## 2. Lay out the configuration

Create one directory for configuration and one volume for state:

```console
$ mkdir -p config/capabilities config/credentials config/tls
$ cp config/gateway.example.yaml config/gateway.yaml
$ cp config/nodes.example.yaml  config/nodes.yaml
$ cp config/routes.example.yaml config/routes.yaml
$ cp -r capabilities/echo config/capabilities/
```

Change the paths in `gateway.yaml` to the container paths:

```yaml
paths:
  nodes: /etc/hanse/nodes.yaml
  routes: /etc/hanse/routes.yaml
  capabilities:
    - /etc/hanse/capabilities
  credentials_dir: /etc/hanse/credentials
  state_dir: /var/lib/hanse

auth:
  tokens_file: /var/lib/hanse/tokens.json
```

Leave `node` out. The example file starts with `node: gateway`, so delete or
comment that line: the Gateway then runs no handler in its own process. Every
`exec` handler runs on a node agent, including the handlers of the host.

The example also sets TLS with files under `/etc/hanse/tls` and listens on
`0.0.0.0:8443`. Either mount a certificate and key there, or, for a first run
behind a TLS proxy, comment the `tls` block out and set
`allow_insecure_http: true`. See
[Serve TLS from the container](#serve-tls-from-the-container) and
[Run the Gateway behind a TLS reverse proxy](reverse-proxy.md).

List the host as a node in `nodes.yaml`:

```yaml
version: 1

nodes:
  gateway-host:
    name: Gateway host
    os: linux
    arch: amd64
    roles: [server]
    address: 192.0.2.10
```

The example files call this node `gateway`. Either keep that ID or rename it
in `nodes.yaml`, in `routes.yaml`, and in the join command below; this guide
uses `gateway-host`.

Route capabilities to it in `routes.yaml`, like to any other node:

```yaml
version: 1

capabilities:
  echo:
    executors: [gateway-host]
    default_executor: gateway-host
```

An `http` capability needs the Gateway host node, which a Gateway without
`node` does not have. The Gateway refuses to start when `routes.yaml` routes
one; run `http` capabilities on a Gateway that sets `node`.

The state directory must be writable by uid 10001. When you use a bind mount
instead of a named volume, set the owner:

```console
$ sudo chown -R 10001:10001 state
```

Capability packages, credential files, and the TLS key stay read-only.

### Serve TLS from the container

Mount the certificate and key under `/etc/hanse/tls` and set:

```yaml
listen: 0.0.0.0:8443
tls:
  cert_file: /etc/hanse/tls/cert.pem
  key_file: /etc/hanse/tls/key.pem
```

`0.0.0.0` without TLS is refused unless `allow_insecure_http: true` is set
(docs/architecture.md section 15). Use that setting only behind a TLS
reverse proxy on a trusted network; see
[Run the Gateway behind a TLS reverse proxy](reverse-proxy.md).

## 3. Create the first API token

The token file is in the state volume, so create the token as the same uid
the container uses:

```console
$ docker run --rm -it \
    --user 10001:10001 \
    --volume "$PWD/config:/etc/hanse:ro" \
    --volume hanse-state:/var/lib/hanse \
    hanse-gateway:local token add admin
hst_<token>
```

The value is printed once.

## 4. Run with Compose

The example file binds `./config` read-only to `/etc/hanse`, keeps the state
in a named volume, publishes the port on loopback, and sets a health check:

```console
$ cd deploy/docker
$ docker compose up --detach --build
$ docker compose ps
$ curl --fail --silent http://127.0.0.1:8443/healthz
ok
```

The health check calls `/healthz`, which needs no token. `/readyz` is the
better signal for a load balancer; see
[Health and readiness endpoints](health.md).

### What the health check assumes

The compose health check runs busybox `wget` inside the container against
`http://127.0.0.1:8443/healthz`. It works only when `listen` in
`gateway.yaml` binds an address that is reachable inside the container:

| Gateway configuration | Health check command |
|---|---|
| `listen: 0.0.0.0:8443` with `allow_insecure_http: true` (behind a TLS proxy) | Plain HTTP to `127.0.0.1:8443`, the example default. |
| `listen: 127.0.0.1:8443` and the proxy runs in another container | Plain HTTP works only if the proxy shares the network namespace. |
| TLS from the container on `0.0.0.0:8443` | `https://127.0.0.1:8443/healthz` with `--no-check-certificate`, because the probe cannot verify the public name. |

For the TLS case, change the health check to:

```yaml
healthcheck:
  test: ["CMD", "wget", "--quiet", "--no-check-certificate", "--output-document=/dev/null", "https://127.0.0.1:8443/healthz"]
  interval: 30s
  timeout: 3s
  retries: 3
  start_period: 5s
```

The probe is a liveness check, not an authorization test: it never sends a
token, so it must not be pointed at a route that requires one.

### Stopping and handler processes

`docker compose stop` sends `SIGTERM` to the Gateway, which stops accepting
requests, waits up to `timeouts.grace_period`, marks interrupted operations
as failed with `gateway_restart`, and closes the stores. The recommended
layout starts no handler inside the container, so the container stop affects
no handler. A `hanse-node` handler on the host keeps running: its agent owns
that process tree and stops the run on its own schedule. `init: true` in the
example still gives the container a small PID 1 that forwards the signal.

## 5. Enroll and run the host node

The host runs `hanse-node`, an ordinary executor node. It sends handlers to
the host kernel, the network, and `sudo`, none of which the container has.

### Install the agent

Build it from the same checkout and install it next to the Gateway binary:

```console
$ go build -trimpath -o hanse-node ./cmd/hanse-node
$ sudo install -m 0755 hanse-node /usr/local/bin/hanse-node
$ hanse-node --version
```

Create a system account and the directories the agent uses:

```console
$ sudo useradd --system --no-create-home --home-dir /var/lib/hanse-node \
    --shell /usr/sbin/nologin hanse-node
$ sudo install -d -o hanse-node -g hanse-node -m 0700 /var/lib/hanse-node
$ sudo install -d -o root -g hanse-node -m 0750 /etc/hanse-node
```

Write `/etc/hanse-node/agent.yaml`. Set `gateway_url` to the URL the host
uses to reach the Gateway, and `state_dir` to the agent's state directory:

```yaml
version: 1

gateway_url: http://127.0.0.1:8443
# ca_file: /etc/hanse-node/ca.pem

state_dir: /var/lib/hanse-node

handlers:
  env_allow: []

elevation:
  enabled: false
```

Keep the file readable by the agent's account and writable only by root: the
agent refuses to start when another account can change it.

```console
$ sudo chown root:hanse-node /etc/hanse-node/agent.yaml
$ sudo chmod 0640 /etc/hanse-node/agent.yaml
```

See [docs/configuration.md section 7](../configuration.md#7-agentyaml) for
every field.

### Print a join token

Run the enrollment command in the container, so it writes to the same state
volume the Gateway reads:

```console
$ cd deploy/docker
$ docker compose exec gateway hanse-gateway node enroll gateway-host
join token for gateway-host (one use, expires 2026-10-31T12:15:00Z):

  hanse-node join https://gateway.example.com hjt_<token>

Run that command on the node. This token is shown only once.
```

The command refuses whenever `gateway.yaml` sets `node` to the same ID: that
Gateway host already runs its handlers in its embedded agent.

The printed command uses `public_url`. The Gateway host can use the published
loopback port instead, which avoids the proxy for this one step; the next
section shows that form.

### Join and start the service

On the host, join as the agent's account, using the URL the agent will use to
connect, then install a systemd unit:

```console
$ sudo -u hanse-node hanse-node join \
    http://127.0.0.1:8443 hjt_<token> --state-dir /var/lib/hanse-node
```

The agent stores the `hnc_` credential in `/var/lib/hanse-node` and prints
the file name, never the credential.

```ini
# /etc/systemd/system/hanse-node.service
[Unit]
Description=Hanse node agent
After=network-online.target
Wants=network-online.target

[Service]
User=hanse-node
Group=hanse-node
ExecStart=/usr/local/bin/hanse-node run --config /etc/hanse-node/agent.yaml
Restart=on-failure
RestartSec=2s
KillMode=control-group
UMask=0077

[Install]
WantedBy=multi-user.target
```

```console
$ sudo systemctl daemon-reload
$ sudo systemctl enable --now hanse-node
$ systemctl status hanse-node
```

`KillMode=control-group` is systemd's default and is kept explicitly: after
the agent exits, systemd stops the handlers it started. Privileged operations
need an agent account that may use `sudo`; see
[docs/architecture.md section 8.5](../architecture.md#85-privileged-operations).

### Connect to the Gateway

The agent connects over one of two paths:

| Path | `gateway_url` and join URL | Use when |
|---|---|---|
| Published loopback port | `http://127.0.0.1:8443` | The agent runs on the same host as the container. Plain HTTP to a loopback address is allowed, and the port is published only on `127.0.0.1`. |
| TLS proxy | `https://gateway.example.com` | Other nodes, and the host node, connect through the proxy. The proxy must pass the WebSocket upgrade; see [Run the Gateway behind a TLS reverse proxy](reverse-proxy.md). |

For a container that serves TLS itself and publishes only on loopback, use
`https://127.0.0.1:8443` and point `ca_file` at the CA that signed the
certificate, because the certificate names the public host.

Check that the node is online:

```console
$ hanse login --url http://127.0.0.1:8443 --token hst_<token>
$ hanse nodes
```

The host node then appears like every other node, and capabilities routed to
it run their handlers on the host.

## 6. Volumes and permissions

| Container path | Mode | Holds |
|---|---|---|
| `/etc/hanse` | read-only | `gateway.yaml`, `nodes.yaml`, `routes.yaml`, `capabilities/`, `credentials/`, `tls/` |
| `/var/lib/hanse` | read-write | History database, enrollment database, `tokens.json` |
| `/tmp` | tmpfs (Compose example) | Nothing persistent; a Gateway without `node` starts no handler process. |

Run the image with `read_only: true` as the example does. The only paths the
Gateway writes are inside `/var/lib/hanse`, which the state volume mounts.

Do not mount the state directory from a network file system. SQLite needs
working file locks and a local file system.

## 7. Upgrades

```console
$ docker compose build --pull
$ docker compose up --detach
```

`build --pull` fetches the current base image and rebuilds the Gateway from
this checkout. If the deployment uses a published registry image, replace
both commands with `docker compose pull` and `docker compose up --detach`.

The named volume survives. An operation that was running during the restart
is stored as `failed` with `gateway_restart`; a client retries it with the
same `Idempotency-Key` when the operation is safe to repeat.

Validate a new configuration before starting the container:

```console
$ docker run --rm \
    --volume "$PWD/config:/etc/hanse:ro" \
    hanse-gateway:local --check-config --config /etc/hanse/gateway.yaml
```

The same applies to the host agent: install the new `hanse-node` binary and
restart `hanse-node.service`. A run that was in flight is resumed or
abandoned as the agent protocol defines.

## 8. Troubleshooting

| Symptom | Cause |
|---|---|
| `permission denied` under `/var/lib/hanse` | The volume or bind mount is not owned by uid 10001. |
| Container exits 1 with `configuration problem(s)` | The file is invalid; the message names the file and the field. Run `--check-config`. |
| `refusing to listen on ... without TLS` | `listen` is not loopback and `allow_insecure_http` is false. |
| Health check fails but the log says `gateway listening` | The health check uses the wrong address or scheme. Probe the configured listen address; with TLS, use `https` and `--no-check-certificate`. See section 4. |
| An `exec` run fails with `executor_unavailable` | The executor's node agent is not connected. Check `systemctl status hanse-node` and the URL in `agent.yaml`. A Gateway without `node` never runs the handler itself. |
| `node ... is the Gateway host` when enrolling | `gateway.yaml` still sets `node`. Remove it to run every `exec` handler on a node agent. |
| Wake-on-LAN does nothing | In this layout the magic packet comes from `hanse-node` on the host, so check that service first. Bridge networking of the container no longer matters; host networking is needed only by an embedded agent that sends the packet itself. |
| A `hanse-node` on the host cannot reach the joined URL | The port is published only on loopback; use `127.0.0.1`, or enroll through the TLS proxy. |
