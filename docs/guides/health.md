# Health and readiness endpoints

The Gateway serves two probe endpoints outside the versioned API and
outside authentication:

| Route | Purpose | Success | Failure |
|---|---|---|---|
| `GET /healthz` | The process is alive. | `200`, body `ok` | No failure answer; a stopped process refuses the connection. |
| `GET /readyz` | The Gateway can serve API traffic. | `200`, body `ready` | `503`, body `not ready` |

Neither route needs a bearer token, because an orchestrator, a container
runtime, or a load balancer has none to send. Neither route returns a count,
a path, a store name, a version, a build string, or any other internal
detail. A failing probe therefore tells an unauthenticated caller only that
the Gateway is not ready.

## 1. `GET /healthz`

`/healthz` answers while the process runs. It does not check the database,
the capability directories, the token file, or any node connection. A probe
that checked a dependency would make an orchestrator restart a healthy
process when the dependency is what broke.

```console
$ curl --fail --silent http://127.0.0.1:8443/healthz
ok
```

## 2. `GET /readyz`

`/readyz` answers `200` after the Gateway has:

- opened the history database and the enrollment database,
- loaded the capability registry and the node registry,
- loaded the API token file, and
- started listening.

It answers `503` when the Gateway has not finished starting, and it answers
`503` again as soon as shutdown begins, so a load balancer stops sending new
requests before the listener closes.

```console
$ curl --silent --write-out '\n%{http_code}\n' http://127.0.0.1:8443/readyz
ready
200
```

A `503` answer while the Gateway is starting is normal for a second or two.
A `503` answer that persists means the startup failed: read the Gateway log
and run `hanse-gateway --check-config --config <path>`.

Use `/readyz` for traffic decisions and `/healthz` for restart decisions:

| Probe | Question | Wrong answer is expensive because |
|---|---|---|
| `/healthz` | Is the process running? | A dependency check would restart a healthy process. |
| `/readyz` | May it receive traffic? | Traffic sent too early gets errors; traffic after shutdown is lost. |

## 3. Authentication and the reverse proxy

Both routes skip the bearer-token middleware. If a reverse proxy in front of
the Gateway has its own access control, leave `/healthz` and `/readyz`
reachable for the proxy's own health check, or probe the Gateway directly on
its listen address. Do not forward the probes to the public host name when
the deployment should not publish them.

## 4. systemd

The unit uses `Restart=on-failure`, which follows the process and needs no
health directive. A monitoring agent on the host can poll the loopback
address:

```console
$ systemctl status hanse-gateway
$ curl --fail --silent http://127.0.0.1:8443/readyz
```

See [Run the Gateway under systemd](systemd.md) for the full unit.

See [Run the Gateway under systemd](systemd.md) for the full unit.

## 5. Docker and Compose

The image has no `HEALTHCHECK` of its own, because the listen address comes
from `gateway.yaml` and the image does not assume one. The Compose example
sets a health check against `/healthz`; see
[Run the Gateway in Docker](docker.md).

```yaml
healthcheck:
  test: ["CMD", "wget", "--quiet", "--output-document=/dev/null", "http://127.0.0.1:8443/healthz"]
  interval: 30s
  timeout: 3s
  retries: 3
  start_period: 5s
```

When the Gateway serves TLS itself, the probe needs `https` and a trusted
certificate. The simplest answer is to probe the loopback address with the
certificate mounted in the container, or to keep the probe on `/healthz` over
HTTP behind a loopback-terminating proxy.

## 6. Kubernetes-style probes

The routes map directly to a container probe:

```yaml
livenessProbe:
  httpGet:
    path: /healthz
    port: 8443
  periodSeconds: 30
readinessProbe:
  httpGet:
    path: /readyz
    port: 8443
  periodSeconds: 10
  failureThreshold: 3
```

No token and no header are needed. Do not set a `Host` header that the
Gateway would route differently: the probes are plain paths.
