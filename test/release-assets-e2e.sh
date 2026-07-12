#!/usr/bin/env bash
# Cross-build the exact release matrix and drive the same fail-closed asset verifier
# used by .github/workflows/release.yml. A missing checksum is the negative control.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
dist="$tmp/dist"
mkdir -p "$dist"
release_version="${NILCORE_RELEASE_VERSION:-v0.0.0-e2e}"
if [[ ! "$release_version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z]+([.-][0-9A-Za-z]+)*)?$ ]]; then
  echo "invalid test release version: $release_version" >&2
  exit 1
fi
ldflags="-s -w -X main.version=${release_version}"

for os in darwin linux; do
  for arch in amd64 arm64; do
    asset="nilcore-${os}-${arch}"
    (
      cd "$repo_root"
      CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "$ldflags" -o "$dist/$asset" ./cmd/nilcore
    )
    if command -v sha256sum >/dev/null 2>&1; then
      ( cd "$dist" && sha256sum "$asset" > "$asset.sha256" )
    else
      ( cd "$dist" && shasum -a 256 "$asset" > "$asset.sha256" )
    fi
  done
done

# Run the native member of the release matrix and pin the operator-visible version.
# The supported matrix contains the CI runner and both supported developer hosts;
# fail explicitly on an unsupported host instead of silently skipping the proof.
host_os="$(go env GOOS)"
host_arch="$(go env GOARCH)"
host_asset="$dist/nilcore-${host_os}-${host_arch}"
if [[ ! -x "$host_asset" ]]; then
  echo "release version E2E has no runnable matrix asset for ${host_os}/${host_arch}" >&2
  exit 1
fi
actual_version="$("$host_asset" version)"
if [[ "$actual_version" != "nilcore ${release_version}" ]]; then
  echo "release binary version mismatch: got '$actual_version', want 'nilcore ${release_version}'" >&2
  exit 1
fi

"$repo_root/scripts/verify-release-assets.sh" "$dist"
test "$(wc -l < "$dist/SHA256SUMS" | tr -d ' ')" = "4"

rm "$dist/nilcore-linux-arm64.sha256"
if "$repo_root/scripts/verify-release-assets.sh" "$dist" >"$tmp/missing.out" 2>&1; then
  echo "release verifier accepted a missing per-asset checksum" >&2
  exit 1
fi
grep -q "missing checksum nilcore-linux-arm64.sha256" "$tmp/missing.out"

echo "release assets E2E: version stamped; four targets verified; partial release refused"
