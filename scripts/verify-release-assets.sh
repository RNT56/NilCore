#!/bin/sh
# Fail-closed release-set verifier shared by the release workflow and its local E2E.
# A publishable NilCore release is exactly four non-empty platform binaries, one valid
# per-binary SHA-256 record each, and a freshly-derived four-line SHA256SUMS manifest.
set -eu

dist="${1:-dist}"
assets="nilcore-darwin-amd64 nilcore-darwin-arm64 nilcore-linux-amd64 nilcore-linux-arm64"

if command -v sha256sum >/dev/null 2>&1; then
  check_record() { sha256sum -c "$1"; }
  write_manifest() { sha256sum $assets > SHA256SUMS; }
elif command -v shasum >/dev/null 2>&1; then
  check_record() { shasum -a 256 -c "$1"; }
  write_manifest() { shasum -a 256 $assets > SHA256SUMS; }
else
  echo "error: no SHA-256 tool (sha256sum/shasum)" >&2
  exit 1
fi

for asset in $assets; do
  test -s "$dist/$asset" || { echo "error: missing release asset $asset" >&2; exit 1; }
  test -s "$dist/$asset.sha256" || { echo "error: missing checksum $asset.sha256" >&2; exit 1; }
  ( cd "$dist" && check_record "$asset.sha256" )
done

( cd "$dist" && write_manifest )
test "$(wc -l < "$dist/SHA256SUMS" | tr -d ' ')" = "4" || {
  echo "error: SHA256SUMS must contain exactly four records" >&2
  exit 1
}

echo "--- SHA256SUMS ---"
cat "$dist/SHA256SUMS"
