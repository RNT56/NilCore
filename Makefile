.PHONY: build vet test verify run tidy

build:
	go build ./...

vet:
	go vet ./...

test:
	go test ./...

# The default check command the agent runs to decide "done". Point nullclaw at
# its own repo with -verify "make verify" to have it dogfood this gate.
verify: build vet test

run:
	go run ./cmd/nullclaw $(ARGS)

tidy:
	go mod tidy
