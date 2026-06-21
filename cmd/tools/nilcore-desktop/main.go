// Command nilcore-desktop is the operator-trusted, in-sandbox desktop driver the
// computer-use tool drives (Phase CU, CU-T04/T05). It is the desktop sibling of
// nilcore-browser: a standalone tool baked into the images/sandbox-desktop image,
// pure stdlib (it SHELLS to the image-baked nilcore-a11y-dump / scrot / xdotool —
// the heavy a11y/X11 machinery lives in the image, never the Go core's go.mod, I6),
// NOT linked into the default nilcore binary.
//
// It runs ONE long-lived `--serve` session over a file-queue on the shared /work
// mount (the same transport as nilcore-browser --serve), keeping the Xvfb desktop
// alive across the host's many turns. Each observe runs the Set-of-Marks ladder
// (internal/desktop + internal/som): AT-SPI refs → CV/SoM-marked screenshot → raw
// screenshot. Each act is translated to an xdotool command. Everything the screen
// returns is UNTRUSTED data (I7) the host fences.
//
// The live X11 path (scrot/xdotool/a11y-dump) is exercised only by a CI desktop-e2e
// job (no X11 in unit tests, no display on a macOS dev host); the pure pieces —
// a11y JSON parsing, the xdotool command builders, the rung assembly — are
// unit-tested hermetically against fakes.
package main

import (
	"context"
	"fmt"
	"os"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "nilcore-desktop:", err)
		os.Exit(1)
	}
}

// run is the testable entrypoint: it parses flags and dispatches. Only --serve is
// supported (the driver has no batch mode — a desktop is inherently stateful).
func run(ctx context.Context, args []string) error {
	serve, control, native, err := parseArgs(args)
	if err != nil {
		return err
	}
	if !serve {
		return fmt.Errorf("nilcore-desktop requires --serve --control <dir>")
	}
	return runServe(ctx, control, native)
}

// parseArgs reads the fixed flag set: --serve --control <dir> [--native]. Hand-parsed
// so the accepted surface is exactly the contract and unknown flags are rejected.
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
