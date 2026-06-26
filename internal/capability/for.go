package capability

import (
	"nilcore/internal/capguard"
	"nilcore/internal/egressprofile"
	"nilcore/internal/policy"
)

// For computes the full capability descriptor for a drive from harness inputs.
// It is total and pure apart from the one file read egressprofile.Resolve already
// owns. Its output reproduces today's scattered decisions exactly:
//
//   - read-only vs full tools + shell, matching cmd/nilcore's capabilityForMode
//     (read-only ⇒ write-free registry, ReadOnlyCommandPolicy, shell off);
//   - the egress allowlist + its source labels, via egressprofile.Resolve;
//   - the capguard.Capabilities axes (A untrusted-input, B private-data, and C
//     derived by capguard from the resolved egress hosts).
//
// A resolution error (an unknown profile name / unparseable egress file) is
// returned to the caller, which fails closed — never a silent deny-all that looks
// like success.
func For(req Request) (Descriptor, error) {
	readOnly := isReadOnlyMode(req.Mode)

	cp := policy.DefaultCommandPolicy()
	if readOnly {
		cp = policy.ReadOnlyCommandPolicy()
	}

	tree, sources, err := egressprofile.Resolve(req.ProfileName, req.EgressFile)
	if err != nil {
		return Descriptor{}, err
	}

	return Descriptor{
		Mode:          req.Mode,
		Tools:         ToolSet{ReadOnly: readOnly},
		ShellEnabled:  !readOnly,
		CommandPolicy: cp,
		Egress:        tree,
		EgressSources: sources,
		Rule: capguard.Capabilities{
			UntrustedInput: req.UntrustedInput,
			PrivateData:    req.ReadRepo,
			EgressHosts:    tree.Allowed, // capguard derives axis C (open egress) from these
		},
		Approver: req.GateAvailable,
	}, nil
}

// isReadOnlyMode mirrors session.Mode.ReadOnly(): the discuss and plan modes are
// read-only, execute and auto are full-capability. It is a string switch so this
// package needn't import internal/session (keeping it a leaf); the cmd-level
// golden test (EXP-T06) is the byte-identical proof against the live
// capabilityForMode, and a divergence would surface there.
func isReadOnlyMode(mode string) bool {
	return mode == "discuss" || mode == "plan"
}
