package main

import (
	"flag"
	"fmt"
	"os"

	"nilcore/internal/registry"
)

// registryMain implements `nilcore registry <list|install>` — the local, versioned
// skill registry UX (P10-T06). It manages skills in the same discovery directory
// the loop reads (skillsDir()). Remote fetch / a marketplace is deliberately out of
// scope (EXT-07, gated behind the external-infra thesis gate).
func registryMain(args []string) {
	if len(args) == 0 {
		registryUsage()
		os.Exit(2)
	}
	switch args[0] {
	case "list":
		registryList()
	case "install":
		registryInstall(args[1:])
	default:
		registryUsage()
		os.Exit(2)
	}
}

func registryUsage() {
	fmt.Fprintln(os.Stderr, "usage:\n  nilcore registry list                 list installed skills + versions\n  nilcore registry install <manifest.json>   install the manifest's skill entries (local sources)")
}

func registryList() {
	dir := skillsDir()
	if dir == "" {
		fmt.Fprintln(os.Stderr, "registry: no skills directory (set NILCORE_SKILLS_DIR)")
		return
	}
	installed, err := registry.Installed(dir)
	if err != nil {
		fatal(err)
	}
	if len(installed) == 0 {
		fmt.Printf("no skills installed in %s\n", dir)
		return
	}
	fmt.Printf("installed skills (%s):\n", dir)
	for _, s := range installed {
		ver := s.Version
		if ver == "" {
			ver = "(unversioned)"
		}
		fmt.Printf("  %-24s %-12s %s\n", s.Name, ver, s.Description)
	}
}

func registryInstall(args []string) {
	fs := flag.NewFlagSet("registry install", flag.ExitOnError)
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fatal(fmt.Errorf("registry install: need exactly one <manifest.json> path"))
	}
	manifest, err := registry.LoadManifest(fs.Arg(0))
	if err != nil {
		fatal(err)
	}
	entries := manifest.Skills()
	if len(entries) == 0 {
		fmt.Println("manifest has no skill entries (mcp-server install is a follow-up; remote fetch is EXT-07)")
		return
	}
	dir := skillsDir()
	if dir == "" {
		fatal(fmt.Errorf("registry: no skills directory (set NILCORE_SKILLS_DIR or a user config dir)"))
	}
	for _, e := range entries {
		if err := registry.InstallSkill(e, dir); err != nil {
			fatal(err)
		}
		ver := e.Version
		if ver == "" {
			ver = "(unversioned)"
		}
		fmt.Printf("installed %s@%s -> %s\n", e.Name, ver, dir)
	}
}
