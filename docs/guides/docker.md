# Run the Gateway in Docker

The repository ships a multi-stage `Dockerfile` that builds a static
`hanse-gateway` binary and runs it as an unprivileged user. This guide shows
the image, the volumes, and the Compose example.

Every value in this guide is a placeholder: `gateway.example.com` for the
public host name and `192.0.2.0/24` for addresses.

The example Compose file is
[`deploy/docker/compose.yaml`](../../deploy/docker/compose.yaml).

## 1. Build or pull the image

Build from the repository root:

```console
$ docker build --tag hanse-gateway:local .
```

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
$ docker compose up --detach
$ docker compose ps
$ curl --fail --silent http://127.0.0.1:8443/healthz
ok
```

The health check calls `/healthz`, which needs no token. `/readyz` is the
better signal for a load balancer; see
[Health and readiness endpoints](health.md).

### Wake-on-LAN and the host network

A Gateway host that is itself an executor for Wake-on-LAN must send the magic
packet on the physical network. Bridge networking puts the container on a
private network, so the broadcast never reaches the machine to wake. Use the
host network for that deployment:

```yaml
services:
  gateway:
    # ...
    network_mode: host
    # Remove the ports block: host networking publishes nothing.
```

The container then shares the host's interfaces, and the `echo` or `pc`
handler that sends the magic packet reaches the LAN. The same applies to a
handler that must see the local subnet, such as one that runs an ARP scan or
binds a fixed local address.

`network_mode: host` also means the Gateway listens on the host's port
directly. Keep `listen: 127.0.0.1:8443` and put the TLS proxy on the host, or
serve TLS from the container with `listen: 0.0.0.0:8443`.

## 5. Volumes and permissions

| Container path | Mode | Holds |
|---|---|---|
| `/etc/hanse` | read-only | `gateway.yaml`, `nodes.yaml`, `routes.yaml`, `capabilities/`, `credentials/`, `tls/` |
| `/var/lib/hanse` | read-write | History database, enrollment database, `tokens.json` |
| `/tmp` | tmpfs (Compose example) | Nothing persistent; the embedded agent works under the state directory. |

Run the image with `read_only: true` as the example does. The only paths the
Gateway writes are inside `/var/lib/hanse`, which the state volume mounts.

Do not mount the state directory from a network file system. SQLite needs
working file locks and a local file system.

## 6. Upgrades

```console
$ docker compose pull
$ docker compose up --detach
```

The named volume survives. An operation that was running during the restart
is stored as `failed` with `gateway_restart`; a client retries it with the
same `Idempotency-Key` when the operation is safe to repeat.

Validate a new configuration before starting the container:

```console
$ docker run --rm \
    --volume "$PWD/config:/etc/hanse:ro" \
    hanse-gateway:local --check-config --config /etc/hanse/gateway.yaml
```

## 7. Troubleshooting

| Symptom | Cause |
|---|---|
| `permission denied` under `/var/lib/hanse` | The volume or bind mount is not owned by uid 10001. |
| Container exits 1 with `configuration problem(s)` | The file is invalid; the message names the file and the field. Run `--check-config`. |
| `refusing to listen on ... without TLS` | `listen` is not loopback and `allow_insecure_http` is false. |
| Health check fails but the log says `gateway listening` | The health check uses the wrong address or scheme. Probe the configured listen address; with TLS, use `https`. |
| Wake-on-LAN does nothing | The container is not on the host network. See section 4. |
