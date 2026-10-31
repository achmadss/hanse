# testkit

The `testkit` capability does no real work. Its operations only write text,
wait, or fail on request, so every Hanse feature can be tested end to end on
real nodes.

- Handler: `sh` on Linux and macOS, PowerShell on Windows. The Linux and
  macOS handler needs `sh` and `jq`. When `jq` is missing, the handler
  returns a `missing_dependency` handler error and exits nonzero.
- The node that runs an operation is chosen with `--executor`, the platform
  mechanism. `testkit.say` takes an optional `target` argument with the
  `hanse:node` format, which exercises node arguments and the `nodes`
  envelope. `testkit.long` takes `node` as the key of its concurrency lock.
- Every log line and every response comes from the real handler. The
  operations cover synchronous and asynchronous runs, logs, cancellation,
  timeouts, output limits, option providers, concurrency, and elevation.

| Operation | Mode | What it covers |
|---|---|---|
| `testkit.say` | sync | Arguments, the target node, the executor, and the result |
| `testkit.long` | async | Live logs, cancellation, and the one-run-per-node concurrency limit |
| `testkit.background` | async | Detach, reconnect, and `hanse logs --follow` over minutes |
| `testkit.fail` | sync | A chosen exit code, and a handler error with `code`, `message`, `details` |
| `testkit.hang` | async | A handler that ignores the stop request, the grace period, and the kill |
| `testkit.timeout` | async | A run that exceeds the manifest timeout |
| `testkit.flood` | async | `handler_output_too_large`, and the log line and total caps |
| `testkit.choose` | sync | An argument whose values come from an option provider |
| `testkit.privileged` | sync | `HANSE_PRIVILEGED` and the real administrator rights |

## Manual end-to-end checklist

The maintainer runs this checklist on Linux, macOS, and Windows before a
release. The addresses, host names, and passwords below are placeholders;
replace them with your own values.

The Linux and macOS handlers need `sh` and `jq`. Install `jq` on every Linux
and macOS node before the checklist; without it, every shell operation fails
with a `missing_dependency` handler error. A macOS node that runs the agent
as a launchd daemon needs `jq` at a system path: the daemon sees only the
system `PATH`, not the Homebrew one. macOS 15 and later ship `/usr/bin/jq`;
on an older macOS, install `jq` at `/usr/bin/jq`, or at another directory in
that system `PATH`.

| Placeholder | Value |
|---|---|
| Gateway URL | `https://gateway.example.com` |
| Gateway host node | `gateway-linux`, `192.0.2.10`, Linux, also a node |
| Windows node | `node-win`, `192.0.2.20`, Windows, runs `hanse-node` |
| macOS node | `node-mac`, `192.0.2.30`, macOS, runs `hanse-node` |
| Elevation password | `placeholder-password` |
| Token | `placeholder-token` |

### 1. Setup

1. Put this directory under the Gateway's capability directory, for example
   `/etc/hanse/capabilities/testkit`, and list that directory in
   `gateway.yaml` under `paths.capabilities`.
2. Add all three nodes to `nodes.yaml`:

   ```yaml
   version: 1

   nodes:
     gateway-linux:
       name: Gateway host
       os: linux
       arch: amd64
       address: 192.0.2.10

     node-win:
       name: Windows node
       os: windows
       arch: amd64
       address: 192.0.2.20

     node-mac:
       name: macOS node
       os: darwin
       arch: arm64
       address: 192.0.2.30
   ```

3. Route the capability in `routes.yaml`:

   ```yaml
   version: 1

   capabilities:
     testkit:
       executors: "*"
       default_executor: gateway-linux
   ```

4. Start the Gateway on the Linux host. The Linux host is also a node, so its
   embedded agent runs every operation whose executor is `gateway-linux`. A
   privileged operation is the one exception; see that step in section 2.
5. Join the Windows and macOS machines with `hanse-node`, and confirm that
   `hanse nodes` shows both as connected. Set `arch` to the machine's
   architecture: `arm64` on Apple silicon, `amd64` on an Intel Mac.
6. Log in on the client: `hanse login --url https://gateway.example.com`.
7. Check discovery: `hanse capabilities show testkit` lists all nine
   operations, and `hanse describe testkit say` shows `target` as optional.

### 2. Operations

Run each step on the Linux host unless it names the Windows or macOS node.
Every step names its executor. The POSIX handler is the same on Linux and
macOS, so every step that names `gateway-linux` also applies to `node-mac`:
use `--executor node-mac`, and `--node node-mac` where the step passes
`--node`. Tick the box only when the output matches.

- [ ] **say, Linux.** `hanse run testkit say --executor gateway-linux --target gateway-linux --text hello`
  prints a result with `arguments`, the target node object, and the executor,
  and the log holds `say: hello`. Run it again without `--target`: the result
  holds a null `target` and the same executor.
- [ ] **say, Windows.** `hanse run testkit say --executor node-win --target node-win --text hello`
  prints the Windows target node object, and the log holds the PowerShell
  handler's `say: hello` line.
- [ ] **say, macOS.** `hanse run testkit say --executor node-mac --target node-mac --text hello`
  prints the macOS target node object, and the log holds the shell handler's
  `say: hello` line.
- [ ] **long, live logs.** `hanse run testkit long --node gateway-linux --executor gateway-linux --seconds 5 --follow`
  prints one `long: second N of 5` line per second, then completes. Repeat
  with `--node node-mac --executor node-mac`.
