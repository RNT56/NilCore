package sandbox

import (
	"strings"
	"testing"
)

func TestPick(t *testing.T) {
	tests := []struct {
		name           string
		prefer         Backend
		nsAvail        bool
		containerAvail bool
		want           Backend
		wantErr        bool
	}{
		{"auto prefers namespace when available", Auto, true, true, NamespaceBackend, false},
		{"auto falls back to container", Auto, false, true, ContainerBackend, false},
		{"empty preference is auto", "", true, false, NamespaceBackend, false},
		{"auto with neither still returns container", Auto, false, false, ContainerBackend, false},
		{"explicit namespace ok", NamespaceBackend, true, true, NamespaceBackend, false},
		{"explicit namespace unsupported errors", NamespaceBackend, false, true, "", true},
		{"explicit container ok", ContainerBackend, false, true, ContainerBackend, false},
		{"explicit container without runtime errors", ContainerBackend, true, false, "", true},
		{"unknown backend errors", Backend("nope"), true, true, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := pick(tt.prefer, tt.nsAvail, tt.containerAvail)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("want error, got backend %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestNewSelectsByCapability exercises the New factory with the platform probes
// swapped out, so the selection wiring is verified on any OS. It deliberately
// never drives the probe to "namespace available" on a non-Linux host (that
// would call the Linux-only constructor); the namespace construction path is
// covered by the Linux confinement tests.
func TestNewSelectsByCapability(t *testing.T) {
	origNS, origC := namespaceProbe, containerRuntimeAvailable
	t.Cleanup(func() { namespaceProbe, containerRuntimeAvailable = origNS, origC })

	namespaceProbe = func() (bool, string) { return false, "no kernel support here" }
	containerRuntimeAvailable = func(string) bool { return true }

	box, err := New(Options{Prefer: Auto, Runtime: "podman", Image: "img", HostDir: "/tmp/wt"})
	if err != nil {
		t.Fatalf("auto with container available: %v", err)
	}
	if _, ok := box.(*Container); !ok {
		t.Fatalf("auto without namespace support should pick *Container, got %T", box)
	}

	// An explicit, unsatisfiable namespace preference must error and carry the
	// probe's reason, so the operator learns why it fell through.
	if _, err := New(Options{Prefer: NamespaceBackend, HostDir: "/tmp/wt"}); err == nil {
		t.Fatal("explicit namespace on an unsupported host should error")
	} else if !strings.Contains(err.Error(), "no kernel support here") {
		t.Fatalf("error should carry the probe reason, got: %v", err)
	}
}

func TestAvailableReportsProbes(t *testing.T) {
	origNS, origC := namespaceProbe, containerRuntimeAvailable
	t.Cleanup(func() { namespaceProbe, containerRuntimeAvailable = origNS, origC })

	namespaceProbe = func() (bool, string) { return false, "disabled" }
	containerRuntimeAvailable = func(string) bool { return true }

	ns, reason, container := Available("podman")
	if ns || reason != "disabled" || !container {
		t.Fatalf("Available = (%v, %q, %v), want (false, %q, true)", ns, reason, container, "disabled")
	}
}
