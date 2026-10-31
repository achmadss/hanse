# Run the Gateway under systemd

This guide installs the Gateway as a systemd system service on Linux. It
assumes a binary installed at `/usr/local/bin/hanse-gateway` and the
documented configuration paths:

| Path | Holds | Access |
|---|---|---|
| `/etc/hanse/gateway.yaml` | The configuration | Read-only |
| `/etc/hanse/nodes.yaml`, `routes.yaml` | Nodes and routing | Read-only |
| `/etc/hanse/capabilities/` | Capability packages | Read-only |
| `/etc/hanse/credentials/` | Secret files | Read-only, mode 0700 |
| `/etc/hanse/tls/` | TLS certificate and key | Read-only |
| `/var/lib/hanse/` | History, enrollment, and the tokens file | Read and write |

Every value in this guide is a placeholder: `gateway.example.com` for the
public host name and `192.0.2.0/24` for addresses.

The unit is in the repository at
[`deploy/systemd/hanse-gateway.service`](../../deploy/systemd/hanse-gateway.service).

## 1. Create the service account

The Gateway needs no privilege. It listens on an unprivileged port and, with
the default `embedded_agent: true`, starts handler processes as its own user,
so a dedicated system account is enough:

```console
$ sudo useradd --system --no-create-home --home-dir /var/lib/hanse \
    --shell /usr/sbin/nologin hanse
```

The unit lets systemd create the two directories with the right owner when
the service starts. If you create them yourself, keep the modes:

```console
$ sudo install -d -o hanse -g hanse -m 0750 /etc/hanse
$ sudo install -d -o hanse -g hanse -m 0700 /var/lib/hanse
```

`StateDirectory=hanse` and `ConfigurationDirectory=hanse` in the unit also
create `/var/lib/hanse` and `/etc/hanse` when they are missing.

## 2. Install the binary

Build from source or take a release archive:

```console
$ go build -trimpath -o hanse-gateway ./cmd/hanse-gateway
$ sudo install -m 0755 hanse-gateway /usr/local/bin/hanse-gateway
$ hanse-gateway --version
```

## 3. Write the configuration

Start from the examples:

```console
$ sudo cp config/gateway.example.yaml /etc/hanse/gateway.yaml
$ sudo cp config/nodes.example.yaml  /etc/hanse/nodes.yaml
$ sudo cp config/routes.example.yaml /etc/hanse/routes.yaml
$ sudo install -d -o hanse -g hanse -m 0750 /etc/hanse/capabilities
$ sudo install -d -o hanse -g hanse -m 0700 /etc/hanse/credentials
```

The credentials directory must be mode `0700`. The Gateway refuses one whose
group or other read bits are set, even when every secret file inside it is
private: `--check-config` reports `paths.credentials_dir: ... is readable by
users other than its owner`. Capability packages stay at `0750`; they hold no
secrets.

Change the values in `gateway.yaml` to the absolute paths under `/etc/hanse`
and `/var/lib/hanse`. Set `node` to the node ID in `nodes.yaml` that is this
host. Keep `embedded_agent: true`, its default: the Gateway then runs this
host's `exec` handlers in its embedded agent, which is the simplest layout
without containers. Then validate before the first start:

```console
$ sudo -u hanse hanse-gateway --check-config --config /etc/hanse/gateway.yaml
/etc/hanse/gateway.yaml: configuration is valid
```

The check prints every problem and exits 1 when anything is wrong. The
service refuses to start on the same problems, so run the check after every
configuration change.

### The Gateway host without an embedded agent

To run this host's `exec` handlers in a node agent instead, set
`embedded_agent: false` in `gateway.yaml` and install `hanse-node` on the
same machine. The Gateway then starts no embedded agent, and this host is an
ordinary executor:

1. List the host in `nodes.yaml` and in `routes.yaml`, like any other node.
2. Enroll it: `sudo -u hanse hanse-gateway node enroll gateway`.
3. Run `hanse-node join` with the printed token, then run the agent as a
   service.

