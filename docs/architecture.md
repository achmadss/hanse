# Hanse architecture

Hanse is a general platform for running controlled operations across a set of machines. A central **Hanse Gateway** publishes **Hanse Capabilities**: named groups of operations that people, scripts, and AI agents can discover and run. The Gateway decides which machine runs each operation, validates every request, runs the operation, and reports the result.

Clients ask for capabilities, not shell commands. Hanse never exposes an endpoint that runs arbitrary commands.

This document describes the concepts, components, and rules that every part of Hanse follows. Three companion documents hold the details:

- [Handler contract](handler-contract.md): how the Gateway and capability handlers talk to each other.
- [Agent protocol](agent-protocol.md): how the Gateway and node agents talk to each other.
- [Configuration](configuration.md): every configuration file and the capability manifest format.

## Contents

1. [Terminology](#1-terminology)
2. [Design rules](#2-design-rules)
3. [Components](#3-components)
4. [Request flow](#4-request-flow)
5. [Executor resolution](#5-executor-resolution)
6. [Targets](#6-targets)
7. [Capabilities and handlers](#7-capabilities-and-handlers)
8. [Node agent](#8-node-agent)
9. [Operation lifecycle](#9-operation-lifecycle)
10. [Operation logs](#10-operation-logs)
11. [Discovery](#11-discovery)
12. [Hanse CLI](#12-hanse-cli)
13. [Configuration overview](#13-configuration-overview)
14. [Errors](#14-errors)
15. [Security](#15-security)
16. [Non-goals and future work](#16-non-goals-and-future-work)

## 1. Terminology

Hanse uses five product terms. Each term names exactly one concept, and the documents never use them as synonyms for each other.

| Term | Meaning |
|---|---|
| **Hanse Gateway** | The server that owns capabilities, routing, validation, execution, and history. It is the single source of truth for what can run, where, and with which arguments. |
| **Hanse CLI** | The `hanse` command-line client. It reads all metadata from the Gateway and keeps no hardcoded copy of capabilities, nodes, or argument values. |
| **Hanse Node** | A known machine. A node can be a client only, an executor, the Gateway host, or more than one of these. |
| **Hanse Capability** | A packaged group of related operations, such as `pc.up`, `pc.down`, and `pc.status` in the `pc` capability. |
| **Hanse Executor** | The node on which an operation's handler runs. Being an executor is a role that a node takes for a given operation. It is not a separate kind of machine. |

Supporting terms:

| Term | Meaning |
|---|---|
| **Operation** | One action inside a capability, written `<capability>.<operation>`, such as `pc.up`. Also used for one run of that action, which has an operation ID. |
| **Handler** | The code that performs an operation. The Gateway runs it on the executor. |
| **Target** | The thing an operation acts on, such as a node, a service, or a file. See [Targets](#6-targets). |
| **Client** | Anything that calls the Gateway API: the Hanse CLI, another program, or an AI agent. |
| **Option provider** | A handler that returns the currently valid values for an argument, such as the items that exist right now. |
| **Node agent** | The `hanse-node` program. It runs on each executor node, connects out to the Gateway, and runs handlers there. See [Node agent](#8-node-agent). |
| **Privileged operation** | An operation that runs with administrator rights on its executor. It needs a password at run time. See [Privileged operations](#85-privileged-operations). |

## 2. Design rules

1. The Gateway is the source of truth for capabilities, operations, nodes, routing, and valid argument values.
2. Clients discover schemas and values from the Gateway. They never invent them.
3. The Gateway validates every request, even when the client already validated it. Client-side validation is a convenience, never a security boundary.
4. A capability can have several eligible executors. An operation can narrow that set.
5. An explicit executor in the request wins over configured defaults. An operation-level default wins over a capability-level default.
6. If a request names no executor and no default exists, the Gateway rejects the request.
7. The Gateway never silently runs an operation on a different executor, and never silently falls back to local execution. Failover and load balancing can only exist as explicit, configured policies.
8. Executor and target are separate concepts. See [Targets](#6-targets).
9. Node configuration, capability definitions, and routing configuration are kept logically separate.
10. Dynamic values come from the capability itself, through option providers. Static manifests never hold values that change at runtime.
11. The Gateway owns all handler code and delivers it to executors. Executor nodes need only the node agent and the interpreter that their handlers use.
12. The Gateway core holds no capability-specific logic. Everything specific to a capability lives in its package.
13. Dedicated clients and aliases are thin wrappers over the Gateway API. They never hold a second copy of capability behavior or routing rules.
14. Nodes connect out to the Gateway. The Gateway never opens a connection to a node, so nodes behind NAT or on changing networks work.

## 3. Components

```text
   Hanse CLI / scripts / AI agents (any node)
                     |
              HTTPS + JSON (+ SSE for logs)
                     |
                     v
   +-----------------------------------------+
   |              Hanse Gateway              |
   |-----------------------------------------|
   | API and authentication                  |
   | Node registry        Capability registry|
   | Argument validation  Executor resolver  |
   | Operation manager    History and logs   |
   | Handler runtimes: builtin, exec, http   |
   | Node connections     Embedded agent     |
   +-----------------------------------------+
            ^                     ^
            |                     |
     WebSocket over HTTPS   WebSocket over HTTPS
     (opened by the node)   (opened by the node)
            |                     |
   +-----------------+   +-----------------+
   | hanse-node      |   | hanse-node      |
   | node agent      |   | node agent      |
   | (exec handlers) |   | (exec handlers) |
   +-----------------+   +-----------------+
     Executor node          Executor node
```

| Component | Responsibility |
|---|---|
| API and authentication | Serves the versioned HTTP API under `/v1`. Authenticates every request with a bearer token and applies per-token permissions. |
| Node registry | Holds every configured node, its connection settings, its `attributes`, and its current availability. |
| Capability registry | Loads capability packages from disk, validates their manifests, and compiles their argument schemas. |
| Argument validation | Checks request arguments against the operation's JSON Schema, including Hanse-specific formats such as `hanse:node`. |
| Executor resolver | Chooses the executor for each request. See [Executor resolution](#5-executor-resolution). |
| Operation manager | Creates operations, enforces idempotency and concurrency rules, dispatches to a runtime, tracks state, and handles cancellation. |
| History and logs | Stores operation records and per-operation logs so that they survive a Gateway restart. |
| Handler runtimes | Run handlers. `builtin` runs Go code inside the Gateway. `exec` runs a program. `http` calls an HTTP endpoint. |
| Node connections | Enrolls node agents, accepts their connections, and tracks which nodes are online. |
| Embedded agent | The node agent's code, built into the Gateway, so the Gateway host can be an executor without a separate agent. |
| Node agent | The `hanse-node` program on each executor node. It receives handler files and run requests, runs handlers, and sends back logs and results. |

The Gateway is a single process. Hanse does not need a message broker, a separate database server, or a service mesh.

## 4. Request flow

A request to run an operation goes through these steps, in this order:

1. **Authenticate.** The Gateway checks the bearer token and the token's permission for the capability and operation.
2. **Look up.** The Gateway finds the capability and the operation in the registry.
3. **Validate.** The Gateway validates the arguments against the operation's schema.
4. **Resolve the executor.** The Gateway chooses the executor and records how it chose. It then checks that the executor is enabled and available.
5. **Check idempotency and concurrency.** A repeated `Idempotency-Key` returns the existing operation. A conflicting running operation causes an `operation conflict` error.
6. **Record.** The Gateway stores the operation as `queued`.
7. **Dispatch.** The Gateway sends the run request to the executor's node agent, and the agent runs the handler. The operation becomes `running`, and the handler's log lines stream into the operation log.
8. **Finish.** The Gateway stores the result and the final state: `completed`, `failed`, or `cancelled`.

The same steps 1 to 4 also back the dry-run endpoint, which validates a request without running it.

## 5. Executor resolution

The Gateway chooses the executor with this precedence:

1. The executor named in the request.
2. The operation-level `default_executor`.
3. The capability-level `default_executor`.
4. Otherwise, reject the request and require an executor.

The chosen executor must be in the operation's eligible executor list. The operation's list, when set, narrows the capability's list.

If the chosen executor is disabled, or its node agent is not connected, the Gateway returns an error. It does not try another executor.

Every accepted operation records the result of resolution:

```json
{
  "executor": "node-a",
  "executor_source": "default"
}
```

`executor_source` is `explicit` or `default`. New values can be added when explicit selection policies are introduced.

`routes.yaml` is the single source of truth for eligible and default executors. See [Routing](configuration.md#9-routing-the-single-source-of-truth).

### Example

The `pc` capability lists `node-a` and `node-b` as eligible executors, and `node-a` is the default. A request to wake `node-c` names `node-c` as its target argument.

- A request with no executor runs on `node-a` and wakes `node-c`.
- A request that names `node-b` runs on `node-b`.
- A request that names `gateway`, or any node that is not in the list, is rejected, because that node is not eligible. A node can run a capability only when `routes.yaml` lists it.
- If `node-a` is offline, a request that relies on the default fails. It does not move to `node-b`.

## 6. Targets

An **executor** is where a handler runs. A **target** is what an operation acts on. They are often different:

| Operation | Executor | Target |
|---|---|---|
| Wake a powered-off machine with Wake-on-LAN | A node on the same network | The machine to wake. It cannot run anything while it is off. |
| Shut down a machine that has no node agent, over SSH | Any node that can reach it | The machine to shut down |
| Report disk usage | The node to inspect | None. The operation acts on its own executor. |

A target is **not** a core request field. An operation that needs a target declares it as an ordinary argument in its schema. This keeps one concept out of the core API and covers targets that are not nodes, such as services or files.

When a target is a node, the argument uses the `hanse:node` string format:

- The Gateway checks that the value is the ID of a known, enabled node.
- A manifest can narrow the allowed nodes, for example to nodes with a given OS or role.
- The options endpoint lists the matching nodes, so clients never guess node IDs.
- The request sent to the handler carries the node's resolved, non-secret configuration, including its `attributes`. The handler does not need to look the node up.

## 7. Capabilities and handlers

### 7.1 Capability packages

A capability is a directory with a `manifest.yaml` and its handler files. The manifest declares:

- The capability's ID, name, and description.
- Each operation's ID, description, and argument schema in JSON Schema.
- Option providers for arguments whose valid values change at runtime.
- The handler kind and an entry command for each supported OS.
- Each operation's mode (synchronous or asynchronous), timeout, and `concurrency` setting.

The Gateway loads every capability directory listed in its configuration. Adding a capability does not require rebuilding Hanse.

### 7.2 Handler kinds

| Kind | How it runs | Use |
|---|---|---|
| `exec` | The Gateway runs a program on the executor, as an argument list and never through a shell. The request goes to stdin as JSON. The final result comes back on stdout as JSON. Every stderr line is a log line. | Most capabilities. Handlers can be written in any language. |
| `http` | The Gateway sends the request as JSON to a configured URL and reads the JSON response. | Capabilities that already have an HTTP service. |
| `builtin` | Go code compiled into the Gateway. | Core Gateway operations only. |

The exact message formats, exit codes, timeout behavior, and cancellation behavior are defined in the [handler contract](handler-contract.md), version `hanse.handler/v1`.

### 7.3 Handler delivery

The Gateway owns every handler. Executor nodes do not install capabilities themselves.

Before the node agent runs an `exec` handler, it checks its cache for the capability's files for its OS, by content hash. When the files are missing or their hash has changed, the Gateway sends them over the agent connection. The agent checks the hash again before every run.

An executor node therefore needs only:

- The node agent, connected to the Gateway.
- The interpreter that the handler's entry command uses, such as a POSIX shell, Python, or PowerShell.

### 7.4 Option providers

Some argument values change at runtime. A capability supplies them through an option provider, which is a handler that returns the current valid values for one argument. The Gateway calls it when a client asks for options. Arguments with the `hanse:node` format need no provider, because the Gateway answers from its node registry.

## 8. Node agent

Every executor node runs the node agent, `hanse-node`. The agent connects out to the Gateway, receives handler files and run requests, runs handlers, and sends their logs and results back. The Gateway never connects to a node.

The Gateway host needs no separate agent. The Gateway contains the agent's code and runs handlers on its own host the same way.

The message format between the Gateway and the agent is defined in the [agent protocol](agent-protocol.md).

### 8.1 Enrollment

A node joins the Gateway once, with a one-time join token:

1. The node must exist in `nodes.yaml`. An operator runs `hanse-gateway node enroll node-a` on the Gateway host. The command prints a join token once.
2. On the node, the operator runs `hanse-node join https://gateway.example.com <join-token>`.
3. The agent sends the join token to the Gateway over HTTPS. The Gateway checks it, marks it used, and returns a node credential.
4. The agent stores the node credential in a file that only the agent's account can read. The Gateway stores only a hash of it.

A join token works once and expires after 15 minutes by default. A node credential belongs to one node ID, so an agent cannot act as another node. `hanse-gateway node revoke node-a` revokes the credential, and the node must enroll again.

### 8.2 Connection and presence

- The agent opens a WebSocket connection to `GET /v1/agent/connect` and authenticates with its node credential. A WebSocket is a long-lived, two-way connection that starts as an ordinary HTTPS request, so it works through reverse proxies and tunnels.
- The connection uses the same TLS rules as the API.
- When it connects, the agent reports its version, its OS and architecture, and its agent protocol version. The Gateway rejects the connection if the OS or architecture differs from `nodes.yaml`, or if it does not support the protocol version.
- Both sides send a heartbeat every 15 seconds. A side that receives nothing for 45 seconds treats the connection as lost.
- A node is `online` while its agent is connected and `offline` otherwise. Executor resolution uses this status.
- The agent reconnects after a lost connection, waiting longer after each failure, up to 60 seconds.
- Each node has at most one connection. A new connection replaces the old one.

A handler keeps running while the connection is down. The agent buffers its log lines and its result, up to the [limits](handler-contract.md#11-limits), and delivers them after it reconnects. If the agent does not reconnect within 5 minutes, the Gateway marks the operation `failed` with the code `executor_connection_lost`. When the agent reconnects later, it stops that handler.

### 8.3 Running handlers

The agent runs each handler as defined in the [handler contract](handler-contract.md). It starts the process, writes the request to stdin, streams stderr lines to the Gateway, and returns the response. It also carries out cancellation and timeouts on its own node, so a stop request works the same way on every executor.

By default, handlers run as the account that runs the agent, with no administrator rights.

### 8.4 Running as a system service

`hanse-node install` registers the agent as a system service that starts at boot, before anyone logs in. The command must run once as root, or from an elevated prompt on Windows. `hanse-node uninstall` removes the service.

| OS | Service manager | Agent account |
|---|---|---|
| Linux | systemd system unit | An account chosen at install time. Privileged operations need an account that may use `sudo`. |
| macOS | launchd daemon | An account chosen at install time, as on Linux. |
| Windows | Windows service, started automatically | The virtual service account `NT SERVICE\hanse-node`, which the installer adds to the local Administrators group. |

On Windows, a service never shows a UAC prompt. It starts with its full set of rights, so the agent works when nobody is at the screen. The agent runs ordinary handlers with a restricted copy of its rights, in which the Administrators group is disabled and administrator privileges are removed. Only privileged operations get the full rights.

A Windows service runs in a separate session, so its handlers cannot open windows on the logged-in user's desktop or interact with it.

### 8.5 Privileged operations

Some operations need administrator rights on their executor, such as a shutdown or a service restart. A manifest marks such an operation with `privileged: true`. Only `exec` operations can be privileged.

A privileged operation needs a password each time it runs:

- The client sends the password with the request. The Hanse CLI prompts for it, or takes it from the `--password` flag.
- The Gateway passes the password to the node agent and keeps it only in memory. It never stores the password in history, logs, idempotency records, or results, and it removes the password from any text it stores or returns.
- The node agent checks the password and starts the handler with administrator rights. The handler never receives the password.
- A request without a password fails with `Elevation required`. A wrong password fails with `Elevation denied`. A password sent for an operation that is not privileged fails with `Invalid arguments`, so that nothing runs with administrator rights by accident.

How the agent checks the password and elevates the handler:

| OS | Password of | How it elevates |
|---|---|---|
| Linux, macOS | The agent's account | The agent runs the handler through `sudo`, which checks the password and runs the handler as root. The agent gives the password to `sudo` through a private pipe, never through arguments or environment variables. |
| Windows | The administrator account named in the agent's configuration | The agent checks the password with the Windows logon API. Then it runs the handler with the service's full rights. |

Each node controls elevation locally. The agent's configuration file can turn elevation off, or allow it only for listed operations. The Gateway cannot change this file. After 5 wrong passwords in 10 minutes, the agent refuses elevation on that node for 10 minutes.

If the agent's account may use `sudo` without a password, `sudo` does not check the password. Do not give the agent's account such a rule.

## 9. Operation lifecycle

### 9.1 States

| State | Meaning |
|---|---|
| `queued` | Accepted and stored, not yet started. |
| `running` | The handler is running on the executor. |
| `completed` | The handler finished and reported success. |
| `failed` | The handler reported an error, timed out, could not start, or the Gateway restarted while it ran. |
| `cancelled` | A client cancelled the operation before it finished. |

### 9.2 Synchronous and asynchronous operations

The manifest declares the mode of each operation:

- A **synchronous** operation returns its result in the response to the submit request.
- An **asynchronous** operation returns an operation ID at once. The client then reads the status, follows the logs, or both.

### 9.3 Idempotency

A client can send an `Idempotency-Key` header. If the Gateway already has an operation with that key from the same requester, it returns that operation instead of starting a new one. Retries therefore never repeat a state-changing action.

### 9.4 Concurrency

An operation's `concurrency` setting limits how many runs can happen at the same time. For example, a manifest can allow only one run at a time for each value of a `node` argument. When a request would break the limit, the Gateway returns an `operation conflict` error that includes the running operation's ID, so the client can follow that operation instead.

### 9.5 Cancellation

A client cancels an operation with `POST /v1/operations/{id}/cancel`:

- A `queued` operation becomes `cancelled` at once.
- For a `running` operation, the Gateway signals the handler, waits for a grace period, and then stops the handler's whole process tree. This works the same way for local and remote executors.

### 9.6 History

The Gateway stores every operation in a local SQLite database. Each record holds the requester, the capability and operation, the arguments with secrets removed, the executor and executor source, timestamps, the status, and a safe summary of the result. Operations that were running when the Gateway stopped are marked `failed` with the reason `gateway_restart`. Old records are removed after a configurable retention period.

## 10. Operation logs

Every operation has an append-only log. Each entry has a sequence number, a timestamp, a stream, and the line text.

| Stream | Source |
|---|---|
| `stderr` | A line that the handler wrote to stderr. The node agent streams it to the Gateway live. |
| `system` | A Gateway event, such as executor resolved, handler started, handler exited, timeout, or cancelled. |

Clients read logs in two ways:

- `GET /v1/operations/{id}/logs` returns the stored lines.
- `GET /v1/operations/{id}/logs?follow=true` streams lines with Server-Sent Events (SSE), a plain HTTP stream that works through reverse proxies. The stream ends with a `status` event when the operation ends. A client that loses the connection resumes with the `Last-Event-ID` header and receives no gaps or duplicates.

The Gateway removes the values of resolved secrets from log lines before it stores or streams them. Logs follow the same retention as history. `http` handlers return only a final result in v1, so they have no live logs.

## 11. Discovery

Discovery and execution are separate parts of the API. Discovery endpoints never change state.

| Endpoint | Purpose |
|---|---|
| `GET /v1/version` | Gateway version, API version, and handler contract version |
| `GET /v1/capabilities` | List capabilities and their operations |
| `GET /v1/capabilities/{id}` | One capability's definition |
| `GET /v1/nodes` | List nodes and their status |
| `GET /v1/nodes/{id}` | One node's details |
| `GET /v1/nodes/{id}/capabilities` | The capabilities and operations a node can execute |
| `GET /v1/operations/{capability}/{operation}/schema` | The argument schema |
| `GET /v1/operations/{capability}/{operation}/describe` | Everything needed to build a valid request |
| `GET /v1/operations/{capability}/{operation}/options/{field}` | The currently valid values for one argument |

Execution endpoints:

| Endpoint | Purpose |
|---|---|
| `POST /v1/operations` | Submit an operation |
| `POST /v1/operations/validate` | Validate a request without running it |
| `GET /v1/operations` | List operations, with filters |
| `GET /v1/operations/{id}` | One operation's status and result |
| `POST /v1/operations/{id}/cancel` | Cancel an operation |
| `GET /v1/operations/{id}/logs` | Read or follow an operation's logs |

Node agent endpoints, used only by `hanse-node`:

| Endpoint | Purpose |
|---|---|
| `POST /v1/agent/join` | Exchange a join token for a node credential |
| `GET /v1/agent/connect` | Open the agent's WebSocket connection |
| `GET /v1/agent/bundles/{hash}` | Download a capability's handler files |

The `describe` response lists the capability and operation IDs, the description, the argument schema, the required arguments, allowed values or references to option providers, the eligible executors, the default executor, whether an executor is required, and availability information when the Gateway has it.

All JSON responses are deterministic, with keys in a stable order, so that scripts and AI agents can parse and compare them.

### Example request

```http
POST /v1/operations
Authorization: Bearer <token>
Content-Type: application/json
```

```json
{
  "capability": "pc",
  "operation": "up",
  "arguments": {
    "node": "node-c"
  }
}
```

A request for a [privileged operation](#85-privileged-operations) also carries the password. In this example, `service.restart` is a privileged operation of a capability named `service`:

```json
{
  "capability": "service",
  "operation": "restart",
  "arguments": { "name": "nginx" },
  "executor": "node-a",
  "elevation": { "password": "<password>" }
}
```

The first request names no executor, so the Gateway resolves one. The response for an asynchronous operation:

```json
{
  "id": "op_01J9Z3",
  "capability": "pc",
  "operation": "up",
  "executor": "node-a",
  "executor_source": "default",
  "status": "running"
}
```

## 12. Hanse CLI

The `hanse` CLI serves people and AI agents equally. It fetches every capability, schema, node, and value from the Gateway, so a new capability works in the CLI with no CLI change.

```bash
hanse capabilities
hanse capabilities show pc
hanse nodes
hanse nodes capabilities node-a

hanse schema pc up
hanse options pc up node
hanse describe pc up
hanse validate pc up --node node-c

hanse run pc up --node node-c
hanse run pc up --node node-c --executor node-b --follow
hanse run pc up --node node-c --detach
hanse run pc down --node node-c

hanse run service restart --name nginx --executor node-a
hanse run service restart --name nginx --executor node-a --password '<password>'

hanse operations
hanse operations show op_01J9Z3
hanse operations cancel op_01J9Z3
hanse logs op_01J9Z3 -f
```

Behavior:

- `run` builds its flags from the operation's schema at runtime. `--args-json` and `--args-file` accept complex input.
- `run` waits for the operation to finish by default. `--detach` returns the operation ID at once. `--follow` streams the logs while waiting.
- Pressing Ctrl+C while following stops the following only. It does not cancel the operation.
- For a privileged operation, `run` prompts for the password without showing it. When the terminal is not interactive, the password must come from `--password`. Without it, `run` fails with `Elevation required`.
- `--password` puts the password on the command line. The shell can save it in its history, and other users of the same machine can see it while the command runs. The CLI prints a warning to stderr every time `--password` is used.
- Every command accepts `--format text|json`. JSON output is deterministic and has no colors. Followed logs in JSON form print one object per line.
- Exit codes map to the [error codes](#14-errors).
- The CLI checks `GET /v1/version` and warns when its API version does not match the Gateway's.

Dedicated commands or aliases, such as a short `pc up`, may exist later. They must be thin wrappers that send the same API requests.

## 13. Configuration overview

| File | Holds |
|---|---|
| `gateway.yaml` | Listen address, TLS, authentication, capability directories, history storage, and default timeouts. |
| `nodes.yaml` | Each node's ID, name, OS, architecture, role, address, `enabled` flag, and free-form `attributes`. |
| `routes.yaml` | Eligible executors and default executors, at capability level and at operation level. |
| `manifest.yaml` | One per capability package. See [Capability packages](#71-capability-packages). |
| `agent.yaml` | On each executor node. The Gateway URL, the path of the stored node credential, the cache directory, and the elevation settings. Only the node's administrator can change it. |

Secrets never appear in these files. A node or handler refers to a secret with a credential reference that points to a protected file. The full format is in [configuration.md](configuration.md).

The Gateway validates all configuration at startup and refuses to start when it is invalid. `hanse-gateway --check-config` runs the same checks without starting the server.

## 14. Errors

The Gateway, the runtimes, and the CLI share one set of error codes. Each code has a fixed HTTP status and a fixed CLI exit code.

| Error | Meaning |
|---|---|
| Unknown capability | The capability does not exist. |
| Unknown operation | The operation does not exist in that capability. |
| Invalid arguments | The arguments do not match the schema. |
| Missing argument | A required argument is absent. |
| Invalid executor | The named executor is not a known node. |
| Executor not eligible | The executor exists but cannot run this operation. |
| No default executor | The request named no executor, and no default is configured. |
| Executor unavailable | The executor is disabled, or its node agent is not connected. |
| Authentication failed | The token is missing, wrong, or revoked. |
| Forbidden | The token is valid but lacks permission for this operation. |
| Elevation required | A privileged operation was requested without a password. |
| Elevation denied | The password is wrong, elevation is turned off or not allowed for this operation on the node, or too many wrong passwords were given. |
| Operation conflict | A run that the `concurrency` setting forbids running alongside this one is already in progress. |
| Handler failed | The handler ran and reported an error, or returned malformed output. |
| Timeout | The operation exceeded its deadline. |
| Cancelled | A client cancelled the operation. |

Error messages never contain secrets.

## 15. Security

- **Authentication:** every request needs a bearer token. The Gateway stores only token hashes and compares tokens in constant time. Tokens are created with `hanse-gateway token add`, which prints the token once.
- **Authorization:** a token can carry an allow list of capabilities and operations.
- **Transport security:** the Gateway serves TLS when it has a certificate. It refuses to listen on a non-loopback address without TLS, unless `allow_insecure_http: true` is set for use behind a TLS reverse proxy.
- **No arbitrary execution:** the Gateway runs only registered operations. Handlers run as argument lists, never through a shell, and a handler must never turn user input into a shell command.
- **Validation:** the Gateway validates every argument against server-owned schemas.
- **Secrets:** secrets live in protected files referenced by configuration, never in ordinary YAML or client binaries. The Gateway removes secret values from logs, history, results, and error messages.
- **Node agents:** an agent joins with a one-time token and then uses a node credential that belongs to one node only. The Gateway stores only its hash and can revoke it. Agents connect out over TLS, so nodes accept no incoming connections.
- **Elevation:** handlers run without administrator rights unless their operation is privileged. A privileged operation needs a password for each run, the node can turn elevation off or limit it, and the agent locks elevation after repeated wrong passwords. The password is never stored.
- **Handler integrity:** the Gateway checks a handler's content hash on the executor before every run.
- **Least environment:** handlers receive only allow-listed environment variables.
- **Limits:** every operation has a timeout, and every connection has a connect timeout. Handler output and logs have size caps.
- **Audit:** every operation records who requested it, what ran, where, when, and how it ended.

## 16. Non-goals and future work

Hanse v1 does not include:

- Automatic failover or load balancing. These may arrive later as explicit, configured policies.
- An SSH transport for nodes that cannot run the node agent. A handler on another node can still reach such a machine over SSH.
- Handlers that use the logged-in user's desktop on Windows.
- Live logs from `http` handlers.
- Capability versioning beyond the manifest and contract versions.

Possible future extensions, added only when a real need appears:

- Round-robin or least-busy executor selection.
- More transports, such as SSH.
- Structured progress events from handlers, in addition to stderr log lines.
