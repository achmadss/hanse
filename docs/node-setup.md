# Run the node agent as a system service

`hanse-node install` registers the node agent as an operating system
service. The service starts at boot, before anyone logs in, so an always-on
node is online without a person at the screen. `hanse-node uninstall`
removes it again.

Both commands need the administrator's rights: root on Linux and macOS, and
an elevated prompt on Windows. The command refuses to run with less, and it
changes nothing when it refuses.

Every value in this guide is a placeholder: `gateway.example.com` for the
Gateway's public host name, `192.0.2.11` for the node's address, and
`00:00:5e:00:53:01` for a node's hardware address. The node ID is `node-a`
and the service account is `hanse-node`.

| File | Linux | macOS | Windows |
|---|---|---|---|
| `agent.yaml` | `/etc/hanse-node/agent.yaml` | `/usr/local/etc/hanse-node/agent.yaml` | `C:\ProgramData\Hanse\node\agent.yaml` |
| State directory | `/var/lib/hanse-node` | `/var/lib/hanse-node` | `C:\ProgramData\Hanse\node\state` |
| Service definition | `/etc/systemd/system/hanse-node.service` | `/Library/LaunchDaemons/dev.hanse.node.plist` | The service manager's database |

## 1. Before you start

1. Add the node to the Gateway's `nodes.yaml`. The agent reports its own OS
   and architecture, and the Gateway refuses a node whose entry disagrees.
2. Install the binary at a path that stays put. The service starts the exact
   path you run `install` from, so run it from its installed location:

   ```console
   # Linux and macOS
   $ sudo install -m 0755 hanse-node /usr/local/bin/hanse-node
   $ hanse-node --version
   ```

   ```powershell
   # Windows: copy hanse-node.exe to C:\Program Files\Hanse\ and run it there
   PS> & 'C:\Program Files\Hanse\hanse-node.exe' --version
   ```

3. Make sure the node's clock is correct. The enrollment token expires after
   15 minutes, and a wrong clock rejects it.

## 2. Create the service account (Linux and macOS)

The agent runs handlers as its own account, so a dedicated system account is
enough. It needs no privileges unless this node runs privileged operations.

Linux:

```console
$ sudo useradd --system --home-dir /var/lib/hanse-node \
    --shell /usr/sbin/nologin hanse-node
```

macOS:

```console
$ sudo sysadminctl -addUser hanse-node -fullName "Hanse node agent" \
    -shell /usr/bin/false
```

Install gives the agent's primary group read access to `agent.yaml`, so the
account also needs a group. Both commands create one with the same name.

Give the account a password only when this node runs privileged operations:
on Linux and macOS the password that authorizes such an operation is the
password of the service account, and `sudo` asks for it. A node without
privileged operations works with an account that cannot log in.

Windows needs no account work. The service runs as the virtual account
`NT SERVICE\hanse-node`, which the service manager creates from the service
name, and `install` adds it to the local Administrators group.

## 3. Enroll the node

Enroll first and install second. `hanse-node join` records the Gateway URL
beside the credential, and `install` copies it into the default
`agent.yaml`, so the agent can start as soon as the service is registered.

On the Gateway host, print a join token once:

```console
$ hanse-gateway node enroll node-a
```

On the node, exchange the token for a node credential. Run this as root, or
from an elevated prompt on Windows:

```console
$ sudo hanse-node join https://gateway.example.com hjt_<join-token>
enrolled node node-a; the credential is stored in /var/lib/hanse-node/credential
```

The credential never appears in the output or in a log. The join token works
once.

## 4. Install the service

### Linux

```console
$ sudo hanse-node install --user hanse-node
hanse-node: created /etc/hanse-node
hanse-node: wrote the default /etc/hanse-node/agent.yaml when there was none, and set its owner and mode
hanse-node: created the state directory /var/lib/hanse-node
hanse-node: gave the node credential in /var/lib/hanse-node to hanse-node
hanse-node: wrote the systemd unit /etc/systemd/system/hanse-node.service
hanse-node: reloaded the systemd manager configuration
hanse-node: enabled hanse-node.service at boot
hanse-node: started hanse-node.service
hanse-node: installed /etc/systemd/system/hanse-node.service for hanse-node, and it is running now
hanse-node: it starts at boot without a login
```

