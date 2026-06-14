// A self-contained module (its own go.mod) so the parent `go test ./...` does
// not descend into this deliberately-failing fixture.
module failingfixture

go 1.23
