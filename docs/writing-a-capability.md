# Writing a capability

This tutorial builds a capability from an empty directory and runs it on a Gateway. The capability is `audit`. It reports the declared details of a node, and it measures a directory tree on the executor. It uses the parts that most capabilities need:

- A synchronous operation and an asynchronous operation.
- An argument with `hanse:node` format.
- An argument with an option provider.
- A handler that reads the request from stdin, writes one response to stdout, and streams progress to stderr.
- Routing, installation, and a reload without a Gateway restart.

The [README quickstart](../README.md#quickstart) starts a Gateway on loopback and enrolls `node-a`. This tutorial continues in that directory, `~/hanse-demo`, and uses the Gateway's embedded agent as the executor. You need:

- The quickstart Gateway, with `hanse` logged in.
- `python3` on every executor. The manifest also has a `python` entry for Windows.
- The `gateway.yaml`, `nodes.yaml`, and `routes.yaml` files from the quickstart.

The handler contract and the manifest format have many more rules than this tutorial shows. Read the [handler contract](handler-contract.md) for the envelopes, logs, exit codes, limits, and cancellation. Read the [configuration spec](configuration.md#6-manifestyaml) for every manifest field.

## 1. Create the package

A capability is one directory with a `manifest.yaml` and its handler files. Create the source directory next to the Gateway's capability directory:

```sh
mkdir audit
```

The directory layout becomes:

```text
~/hanse-demo/
├── gateway.yaml
├── nodes.yaml
├── routes.yaml
├── capabilities/          # the Gateway scans this directory
│   └── echo/
├── audit/                 # the package you write
│   ├── manifest.yaml
│   └── handler.py
└── ...
```

`hanse capabilities install` copies the package into `capabilities/` later. Keep the source directory separate, so you can edit and install again.

The Gateway sends the whole package to each executor as a bundle, and the agent checks its content hash before every run. The directory must hold only the files the handler needs. It must not contain symbolic links or secrets ([configuration](configuration.md#6-manifestyaml)).

## 2. Write the manifest

Create `audit/manifest.yaml`:

```yaml
version: 1
id: audit
name: Audit
description: Report declared node details and measure directory trees on the executor
contract: hanse.handler/v1

handler:
  kind: exec
  entry:
    linux: [python3, handler.py]
    darwin: [python3, handler.py]
    windows: [python, handler.py]

operations:
  node:
    description: Report the declared details of one node
    mode: sync
    timeout: 10s
    arguments:
      type: object
      required: [node]
      additionalProperties: false
      properties:
        node:
          type: string
          format: "hanse:node"
          description: The node to report

  files:
    description: Walk a directory tree and report how many files and bytes it holds
    mode: async
    timeout: 5m
    arguments:
      type: object
      required: [path]
      additionalProperties: false
      properties:
        path:
          type: string
          minLength: 1
          description: The directory to scan on the executor
        unit:
          type: string
          enum: [auto, bytes, KiB, MiB]
          description: The unit of the reported size
    options:
      unit: provider
```

The manifest declares two operations:

| Operation | Mode | Why |
|---|---|---|
| `node` | `sync` | It answers at once, so the submit response carries the result. Its `node` argument has the `hanse:node` format, so the Gateway validates it against the node registry and sends the node object in the request envelope. |
| `files` | `async` | It can run for a long time, so the submit response returns an operation ID at once. The handler streams progress to stderr while it works. |

The `unit` argument is optional and has an option provider. `hanse options audit files unit` asks the handler for the current values. The values are also in the schema, so the Gateway rejects any other value before the handler starts.

`additionalProperties: false` makes a misspelled argument an error instead of a silently ignored field. The `entry` map names the interpreter for each OS. A node whose OS has no entry command cannot run this capability.

## 3. Write the handler

Create `audit/handler.py`:

```python
#!/usr/bin/env python3
"""audit capability handler, hanse.handler/v1.

Read one request envelope from stdin, write one response envelope to stdout,
and send log lines to stderr only.
"""
import json
import os
import signal
import sys
from datetime import datetime, timezone

CONTRACT = "hanse.handler/v1"
UNITS = ("auto", "bytes", "KiB", "MiB")
STOP = False


def on_term(_signum, _frame):
    global STOP
    STOP = True


def log(line):
    # Every stderr line is one log line. Flush so it streams live.
    print(line, file=sys.stderr, flush=True)


def reply(status, **fields):
    body = {"contract": CONTRACT, "status": status}
    body.update(fields)
    json.dump(body, sys.stdout, separators=(",", ":"))
    sys.stdout.write("\n")
    sys.stdout.flush()
    return 0 if status != "error" else 1


def error(code, message):
    return reply("error", error={"code": code, "message": message})


def stop_requested():
    cancel_file = os.environ.get("HANSE_CANCEL_FILE")
    return STOP or (cancel_file is not None and os.path.exists(cancel_file))


def past_deadline(req):
    deadline = datetime.fromisoformat(req["deadline"].replace("Z", "+00:00"))
    return datetime.now(timezone.utc) > deadline


def size_for(count, unit):
    if unit == "bytes":
        return f"{count} B"
    if unit in ("KiB", "MiB"):
        scale = 1024 if unit == "KiB" else 1024 * 1024
        return f"{count / scale:.1f} {unit}"
    for suffix, scale in (("MiB", 1024 * 1024), ("KiB", 1024)):
        if count >= scale:
            return f"{count / scale:.1f} {suffix}"
    return f"{count} B"


def options(req):
    if req.get("field") != "unit":
        return error("unknown_field", f"no options for {req.get('field')}")
    return reply("ok", result={"options": [
        {"value": "auto", "label": "Automatic", "description": "Pick KiB or MiB when the size is large enough"},
        {"value": "bytes", "label": "Bytes"},
        {"value": "KiB", "label": "KiB"},
        {"value": "MiB", "label": "MiB"},
    ]})


def node(req):
    node_id = req["arguments"]["node"]
    target = req["nodes"].get(node_id)
    if target is None:
        return error("unknown_node", f"the Gateway did not send node {node_id}")
    log(f"reporting node {node_id}")
    return reply(
        "ok",
        result={
            "id": target["id"],
            "name": target["name"],
            "os": target["os"],
            "arch": target["arch"],
            "address": target["address"],
            "attributes": target["attributes"],
        },
        summary=f"{node_id} runs {target['os']}/{target['arch']}",
    )


def files(req):
    arguments = req["arguments"]
    path = arguments["path"]
    unit = arguments.get("unit", "auto")
    if unit not in UNITS:
        return error("unknown_unit", f"unknown unit: {unit}")
    if not os.path.isdir(path):
        return error("not_a_directory", f"{path} is not a directory")

    files_seen = 0
    bytes_seen = 0
    directories = 0
    for root, _dirnames, filenames in os.walk(path):
        if stop_requested():
            log(f"stop requested after {files_seen} files")
            return reply("cancelled", result={"files": files_seen, "bytes": bytes_seen})
        if past_deadline(req):
            return error("deadline_exceeded", "the scan did not finish before the deadline")
        directories += 1
        for name in filenames:
            try:
                bytes_seen += os.path.getsize(os.path.join(root, name))
            except OSError:
                continue
            files_seen += 1
        log(f"scanned {root}: {files_seen} files, {size_for(bytes_seen, unit)}")

    return reply(
        "ok",
        result={
            "path": path,
            "files": files_seen,
            "directories": directories,
            "bytes": bytes_seen,
            "size": size_for(bytes_seen, unit),
        },
        summary=f"{files_seen} files in {directories} directories, {size_for(bytes_seen, unit)}",
    )


def main():
    signal.signal(signal.SIGTERM, on_term)
    req = json.load(sys.stdin)
    if req.get("kind") == "options":
        return options(req)
    handlers = {"node": node, "files": files}
    handler = handlers.get(req.get("operation"))
    if handler is None:
        return error("unknown_operation", f"unknown operation: {req.get('operation')}")
    return handler(req)


if __name__ == "__main__":
    sys.exit(main())
```

The handler follows the shape of every `exec` handler:

- It reads the whole request envelope from stdin and decodes it as JSON.
- It dispatches on `kind` first, because an option provider call is not an operation run, and then on `operation`.
- It writes exactly one response envelope to stdout. The `reply` helper serializes it compactly and flushes it.
- It writes every log line to stderr, and flushes each line so that `--follow` shows it at once.
- It exits with code `0` after `ok` or `cancelled`, and code `1` after `error`.

The error response is a normal part of the contract. `not_a_directory` belongs to this capability. The Gateway keeps the code and message inside the operation's error and marks the operation `failed` ([handler contract](handler-contract.md#5-exit-codes-and-final-states)).

Two request fields matter here:

- `nodes` holds the node objects that the `hanse:node` argument refers to. The handler looks up `nodes[arguments.node]` and never connects to the node.
- `deadline` is the time by which the handler must finish. `files` checks it before each directory. The agent also enforces it, but a handler that checks can return a useful error of its own.

The handler also checks `HANSE_CANCEL_FILE` and handles `SIGTERM`. A client can cancel `files` while it walks a large tree, and the handler then returns a `cancelled` response with its partial count.

> **Security:** the handler runs as the agent's user, so it can read every file that user can read. The `path` argument selects what the capability exposes. In a real deployment, restrict it to a fixed root, or remove the argument and use a constant path, when the agent's account can read files that callers must not see.

## 4. Test the handler without a Gateway

A handler is an ordinary program. Pipe a request envelope into it from the package directory.

The `node` operation reads only the envelope, so this test needs no files:

```sh
cd audit
python3 handler.py <<'EOF'
{"contract":"hanse.handler/v1","kind":"operation","id":"op_local","capability":"audit","operation":"node","arguments":{"node":"node-a"},"nodes":{"node-a":{"id":"node-a","name":"Build server","os":"linux","arch":"amd64","address":"192.0.2.20","attributes":{"rack":"r1"}}},"executor":{"id":"gateway","name":"Gateway host","os":"linux","arch":"amd64","address":"192.0.2.10","attributes":{}},"secrets":{},"deadline":"2030-01-01T00:00:00Z"}
EOF
```

The log line goes to stderr, and the response goes to stdout:

```text
reporting node node-a
{"contract":"hanse.handler/v1","status":"ok","result":{"id":"node-a","name":"Build server","os":"linux","arch":"amd64","address":"192.0.2.20","attributes":{"rack":"r1"}},"summary":"node-a runs linux/amd64"}
```

Now test the handler's own error path. `manifest.yaml` is a file, not a directory:

```sh
python3 handler.py <<'EOF'
{"contract":"hanse.handler/v1","kind":"operation","id":"op_local","capability":"audit","operation":"files","arguments":{"path":"manifest.yaml"},"nodes":{},"executor":{"id":"gateway","name":"Gateway host","os":"linux","arch":"amd64","address":"192.0.2.10","attributes":{}},"secrets":{},"deadline":"2030-01-01T00:00:00Z"}
EOF
echo "exit code: $?"
```

```text
{"contract":"hanse.handler/v1","status":"error","error":{"code":"not_a_directory","message":"manifest.yaml is not a directory"}}
exit code: 1
```

Test the option provider with an `options` envelope. It has `"kind":"options"` and a `field`:

```sh
python3 handler.py <<'EOF'
{"contract":"hanse.handler/v1","kind":"options","id":"opt_local","capability":"audit","operation":"files","field":"unit","arguments":{},"nodes":{},"executor":{"id":"gateway","name":"Gateway host","os":"linux","arch":"amd64","address":"192.0.2.10","attributes":{}},"secrets":{},"deadline":"2030-01-01T00:00:00Z"}
EOF
```

```text
{"contract":"hanse.handler/v1","status":"ok","result":{"options":[{"value":"auto","label":"Automatic","description":"Pick KiB or MiB when the size is large enough"},{"value":"bytes","label":"Bytes"},{"value":"KiB","label":"KiB"},{"value":"MiB","label":"MiB"}]}}
```

The sample envelopes above are valid `hanse.handler/v1` requests. The [handler contract](handler-contract.md#2-request-envelope) describes every field.

## 5. Route and install it

The Gateway learns where a capability may run from `routes.yaml`. Add an `audit` entry under `capabilities`, and keep the existing `echo` entry:

```yaml
version: 1
capabilities:
  audit:
    executors: [gateway]
    default_executor: gateway

  echo:
    executors: [gateway, node-a]
    default_executor: gateway
```

This allows only the Gateway host node to run `audit`, and makes it the default. The embedded agent is always online, so the tutorial needs no second agent. To run on enrolled nodes instead, list them and set one as `default_executor` ([configuration](configuration.md#5-routesyaml)).

The Gateway reads `routes.yaml` and `nodes.yaml` at startup, and a reload re-reads both files as well as the capability directories. `hanse capabilities install` performs that reload, so the route and the new capability become live together, with no restart.

If your `nodes.yaml` does not give `node-a` an address and attributes, add them before you install. The reload reads `nodes.yaml` too. The `audit node` example below uses these values:

```yaml
version: 1
nodes:
  gateway:
    name: Gateway host
    os: linux
    arch: amd64
    address: 192.0.2.10

  node-a:
    name: Build server
    os: linux
    arch: amd64
    address: 192.0.2.20
    attributes:
      rack: r1
```

Check the configuration:

```sh
hanse-gateway --config gateway.yaml --check-config
```

```text
gateway.yaml: configuration is valid
```

Install the package:

```sh
hanse capabilities install ./audit --config ./gateway.yaml --yes
```

The install command reads `paths.capabilities` from `gateway.yaml`, copies the package into the first entry, and asks the Gateway to reload. The reload rescans the capability directories and re-reads `routes.yaml` and `nodes.yaml`, so the new route is live without a restart. `--yes` skips the confirmation prompt and is required when stdin is not a terminal. The output is:

```text
Capability: audit
Name: Audit
Description: Report declared node details and measure directory trees on the executor
Target: capabilities

OPERATION  PRIVILEGED  DESCRIPTION
files      no          Walk a directory tree and report how many files and bytes it holds
node       no          Report the declared details of one node
Installed capability "audit" in capabilities/audit.
Reloaded the Gateway: 2 capabilities.
```

The CLI reloads with the login from `hanse login`. When the CLI is not logged in or the Gateway is unreachable, it warns that the change takes effect after a reload or restart instead. In that case, copy the directory into `capabilities/` by hand and send the Gateway `SIGHUP`.

To pick up a later `routes.yaml` or `nodes.yaml` edit without installing anything, reload the Gateway the same way: send it `SIGHUP` (`kill -HUP <pid>`), or call `POST /v1/capabilities/reload`.

Check that the Gateway sees the capability:

```sh
hanse capabilities
```

```text
ID     NAME   HANDLER  OPERATIONS
audit  Audit  exec     files, node
echo   Echo   exec     say, sleep
```

## 6. Discover and run it

The CLI builds its commands and flags from the Gateway's definitions, so `audit` needs no CLI change. Inspect the synchronous operation:

```sh
hanse describe audit node
```

```text
Capability:         audit
Operation:          node
Description:        Report the declared details of one node
Mode:               sync
Privileged:         no
Executor required:  no
Default executor:   gateway

Arguments:
  FIELD  TYPE                 REQUIRED  DESCRIPTION
  node   string (hanse:node)  yes       The node to report

Executors:
  ID       STATUS  DEFAULT
  gateway  online  yes

Schema:
{
  "additionalProperties": false,
  "properties": {
    "node": {
      "description": "The node to report",
      "format": "hanse:node",
      "type": "string"
    }
  },
  "required": [
    "node"
  ],
  "type": "object"
}
```

The schema became CLI flags. Run the operation and name the target node:

```sh
hanse run audit node --node node-a
```

```text
ID:               op_5e593f325ec141303b7eed26266fa380
Capability:       audit
Operation:        node
Mode:             sync
Status:           completed
Requester:        me
Executor:         gateway
Executor source:  default
Created:          2026-10-31T02:24:43Z
Started:          2026-10-31T02:24:43Z
Finished:         2026-10-31T02:24:43Z
Summary:          node-a runs linux/amd64

Result:
{
  "address": "192.0.2.20",
  "arch": "amd64",
  "attributes": {
    "rack": "r1"
  },
  "id": "node-a",
  "name": "Build server",
  "os": "linux"
}
```

`node-a` did not run anything. The Gateway sent its declared details in the request envelope, and the handler returned them. The operation ran on `gateway`, the eligible executor.

Ask the option provider for the valid units:

```sh
hanse options audit files unit
```

```text
VALUE  LABEL      DESCRIPTION
auto   Automatic  Pick KiB or MiB when the size is large enough
bytes  Bytes
KiB    KiB
MiB    MiB
```

Create a small tree to measure. Use an absolute path: the handler runs with its own copy of the package as its working directory, in the agent's handler cache, so `./demo-data` would resolve there and not in `~/hanse-demo` ([handler contract](handler-contract.md#42-process-environment)).

```sh
mkdir -p demo-data/docs demo-data/images
printf 'first note\n' > demo-data/notes.txt
printf 'alpha\nbeta\n' > demo-data/docs/a.txt
printf 'gamma\n' > demo-data/docs/b.txt
printf 'image placeholder\n' > demo-data/images/one.png

hanse run audit files --path "$PWD/demo-data" --unit bytes --follow
```

`--follow` streams the logs while the operation runs, then prints the final operation:

```text
system: executor resolved: gateway (default)
system: handler started
stderr: scanned /home/you/hanse-demo/demo-data: 1 files, 11 B
stderr: scanned /home/you/hanse-demo/demo-data/docs: 3 files, 28 B
stderr: scanned /home/you/hanse-demo/demo-data/images: 4 files, 46 B
system: handler exited: completed
ID:               op_0799cbd35c8ce92196cac98313b76a63
Capability:       audit
Operation:        files
Mode:             async
Status:           completed
Requester:        me
Executor:         gateway
Executor source:  default
Created:          2026-10-31T02:24:43Z
Started:          2026-10-31T02:24:43Z
Finished:         2026-10-31T02:24:43Z
Summary:          4 files in 3 directories, 46 B

Result:
{
  "bytes": 46,
  "directories": 3,
  "files": 4,
  "path": "/home/you/hanse-demo/demo-data",
  "size": "46 B"
}
```

To start the operation and read its log separately, use `--detach`. It prints only the operation ID:

```sh
op=$(hanse run audit files --path "$PWD/demo-data" --unit bytes --detach)
hanse logs "$op" -f
hanse operations show "$op"
```

`hanse logs -f` prints the same `stderr:` lines and the Gateway's `system:` events. `hanse operations show` prints the summary and the result after the run ends.

Finally, trigger the handler's own error. Pass a file instead of a directory:

```sh
hanse run audit files --path "$PWD/demo-data/notes.txt"
echo "exit code: $?"
```

```text
ID:               op_d77fcc932b10cfcebace41e7864be4fb
Capability:       audit
Operation:        files
Mode:             async
Status:           failed
Requester:        me
Executor:         gateway
Executor source:  default
Created:          2026-10-31T02:25:04Z
Started:          2026-10-31T02:25:04Z
Finished:         2026-10-31T02:25:04Z

Error:
  Code:     not_a_directory
  Message:  /home/you/hanse-demo/demo-data/notes.txt is not a directory
hanse: handler_failed: not_a_directory: /home/you/hanse-demo/demo-data/notes.txt is not a directory ([code operation_id message])
exit code: 33
```

The names in parentheses are the error's detail keys, in no fixed order. The CLI exits with code `33` (`handler_failed`). The operation's error keeps the handler's own code, `not_a_directory`, with its message and details.

## 7. Change it

The installed copy is a snapshot. After you edit the source in `audit/`, remove the installed copy and install it again:

```sh
hanse capabilities remove audit --config ./gateway.yaml --yes
hanse capabilities install ./audit --config ./gateway.yaml --yes
```

You can also edit `capabilities/audit` directly and call `POST /v1/capabilities/reload`. The install and remove commands do that reload for you.

Remove the tutorial capability when you are done:

```sh
hanse capabilities remove audit --config ./gateway.yaml --yes
```

The `echo` capability is a smaller complete example, and its `handler.sh` shows the same contract in a POSIX shell. Continue with these documents when you need more:

- [Handler contract](handler-contract.md): every envelope field, the log rules, exit codes, timeouts, cancellation, and option providers.
- [Configuration](configuration.md): every manifest and `routes.yaml` field, including secrets, privileged operations, concurrency limits, node filters, and `http` handlers.
- [Architecture](architecture.md#7-capabilities-and-handlers): how the Gateway delivers handlers, validates arguments, and resolves executors.
- [Agent protocol](agent-protocol.md): how the Gateway and the node agent exchange bundles, logs, and results.
