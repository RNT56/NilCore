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

// fileSchemaVersion is the on-disk schema this build reads and writes for a
// project-local allowlist. LoadFile validates a file's declared schema_version
// against it (a zero/absent version is accepted as the current schema for
// back-compat; any other value is refused via ErrUnsupportedSchema).
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

// ErrUnsupportedSchema is returned (wrapped) by LoadFile when the allowlist file
// declares a schema_version this build cannot read. It fails closed: an
// unrecognized schema is a malformed file, never a silent deny-all — the operator
// must upgrade NilCore or fix the file. errors.Is-distinguishable from
// ErrFileNotFound so a bad version is not mistaken for "no file".
var ErrUnsupportedSchema = errors.New("egressprofile: unsupported allowlist schema_version")

// LoadFile reads and parses a project-local allowlist file into a policy.Egress
// whose Allowed equals the file's allow[] (order preserved, each entry trimmed).
// A missing file returns ErrFileNotFound (errors.Is-distinguishable); malformed
// JSON returns a parse error; an unsupported schema_version returns
// ErrUnsupportedSchema — never a silent zero-value.
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
	// Validate schema_version: only fileSchemaVersion is understood. A zero (absent)
	// version is treated as the current schema for back-compat with pre-versioning
	// files, which carried no schema_version and matched today's shape. Any other
	// value is a fail-closed error (never a silent deny-all) so a file written by a
	// newer build is refused loudly instead of misread.
	if spec.SchemaVersion != 0 && spec.SchemaVersion != fileSchemaVersion {
		return policy.Egress{}, fmt.Errorf("egress file %s: %w %d (this build reads %d)",
			path, ErrUnsupportedSchema, spec.SchemaVersion, fileSchemaVersion)
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
