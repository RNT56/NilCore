.PHONY: build vet test verify run tidy tui tui-verify

build:
	go build ./...

vet:
	go vet ./...

test:
	go test ./...

# The default check command the agent runs to decide "done". Point nilcore at
# its own repo with -verify "make verify" to have it dogfood this gate.
verify: build vet test

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
