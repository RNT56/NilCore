// Command nilcore-desktop-darwin is the native-macOS desktop driver (Phase CU,
// docs/ROADMAP-COMPUTER-USE-DARWIN.md — the MVP tier, CU-MAC-T01..T04). It is the
// darwin sibling of cmd/tools/nilcore-desktop: the SAME desktopwire file-queue
// protocol and the SAME Set-of-Marks ladder (internal/desktop + internal/som), but
// it drives the REAL Mac desktop by shelling to the OS-baked `screencapture` and
// `cliclick` — pure stdlib, no CGO, no module (I6), exactly like the Linux driver
// shells to scrot/xdotool.
//
// MVP scope: Rungs 2/3 only (no AT-SPI/AXUIElement — that is the production tier's
// signed helper, CU-MAC-T05). The model perceives a SoM-marked or raw screenshot
// and acts by coordinate; the driver converts the model's resized-image coordinate
// to a true macOS POINT (resized→pixel→point, the #1 mis-click bug owned in coords.go).
//
// SECURITY: this drives the user's real desktop = host ambient authority (I4 is
// relaxed). It is the gated, non-default host-control tier — only reached via
// `nilcore desktop --mac-host` behind NILCORE_DESKTOP_HOST + forced approval. The
// live path needs macOS Accessibility (cliclick) + Screen Recording (screencapture)
// TCC grants; without them it fails closed. The pure pieces are unit-tested.
package main

import (
	"context"
	"fmt"
	"os"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "nilcore-desktop-darwin:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	// --probe is a standalone onboarding mode (CU-MAC-T07): report the live TCC
	// grants and exit non-zero when not ready, so it doubles as a host-readiness gate.
	for _, a := range args {
		if a == "--probe" {
			r := probePermissions(ctx)
			fmt.Print(r.String())
			if !r.Ready {
				os.Exit(1)
			}
			return nil
		}
	}
	serve, control, native, err := parseArgs(args)
	if err != nil {
		return err
	}
	if !serve {
		return fmt.Errorf("nilcore-desktop-darwin requires --serve --control <dir>")
	}
	return runServe(ctx, control, native)
}

// parseArgs reads the fixed flag set: --serve --control <dir> [--native].
func parseArgs(args []string) (serve bool, control string, native bool, err error) {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--serve":
			serve = true
		case a == "--native":
			native = true
		case a == "--control":
			if i+1 >= len(args) {
				return false, "", false, fmt.Errorf("--control requires a value")
			}
			control = args[i+1]
			i++
		case len(a) > 10 && a[:10] == "--control=":
			control = a[10:]
		default:
			return false, "", false, fmt.Errorf("unexpected argument %q", a)
		}
	}
	if serve && control == "" {
		return false, "", false, fmt.Errorf("--serve requires --control <dir>")
	}
	return serve, control, native, nil
}
