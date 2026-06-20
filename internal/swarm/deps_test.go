package swarm_test

// deps_test.go — the executable LEAF guard.
//
// The swarm leaf must not import the orchestrator (super/agent/project) nor open
// any standing transport (a network/RPC server, an HTTP server, or a remote-DB
// driver). Resume is local-process-restart over the local SQLite store only —
// never cross-host — so a remote transport in the dependency closure would be an
// architectural regression. This test makes that a build-breaking assertion by
// walking the full transitive import set via `go list -deps` and failing on any
// forbidden package. It lives in the external test package (swarm_test) so it
// inspects the package's real dependency closure from the outside.

import (
	"os/exec"
	"strings"
	"testing"
)

func TestSwarmLeafDependencyClosure(t *testing.T) {
	// `go list -deps` prints the full transitive import set, one import path per
	// line, for the named package — the authoritative closure (not just direct
	// imports). Run it against this very package.
	out, err := exec.Command("go", "list", "-deps", "nilcore/internal/swarm").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed: %v\n%s", err, out)
	}
	deps := strings.Split(strings.TrimSpace(string(out)), "\n")

	// Forbidden ORCHESTRATOR packages: a leaf importing any of these would invert
	// the dependency direction (orchestrator depends on leaves, never the reverse).
	forbiddenExact := map[string]string{
		"nilcore/internal/super":   "orchestrator (super)",
		"nilcore/internal/agent":   "orchestrator (agent)",
		"nilcore/internal/project": "orchestrator (project)",
	}

	// Forbidden TRANSPORT / remote-store packages by exact path: a standing server,
	// an RPC stack, or a remote-DB driver would open a cross-host dependency the
	// swarm leaf must never have. Note net/http is allowed for a CLIENT but its
	// server type would be a regression; we scope the ban to the unambiguous
	// server/RPC/remote-DB stdlib + driver paths and let the substring scan below
	// catch obvious third-party network drivers.
	for _, d := range deps {
		if reason, bad := forbiddenExact[d]; bad {
			t.Errorf("swarm leaf imports forbidden package %q (%s)", d, reason)
		}
	}

	// Substring bans catch the network/RPC/remote-DB families regardless of exact
	// path. net/http is intentionally NOT banned outright (a sandboxed pack may use
	// an http CLIENT transitively); we ban the unambiguous SERVER/RPC/remote-DB
	// transports that would imply this leaf opens or speaks to a standing service.
	bannedSubstrings := []struct {
		needle, why string
	}{
		{"net/rpc", "RPC stack"},
		{"net/http/httptest", "HTTP test server"},
		{"google.golang.org/grpc", "gRPC transport"},
		{"go-sql-driver/mysql", "remote MySQL driver"},
		{"lib/pq", "remote Postgres driver"},
		{"jackc/pgx", "remote Postgres driver"},
		{"go.mongodb.org", "remote MongoDB driver"},
		{"redis", "remote Redis client"},
	}
	for _, d := range deps {
		for _, ban := range bannedSubstrings {
			if strings.Contains(d, ban.needle) {
				t.Errorf("swarm leaf imports forbidden transport %q (%s)", d, ban.why)
			}
		}
	}
}