The unit is a system unit, so it starts with nobody logged in. It runs as
the account from `--user` and restarts the agent after a crash
(`Restart=on-failure`, `RestartSec=2s`). A clean stop, such as
`systemctl stop`, stays stopped.

Its hardening is deliberately light:

- `UMask=0077` keeps the files the agent creates private to it.
- `PrivateTmp=yes` gives handlers their own `/tmp`.
- It does **not** set `NoNewPrivileges`. A privileged operation needs `sudo`
  to gain root, and `NoNewPrivileges` would make that impossible.
- It does **not** set `CapabilityBoundingSet=`, `ProtectSystem`, or
  `ProtectKernelTunables`. A unit's sandbox applies to `sudo` and to the root
  supervisor it starts, so those settings would also block a privileged
  handler that restarts a service or shuts the node down.

### macOS

```console
$ sudo hanse-node install --user hanse-node
hanse-node: created /usr/local/etc/hanse-node
hanse-node: wrote the default /usr/local/etc/hanse-node/agent.yaml when there was none, and set its owner and mode
hanse-node: created the state directory /var/lib/hanse-node
hanse-node: gave the node credential in /var/lib/hanse-node to hanse-node
hanse-node: unloaded an earlier dev.hanse.node
hanse-node: wrote the launchd daemon /Library/LaunchDaemons/dev.hanse.node.plist
hanse-node: loaded dev.hanse.node into launchd
hanse-node: enabled dev.hanse.node at boot
hanse-node: started dev.hanse.node
hanse-node: installed /Library/LaunchDaemons/dev.hanse.node.plist for hanse-node, and it is running now
hanse-node: it starts at boot without a login
```

The daemon runs as the account from `--user`. `RunAtLoad` starts it at boot,
and `KeepAlive` with `SuccessfulExit` false restarts it after a crash and
leaves a clean stop alone. Its output goes to `/var/log/hanse-node.log`.

### Windows

Run from an elevated prompt:

```powershell
PS> & 'C:\Program Files\Hanse\hanse-node.exe' install
hanse-node: created C:\ProgramData\Hanse\node
hanse-node: registered the hanse-node service as NT SERVICE\hanse-node
hanse-node: added NT SERVICE\hanse-node to the Administrators group
hanse-node: wrote the default C:\ProgramData\Hanse\node\agent.yaml when there was none
hanse-node: gave NT SERVICE\hanse-node read access to C:\ProgramData\Hanse\node\agent.yaml
hanse-node: created the state directory C:\ProgramData\Hanse\node\state
hanse-node: gave the state directory C:\ProgramData\Hanse\node\state to NT SERVICE\hanse-node
hanse-node: gave the node credential in C:\ProgramData\Hanse\node\state to NT SERVICE\hanse-node
hanse-node: started the hanse-node service
hanse-node: installed hanse-node for NT SERVICE\hanse-node, and it is running now
hanse-node: it starts at boot without a login
```

The service starts automatically. It runs as `NT SERVICE\hanse-node`, which
holds its administrator rights without a UAC prompt, so privileged
operations work while nobody is at the screen. The installer finds the local
Administrators group by its well-known SID `S-1-5-32-544`, because the
group's name is localized.

The service manager restarts the agent after a crash, or after an exit with
a non-zero code that nobody asked for: 5 seconds after the first failure,
then 10, then 30, and the count resets after a day. It never restarts the
agent after `Stop-Service` or `sc stop`, because the agent reports a clean
stop for those.

A Windows service runs in its own session, so its handlers cannot open
windows on the logged-in user's desktop.

## 5. What install writes

| Path | Mode and owner | Purpose |
|---|---|---|
| `agent.yaml` | `0640`, root with the agent's group; on Windows an access control list for Administrators and SYSTEM to write, and the service account to read | The agent's settings. Written only when the file does not exist. |
| State directory | `0700`, the service account; on Windows the same access control list | The credential, the run journal, the handler cache, and scratch directories. |
| The credential inside it | `0600`, the service account | Written by `hanse-node join`. |
| The unit or plist | `0644`, root | The service definition. |

