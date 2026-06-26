.PHONY: build vet test verify test-race run tidy tui tui-verify desktop-e2e desktop-image desktop-mac desktop-mac-smoke desktop-mac-probe

build:
	go build ./...

vet:
	go vet ./...

test:
	go test ./...

# The default check command the agent runs to decide "done". Point nilcore at
# its own repo with -verify "make verify" to have it dogfood this gate.
verify: build vet test

# Race-detector pass over the concurrency-bearing packages (the conversational
# session lifecycle). NOT folded into `verify` (the race build is slower and the
# detector needs CGO), but the canonical gate for changes to session/Cancel/Turn —
# run it after touching the drive lifecycle. See cancel_race_test.go.
test-race:
	go test -race ./internal/session/...

run:
	go run ./cmd/nilcore $(ARGS)

# The full-screen TUI is an OPT-IN build: only `-tags tui` links the Charm stack,
# so the default `make build` binary stays dependency-free (invariant I6 — the
# core never imports Charm; only cmd/nilcore/tui.go does, under this tag).
tui:
	go build -tags tui -o nilcore-tui ./cmd/nilcore

# Build/vet/test the tui-tagged code (the default `verify` excludes it).
tui-verify:
	go build -tags tui ./... && go vet -tags tui ./cmd/nilcore && go test -tags tui ./cmd/nilcore

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
