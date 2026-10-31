# Contributing to Hanse

Thanks for helping. Hanse is small on purpose: a change that keeps the Gateway, the node agent, and the CLI easy to reason about is easier to accept than a large feature. Bug reports, documentation fixes, and focused pull requests are all welcome.

## Build and test

Hanse needs Go 1.27 or later. Clone the repository and build the three binaries:

```sh
git clone https://github.com/achmadss/hanse.git
cd hanse
make build
export PATH="$PWD/bin:$PATH"

hanse version
hanse-gateway --version
hanse-node --version
```

`make build` writes `bin/hanse`, `bin/hanse-gateway`, and `bin/hanse-node`. `make test` runs the Go tests. `make clean` removes `bin/`.

The [README quickstart](README.md#quickstart) runs a Gateway, a node agent, and the CLI on one loopback machine. Use it to try a change by hand.

## Checks before a pull request

Run these from the repository root. `gofmt -l .` must print nothing.

```sh
gofmt -l .
go vet ./...
go test -race ./...
go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 run ./...
```

Build every release target before you change a build tag, a platform-specific file, or an entry command:

```sh
make cross
```

`make cross` builds Linux, macOS, and Windows on amd64 and arm64.

CI runs on Linux, macOS, and Windows. It runs `go vet`, the linter on Linux, and the Go tests. A separate job runs `make cross`. A pull request is ready when every job passes.

## Commits

Use [Conventional Commits](https://www.conventionalcommits.org/):

```text
<type>(<scope>): <summary>

<body, when it helps>
```

Common types are `feat`, `fix`, `docs`, `test`, `refactor`, `build`, `ci`, and `chore`. The scope is optional, for example `cli`, `api`, `agent`, or `config`.

```text
docs: add the capability tutorial
fix(cli): keep the operation ID in JSON log output
feat(api): add the reload warning list
```

Keep each commit focused. Do not add a `Co-Authored-By` trailer or any other generated-by line.

## Pull requests

Describe what changed and why. Link the issue that the pull request closes with `Closes #N`:

```text
Closes #35

Notes:
- The reload endpoint rescans capabilities but not routes.yaml.
- The tutorial uses gateway.example.com for every example URL.
```

Use one `Closes #N` line per issue. Add `Notes:` only when the change differs from the issue, makes an assumption, or leaves something for a later change.

## Documentation

The documentation is part of the product. Update it in the same pull request as the behavior it describes.

| Document | Contents |
|---|---|
| [Architecture](docs/architecture.md) | Concepts, components, request flow, errors, and security. |
| [Configuration](docs/configuration.md) | Every configuration file and the capability manifest format. |
| [Handler contract](docs/handler-contract.md) | The request and response envelopes, logs, exit codes, timeouts, and cancellation. |
| [Agent protocol](docs/agent-protocol.md) | The WebSocket protocol between the Gateway and `hanse-node`. |
| [Writing a capability](docs/writing-a-capability.md) | A tutorial that builds and runs a small capability. |
| [Guides](docs/guides/systemd.md) | Deployment: systemd, Docker, a TLS reverse proxy, and health checks. |
| [Node setup](docs/node-setup.md) | Installing and enrolling `hanse-node` on each OS. |

## Examples and placeholders

Examples must not contain a real address, host name, token, key, or personal data. Use the documentation ranges:

- IPv4 addresses: `192.0.2.0/24`, such as `192.0.2.10` and `192.0.2.20`.
- MAC addresses: `00:00:5e:00:53:xx`, such as `00:00:5e:00:53:01`.
- Host names: `gateway.example.com`, `node.example.com`, and similar names under `example.com`.

Never paste a real credential into an issue, a pull request, a test, or a commit. If a secret is ever committed, treat it as exposed: rotate it, and tell the maintainers.

## Security issues

Do not report a security issue in a public issue, discussion, or pull request. Use GitHub private vulnerability reporting: open the repository's **Security** tab and choose **Report a vulnerability**. This opens a private security advisory that only the maintainers can read.

Include the affected version and platform, the steps that reproduce the problem, and the impact you expect. Use placeholders for every address and credential, and do not include a real secret. The maintainers will use the advisory to discuss, fix, and publish the issue.

## License

By contributing, you agree that your contribution is licensed under the [MIT License](LICENSE).
