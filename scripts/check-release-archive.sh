#!/usr/bin/env bash
#
# Check the release archives that a snapshot build produces.
#
# The hanse-gateway archive must carry the bundled capabilities and the example
# configuration files, and the handler scripts must stay executable inside the
# tar.gz. The hanse and hanse-node archives must not grow those extras.
#
# Usage: scripts/check-release-archive.sh
#
# Env:
#   GOOS, GOARCH       target to inspect (default: the host target)
#   GORELEASER         goreleaser binary to run (default: go run of the pinned version)
#   GORELEASER_VERSION pinned GoReleaser version for the go run fallback (default: v2.19.0)

set -euo pipefail

cd "$(dirname "$0")/.."

goos="${GOOS:-$(go env GOOS)}"
goarch="${GOARCH:-$(go env GOARCH)}"
goreleaser_version="${GORELEASER_VERSION:-v2.19.0}"

if [ -n "${GORELEASER:-}" ]; then
  run_goreleaser() {
    "$GORELEASER" "$@"
  }
else
  run_goreleaser() {
    go run "github.com/goreleaser/goreleaser/v2@${goreleaser_version}" "$@"
  }
fi

fail() {
  echo "check-release-archive: $*" >&2
  exit 1
}

run_goreleaser check
run_goreleaser release --snapshot --clean --skip=publish

archive_for() {
  local name="$1" matches=()
  while IFS= read -r file; do matches+=("$file"); done < <(
    find dist -maxdepth 1 -type f -name "${name}_*_${goos}_${goarch}.tar.gz" | sort
  )
  [ "${#matches[@]}" -eq 1 ] ||
    fail "expected one ${name} archive for ${goos}/${goarch} in dist, found ${#matches[@]}"
  echo "${matches[0]}"
}

gateway_archive="$(archive_for hanse-gateway)"

entries="$(tar tzf "$gateway_archive")"
details="$(tar tvzf "$gateway_archive")"

required=(
  capabilities/echo/manifest.yaml
  capabilities/testkit/manifest.yaml
  config/gateway.example.yaml
)

for path in "${required[@]}"; do
  grep -qxF "$path" <<<"$entries" || fail "missing ${path} in ${gateway_archive}"
done

executables=(
  capabilities/echo/handler.sh
  capabilities/testkit/handler.sh
)

for path in "${executables[@]}"; do
  grep -qxF "$path" <<<"$entries" || fail "missing ${path} in ${gateway_archive}"
  awk -v path="$path" '$NF == path { found = substr($1, 4, 1) == "x" } END { exit !found }' \
    <<<"$details" || fail "${path} is not executable in ${gateway_archive}"
done

for name in hanse hanse-node; do
  archive="$(archive_for "$name")"
  if tar tzf "$archive" | grep -qE '^(capabilities|config)/'; then
    fail "${archive} must not carry capabilities/ or config/"
  fi
done

echo "check-release-archive: ok"
for path in "${required[@]}" "${executables[@]}"; do
  echo "  ${path}"
done
