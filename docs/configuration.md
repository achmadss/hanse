# Hanse configuration

This document defines every Hanse configuration file and the capability manifest format. The `config/` directory of the repository holds a complete example of each file. The examples below are the same files.

Read the [architecture document](architecture.md) first. The [handler contract](handler-contract.md) and the [agent protocol](agent-protocol.md) describe how the Gateway uses these settings at run time.

## Contents

1. [Files](#1-files)
2. [Common rules](#2-common-rules)
3. [gateway.yaml](#3-gatewayyaml)
4. [nodes.yaml](#4-nodesyaml)
5. [routes.yaml](#5-routesyaml)
6. [manifest.yaml](#6-manifestyaml)
7. [agent.yaml](#7-agentyaml)
8. [Secrets and credential references](#8-secrets-and-credential-references)
9. [Routing: the single source of truth](#9-routing-the-single-source-of-truth)
10. [Validation](#10-validation)

## 1. Files

| File | Where | Holds | Example |
|---|---|---|---|
| `gateway.yaml` | Gateway host | Network, TLS, paths, timeouts, limits, and agent settings | `config/gateway.example.yaml` |
| `nodes.yaml` | Gateway host | Every node and its non-secret details | `config/nodes.example.yaml` |
| `routes.yaml` | Gateway host | Eligible and default executors | `config/routes.example.yaml` |
| `manifest.yaml` | Each capability directory | The capability's operations, arguments, and handler | `config/manifest.example.yaml` |
| `agent.yaml` | Each executor node | The node agent's settings, including elevation | `config/agent.example.yaml` |

The Gateway also uses two files that its commands manage. Do not edit them by hand:

- The tokens file, named in `gateway.yaml`, which `hanse-gateway token` manages.
- The database in the state directory, which holds history, logs, join tokens, and node credential hashes.

### 1.1 Default locations

| Program | Linux | macOS | Windows |
|---|---|---|---|
| `hanse-gateway` | `/etc/hanse/gateway.yaml` | `/usr/local/etc/hanse/gateway.yaml` | `C:\ProgramData\Hanse\gateway\gateway.yaml` |
| `hanse-node` | `/etc/hanse-node/agent.yaml` | `/usr/local/etc/hanse-node/agent.yaml` | `C:\ProgramData\Hanse\node\agent.yaml` |

Both programs accept `--config <path>` to use another file.

## 2. Common rules

- Every file is YAML and starts with `version: 1`. The version names the file format, not the Hanse release.
- Unknown keys are errors. A misspelled key never gets ignored silently.
- A relative path resolves from the directory of the file that contains it.
- **IDs** of nodes, capabilities, and operations match `^[a-z][a-z0-9_-]*$` and are at most 63 characters long.
- **Durations** are a number followed by a unit: `ms`, `s`, `m`, `h`, or `d`. Examples: `500ms`, `10s`, `5m`, `30d`.
- **Sizes** are a number followed by a unit: `B`, `KiB`, `MiB`, or `GiB`. Example: `16KiB`.
- Secrets never appear in these files. See [Secrets and credential references](#8-secrets-and-credential-references).

## 3. gateway.yaml

### 3.1 Fields

| Field | Type | Default | Meaning |
|---|---|---|---|
| `version` | integer | required | `1` |
| `node` | ID | required | The node in `nodes.yaml` that is the Gateway host. Its handlers run in the Gateway's embedded agent. |
| `public_url` | URL | required | The URL that clients and node agents use to reach the Gateway. `hanse-gateway node enroll` prints it in the join command. |
| `listen` | host:port | `127.0.0.1:8443` | The address the API listens on. |
| `tls.cert_file`, `tls.key_file` | path | none | The TLS certificate and key, in PEM format. Set both or neither. |
| `allow_insecure_http` | boolean | `false` | Allow plain HTTP on a non-loopback address. Use it only behind a TLS reverse proxy. |
| `paths.nodes` | path | `nodes.yaml` | The nodes file. |
| `paths.routes` | path | `routes.yaml` | The routes file. |
| `paths.capabilities` | list of paths | required | Directories to scan for capabilities. Each subdirectory that holds a `manifest.yaml` is one capability. |
| `paths.credentials_dir` | path | required | The directory of secret files. See [Secrets](#8-secrets-and-credential-references). |
| `paths.state_dir` | path | required | The directory for the database. The Gateway creates it if needed. |
| `auth.tokens_file` | path | `<state_dir>/tokens.json` | The API token hashes, managed by `hanse-gateway token`. |
| `history.retention` | duration | `30d` | How long the Gateway keeps operation records and logs. |
| `timeouts.operation` | duration | `5m` | The timeout of an operation whose manifest sets none. |
| `timeouts.options` | duration | `10s` | The timeout of an option provider call. |
| `timeouts.grace_period` | duration | `10s` | The time between a stop request and the kill, when the manifest sets none. |
| `limits.response_size` | size | `1MiB` | The largest response a handler may return. |
| `limits.log_line` | size | `16KiB` | The longest log line before the line is split. |
| `limits.log_total` | size | `10MiB` | The most log data stored for one operation. |
| `limits.summary` | size | `1KiB` | The longest `summary` kept from a response. |
| `limits.error_message` | size | `4KiB` | The longest `error.message` kept from a response. |
| `handlers.env_allow` | list of names | `[]` | Extra environment variables passed to handlers on the Gateway host. The handler contract lists the variables that every handler gets. |
| `agents.join_token_ttl` | duration | `15m` | How long a join token stays valid. |
| `agents.reconnect_window` | duration | `5m` | How long the Gateway waits for a disconnected agent before it fails the agent's running operations. |
| `agents.heartbeat_interval` | duration | `15s` | Sent to agents in `welcome`. |
| `agents.heartbeat_timeout` | duration | `45s` | Sent to agents in `welcome`. Must be at least twice `heartbeat_interval`. |
| `log.level` | string | `info` | `debug`, `info`, `warn`, or `error`. |
| `log.format` | string | `text` | `text` or `json`. |

### 3.2 Rules

- Without TLS, the Gateway listens only on a loopback address, unless `allow_insecure_http` is `true`.
- `public_url` must use `https`, unless its host is a loopback address.
- `node` must name an enabled node in `nodes.yaml`.

### 3.3 Example

```yaml
# Hanse Gateway configuration. See docs/configuration.md.
version: 1

# The node in nodes.yaml that is this Gateway host. Its handlers run in the
# Gateway's embedded agent.
node: gateway

# The URL that clients and node agents use to reach the Gateway.
public_url: https://gateway.example.com

listen: 0.0.0.0:8443
tls:
  cert_file: /etc/hanse/tls/cert.pem
  key_file: /etc/hanse/tls/key.pem
# Set to true only when the Gateway listens on loopback behind a TLS reverse proxy.
allow_insecure_http: false

paths:
  nodes: nodes.yaml
  routes: routes.yaml
  capabilities:
    - /etc/hanse/capabilities
  credentials_dir: /etc/hanse/credentials
  state_dir: /var/lib/hanse

auth:
  tokens_file: /var/lib/hanse/tokens.json

history:
  retention: 30d

timeouts:
  operation: 5m
  options: 10s
  grace_period: 10s

limits:
  response_size: 1MiB
  log_line: 16KiB
  log_total: 10MiB
  summary: 1KiB
  error_message: 4KiB

handlers:
  env_allow: []

agents:
  join_token_ttl: 15m
  reconnect_window: 5m
  heartbeat_interval: 15s
  heartbeat_timeout: 45s

log:
  level: info
  format: text
```

## 4. nodes.yaml

### 4.1 Fields

`nodes` is a map from node ID to the node's settings.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `version` | integer | required | `1` |
| `nodes.<id>.name` | string | the ID | A human-readable name. |
| `nodes.<id>.os` | string | required | `linux`, `darwin`, or `windows`. The agent's reported OS must match. |
| `nodes.<id>.arch` | string | required | `amd64` or `arm64`. The agent's reported architecture must match. |
| `nodes.<id>.roles` | list of strings | `[]` | Free-form labels. Manifests use them in [node filters](#64-node-filters). |
| `nodes.<id>.address` | string | `""` | The host name or IP address that other nodes use to reach this node. Handlers receive it. The Gateway never connects to it. |
| `nodes.<id>.enabled` | boolean | `true` | A disabled node runs nothing, cannot be a target, and cannot connect. |
| `nodes.<id>.attributes` | map | `{}` | Free-form values for capabilities, such as a MAC address. Handlers receive them in the request envelope. |

Nodes have no connection settings, because each node's agent connects to the Gateway. A node that is only a target, such as a machine that other nodes wake or shut down, needs no agent. It stays `offline` and can never be an executor, but requests can still name it in a `hanse:node` argument. Attributes must not hold secrets, because every handler that targets the node receives them.

### 4.2 Example

```yaml
# Hanse nodes. See docs/configuration.md.
version: 1

nodes:
  gateway:
    name: Gateway host
    os: linux
    arch: amd64
    roles: [server]
    address: 192.0.2.10

  node-a:
    name: Always-on server
    os: linux
    arch: arm64
    roles: [server]
    address: 192.0.2.11

  node-b:
    name: Small board computer
    os: linux
    arch: arm64
    roles: [server]
    address: 192.0.2.12

  # A target only. It runs no agent, and other nodes act on it.
  node-c:
    name: Workstation
    os: windows
    arch: amd64
    roles: [pc]
    address: 192.0.2.20
    attributes:
      mac: "00:00:5e:00:53:01"
      broadcast: 192.0.2.255
      ssh_user: hanse

  node-d:
    name: Old laptop
    os: linux
    arch: amd64
    enabled: false
```

## 5. routes.yaml

### 5.1 Fields

`capabilities` is a map from capability ID to its routing.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `version` | integer | required | `1` |
| `capabilities.<cap>.executors` | list of node IDs, or `"*"` | `[]` | The nodes that may run the capability's operations. `"*"` means every enabled node whose OS has an entry command in the manifest. |
| `capabilities.<cap>.default_executor` | node ID | none | The executor when a request names none. |
| `capabilities.<cap>.operations.<op>.executors` | list of node IDs, or `"*"` | the capability's list | Narrows the capability's list for one operation. It cannot add nodes. |
| `capabilities.<cap>.operations.<op>.default_executor` | node ID | the capability's default | The default for one operation. |

### 5.2 Rules

- A default executor must be in the eligible list of its level.
- An operation's list must be a subset of the capability's list. `"*"` at operation level means the capability's whole list.
- The Gateway makes `http` handler calls itself, so an `http` capability's list may contain only the Gateway host node, named by `node` in `gateway.yaml`. `"*"` means that node only.
- A capability that `routes.yaml` does not mention has no eligible executors. The Gateway logs a warning at startup, and every request for it fails with `Executor not eligible`.
- A request can run an operation only on a node from its eligible list. A request that names any other node fails with `Executor not eligible`, even if that node runs an agent.

### 5.3 Example

```yaml
# Hanse routing: which nodes may run each capability, and the defaults.
# A node that is not listed here cannot run the capability. See docs/configuration.md.
version: 1

capabilities:
  demo:
    executors: "*"
    default_executor: gateway

  # node-a and node-b run the pc operations. The node they act on, such as
  # node-c, is an argument of each request.
  pc:
    executors: [node-a, node-b]
    default_executor: node-a
```

## 6. manifest.yaml

A capability is a directory that holds a `manifest.yaml` and the handler files. The Gateway sends the whole directory to executor nodes as a bundle, so the directory must not contain symbolic links, secrets, or files the handler does not need.

### 6.1 Capability fields

| Field | Type | Default | Meaning |
|---|---|---|---|
| `version` | integer | required | `1` |
| `id` | ID | required | The capability ID. It must be unique across all capability directories. |
| `name` | string | the ID | A human-readable name. |
| `description` | string | required | One sentence that says what the capability does. |
| `contract` | string | required | The handler contract version, such as `hanse.handler/v1`. |
| `handler.kind` | string | required | `exec` or `http`. |
| `handler.entry.<os>` | list of strings | none | `exec` only. The entry command for `linux`, `darwin`, or `windows`, as an argument list. At least one OS is required. |
| `handler.url` | URL | none | `http` only. The URL the Gateway posts the request envelope to. Required for `http`. |
| `handler.auth.type` | string | none | `http` only. `bearer` or `header`. |
| `handler.auth.header` | string | none | `http` only, with `type: header`. The name of the header that carries the credential. |
| `handler.auth.credential` | name | none | `http` only. The credential reference for the token. See [Secrets](#8-secrets-and-credential-references). |
| `handler.ca_file` | path | none | `http` only. An extra CA certificate to trust, in PEM format. |
| `secrets` | map | `{}` | The secrets that the capability's operations may request. A map from secret name to a short description. |
| `operations` | map | required | A map from operation ID to the operation's settings. |

### 6.2 Operation fields

| Field | Type | Default | Meaning |
|---|---|---|---|
| `description` | string | required | One sentence that says what the operation does. |
| `mode` | string | `sync` | `sync` returns the result in the submit response. `async` returns an operation ID at once. |
| `timeout` | duration | `timeouts.operation` | The operation's timeout. |
| `grace_period` | duration | `timeouts.grace_period` | The time between a stop request and the kill. |
| `privileged` | boolean | `false` | The operation needs administrator rights and a password at run time. `exec` only. |
| `secrets` | list of names | `[]` | The secrets the handler receives. Each name must be declared in the capability's `secrets`. |
| `concurrency.limit` | integer | none | The most runs of this operation that may run at the same time. |
| `concurrency.key` | list of argument names | `[]` | Count runs separately for each combination of these argument values. With `limit: 1` and `key: [node]`, each node allows one run at a time. |
| `arguments` | JSON Schema | required | The argument schema. Its top level must be `type: object`. |
| `options` | map | `{}` | A map from argument name to `provider`. These arguments get their valid values from the handler's [option provider](handler-contract.md#8-option-providers). |
| `node_filters` | map | `{}` | A map from a `hanse:node` argument name to a [node filter](#64-node-filters). |
| `entry.<os>` | list of strings | `handler.entry.<os>` | `exec` only. Overrides the entry command for this operation. |
| `url` | URL | `handler.url` | `http` only. Overrides the URL for this operation. |

### 6.3 Argument schemas

`arguments` is a JSON Schema, draft 2020-12, written in YAML. The Gateway compiles it at startup and validates every request with it.

The Gateway adds one format: `hanse:node`. A string with this format must be the ID of a known, enabled node that passes the argument's node filter. The handler receives each such node's details in the request envelope.

A schema should set `additionalProperties: false`, so that a misspelled argument fails instead of being ignored.

### 6.4 Node filters

A node filter narrows the nodes that a `hanse:node` argument accepts. A node must pass every field that the filter sets.

| Field | Type | A node passes when |
|---|---|---|
| `os` | list of strings | its OS is in the list |
| `arch` | list of strings | its architecture is in the list |
| `roles` | list of strings | it has at least one of the roles |
| `attributes` | list of names | it has every one of these attributes |

The options endpoint for the argument lists only the nodes that pass.

### 6.5 Example

```yaml
# A capability manifest. Each capability directory holds one manifest.yaml.
# See docs/configuration.md.
version: 1
id: pc
name: PC power
description: Wake, shut down, and check machines from another node
contract: hanse.handler/v1

handler:
  kind: exec
  entry:
    linux: [sh, handler.sh]
    darwin: [sh, handler.sh]

secrets:
  ssh_key: Private key that can log in to target nodes and shut them down

operations:
  up:
    description: Wake a node with Wake-on-LAN and wait until it answers
    mode: async
    timeout: 3m
    concurrency:
      limit: 1
      key: [node]
    node_filters:
      node:
        roles: [pc]
        attributes: [mac, broadcast]
    arguments:
      type: object
      required: [node]
      additionalProperties: false
      properties:
        node:
          type: string
          format: hanse:node
          description: The node to wake

  down:
    description: Shut down a node over SSH and wait until it stops answering
    mode: async
    timeout: 2m
    secrets: [ssh_key]
    concurrency:
      limit: 1
      key: [node]
    node_filters:
      node:
        roles: [pc]
        attributes: [ssh_user]
    arguments:
      type: object
      required: [node]
      additionalProperties: false
      properties:
        node:
          type: string
          format: hanse:node
          description: The node to shut down

  status:
    description: Report whether a node is up
    mode: sync
    timeout: 15s
    node_filters:
      node:
        roles: [pc]
    arguments:
      type: object
      required: [node]
      additionalProperties: false
      properties:
        node:
          type: string
          format: hanse:node
        probe:
          type: string
          description: How to check the node
    options:
      probe: provider
```

## 7. agent.yaml

Each executor node has an `agent.yaml`. Only the node's administrator may be able to change it: on Linux and macOS, root owns it and the agent's account cannot write it. On Windows, only Administrators and SYSTEM can write it. The agent refuses to start when the file is writable by others. The Gateway cannot change this file, so a node keeps control over what runs on it with administrator rights.

### 7.1 Fields

| Field | Type | Default | Meaning |
|---|---|---|---|
| `version` | integer | required | `1` |
| `gateway_url` | URL | required | The Gateway's `public_url`. It must use `https`, unless its host is a loopback address. `hanse-node join` writes it. |
| `ca_file` | path | none | An extra CA certificate to trust for the Gateway, in PEM format. |
| `state_dir` | path | `/var/lib/hanse-node`, or `C:\ProgramData\Hanse\node\state` on Windows | Holds the node credential, the run journal, the handler cache, and scratch directories. Only the agent's account can read it. |
| `max_concurrent_runs` | integer | `16` | The most handlers that run at the same time. |
| `buffer_limit` | size | `10MiB` | The most unacknowledged log data held while disconnected. |
| `handlers.env_allow` | list of names | `[]` | Extra environment variables passed to handlers on this node. |
| `elevation.enabled` | boolean | `false` | Allow privileged operations on this node. |
| `elevation.operations` | list of `<cap>.<op>` | `[]` | Allow only these privileged operations. An empty list allows all of them. |
| `elevation.account` | string | none | Windows only, and required there when elevation is on. The administrator account whose password authorizes elevation. On Linux and macOS, `sudo` checks the password of the agent's account instead. |
| `elevation.lockout.attempts` | integer | `5` | Wrong passwords allowed within `window`. |
| `elevation.lockout.window` | duration | `10m` | The period in which wrong passwords are counted. |
| `elevation.lockout.duration` | duration | `10m` | How long elevation stays refused after too many wrong passwords. |
| `log.level` | string | `info` | `debug`, `info`, `warn`, or `error`. |
| `log.format` | string | `text` | `text` or `json`. |

### 7.2 Example

```yaml
# Hanse node agent configuration. See docs/configuration.md.
# Only the node's administrator may be able to change this file.
version: 1

gateway_url: https://gateway.example.com
# ca_file: /etc/hanse-node/ca.pem

state_dir: /var/lib/hanse-node

max_concurrent_runs: 16
buffer_limit: 10MiB

handlers:
  env_allow: []

elevation:
  enabled: false
  # Allow only these privileged operations. An empty list allows all of them.
  operations: []
  # Windows only: the administrator account whose password authorizes elevation.
  # account: admin-user
  lockout:
    attempts: 5
    window: 10m
    duration: 10m

log:
  level: info
  format: text
```

## 8. Secrets and credential references

Secrets never appear in configuration files, manifests, or capability directories. A configuration names a secret, and the secret's value lives in its own file.

### 8.1 Where secret files live

A secret named in a manifest resolves to one file:

```text
<credentials_dir>/<capability id>/<secret name>
```

For example, the secret `api_token` of the capability `backup` lives in `/etc/hanse/credentials/backup/api_token`. The same rule applies to `handler.auth.credential` for `http` handlers.

- The Gateway reads the file each time a handler needs it, so you can replace a secret without a restart.
- The Gateway removes one trailing newline from the value.
- On Linux and macOS, the Gateway refuses a secret file or directory that the group or other users can read. On Windows, it refuses one that `Users`, `Authenticated Users`, or `Everyone` can read.
- A missing file fails the operation with `Handler failed` and a message that names the secret, never its value.

### 8.2 Other secrets

| Secret | Where it lives |
|---|---|
| API token hashes | The tokens file, managed by `hanse-gateway token` |
| Join token and node credential hashes | The Gateway database |
| The node credential | `<state_dir>/credential` on the node, written by `hanse-node join` |
| Elevation passwords | Nowhere. Clients send them with each request, and they stay in memory. |
| TLS private key | `tls.key_file`, with the same permission rules as secret files |

## 9. Routing: the single source of truth

`routes.yaml` is the only place that says where operations run. It holds the eligible executors and the defaults. Manifests describe what a capability needs. They never name nodes.

One manifest setting still affects routing, because it is a fact about the capability, not a choice of the site: `handler.entry`. A node whose OS has no entry command cannot run the capability. If `routes.yaml` lists such a node by ID, the Gateway refuses to start. `"*"` skips such nodes.

The executor and the target are separate. In the example files, `node-a` and `node-b` are the only executors of `pc`, and `node-c` is a target that a request names in its `node` argument:

| Request | Result |
|---|---|
| `hanse run pc down --node node-c` | Runs on `node-a`, the default, and shuts down `node-c`. |
| `hanse run pc down --node node-c --executor node-b` | Runs on `node-b` and shuts down `node-c`. |
| `hanse run pc down --node node-c --executor gateway` | Fails with `Executor not eligible`, because `gateway` is not in the list for `pc`. |

[Executor resolution](architecture.md#5-executor-resolution) describes how the Gateway picks the executor from these settings.

## 10. Validation

The Gateway validates all its configuration at startup and refuses to start when anything is wrong. `hanse-gateway --check-config` runs the same checks and prints every problem without starting the server. `hanse-node --check-config` does the same for `agent.yaml`.

The checks include:

- Every file has `version: 1`, and every key is known.
- Every ID is valid and unique, and every reference points to something that exists: nodes in `routes.yaml`, `gateway.yaml`'s `node`, secrets in operations, and arguments in `concurrency.key`, `options`, and `node_filters`.
- Every argument schema compiles, and its top level is `type: object`.
- `privileged` appears only on `exec` operations.
- An `http` capability lists no executor other than the Gateway host node.
- Every node that `routes.yaml` lists by ID has an entry command for its OS.
- Every default executor is eligible, and every operation's list is a subset of its capability's list.
- Secret files, the TLS key, and `agent.yaml` have safe permissions.
- `heartbeat_timeout` is at least twice `heartbeat_interval`.

A capability directory with an invalid manifest stops startup too. Skipping it silently could make the Gateway run without operations that clients depend on.
