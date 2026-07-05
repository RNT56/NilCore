.PHONY: build vet test verify lint test-race run tidy tui tui-verify desktop-e2e desktop-image desktop-mac desktop-mac-smoke desktop-mac-probe

build:
	go build ./...

vet:
	go vet ./...

test:
	go test ./...

# golangci-lint (config: .golangci.yml, pinned v2.12.2 in CI). CLAUDE.md §4 mandates a
# clean lint, so it is folded into `verify` below — this keeps the agent's dogfooded
# local gate identical to CI (previously lint ran ONLY in a separate CI job, so a green
# `make verify` could ship code that failed errcheck/staticcheck/gofmt, discovered only
# after opening a PR). When the linter is absent the step warns LOUD and continues (the
# dedicated CI `lint` job still enforces it) rather than blocking a build for lack of a tool.
lint:
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo ">>> WARNING: golangci-lint not installed — skipping local lint (CI still enforces it)."; \
		echo ">>> Install the pinned version for local/CI parity (see .github/workflows/ci.yml)."; \
	fi

# The default check command the agent runs to decide "done". Point nilcore at
# its own repo with -verify "make verify" to have it dogfood this gate. Includes lint so
# a green local verify matches the CI gate and CLAUDE.md §4.
verify: build vet lint test

# Race-detector pass over the concurrency-bearing packages. NOT folded into `verify`
# (the race build is slower and the detector needs CGO), but the canonical gate for
# changes to any concurrent path — the conversational session lifecycle, the multi-
# agent bus/supervisor/swarm fan-out, the bounded scheduler/pool, the chat transports,
# and the closed-loop budget fence. A `*Race*`/`*Concurrent*` test only proves anything
# under this lane, so CI runs it (see .github/workflows/ci.yml). The provider/model,
# meter, mcp, trace, trust, and graapprove packages each ship a `*Race*`/`*Concurrent*`
# test over shared mutable state (the resilient client, the shared cost ledger, the MCP
# manager, the trust/gate rebuild) — they are included so those names are not tautological.
test-race:
	go test -race \
		./internal/session/... \
		./internal/swarm/... \
		./internal/agent/... \
		./internal/super/... \
		./internal/scheduler/... \
		./internal/pool/... \
		./internal/channel/... \
		./internal/blastbudget/... \
		./internal/model/... \
		./internal/meter/... \
		./internal/mcp/... \
		./internal/trace/... \
		./internal/trust/... \
		./internal/graapprove/...

run:
	go run ./cmd/nilcore $(ARGS)

# The full-screen TUI is an OPT-IN build: only `-tags tui` links the Charm stack,
# so the default `make build` binary stays dependency-free (invariant I6 — the
# core never imports Charm; only cmd/nilcore/tui.go does, under this tag).
tui:
	go build -tags tui -o nilcore-tui ./cmd/nilcore

# Build/vet/test the tui-tagged code (the default `verify` excludes it). Vet+test cover
# EVERY package that carries `//go:build tui` — not just cmd/nilcore: the swarm board and
# the trace renderer also have tui-tagged code, which was previously compiled by the
# `./...` build but never vetted or tested under the tag.
tui-verify:
	go build -tags tui ./... \
		&& go vet -tags tui ./cmd/nilcore ./internal/swarm/board ./internal/trace \
		&& go test -tags tui ./cmd/nilcore ./internal/swarm/board ./internal/trace

tidy:
	go mod tidy

# Live desktop computer-use smoke (Phase CU) — builds the nilcore/sandbox-desktop
# image and exercises the REAL Xvfb/scrot/xdotool/AT-SPI stack inside the container.
# MODEL-FREE (no API key) and host-display-free (Xvfb is headless), so it runs the
# SAME on macOS or Linux via Docker/Podman — NOT CI-only. `make verify` excludes it
# (it needs a container runtime + an image build); run it on demand.
desktop-e2e:
	bash test/desktop-e2e.sh

# Just build the optional desktop image.
desktop-image:
	$${RUNTIME:-podman} build -f images/sandbox-desktop/Dockerfile -t nilcore/sandbox-desktop:latest .

# Build the native-macOS desktop driver (the CU-MAC host-control MVP). Put it on PATH
# (or set NILCORE_DESKTOP_DRIVER) for `nilcore desktop --mac-host`.
desktop-mac:
	go build -o nilcore-desktop-darwin ./cmd/tools/nilcore-desktop-darwin

# Host smoke for the macOS driver: builds it and drives one LIVE observe over the
# file-queue (non-destructive). Needs macOS Screen Recording permission for the live
# capture; warns (does not fail) without it. Host-only — skips on non-Darwin.
desktop-mac-smoke:
	bash test/desktop-mac-smoke.sh

# Probe macOS host-control readiness (Screen Recording + cliclick/Accessibility).
# Builds the driver, then runs its live --probe; exits non-zero when not ready.
desktop-mac-probe: desktop-mac
	./nilcore-desktop-darwin --probe