Only the node's administrator can change `agent.yaml`. The agent refuses to
start when the file is writable by others, and the Gateway cannot change it.

`install` never replaces an existing `agent.yaml` and never touches the
credential. It does repair the owner and the mode of an existing
`agent.yaml`, so a file that `join` wrote as root stays readable by the
service account and writable by nobody else.

If you ran `install` before enrolling, add the Gateway URL and run `install`
again. Nothing replaces the file, so `join` is the command that fills its
one missing line:

```console
$ sudo hanse-node join https://gateway.example.com hjt_<join-token> \
    --config /etc/hanse-node/agent.yaml
$ sudo hanse-node install --user hanse-node
```

The second install repairs the file's owner and group, and starts the
service.

## 6. Check the status

The agent logs one line when it connects:

```console
# Linux
$ systemctl status hanse-node
$ journalctl -u hanse-node -f
hanse-node: connecting to https://gateway.example.com
```

```console
# macOS
$ sudo launchctl print system/dev.hanse.node | head -n 20
$ sudo tail -f /var/log/hanse-node.log
```

```powershell
# Windows: the service state and the agent's log
PS> Get-Service hanse-node
PS> Get-Content 'C:\ProgramData\Hanse\node\state\hanse-node.log' -Wait
```

A Windows service has no console, and the service manager discards its
standard output and standard error, so the agent writes its own lines to
`<state_dir>\hanse-node.log`. The file rotates to `hanse-node.log.1` when it
reaches 10 MiB, and only that one old file is kept. It is created inside the
state directory, so it has the state directory's access control list:
Administrators and SYSTEM, and the service account. It never holds the node
credential or an elevation password; the agent does not log either.

From the Gateway host, the node is online while its agent stays connected:

```console
$ hanse nodes show node-a
```

## 7. Privileged operations

Privileged operations are off until `agent.yaml` turns them on. The flag is
local to the node, and the Gateway cannot change it:

```yaml
elevation:
  enabled: true
  # Optional: allow only these operations. An empty list allows all of them.
  operations:
    - power.reboot
```

Restart the service after the change.

### sudoers rule (Linux and macOS)

On Linux and macOS the agent runs a privileged handler through `sudo`. Give
the service account permission for that one command, and for nothing else:

```console
$ sudo tee /etc/sudoers.d/hanse-node >/dev/null <<'EOF'
hanse-node ALL=(root) /usr/local/bin/hanse-node supervise *
EOF
$ sudo chmod 0440 /etc/sudoers.d/hanse-node
$ sudo visudo -cf /etc/sudoers.d/hanse-node
/etc/sudoers.d/hanse-node: parsed OK
```

Use the absolute path of the installed binary. If the agent runs as another
account, replace `hanse-node` at the start of the rule with that account.

The rule must require a password. **Never add `NOPASSWD`.** The agent passes
the password to `sudo` on a private pipe, and it checks that `sudo` really
read one. With `NOPASSWD`, `sudo` accepts any password, so the agent refuses
every privileged run instead of trusting the answer. The rule above leaves
the password check in place.

Why the wildcard after `supervise`: `sudo` matches the whole argument string
of the command, and the supervisor's arguments name the handler's working
directory and a control file, which differ for every run. The rule allows
`/usr/local/bin/hanse-node supervise` with any arguments and no other
command, and the supervisor itself runs as root only through this rule. The
agent's own password check calls the same command with `--check`, so the one
rule covers both.

### Windows

Windows needs no sudoers rule. The service already holds its administrator
rights. Set the administrator account whose password authorizes a
privileged operation, because `agent.yaml` requires it when elevation is on:

```yaml
elevation:
  enabled: true
  account: admin-user
```

## 8. Uninstall

```console
# Linux
$ sudo hanse-node uninstall
hanse-node: stopped hanse-node.service and disabled it at boot
hanse-node: removed /etc/systemd/system/hanse-node.service
hanse-node: reloaded the systemd manager configuration
hanse-node: removed /etc/systemd/system/hanse-node.service; /etc/hanse-node/agent.yaml and the node credential in /var/lib/hanse-node are kept
```

