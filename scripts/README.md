# Distribution scripts (P1-T13)

One cross-compiled binary ships to macOS and Linux (amd64/arm64). The
[`Release` workflow](../.github/workflows/release.yml) builds all four targets on
every `v*` tag and attaches them to a GitHub Release.

## Install (Linux / macOS)

```sh
curl -fsSL https://raw.githubusercontent.com/RNT56/NilCore/main/scripts/install.sh | sh
```

Overrides: `NILCORE_VERSION=v0.2.0`, `NILCORE_BINDIR=$HOME/.local/bin`.

## Run unattended (Linux VPS)

See [`nilcore.service`](./nilcore.service) for a hardened systemd unit. Secrets
load from a root-owned `0600` `EnvironmentFile` — never inline, never logged
(invariant I3).

## Homebrew tap (macOS)

A tap will live at `RNT56/homebrew-nilcore` with a formula that downloads the
release binary for the host arch:

```ruby
class Nilcore < Formula
  desc "Tiny, robust coding agent"
  homepage "https://github.com/RNT56/NilCore"
  version "0.1.0"
  on_macos do
    on_arm   { url "https://github.com/RNT56/NilCore/releases/download/v0.1.0/nilcore-darwin-arm64" }
    on_intel { url "https://github.com/RNT56/NilCore/releases/download/v0.1.0/nilcore-darwin-amd64" }
  end
  def install
    bin.install Dir["nilcore-*"].first => "nilcore"
  end
end
```

Install with `brew install RNT56/nilcore/nilcore` once the tap is published.
