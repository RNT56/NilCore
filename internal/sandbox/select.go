package sandbox

import (
	"fmt"
	"os/exec"
)

// Backend names a sandbox implementation. The zero value ("") is treated as
// Auto.
type Backend string

const (
	// Auto prefers the namespace backend when the host kernel supports it
	// (Linux + Landlock + unprivileged user namespaces) and otherwise falls back
	// to the container backend. It is the default.
	Auto Backend = "auto"
	// NamespaceBackend confines via Linux user/mount/pid/net namespaces +
	// Landlock, with no container runtime, image, or daemon. Linux-only.
	NamespaceBackend Backend = "namespace"
	// ContainerBackend confines via a rootless container (podman/docker).
	ContainerBackend Backend = "container"
)

// Options selects and configures a sandbox for one worktree. Runtime/Image are
// consulted only when the container backend is chosen.
type Options struct {
	Prefer  Backend // Auto (default) | NamespaceBackend | ContainerBackend
	Runtime string  // container runtime (podman/docker)
	Image   string  // container image
	HostDir string  // absolute path to the worktree
}

// namespaceProbe reports whether this host can run the namespace backend, plus a
// human-readable reason when it cannot. It is defined per-platform
// (namespace_linux.go / namespace_other.go) and is a package var so selection
// is unit-testable on any OS by swapping it.
var namespaceProbe = detectNamespace

// containerRuntimeAvailable reports whether a container runtime is on PATH. A
// package var so selection logic is unit-testable without a real runtime.
var containerRuntimeAvailable = func(runtime string) bool {
	if runtime == "" {
		runtime = "podman"
	}
	_, err := exec.LookPath(runtime)
	return err == nil
}

// pick is the pure backend-selection decision, factored out so every
// (preference × capability) combination is table-testable on any OS. Auto never
// errors: it falls back to the container backend, which surfaces a missing
// runtime at exec time exactly as before this change.
func pick(prefer Backend, nsAvail, containerAvail bool) (Backend, error) {
	switch prefer {
	case NamespaceBackend:
		if !nsAvail {
			return "", fmt.Errorf("namespace sandbox requested but unavailable on this host")
		}
		return NamespaceBackend, nil
	case ContainerBackend:
		if !containerAvail {
			return "", fmt.Errorf("container sandbox requested but no container runtime found")
		}
		return ContainerBackend, nil
	case Auto, "":
		if nsAvail {
			return NamespaceBackend, nil
		}
		return ContainerBackend, nil
	default:
		return "", fmt.Errorf("unknown sandbox backend %q (want auto|namespace|container)", prefer)
	}
}

// New selects and constructs the sandbox for opts. With Auto (the default) it
// prefers the namespace backend when the kernel supports it and otherwise falls
// back to a container — so a host without podman/docker still runs the loop
// sandboxed (invariant I4) wherever Landlock + user namespaces are available.
// An explicit, unsatisfiable preference is an error; Auto is infallible.
func New(opts Options) (Sandbox, error) {
	nsAvail, nsReason := namespaceProbe()
	containerAvail := containerRuntimeAvailable(opts.Runtime)

	chosen, err := pick(opts.Prefer, nsAvail, containerAvail)
	if err != nil {
		if opts.Prefer == NamespaceBackend && !nsAvail && nsReason != "" {
			return nil, fmt.Errorf("%w: %s", err, nsReason)
		}
		return nil, err
	}
	if chosen == NamespaceBackend {
		return newNamespace(opts.HostDir)
	}
	return NewContainer(opts.Runtime, opts.Image, opts.HostDir), nil
}

// Available reports the selectable backends on this host, for `nilcore doctor`
// and config validation: namespace is listed only when the kernel supports it,
// container only when a runtime is on PATH.
func Available(runtime string) (namespace bool, namespaceReason string, container bool) {
	ns, reason := namespaceProbe()
	return ns, reason, containerRuntimeAvailable(runtime)
}