```console
# macOS
$ sudo hanse-node uninstall
hanse-node: unloaded dev.hanse.node
hanse-node: removed /Library/LaunchDaemons/dev.hanse.node.plist
hanse-node: removed /Library/LaunchDaemons/dev.hanse.node.plist; /usr/local/etc/hanse-node/agent.yaml and the node credential in /var/lib/hanse-node are kept
```

```powershell
# Windows, from an elevated prompt
PS> & 'C:\Program Files\Hanse\hanse-node.exe' uninstall
hanse-node: stopped the hanse-node service
hanse-node: removed NT SERVICE\hanse-node from the Administrators group
hanse-node: deleted the hanse-node service
hanse-node: removed hanse-node; C:\ProgramData\Hanse\node\agent.yaml and the node credential in C:\ProgramData\Hanse\node\state are kept
```

A second uninstall is safe: every service step treats a missing service,
unit, or group membership as already done, and the command reports the same
removal.

Uninstall keeps `agent.yaml` and the credential, so a later install needs no
new enrollment. It removes the Windows service and the account's membership
of the Administrators group. To remove the node's data as well, delete the
state directory by hand, and remove the service account on Linux and macOS:

```console
$ sudo userdel hanse-node
```

If an uninstall cannot run, remove the definition by hand: delete the unit
followed by `systemctl daemon-reload`, or delete the plist, or run
`sc delete hanse-node` from an elevated prompt.

## 9. Manual check: start after a reboot

The service must start with nobody logged in. Check it once on each
operating system, on a real machine:

1. Confirm the node is online first, so a failure is not a stale connection.
2. Log out of every session on the node, then reboot it. Do not log in.
3. Wait two minutes.
4. From another machine, ask the Gateway for the node: `hanse nodes show
   node-a` reports it online.
5. Run one harmless operation on it. The `echo` capability proves that
   handlers run: `hanse capabilities show echo`, then run `echo.say` with a
   message. The result names `node-a` as the executor.
6. Check that the service is what started it:

   | OS | Check |
   |---|---|
   | Linux | `systemctl show hanse-node -p ActiveState -p NRestarts` reports `ActiveState=active` |
   | macOS | `sudo launchctl print system/dev.hanse.node` shows `state = running` |
   | Windows | `Get-Service hanse-node` shows `Running` |

7. Kill the agent process to prove the crash restart, then check again:

   ```console
   # Linux
   $ sudo systemctl kill -s SIGKILL hanse-node
   $ sleep 5 && systemctl show hanse-node -p NRestarts
   ```

   ```console
   # macOS
   $ sudo kill -9 $(pgrep -f '/usr/local/bin/hanse-node run')
   $ sleep 10 && sudo launchctl print system/dev.hanse.node | grep -m1 state
   ```

   ```powershell
   # Windows: stop the process, not the service, so the manager sees a failure
   PS> Get-Process hanse-node | Stop-Process -Force
   PS> Start-Sleep 30; Get-Service hanse-node
   ```

The node is correct when it is online after a reboot with no session open,
runs a handler, and comes back after the agent is killed.

## 10. Troubleshooting

- `the account "hanse-node" does not exist`: create the account, or pass
  `--user` with an existing one.
- `hanse-node: install must run as root`: run the command behind `sudo`. On
  Windows, open a terminal as an administrator.
- The service starts and stops at once: read the log. The agent refuses to
  start when `agent.yaml` has a problem, when `gateway_url` is missing, or
  when the credential is missing. The reason is in the log, which is
  `journalctl -u hanse-node` on Linux, `/var/log/hanse-node.log` on macOS,
  and `<state_dir>\hanse-node.log` on Windows.
- `elevation denied ... cannot be checked`: the sudoers rule is missing, has
  the wrong path, or uses `NOPASSWD`. Check it with `sudo visudo -cf`.
- The node stays offline on the Gateway: check that the node's OS and
  architecture match `nodes.yaml`, and that the Gateway URL in `agent.yaml`
  is the Gateway's `public_url`.