- [ ] **long, concurrency.** Start `hanse run testkit long --node gateway-linux --executor gateway-linux --seconds 60 --detach`
  and, while it runs, run the same command again. The second run fails with
  the `operation conflict` error, and its details carry the running
  operation ID. Run `hanse run testkit long --node node-win --executor node-win --seconds 60 --detach`
  at the same time; it starts, because the lock applies per node argument.
- [ ] **long, cancellation.** Start `hanse run testkit long --node gateway-linux --executor gateway-linux --seconds 600 --detach`,
  then run `hanse operations cancel <id>`. The state becomes `cancelled`
  within the grace period, the log holds the handler's `stop requested`
  line, and a system entry names the cancellation. Repeat with
  `--node node-mac --executor node-mac`: the shell handler turns the SIGTERM
  into the same `stop requested` line.
- [ ] **background, detach and reconnect.** `hanse run testkit background --executor gateway-linux --seconds 300 --interval_seconds 30 --detach`
  returns at once. `hanse logs <id> -f` prints one `background: tick` line
  every 30 seconds. Close the client, wait, and follow again: the earlier
  lines are still there and the run is still going. Repeat with
  `--executor node-mac`.
- [ ] **fail, exit code.** `hanse run testkit fail --executor gateway-linux --exit_code 3`
  fails with the `handler_exit` error.
- [ ] **fail, handler error.** `hanse run testkit fail --executor gateway-linux --args-json '{"error_code":"target_unreachable","message":"no answer","details":{"waited_seconds":120}}'`
  fails with the `Handler failed` category, and its details keep the code
  `target_unreachable`, the message, and `waited_seconds`. The `details`
  object has no flag, so it travels in `--args-json`.
- [ ] **hang, grace and kill.** `hanse run testkit hang --executor gateway-linux --seconds 600 --detach`,
  then `hanse operations cancel <id>`. The stop request is ignored, the
  operation ends as `cancelled` after the manifest grace period of 2
  seconds, and the log shows the kill. Repeat with `--executor node-win`: the
  agent ends the job object after the grace period. Repeat with
  `--executor node-mac`: the handler ignores SIGTERM, and the agent kills its
  process group after the grace period.
- [ ] **timeout.** `hanse run testkit timeout --executor gateway-linux` fails
  with the `timeout` error after about 3 seconds, the manifest timeout,
  although `seconds` defaults to 30. Repeat with `--executor node-win` and
  `--executor node-mac`.
- [ ] **flood, response limit.** `hanse run testkit flood --executor gateway-linux --mode stdout --kilobytes 2048`
  fails with the `handler_output_too_large` error: the response is larger
  than 1 MiB. Repeat with `--executor node-win` and `--executor node-mac`.
- [ ] **flood, log caps.** `hanse run testkit flood --executor gateway-linux --mode stderr --kilobytes 12288`
  completes, the log ends with the `log limit reached` system entry, and no
  line is longer than 16 KiB. Repeat with a smaller `--kilobytes` to see the
  lines without the cap, and with `--executor node-win` on Windows and
  `--executor node-mac` on macOS.
- [ ] **choose, option provider.** `hanse options testkit choose choice`
  prints `alpha`, `beta`, and `gamma`. `hanse run testkit choose --executor gateway-linux --choice beta`
  completes with `known: true`. Repeat with `--executor node-mac`.
- [ ] **privileged, password prompt.** Run this step only when
  `gateway-linux` is an ordinary executor: `gateway.yaml` sets
  `embedded_agent: false`, `hanse-node` runs on that host with elevation
  enabled, and the unit omits `NoNewPrivileges`. The embedded agent cannot
  run privileged operations, and the Gateway's systemd unit sets
  `NoNewPrivileges=yes`; see
  [privileged operations on the Gateway host](../../docs/guides/systemd.md#privileged-operations-on-the-gateway-host).
  In that layout, as a non-root user on the Linux host,
  `hanse run testkit privileged --executor gateway-linux` prompts for the
  elevation password without echoing it. The result shows
  `hanse_privileged: "1"`, `privileged: true`, and `admin: true`. With a wrong
  password, the run fails with `elevation_denied` before the handler starts.
  Under the embedded agent the same run fails with `elevation_denied` and
  `the embedded agent does not elevate handlers`.
- [ ] **privileged, Windows.** In `agent.yaml` on `node-win`, set
  `elevation.enabled: true` and `elevation.account: <admin account>`, the
  administrator account whose password authorizes the run
  ([node setup](../../docs/node-setup.md#7-privileged-operations)), then
  restart the agent. Run
  `hanse run testkit privileged --executor node-win --password placeholder-password`.
  The result shows `hanse_privileged: "1"`, `privileged: true`,
  `admin: true`, `os: windows`, and the account of the agent service. With a
  wrong password, the run fails with `elevation_denied` before the handler
  starts.
- [ ] **privileged, sudoers.** On `node-mac` (or another Linux node that is
  not the Gateway host), add the sudoers rule from
  [node setup](../../docs/node-setup.md#7-privileged-operations), then run
  `hanse run testkit privileged --executor node-mac`. The prompt reads the
  password of the `hanse-node` account without echoing it. The log holds
  `privileged: HANSE_PRIVILEGED=1 admin=true user=root`, and the result shows
  `hanse_privileged: "1"`, `privileged: true`, `admin: true`, and
  `os: Darwin`.
- [ ] **history.** `hanse operations` lists every run above with its final
  state, and `hanse operations show <id>` keeps the arguments and the
  executor.