The [Docker guide](docker.md#5-enroll-and-run-the-host-node) walks through
the agent installation and its systemd unit, and the same steps apply here.
Use `https://gateway.example.com`, or a loopback URL when the Gateway listens
on `127.0.0.1`. With the default `embedded_agent: true`, the Gateway refuses
a `hanse-node` enrollment or connection for that same ID, because one node
never has two executors.

`http` capabilities work in both layouts, because the Gateway makes those
calls itself.

### Privileged operations on the Gateway host

The unit sets `NoNewPrivileges=yes` (section 8), so no process it starts can
raise its rights through `sudo`. With `embedded_agent: true` a privileged
operation therefore cannot run on the Gateway host. The embedded agent never
asks for an elevation password, and the run fails with HTTP 403, code
`elevation_denied`, and the message `the embedded agent does not elevate
handlers`. The `hanse` CLI sees `can_elevate: false` for that executor in the
`describe` response, so it fails the same way before it prompts for a
password.

Do not remove `NoNewPrivileges` from the Gateway unit to make a privileged
operation work. Run the operation on an ordinary executor node instead, as
[Run the node agent as a system service](../node-setup.md) describes,
including its [sudoers rule](../node-setup.md#sudoers-rule-linux-and-macos).

To run privileged operations on this machine, use the layout above: set
`embedded_agent: false` and run `hanse-node` here with that sudoers rule. The
node agent unit deliberately omits `NoNewPrivileges`, because the agent needs
`sudo` to gain root ([docs/node-setup.md](../node-setup.md#linux)). An agent
that does run with `NoNewPrivileges=yes` refuses every privileged run with
`elevation_denied` and a message that names the setting.

### TLS or a reverse proxy

The Gateway serves TLS itself when `tls.cert_file` and `tls.key_file` are
set:

```yaml
listen: 0.0.0.0:8443
tls:
  cert_file: /etc/hanse/tls/cert.pem
  key_file: /etc/hanse/tls/key.pem
```

Without TLS, the listen address must be loopback. To place a TLS reverse proxy
on the same host, keep `listen: 127.0.0.1:8443` and leave
`allow_insecure_http: false`. See
[Run the Gateway behind a TLS reverse proxy](reverse-proxy.md).

## 4. Create the first API token

The tokens file lives in the state directory by default. Create a token as
the service user, so its file mode stays `0600`:

```console
$ sudo -u hanse hanse-gateway token add admin --config /etc/hanse/gateway.yaml
hst_<token>
```

The token is printed once. Store it in a password manager; the Gateway keeps
only its hash.

## 5. Install the unit

```console
$ sudo install -m 0644 deploy/systemd/hanse-gateway.service \
    /etc/systemd/system/hanse-gateway.service
$ sudo systemctl daemon-reload
$ sudo systemctl enable --now hanse-gateway
```

## 6. Verify the service

```console
$ systemctl status hanse-gateway
$ curl --fail --silent http://127.0.0.1:8443/healthz
ok
$ curl --fail --silent http://127.0.0.1:8443/readyz
ready
```

`/healthz` and `/readyz` need no token. See
[Health and readiness endpoints](health.md) for what each one promises.

Follow the log with:

```console
$ journalctl -u hanse-gateway --follow
```

The Gateway writes structured log lines to standard error. Set
`log.format: json` in `gateway.yaml` for one JSON object per line, which a
log shipper can parse.

## 7. Stop and restart

```console
$ sudo systemctl stop hanse-gateway
$ sudo systemctl restart hanse-gateway
```

### Reload without a restart

A capability package, `nodes.yaml`, or `routes.yaml` change does not need a
restart. `POST /v1/capabilities/reload` and `SIGHUP` re-read the capability
directories, `nodes.yaml`, and `routes.yaml`, validate them together, and
apply them to the running Gateway. `gateway.yaml` itself is not re-read, so
change its settings, including the listen address, TLS, timeouts, and the
host `node`, with a restart.

Send the signal to the service:

```console
$ sudo systemctl kill --signal=SIGHUP hanse-gateway
$ journalctl -u hanse-gateway --since "1 minute ago"
```

The result is logged. A reload with an invalid file reports every problem,
keeps the old configuration running, and does not stop the service. See
[Reloading configuration](../configuration.md#11-reloading-configuration).

### Stop and shutdown

On `SIGTERM` the Gateway stops accepting connections, waits for in-flight
requests up to `timeouts.grace_period`, marks the operations that were still
queued or running as failed with the reason `gateway_restart`
(docs/architecture.md section 9.6), and closes the stores. `TimeoutStopSec=30s`
is longer than the default 10 second grace period, so a normal stop is
graceful.

A handler that the embedded agent started is not cancelled by the Gateway's
shutdown sequence; it keeps running until it finishes or the service manager
stops the whole control group. The unit keeps `KillMode=control-group`, which
is systemd's default: after the main process exits, systemd sends `SIGTERM`
to every remaining process in the unit and then `SIGKILL` after
`TimeoutStopSec`. The handler therefore never outlives the service. This
applies only with `embedded_agent: true`. With `false` the Gateway starts no
handler; a handler on a node agent, including `hanse-node` on this host, is
unaffected: its own agent still owns the process tree and stops the run on
its own schedule.

A configuration error makes the process exit 1 and `Restart=on-failure`
retries it every two seconds. Fix the file, run `--check-config`, then
restart; `journalctl -u hanse-gateway` holds every problem the Gateway
reported.

## 8. Hardening the unit

The unit applies these settings. Each one costs the Gateway nothing, because
the Gateway never needs the capability it removes.

| Setting | Effect |
|---|---|
| `User=hanse`, `Group=hanse` | Handlers run as an unprivileged service account. |
| `NoNewPrivileges=yes` | A handler process cannot gain rights through a setuid binary or through `sudo`, so a privileged operation cannot run on this host's embedded agent. See [Privileged operations on the Gateway host](#privileged-operations-on-the-gateway-host). |
| `CapabilityBoundingSet=`, `AmbientCapabilities=` | No Linux capability at all. Port 8443 needs none. |
| `ProtectSystem=strict` | The whole file system is read-only except the paths below. |
| `ReadWritePaths=/var/lib/hanse` | The only writable directory: the databases and the tokens file. |
| `StateDirectory=hanse` | Creates `/var/lib/hanse` with mode 0700 and the right owner. |
| `ProtectHome=yes` | No access to the home directories of logged-in users. |
| `PrivateTmp=yes` | A private `/tmp`, so another service cannot read handler files. |
| `PrivateDevices=yes` | No access to physical devices. |
| `ProtectKernelTunables`, `ProtectKernelModules`, `ProtectKernelLogs` | No kernel configuration, module loading, or kernel log access. |
| `ProtectControlGroups`, `ProtectClock`, `ProtectHostname` | No control-group, clock, or host name changes. |
| `ProtectProc=invisible` | Other users' processes are hidden from the service. |
| `RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6` | Only the socket families the Gateway uses. |
| `RestrictNamespaces=yes`, `RestrictRealtime=yes`, `RestrictSUIDSGID=yes` | No new namespaces, no realtime scheduling, no setuid or setgid files. |
| `LockPersonality=yes`, `MemoryDenyWriteExecute=yes` | No execution-personality change and no writable, executable memory. |
| `SystemCallFilter=@system-service`, `SystemCallErrorNumber=EPERM` | Only the system-service system calls; anything else gets `EPERM`. |
| `KillMode=control-group` | On stop, systemd also stops a handler process that outlived the Gateway. This matters only with `embedded_agent: true`. The default is kept explicitly. |
| `UMask=0077` | Every file the Gateway creates is private. |

If an `exec` capability needs to run a program that the filter blocks, add the
program's calls to the unit with an override instead of removing the filter:

```ini
# /etc/systemd/system/hanse-gateway.service.d/exec-handler.conf
[Service]
SystemCallFilter=~@mount
```

`ProtectSystem=strict` makes `/etc/hanse` read-only, which is what the
Gateway expects. Do not add it to `ReadWritePaths`; replace a capability
package, `nodes.yaml`, or `routes.yaml` on disk and reload: use
`POST /v1/capabilities/reload`, `hanse capabilities install`, or
`systemctl kill --signal=SIGHUP hanse-gateway`. Secret files are read on
every use, so a replaced file needs no restart.

## 9. Upgrade

```console
$ sudo systemctl stop hanse-gateway
$ sudo install -m 0755 hanse-gateway-new /usr/local/bin/hanse-gateway
$ sudo -u hanse hanse-gateway --check-config --config /etc/hanse/gateway.yaml
$ sudo systemctl start hanse-gateway
```

The databases use SQLite in WAL mode and survive the restart. An operation
that was running during the stop is stored as `failed` with
`gateway_restart`; a client retries it with the same `Idempotency-Key` when
the operation is safe to repeat.
