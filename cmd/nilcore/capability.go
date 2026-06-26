package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"nilcore/internal/capability"
)

// capabilityMain prints the capability descriptor a drive would get for a given
// mode (Phase 16, EXP-T07): the legible single-source view of read-only/shell,
// the egress source labels, and the Rule-of-Two verdict. Read-only; it writes no
// event. It deliberately prints egress SOURCE LABELS, never the host allowlist
// itself, and never a secret (I3/I7) — the same projection the audit event uses.
func capabilityMain(args []string) {
	fs := flag.NewFlagSet("capability", flag.ExitOnError)
	mode := fs.String("mode", "auto", "auto | discuss | plan | execute")
	profile := fs.String("profile", "", "named egress profile (empty = deny-all)")
	readRepo := fs.Bool("read-repo", false, "the drive can read repo / private context (capguard axis B)")
	untrusted := fs.Bool("untrusted-input", false, "the drive ingests untrusted content, e.g. browse/desktop (capguard axis A)")
	format := fs.String("format", "text", "text | json")
	_ = fs.Parse(args)

	d, err := capability.For(capability.Request{
		Mode: *mode, ProfileName: *profile, ReadRepo: *readRepo, UntrustedInput: *untrusted, GateAvailable: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "capability: %v\n", err)
		os.Exit(1)
	}
	if *format == "json" {
		b, _ := json.MarshalIndent(d.Event(), "", "  ") // the metadata-only projection
		fmt.Println(string(b))
		return
	}
	dec := d.Evaluate()
	fmt.Printf("mode=%s  read-only=%v  shell=%v\n", d.Mode, d.Tools.ReadOnly, d.ShellEnabled)
	fmt.Printf("egress sources: %v\n", d.EgressSources)
	fmt.Printf("rule-of-two verdict: %s (axes %v)\n", dec.Verdict, dec.Axes)
}
