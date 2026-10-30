# Hanse handler contract

Version: `hanse.handler/v1`

A handler is the code that performs a Hanse operation. This document defines how the Hanse Gateway and a handler talk to each other. Anyone can write a handler in any language by following it. A handler needs no Hanse library.

Read the [architecture document](architecture.md) first. It defines the terms used here: Gateway, node, node agent, capability, operation, executor, target, option provider, and privileged operation.

On every executor, the node agent runs `exec` handlers. On the Gateway host, the Gateway's embedded agent does the same. This document says "the agent" for both.

The words **must**, **must not**, **should**, and **may** carry their usual specification meaning.

## Contents

1. [Handler kinds](#1-handler-kinds)
2. [Request envelope](#2-request-envelope)
3. [Response envelope](#3-response-envelope)
4. [exec handlers](#4-exec-handlers)
5. [Exit codes and final states](#5-exit-codes-and-final-states)
6. [Timeouts](#6-timeouts)
7. [Cancellation](#7-cancellation)
8. [Option providers](#8-option-providers)
9. [http handlers](#9-http-handlers)
10. [builtin handlers](#10-builtin-handlers)
11. [Limits](#11-limits)
12. [Rules for handler authors](#12-rules-for-handler-authors)
13. [Versioning and compatibility](#13-versioning-and-compatibility)
14. [JSON Schemas](#14-json-schemas)
15. [Example handlers](#15-example-handlers)

## 1. Handler kinds

| Kind | Transport of the request | Logs | Who may use it |
|---|---|---|---|
| `exec` | A program that the agent runs on the executor. The request goes to stdin, and the response comes from stdout. | Every stderr line, live | Any capability |
| `http` | An HTTP `POST` from the Gateway to a configured URL | None in v1 | Any capability |
| `builtin` | A Go function inside the Gateway | Gateway `system` events | Core Gateway operations only |

All three kinds receive the same [request envelope](#2-request-envelope) and return the same [response envelope](#3-response-envelope).

## 2. Request envelope

The Gateway sends one JSON object to the handler for each call. A call is either an operation run or an option provider call.

```json
{
  "contract": "hanse.handler/v1",
  "kind": "operation",
  "id": "op_01J9Z3",
  "capability": "pc",
  "operation": "up",
  "arguments": {
    "node": "node-b"
  },
  "nodes": {
    "node-b": {
      "id": "node-b",
      "name": "Workstation",
      "os": "windows",
      "arch": "amd64",
      "address": "192.0.2.20",
      "attributes": {
        "mac": "00:00:5e:00:53:01"
      }
    }
  },
  "executor": {
    "id": "node-a",
    "name": "Home server",
    "os": "linux",
    "arch": "arm64",
    "address": "192.0.2.10",
    "attributes": {}
  },
  "secrets": {},
  "deadline": "2026-10-30T08:15:30.000Z"
}
```

| Field | Type | Meaning |
|---|---|---|
| `contract` | string | Always `hanse.handler/v1` for this version. |
| `kind` | string | `operation` for an operation run. `options` for an [option provider](#8-option-providers) call. |
| `id` | string | A unique ID for this call. For `operation`, it is the operation ID, which also appears in history and logs. For `options`, it starts with `opt_`. |
| `capability` | string | The capability ID, such as `pc`. |
| `operation` | string | The operation ID inside the capability, such as `up`. |
| `field` | string | Present only when `kind` is `options`. The argument whose values are requested. |
| `arguments` | object | The arguments. For `operation`, the Gateway has validated them against the operation's schema. For `options`, see [Option providers](#8-option-providers). |
| `nodes` | object | Every node that an argument with the `hanse:node` format refers to, keyed by node ID. It is empty when no argument refers to a node. |
| `executor` | object | The node that runs this handler. |
| `secrets` | object | The secret values that the manifest declares for this operation, keyed by secret name. It is empty when none are declared. |
| `deadline` | string | The time by which the handler must finish, in RFC 3339 format and UTC. See [Timeouts](#6-timeouts). |

### 2.1 Node objects

`nodes` values and `executor` use the same node object. It holds only non-secret configuration.

| Field | Type | Meaning |
|---|---|---|
| `id` | string | The node ID. |
| `name` | string | A human-readable name. |
| `os` | string | The operating system, using Go's `GOOS` values, such as `linux`, `windows`, or `darwin`. |
| `arch` | string | The CPU architecture, using Go's `GOARCH` values, such as `amd64` or `arm64`. |
| `address` | string | The host name or IP address that other nodes use to reach this node. It is empty when the node has none. |
| `attributes` | object | Free-form values from the node's configuration, such as a MAC address. Their meaning belongs to the capabilities that use them. |

A `hanse:node` argument can appear anywhere in the arguments, including inside arrays and nested objects. The handler finds the node by looking up the argument value in `nodes`:

```text
nodes[arguments.node]
```

### 2.2 Secrets

A manifest declares the secrets that an operation needs, by name. The [configuration spec](configuration.md) maps each name to a protected file. The Gateway reads the files and passes their contents in `secrets`, as strings. A value can span several lines, such as a private key.

Secrets travel only inside the envelope. The Gateway never puts them in command-line arguments or environment variables, where other users of the machine could read them. The Gateway removes every secret value from logs, results, error messages, and history.

## 3. Response envelope

The handler returns one JSON object.

A successful run:

```json
{
  "contract": "hanse.handler/v1",
  "status": "ok",
  "result": {
    "state": "online"
  },
  "summary": "node-b is online"
}
```

A failed run:

```json
{
  "contract": "hanse.handler/v1",
  "status": "error",
  "error": {
    "code": "target_unreachable",
    "message": "node-b did not answer on port 22 within 120 seconds",
    "details": {
      "waited_seconds": 120
    }
  }
}
```

| Field | Type | Meaning |
|---|---|---|
| `contract` | string | Optional. When present, it must be `hanse.handler/v1`. |
| `status` | string | `ok`, `error`, or `cancelled`. |
| `result` | object or null | The operation's result. Optional for `ok` and `cancelled`, where it can describe partial work. Ignored for `error`. |
| `summary` | string | Optional. One short line for people, such as the CLI's text output and the history list. |
| `error` | object | Required when `status` is `error`. |
| `error.code` | string | A code that the capability defines, in `snake_case`, such as `target_unreachable`. |
| `error.message` | string | A message for people. |
| `error.details` | object | Optional structured data about the error. |

The Gateway ignores response fields that this version does not define.

Handler error codes belong to the capability. The Gateway reports every handler error to clients under the `Handler failed` error category from the [architecture](architecture.md#14-errors), and it keeps the handler's `code`, `message`, and `details` inside that error.

## 4. exec handlers

### 4.1 Entry command

The manifest gives an entry command for each OS that the capability supports, as an argument list:

```yaml
entry:
  linux: [bash, handler.sh]
  darwin: [bash, handler.sh]
  windows: [powershell, -NoProfile, -NonInteractive, -File, handler.ps1]
```

The full manifest format is in the [configuration spec](configuration.md). One entry command serves every operation and option provider of the capability, because the request names the operation. A manifest can override the entry command for one operation.

The agent runs the entry command as an argument list, never through a shell. Argument values from clients never appear on the command line. They reach the handler only through stdin.

### 4.2 Process environment

| Item | Value |
|---|---|
| Working directory | The capability's directory in the agent's handler cache. Relative paths in the entry command resolve from here. |
| stdin | The request envelope as UTF-8 JSON. The Gateway closes stdin after the envelope. |
| stdout | Reserved for the response envelope. |
| stderr | Log lines. |
| Terminal | None. The handler must not prompt for input. |
| User | The agent's account, without administrator rights. A [privileged operation](#45-privileged-operations) runs as root on Linux and macOS, and with the agent service's full rights on Windows. |

The handler receives only these environment variables, plus the variables that the configuration allow-lists:

| Variable | Value |
|---|---|
| `HANSE_CONTRACT` | `hanse.handler/v1` |
| `HANSE_CALL_ID` | The same value as the envelope's `id`. |
| `HANSE_WORK_DIR` | An empty scratch directory for this call. Only the handler's user can read it. The agent deletes it after the call ends. |
| `HANSE_CANCEL_FILE` | A path that does not exist yet. The agent creates this file to request cancellation. See [Cancellation](#7-cancellation). |
| `HANSE_PRIVILEGED` | `1` when the handler runs with administrator rights, otherwise `0`. |
| `PATH` | The default `PATH` of the executor's user. |
| `HOME` (POSIX) or `USERPROFILE` (Windows) | The user's home directory. |
| `TMPDIR` (POSIX) or `TEMP` and `TMP` (Windows) | The same value as `HANSE_WORK_DIR`. |
| `SystemRoot` (Windows only) | The Windows directory. Many Windows programs fail without it. |
| `LANG` (POSIX) | `C.UTF-8` |

### 4.3 stdout

stdout carries exactly one JSON object, the response envelope. Whitespace before and after it is allowed. Anything else on stdout makes the response invalid, so a handler must send the stdout of the programs it runs to stderr or to a file.

The agent reads the response after the process exits. The handler can write it at any time before exiting.

### 4.4 stderr and logs

Every line that the handler writes to stderr becomes one log entry with the stream `stderr`. The agent reads stderr while the handler runs and sends each line to the Gateway as soon as the line ends. The Gateway streams it on to clients.

- A line ends at `\n`. The agent removes a trailing `\r`.
- The agent replaces bytes that are not valid UTF-8 with U+FFFD.
- When the process exits, the agent also sends a last line that has no `\n`.
- Lines keep their order. Blank lines are kept.
- The agent splits lines that are longer than the [line limit](#11-limits).

Log lines are plain text. The contract gives them no structure, so a handler needs no library to write logs. Many runtimes buffer stderr when it is not a terminal, so a handler should flush after each line. Otherwise, lines can arrive late and all at once.

The agent reads stdout and stderr at the same time, so a handler never blocks because the agent is not reading.

### 4.5 Privileged operations

A manifest marks an operation that needs administrator rights with `privileged: true`. Only `exec` operations can be privileged. The [architecture](architecture.md#85-privileged-operations) describes how a client supplies the password and how each node controls elevation.

For a privileged operation:

- The agent checks the password before it starts the handler. When the check fails, the handler does not start, and the operation fails with `elevation_denied`.
- On Linux and macOS, the agent runs the entry command through `sudo` and its root supervisor, so the handler runs as root. See [Behavior by platform](#72-behavior-by-platform). On Windows, the handler runs with the agent service's full administrator rights. No UAC prompt appears.
- The handler receives the same request envelope, working directory, and environment variables as any other handler, with `HANSE_PRIVILEGED` set to `1`.
- The handler never receives the password.
- [Option providers](#8-option-providers) never run with administrator rights, even for a privileged operation.

A privileged handler should do as little as possible as root. It must treat every argument as untrusted data, as all handlers must.

## 5. Exit codes and final states

A handler should exit with code `0` after writing an `ok` or `cancelled` response, and with code `1` after writing an `error` response.

The Gateway decides the operation's final state from the response and the exit code, in this order. The first matching row wins.

| Case | Final state | Error code |
|---|---|---|
| The Gateway stopped the handler because the deadline passed, and no `ok` response with exit code `0` arrived | `failed` | `timeout` |
| A valid `ok` response, and exit code `0` | `completed` | |
| A valid `error` response, with any exit code | `failed` | the handler's code |
| A valid `cancelled` response after the Gateway requested cancellation, with any exit code | `cancelled` | |
| The Gateway stopped the handler after a cancellation request, and no valid response arrived | `cancelled` | |
| A valid `cancelled` response without a cancellation request | `failed` | `handler_output_invalid` |
| A valid `ok` response, and a nonzero exit code | `failed` | `handler_exit` |
| Exit code `0`, and the response is missing or invalid | `failed` | `handler_output_invalid` |
| A nonzero exit code, and the response is missing or invalid | `failed` | `handler_exit` |
| stdout exceeded the [size limit](#11-limits) | `failed` | `handler_output_too_large` |
| The entry command could not start | `failed` | `handler_start_failed` |
| The handler files on the executor did not match their content hash | `failed` | `handler_integrity` |
| The node agent refused elevation for a privileged operation | `failed` | `elevation_denied` |
| The node agent's connection was lost and did not come back in time | `failed` | `executor_connection_lost` |

The agent reports the exit code, or the signal that ended the process. The Gateway records it in the operation's `system` log.

An `ok` response with exit code `0` always produces `completed`, even when the response arrives after a cancellation request. The work finished, so the Gateway reports that it finished.

The Gateway never retries a handler call on its own.

## 6. Timeouts

Each operation has a timeout from its manifest, or the default from `gateway.yaml`. The Gateway sets `deadline` to the start time of the call plus the timeout.

A handler should check `deadline` before slow steps and stop early when it cannot finish in time. A handler that stops early returns an `error` response with a code of its choice.

The agent enforces the deadline on its own node, so it applies even while the agent is disconnected. When the deadline passes, the agent stops the handler with the same steps as [cancellation](#7-cancellation): it requests a stop, waits for the grace period, and then kills the process tree. The final state is `failed` with the code `timeout`.

## 7. Cancellation

The agent stops a handler when a client cancels the operation or when the deadline passes. The Gateway forwards a client's cancellation to the agent.

### 7.1 Steps

1. **Request a stop.** The agent creates the file at `HANSE_CANCEL_FILE`. On POSIX systems, it also sends `SIGTERM` to the handler's process group.
2. **Wait.** The agent waits for the grace period. The default is 10 seconds. A manifest can set a different grace period for each operation.
3. **Kill.** If the handler is still running, the agent kills its whole process tree.

### 7.2 Behavior by platform

| | POSIX (Linux, macOS) | Windows |
|---|---|---|
| Process grouping | The handler starts as the leader of a new process group, and its children join that group. | The handler starts inside a new job object, and its children join that job object. |
| Stop request | `SIGTERM` to the process group, and the cancel file | The cancel file only. Windows has no reliable signal for a process without a console. |
| Kill | `SIGKILL` to the process group | The agent ends the job object. |

A privileged handler on Linux or macOS runs as root, and the agent's account cannot signal it. The agent therefore starts it through `sudo` and a small supervisor mode of `hanse-node`, which runs as root. The supervisor carries out the stop request and the kill when the agent asks, and it kills the handler if the agent itself exits.

### 7.3 What a handler does

- A handler that runs for more than a few seconds should notice a stop request. It does this by handling `SIGTERM`, by checking whether the cancel file exists, or both. Checking the cancel file works on every platform.
- To stop cleanly, the handler undoes or finishes what it must, writes a `cancelled` response, and exits. The response can carry a `result` that describes the partial work.
- A handler that ignores the stop request is killed when the grace period ends. Its children are killed with it.

## 8. Option providers

An option provider returns the currently valid values for one argument. It uses the same handler and entry command as the capability's operations. The Gateway calls it when a client requests `GET /v1/operations/{capability}/{operation}/options/{field}`.

The request envelope differs from an operation run in these ways:

- `kind` is `options`.
- `field` names the argument.
- `arguments` holds the arguments that the client sent with the options request. They can be incomplete. The Gateway checks the type of each value that is present, but it does not check required arguments. A provider can use these values to narrow its answer.
- `id` starts with `opt_`. The call does not create an operation and does not appear in history.
- The call runs on the executor that the operation would use, chosen by the normal [executor resolution](architecture.md#5-executor-resolution).
- The default timeout is 10 seconds.

The response is an `ok` response whose `result` lists the options:

```json
{
  "status": "ok",
  "result": {
    "options": [
      { "value": "default", "label": "Default profile" },
      { "value": "fast", "label": "Fast profile", "description": "Lower quality, shorter run" }
    ]
  }
}
```

| Field | Type | Meaning |
|---|---|---|
| `value` | any JSON value | A value that is valid for the argument. |
| `label` | string | Optional. A short name for people. |
| `description` | string | Optional. More detail for people. |

The provider runs without administrator rights. Its stderr lines go to the Gateway's own log, not to an operation log. An `error` response, an invalid response, or a timeout makes the options request fail.

Arguments with the `hanse:node` format need no provider. The Gateway answers from its node registry.

## 9. http handlers

An `http` handler is a service that already runs somewhere and accepts the request envelope over HTTP.

### 9.1 Request

The Gateway sends the request envelope as the body of a `POST` to the URL that the manifest gives for the operation:

```http
POST /hanse/v1 HTTP/1.1
Content-Type: application/json
Hanse-Contract: hanse.handler/v1
Hanse-Call-Id: op_01J9Z3
Authorization: Bearer <token>
```

- The Gateway makes the call itself. The executor of an `http` operation is therefore always the Gateway host node.
- An `http` operation cannot be privileged.
- The manifest configures authentication with a credential reference. It can be a bearer token or a named header. The [configuration spec](configuration.md) defines the format.
- The Gateway always verifies the server's TLS certificate. The configuration can add a trusted CA.
- The Gateway does not follow redirects.

### 9.2 Response

The service answers with status `200` and a response envelope as the body.

| HTTP status | Body | Final state |
|---|---|---|
| `200` | A valid response envelope | Decided by `status`, as for `exec` handlers |
| `200` | Missing or invalid | `failed`, `handler_output_invalid` |
| Any other status | A valid `error` response envelope | `failed`, with the handler's error |
| Any other status | Anything else | `failed`, `http_status`, with the HTTP status in the error details |

### 9.3 Timeouts and cancellation

- The Gateway sets the request timeout to `deadline`. When the deadline passes, it closes the connection, and the operation becomes `failed` with the code `timeout`.
- To cancel, the Gateway closes the connection, and the operation becomes `cancelled`. In v1, an `http` handler cannot report a clean stop.
- The service should treat `Hanse-Call-Id` as an idempotency key.

`http` handlers produce no live logs in v1.

## 10. builtin handlers

A `builtin` handler is Go code compiled into the Gateway. It receives the request envelope as a Go value and returns the response envelope as a Go value, so the same rules for results, errors, and final states apply.

`builtin` is reserved for core Gateway operations. A capability package cannot declare a `builtin` handler. Its handlers are always `exec` or `http`.

## 11. Limits

The configuration can change each default.

| Limit | Default | When exceeded |
|---|---|---|
| Size of the response on stdout or in the HTTP body | 1 MiB | The agent kills the handler. The operation fails with `handler_output_too_large`. |
| Length of one log line | 16 KiB | The Gateway splits the line into several entries. |
| Total log size for one operation | 10 MiB | The Gateway stores and streams no further lines, and logs one `system` event that says so. The handler keeps running. |
| Buffered logs and result while the agent is disconnected | 10 MiB | The agent drops the oldest buffered log lines first and logs one `system` event that says so. It never drops the result. |
| Length of `summary` | 1 KiB | The Gateway shortens it. |
| Length of `error.message` | 4 KiB | The Gateway shortens it. |
| Grace period | 10 seconds | |
| Option provider timeout | 10 seconds | The options request fails. |

## 12. Rules for handler authors

A handler:

- **must** read the whole request from stdin before relying on it.
- **must** write exactly one response envelope to stdout, and nothing else.
- **must** send the stdout of the programs it runs to stderr or to a file.
- **must not** pass argument values to a shell, an `eval`, or any other interpreter as code. It passes them as separate arguments or as data.
- **must not** put secrets in results, summaries, error messages, or log lines. The Gateway also redacts known secret values, but that is a safety net, not a permission.
- **must** ignore request fields that it does not know.
- **must not** prompt for input.
- **should** flush stderr after each log line.
- **should** notice a stop request when it runs for more than a few seconds.
- **should** finish before `deadline`.
- **should** write temporary files only in `HANSE_WORK_DIR`.
- **should** make state-changing operations safe to repeat when that is practical, for example by checking the current state first.

## 13. Versioning and compatibility

A manifest declares the contract version that its handlers use:

```yaml
contract: hanse.handler/v1
```

The Gateway refuses to load a manifest with a contract version that it does not support. `GET /v1/version` lists the supported versions.

Within `hanse.handler/v1`, the following changes are allowed. Handlers must keep working when they happen.

- The Gateway adds an optional field to the request envelope or to a node object.
- The agent adds an environment variable.
- The Gateway starts to accept an optional response field. A handler that does not send it keeps the old behavior.
- A limit default grows.

Any other change needs a new major version, such as `hanse.handler/v2`. Examples are removing or renaming a field, changing a field's meaning or type, and changing how the final state is decided.

When a new major version appears, the Gateway keeps supporting the previous one for at least one Gateway minor release, and it sends each handler the version that the handler's manifest declares. A deprecation warning appears in the Gateway log for every manifest that uses the old version.

## 14. JSON Schemas

Both schemas use JSON Schema draft 2020-12.

### 14.1 Request envelope

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "urn:hanse:handler:v1:request",
  "title": "Hanse handler request, hanse.handler/v1",
  "type": "object",
  "required": ["contract", "kind", "id", "capability", "operation", "arguments", "nodes", "executor", "secrets", "deadline"],
  "properties": {
    "contract": { "const": "hanse.handler/v1" },
    "kind": { "enum": ["operation", "options"] },
    "id": { "type": "string", "minLength": 1 },
    "capability": { "type": "string", "pattern": "^[a-z][a-z0-9_-]*$" },
    "operation": { "type": "string", "pattern": "^[a-z][a-z0-9_-]*$" },
    "field": { "type": "string", "minLength": 1 },
    "arguments": { "type": "object" },
    "nodes": {
      "type": "object",
      "additionalProperties": { "$ref": "#/$defs/node" }
    },
    "executor": { "$ref": "#/$defs/node" },
    "secrets": {
      "type": "object",
      "additionalProperties": { "type": "string" }
    },
    "deadline": { "type": "string", "format": "date-time" }
  },
  "if": { "properties": { "kind": { "const": "options" } } },
  "then": { "required": ["field"] },
  "else": { "not": { "required": ["field"] } },
  "$defs": {
    "node": {
      "type": "object",
      "required": ["id", "name", "os", "arch", "address", "attributes"],
      "properties": {
        "id": { "type": "string", "minLength": 1 },
        "name": { "type": "string" },
        "os": { "type": "string", "minLength": 1 },
        "arch": { "type": "string", "minLength": 1 },
        "address": { "type": "string" },
        "attributes": { "type": "object" }
      }
    }
  }
}
```

Request and node objects allow additional properties, because later `v1` releases can add optional fields.

### 14.2 Response envelope

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "urn:hanse:handler:v1:response",
  "title": "Hanse handler response, hanse.handler/v1",
  "type": "object",
  "required": ["status"],
  "properties": {
    "contract": { "const": "hanse.handler/v1" },
    "status": { "enum": ["ok", "error", "cancelled"] },
    "result": { "type": ["object", "null"] },
    "summary": { "type": "string" },
    "error": {
      "type": "object",
      "required": ["code", "message"],
      "properties": {
        "code": { "type": "string", "pattern": "^[a-z][a-z0-9_]*$", "maxLength": 64 },
        "message": { "type": "string" },
        "details": { "type": "object" }
      }
    }
  },
  "if": { "properties": { "status": { "const": "error" } } },
  "then": { "required": ["error"] }
}
```

### 14.3 Options result

When `kind` is `options`, the `result` of an `ok` response must also match this schema:

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "urn:hanse:handler:v1:options-result",
  "title": "Hanse option provider result, hanse.handler/v1",
  "type": "object",
  "required": ["options"],
  "properties": {
    "options": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["value"],
        "properties": {
          "value": true,
          "label": { "type": "string" },
          "description": { "type": "string" }
        }
      }
    }
  }
}
```

## 15. Example handlers

Both examples implement the same small `demo` capability:

- `demo.count` counts from 1 to `to`, one number per second. It logs each number, and it stops cleanly when cancelled.
- `demo.node_info` takes a `node` argument with the `hanse:node` format and returns that node's ID, OS, and address from the request envelope.
- An option provider suggests values for the `to` argument of `demo.count`.

The capability's manifest, in short. The [configuration spec](configuration.md) has the full format.

```yaml
id: demo
name: Demo
description: Example capability for the handler contract
contract: hanse.handler/v1
handler:
  kind: exec
  entry:
    linux: [bash, handler.sh]
    darwin: [bash, handler.sh]
    windows: [python, handler.py]
operations:
  count:
    description: Count to a number, one per second
    mode: async
    timeout: 5m
    arguments:
      type: object
      required: [to]
      properties:
        to: { type: integer, minimum: 1, maximum: 600 }
    options:
      to: provider
  node_info:
    description: Show a node's basic details
    mode: sync
    timeout: 10s
    arguments:
      type: object
      required: [node]
      properties:
        node: { type: string, format: "hanse:node" }
```

### 15.1 bash

This handler needs bash and `jq`.

```bash
#!/usr/bin/env bash
# demo capability handler, hanse.handler/v1
set -euo pipefail

contract=hanse.handler/v1
req=$(cat) # the request envelope is all of stdin

field() { jq -r "$1" <<<"$req"; }
log() { printf '%s\n' "$*" >&2; } # every stderr line is a log line

reply_ok() { # $1: result as JSON
  jq -cn --arg c "$contract" --argjson r "$1" '{contract: $c, status: "ok", result: $r}'
  exit 0
}

reply_error() { # $1: code, $2: message
  jq -cn --arg c "$contract" --arg code "$1" --arg msg "$2" \
    '{contract: $c, status: "error", error: {code: $code, message: $msg}}'
  exit 1
}

# A stop request arrives as SIGTERM, as the cancel file, or both.
term=0
trap 'term=1' TERM
stop_requested() { [[ $term == 1 || -e ${HANSE_CANCEL_FILE:-} ]]; }

kind=$(field .kind)
op=$(field .operation)

if [[ $kind == options ]]; then
  [[ $op.$(field .field) == count.to ]] || reply_error unknown_field "no options for $op.$(field .field)"
  reply_ok '{"options": [{"value": 5, "label": "5 seconds"}, {"value": 60, "label": "1 minute"}]}'
fi

case $op in
  count)
    to=$(field .arguments.to)
    for ((i = 1; i <= to; i++)); do
      if stop_requested; then
        log "stop requested, stopping at $((i - 1))"
        jq -cn --arg c "$contract" --argjson n "$((i - 1))" \
          '{contract: $c, status: "cancelled", result: {counted: $n}}'
        exit 0
      fi
      log "count $i of $to"
      # Run sleep in the background so that the TERM trap runs at once.
      sleep 1 &
      wait $! || true
    done
    reply_ok "{\"counted\": $to}"
    ;;
  node_info)
    reply_ok "$(jq -c '.nodes[.arguments.node] | {id, os, address}' <<<"$req")"
    ;;
  *)
    reply_error unknown_operation "unknown operation: $op"
    ;;
esac
```

### 15.2 Python

This handler needs Python 3.8 or later and no packages. It runs on Linux, macOS, and Windows.

```python
#!/usr/bin/env python3
"""demo capability handler, hanse.handler/v1"""
import json
import os
import signal
import sys
import time

CONTRACT = "hanse.handler/v1"
term = False


def on_term(signum, frame):
    global term
    term = True


def stop_requested():
    # SIGTERM arrives only on POSIX. The cancel file works everywhere.
    cancel_file = os.environ.get("HANSE_CANCEL_FILE")
    return term or (cancel_file is not None and os.path.exists(cancel_file))


def log(line):
    # Every stderr line is a log line. Flush so that it streams live.
    print(line, file=sys.stderr, flush=True)


def reply(status, **fields):
    json.dump({"contract": CONTRACT, "status": status, **fields}, sys.stdout)
    sys.stdout.write("\n")
    return 0 if status != "error" else 1


def error(code, message):
    return reply("error", error={"code": code, "message": message})


def count(req):
    to = req["arguments"]["to"]
    for i in range(1, to + 1):
        if stop_requested():
            log(f"stop requested, stopping at {i - 1}")
            return reply("cancelled", result={"counted": i - 1})
        log(f"count {i} of {to}")
        time.sleep(1)
    return reply("ok", result={"counted": to}, summary=f"counted to {to}")


def node_info(req):
    node = req["nodes"][req["arguments"]["node"]]
    return reply("ok", result={k: node[k] for k in ("id", "os", "address")})


def options(req):
    if (req["operation"], req["field"]) != ("count", "to"):
        return error("unknown_field", f"no options for {req['operation']}.{req['field']}")
    return reply("ok", result={"options": [
        {"value": 5, "label": "5 seconds"},
        {"value": 60, "label": "1 minute"},
    ]})


def main():
    signal.signal(signal.SIGTERM, on_term)
    req = json.load(sys.stdin)
    if req["kind"] == "options":
        return options(req)
    handlers = {"count": count, "node_info": node_info}
    handler = handlers.get(req["operation"])
    if handler is None:
        return error("unknown_operation", f"unknown operation: {req['operation']}")
    return handler(req)


if __name__ == "__main__":
    sys.exit(main())
```

### 15.3 Running a handler by hand

A handler is an ordinary program, so you can test it without a Gateway:

```bash
echo '{"contract":"hanse.handler/v1","kind":"operation","id":"op_test","capability":"demo",
"operation":"count","arguments":{"to":3},"nodes":{},"executor":{"id":"local","name":"",
"os":"linux","arch":"amd64","address":"","attributes":{}},"secrets":{},
"deadline":"2030-01-01T00:00:00Z"}' | bash handler.sh
```

The log lines appear on stderr, and the response appears on stdout:

```text
count 1 of 3
count 2 of 3
count 3 of 3
{"contract":"hanse.handler/v1","status":"ok","result":{"counted":3}}
```
