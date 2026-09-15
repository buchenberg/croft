package containerd

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"

	shepherd "github.com/buchenberg/shepherd-kernel-go"
)

func testSandbox() *ContainerdSandbox {
	return New(Config{Image: "golang:1.22", Namespace: "shepherd-test"})
}

func TestContainerdSandbox_Backend(t *testing.T) {
	if got := testSandbox().Backend(); got != BackendName {
		t.Errorf("Backend() = %q, want %q", got, BackendName)
	}
}

// TestContainerdSandbox_Capabilities pins the declared contract. These are the
// values callers branch on, so changing them is a deliberate act: a false here
// would make the corresponding operation unavailable even once it is wired.
func TestContainerdSandbox_Capabilities(t *testing.T) {
	caps := testSandbox().Capabilities()

	if !caps.Lifecycle {
		t.Error("Lifecycle must be true: Create and Destroy manage real resources")
	}
	if !caps.Exec {
		t.Error("Exec must be true")
	}
	if !caps.FileIO {
		t.Error("FileIO must be true")
	}
	if !caps.Diff {
		t.Error("Diff must be true")
	}
	if !caps.Isolated {
		t.Error("Isolated must be true: the workspace is not the host filesystem")
	}
	if caps.Containment != shepherd.ContainContained {
		t.Errorf("Containment = %q, want %q (a container is not a VM boundary)",
			caps.Containment, shepherd.ContainContained)
	}
}

// TestContainerdSandbox_NotImplemented asserts every operation reports
// ErrNotImplemented rather than silently succeeding or claiming unsupported.
// A stub that returned nil here would let a caller believe work happened.
func TestContainerdSandbox_NotImplemented(t *testing.T) {
	ctx := context.Background()
	sb := testSandbox()
	state := snapshotState("golang:1.22", "snap-1", "")

	t.Run("Destroy", func(t *testing.T) {
		if err := sb.Destroy(ctx); !errors.Is(err, ErrNotImplemented) {
			t.Errorf("Destroy err = %v, want ErrNotImplemented", err)
		}
	})

	t.Run("Capture", func(t *testing.T) {
		if _, err := sb.Capture(ctx); !errors.Is(err, ErrNotImplemented) {
			t.Errorf("Capture err = %v, want ErrNotImplemented", err)
		}
	})

	t.Run("Apply", func(t *testing.T) {
		if err := sb.Apply(ctx, state); !errors.Is(err, ErrNotImplemented) {
			t.Errorf("Apply err = %v, want ErrNotImplemented", err)
		}
	})

	t.Run("Diff", func(t *testing.T) {
		if _, _, err := sb.Diff(ctx, state, 0); !errors.Is(err, ErrNotImplemented) {
			t.Errorf("Diff err = %v, want ErrNotImplemented", err)
		}
	})

	t.Run("Exec", func(t *testing.T) {
		if _, err := sb.Exec(ctx, shepherd.ExecRequest{Command: "true"}); !errors.Is(err, ErrNotImplemented) {
			t.Errorf("Exec err = %v, want ErrNotImplemented", err)
		}
	})

	t.Run("ReadFile", func(t *testing.T) {
		if _, err := sb.ReadFile(ctx, "/etc/hostname"); !errors.Is(err, ErrNotImplemented) {
			t.Errorf("ReadFile err = %v, want ErrNotImplemented", err)
		}
	})

	t.Run("WriteFile", func(t *testing.T) {
		if err := sb.WriteFile(ctx, "/tmp/x", []byte("x"), fs.FileMode(0o644)); !errors.Is(err, ErrNotImplemented) {
			t.Errorf("WriteFile err = %v, want ErrNotImplemented", err)
		}
	})
}

// TestContainerdSandbox_CreateValidatesImage covers the one piece of real
// behaviour in the skeleton: a sandbox with no base filesystem is rejected
// before any provisioning is attempted.
func TestContainerdSandbox_CreateValidatesImage(t *testing.T) {
	ctx := context.Background()

	err := New(Config{}).Create(ctx, shepherd.SandboxSpec{})
	if err == nil {
		t.Fatal("Create without Config.Image must fail")
	}
	if !strings.Contains(err.Error(), "Image") {
		t.Errorf("error should name the missing field: %v", err)
	}
	if errors.Is(err, ErrNotImplemented) {
		t.Error("a config error must not be reported as not-implemented")
	}

	// A valid config proceeds past validation and stops at the unimplemented
	// boundary.
	if err := testSandbox().Create(ctx, shepherd.SandboxSpec{}); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("Create with a valid config = %v, want ErrNotImplemented", err)
	}
}

// TestSnapshotState_RoundTrip pins the workspace-state encoding, which is the
// interface the kernel and any future tooling depend on.
func TestSnapshotState_RoundTrip(t *testing.T) {
	state := snapshotState("golang:1.22", "snap-7", "snap-3")

	if state.Backend != BackendName {
		t.Errorf("Backend = %q, want %q", state.Backend, BackendName)
	}
	if state.Revision != "snap-7" {
		t.Errorf("Revision = %q, want the snapshot key", state.Revision)
	}
	if state.Data[StateKeyParentKey] != "snap-3" {
		t.Errorf("parent = %v, want snap-3", state.Data[StateKeyParentKey])
	}
	if state.Data[StateKeyImage] != "golang:1.22" {
		t.Errorf("image = %v, want golang:1.22", state.Data[StateKeyImage])
	}

	key, err := stateSnapshotKey(state)
	if err != nil {
		t.Fatalf("stateSnapshotKey: %v", err)
	}
	if key != "snap-7" {
		t.Errorf("key = %q, want snap-7", key)
	}

	// The state must digest stably, so trace records and drift detection work.
	d1, err := state.Digest()
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	d2, err := snapshotState("golang:1.22", "snap-7", "snap-3").Digest()
	if err != nil {
		t.Fatalf("Digest (second): %v", err)
	}
	if d1 != d2 {
		t.Errorf("digest is not stable: %s vs %s", d1, d2)
	}
}

// TestStateSnapshotKey_RejectsBadStates guards the boundary between backends: a
// git state must never be applied to a containerd sandbox.
func TestStateSnapshotKey_RejectsBadStates(t *testing.T) {
	t.Run("foreign backend", func(t *testing.T) {
		_, err := stateSnapshotKey(shepherd.WorkspaceState{
			Backend:  "git",
			Revision: "abc123",
			Data:     map[string]any{"head_sha": "abc123"},
		})
		if err == nil {
			t.Fatal("a git state must be rejected")
		}
		if !strings.Contains(err.Error(), "git") {
			t.Errorf("error should name the offending backend: %v", err)
		}
	})

	t.Run("missing snapshot key", func(t *testing.T) {
		_, err := stateSnapshotKey(shepherd.WorkspaceState{
			Backend: BackendName,
			Data:    map[string]any{},
		})
		if err == nil {
			t.Fatal("a state without a snapshot key must be rejected")
		}
	})

	t.Run("Apply surfaces the rejection", func(t *testing.T) {
		err := testSandbox().Apply(context.Background(), shepherd.WorkspaceState{Backend: "git"})
		if err == nil {
			t.Fatal("Apply must reject a foreign state")
		}
		if errors.Is(err, ErrNotImplemented) {
			t.Error("state validation must run before the unimplemented boundary")
		}
	})
}
