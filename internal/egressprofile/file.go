package egressprofile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"nilcore/internal/policy"
)

// DefaultFilePath is the fixed, committed location of a project-local egress
// allowlist, relative to the repo root. It holds hostnames only and is meant to
// be checked in — NEVER a secret (I3); keyed sources resolve their key value
// from the SecretStore at the wiring layer.
const DefaultFilePath = ".nilcore/egress.json"

// fileSchemaVersion is the on-disk schema for a project-local allowlist.
const fileSchemaVersion = 1

// FileSpec is the JSON shape of a project-local egress allowlist file. Allow is
// a list of hostnames or "*.suffix" wildcards (same grammar as policy.Egress);
// reachability is enforced by the proxy, not validated here.
type FileSpec struct {
	SchemaVersion int      `json:"schema_version"`
	Allow         []string `json:"allow"`
}

// ErrFileNotFound is returned (wrapped) by LoadFile when the allowlist file does
// not exist. It is errors.Is-distinguishable from a parse error so the caller can
// treat "no file" as "no project-local hosts" while still failing closed on a
// malformed file.
var ErrFileNotFound = errors.New("egressprofile: allowlist file not found")

// LoadFile reads and parses a project-local allowlist file into a policy.Egress
// whose Allowed equals the file's allow[] (order preserved, each entry trimmed).
// A missing file returns ErrFileNotFound (errors.Is-distinguishable); malformed
// JSON returns a parse error — never a silent zero-value.
func LoadFile(path string) (policy.Egress, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return policy.Egress{}, fmt.Errorf("%w: %s", ErrFileNotFound, path)
		}
		return policy.Egress{}, fmt.Errorf("reading egress file %s: %w", path, err)
	}
	var spec FileSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return policy.Egress{}, fmt.Errorf("parsing egress file %s: %w", path, err)
	}
	allow := make([]string, 0, len(spec.Allow))
	for _, h := range spec.Allow {
		if h = strings.TrimSpace(h); h != "" {
			allow = append(allow, h)
		}
	}
	return policy.Egress{Allowed: allow}, nil
}

// UnknownProfileError reports an egress profile name outside the closed Names()
// set. The error message lists the valid names so the front-door wiring (P11-T28)
// can surface an actionable diagnostic.
type UnknownProfileError struct{ Name string }

func (e *UnknownProfileError) Error() string {
	return fmt.Sprintf("egressprofile: unknown profile %q (valid: %s)", e.Name, strings.Join(Names(), ", "))
}
