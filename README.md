# Hanse

> Run named, validated operations on your machines: from a terminal, a script, or an AI agent. No remote shell is involved.

[![CI](https://github.com/achmadss/hanse/actions/workflows/ci.yml/badge.svg)](https://github.com/achmadss/hanse/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Hanse is a small, self-hosted platform. A central **Gateway** publishes **capabilities**. A capability is a named group of operations, such as `pc.up` or `backup.run`. Clients discover these operations and run them on the **nodes** (machines) you allow. The Gateway checks every argument against a schema, picks the machine, runs the handler, and keeps the result and the logs.

**Status: pre-release.** The core works and is tested on Linux, macOS, and Windows, but there is no tagged release yet, and the configuration format may still change.

## Why Hanse?

- **Capabilities, not shell access.** Clients can only run the operations you package. The Gateway has no endpoint that runs arbitrary commands, so an AI agent or a script gets exactly the actions you allow.
- **The CLI learns from the Gateway.** `hanse` reads every capability, schema, node, and valid value from the Gateway, so a new capability works in the CLI with no CLI change. This makes it easy for AI agents to discover what they can do.
- **One small server.** The Gateway is a single Go binary with an embedded SQLite database. It needs no message broker and no database server. Nodes connect out to it over a WebSocket, so they need no open inbound ports.

## Features

- **Handlers in any language.** A handler is an `exec` program (JSON on stdin, JSON on stdout) or an `http` service.
- **Strict validation.** Every argument is checked against JSON Schema on the Gateway, and the CLI turns the schema into flags.
- **Routing.** `routes.yaml` controls which nodes may run each capability, and which node runs it by default.
- **Live logs.** You can stream a handler's stderr as it runs, with automatic reconnects and secret redaction.
- **History.** Every operation is stored with its status, result, and logs. Idempotency keys stop a retry from running an action twice.
- **Privileged operations.** An operation can run with administrator rights after a password check at run time: `sudo` on Linux and macOS, the Windows logon API on Windows. The password is never stored.
- **Cancellation, timeouts, and concurrency limits** on every operation.
- **Runs as a service.** The Gateway runs under systemd or Docker. `hanse-node install` sets up the node agent as a systemd, launchd, or Windows service.

## How it fits together

```
 hanse CLI / script / AI agent
              │  HTTPS + bearer token
              ▼
        Hanse Gateway ── capabilities, routes, history
         │          │
 embedded agent    WebSocket (agent dials out)
 (Gateway host)     │
                    ▼
               hanse-node  (each executor node)
```

The repository builds three binaries:

| Binary | Runs on | Purpose |
|---|---|---|
| `hanse-gateway` | One server | The Gateway: API, routing, validation, history. It can also run handlers for its own machine. |
| `hanse-node` | Each executor node | The node agent: it connects to the Gateway and runs handlers there. |
| `hanse` | Anywhere | The CLI client. |

## Quickstart

This walkthrough runs everything on one Linux or macOS machine, on loopback, without root. It starts a Gateway, enrolls one node, and runs the bundled `echo` capability on that node. It needs Go 1.27 or later.

**1. Build the binaries.**

```sh
git clone https://github.com/achmadss/hanse.git
cd hanse
make build          # writes bin/hanse, bin/hanse-gateway, bin/hanse-node
export PATH="$PWD/bin:$PATH"
```

**2. Write the Gateway configuration** in a new directory.

```sh
mkdir -p ~/hanse-demo && cd ~/hanse-demo
mkdir -m 700 state credentials
cp -r "$OLDPWD/capabilities" ./capabilities

cat > gateway.yaml <<'EOF'
version: 1
node: gateway                 # this machine, as a node
public_url: http://127.0.0.1:8443
listen: 127.0.0.1:8443        # plain HTTP is allowed only on loopback
paths:
  nodes: nodes.yaml
  routes: routes.yaml
  capabilities: [./capabilities]
  credentials_dir: ./credentials
  state_dir: ./state
EOF

cat > nodes.yaml <<'EOF'
version: 1
nodes:
  gateway: { os: linux, arch: amd64 }
  node-a:  { os: linux, arch: amd64 }
EOF

cat > routes.yaml <<'EOF'
version: 1
capabilities:
  echo:
    executors: [gateway, node-a]
    default_executor: gateway
EOF

hanse-gateway --config gateway.yaml --check-config
```

Set `os` and `arch` to your machine's values, for example `darwin` and `arm64` on an Apple silicon Mac.

**3. Create an API token and start the Gateway.**

```sh
hanse-gateway --config gateway.yaml token add me    # prints the token once
hanse-gateway --config gateway.yaml &                # leave it running
hanse login --url http://127.0.0.1:8443             # paste the token at the prompt
```

**4. Enroll `node-a` and start its agent.** The agent normally runs on another machine. Here it runs on the same one, in its own directory.

```sh
hanse-gateway --config gateway.yaml node enroll node-a
# prints: hanse-node join http://127.0.0.1:8443 hjt_...

mkdir -m 700 node
hanse-node join http://127.0.0.1:8443 hjt_... --state-dir ./node --config ./node/agent.yaml
hanse-node run --config ./node/agent.yaml &
```

**5. Run an operation on the node.**

```sh
hanse nodes
hanse run echo say --executor node-a --message "hello from node-a"
hanse run echo sleep --executor node-a --seconds 3 --follow
```

Expected result:

```
ID       OS     ARCH   ROLES  STATUS  ENABLED
gateway  linux  amd64         online  yes
node-a   linux  amd64         online  yes

...
Status:           completed
Executor:         node-a
...
Result:
{
  "said": "hello from node-a"
}
```

The `--follow` run streams each log line as the handler writes it (`stderr: sleep 1 of 3`, and so on), then prints the result.

## Usage

```sh
hanse capabilities                       # what can run
hanse describe echo sleep                # arguments, mode, eligible nodes
hanse run echo sleep --help              # flags built from the schema
hanse run echo sleep --seconds 60 --detach    # print the operation ID and return
hanse logs <operation-id> -f             # follow its log
hanse operations                         # recent operations
hanse operations cancel <operation-id>
```

Every command accepts `--format json` for scripts and AI agents. `logs -f` prints one JSON object per line.

## Installation

There are no published release binaries yet. Build from source with Go 1.27 or later:

```sh
go install github.com/achmadss/hanse/cmd/hanse@latest
go install github.com/achmadss/hanse/cmd/hanse-gateway@latest
go install github.com/achmadss/hanse/cmd/hanse-node@latest
```

`make cross` builds all three binaries for Linux, macOS, and Windows on amd64 and arm64.

For a real deployment, use one of these guides:

- **Gateway:** [systemd](docs/guides/systemd.md), [Docker](docs/guides/docker.md), and [behind a TLS reverse proxy](docs/guides/reverse-proxy.md). [Health checks](docs/guides/health.md) cover monitoring.
- **Nodes:** [Node setup](docs/node-setup.md) covers installing `hanse-node` as a service, enrolling it, and the `sudo` rule for privileged operations, for each OS.

## Documentation

| Document | Contents |
|---|---|
| [Architecture](docs/architecture.md) | Concepts, components, request flow, errors, and security. |
| [Configuration](docs/configuration.md) | Every configuration file and the capability manifest format. |
| [Handler contract](docs/handler-contract.md) | How to write a handler: the request and response envelopes, logs, and exit codes. |
| [Agent protocol](docs/agent-protocol.md) | The WebSocket protocol between the Gateway and `hanse-node`. |

`capabilities/echo` is the smallest complete capability, and it makes a good template for your own.

## Contributing

Issues and pull requests are welcome. Before you open a pull request, run the same checks that CI runs:

```sh
gofmt -l .                # must print nothing
go vet ./...
go test -race ./...
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 run ./...
```

Use [Conventional Commits](https://www.conventionalcommits.org/) for commit messages, for example `feat(cli): ...` or `fix(api): ...`.

## License

[MIT](LICENSE)
