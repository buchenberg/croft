package containerd

import (
	"strings"
	"testing"

	sbx "github.com/buchenberg/shepherd-kernel-go/sandbox"
)

// TestRegistry_BackendIsRegistered pins the OpenSandbox registration this
// package's init performs: the backend is resolvable by name, and the factory
// returns this package's sandbox unprovisioned — Create remains the caller's
// explicit lifecycle step.
func TestRegistry_BackendIsRegistered(t *testing.T) {
	found := false
	for _, name := range sbx.SandboxBackends() {
		if name == BackendName {
			found = true
		}
	}
	if !found {
		t.Fatalf("SandboxBackends() = %v, want %q registered by this package's init", sbx.SandboxBackends(), BackendName)
	}

	sb, err := sbx.OpenSandbox(BackendName, Config{Image: "example/dev:latest"})
	if err != nil {
		t.Fatalf("OpenSandbox: %v", err)
	}
	cs, ok := sb.(*ContainerdSandbox)
	if !ok {
		t.Fatalf("OpenSandbox returned %T, want *ContainerdSandbox", sb)
	}
	if cs.ID() != "" {
		t.Errorf("the factory must not provision: ID = %q, want empty before Create", cs.ID())
	}
}

// TestRegistry_RejectsWrongConfiguration pins the loud type check: a nil or
// mismatched configuration is a startup error naming what was received, never a
// silent default backend.
func TestRegistry_RejectsWrongConfiguration(t *testing.T) {
	_, err := sbx.OpenSandbox(BackendName, nil)
	if err == nil || !strings.Contains(err.Error(), "nil is not one") {
		t.Errorf("OpenSandbox(nil) = %v, want an explicit refusal", err)
	}

	_, err = sbx.OpenSandbox(BackendName, "containerd, but as a string")
	if err == nil || !strings.Contains(err.Error(), "string") {
		t.Errorf("OpenSandbox(string) = %v, want the offending type named", err)
	}
}
