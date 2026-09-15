// Package containerd implements shepherd's Sandbox interface on top of
// containerd's overlayfs snapshotter, giving each scope a real Linux filesystem
// boundary with cheap forking.
//
// # Status
//
// Skeleton. The type, the capability contract, and the workspace-state encoding
// are pinned; the containerd calls are not wired yet. Every operation reports
// ErrNotImplemented so a caller cannot mistake this for a working backend.
//
// Pinning the contract now is deliberate: this file fails to compile if
// shepherd.Sandbox changes, so the adapter cannot silently drift from the
// interface it claims to satisfy. Capabilities describes what the finished
// backend will support, not what this skeleton does — which is why the
// operations return ErrNotImplemented rather than shepherd.ErrUnsupported.
//
// # Design constraints
//
// Each of these contradicts a naive implementation, so read them before wiring
// containerd:
//
//   - The trace is not replayable. Workspace state resumes from a committed
//     snapshot, never by re-running recorded shell commands. The kernel records
//     declarations and observations, not invertible mutations; shell commands
//     are audit-only. Reconstructing a filesystem by replaying them cannot work,
//     because outcomes depend on network, wall-clock, randomness, and external
//     state.
//
//   - Use the snapshotter, not unix.Mount. An active upperdir cannot serve as
//     another mount's lowerdir; only a committed (read-only) layer can. The
//     overlayfs snapshotter's Prepare -> Commit -> Remove sequence is the
//     supported primitive, and it already handles lowerdir chain depth and
//     mount-option length limits that hand-rolled mounting would force you to
//     reimplement.
//
//   - Merge is a rebase, not a copy. Committing the child's snapshot and making
//     it the parent's new base is O(1). Copying upper directories is wrong:
//     deletions are represented as whiteout character devices and directory
//     opacity is a trusted.overlay.opaque xattr, so a naive copy resurrects
//     deleted files.
//
//   - Linux only. OverlayFS and the containerd daemon require Linux with root
//     or user namespaces. This backend can never be the default on Windows, so
//     the kernel's git backend remains the cross-platform default.
//
//   - Containment is "contained", not "full". A container is namespaces plus a
//     separate rootfs: strong, but a kernel exploit escapes it. Claiming
//     ContainFull would overstate the guarantee.
//
// # Intended lifecycle
//
//	Create   provision the base snapshot (from Image) and start the task
//	Capture  commit the active snapshot; return it as a WorkspaceState
//	Apply    prepare a new active snapshot from a committed key and rebind
//	Fork     commit the parent's active snapshot, then prepare the child with
//	         that committed snapshot as its parent
//	Destroy  remove the snapshot and delete the task/container; prune on startup
//	         so a crashed run cannot leak snapshots
//
// Apply is single-use per state: unlike the git backend, preparing from a
// committed snapshot rebinds this sandbox's identity rather than replaying into
// an existing tree. Callers must not assume the git backend's reusability.
package containerd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"sync"

	shepherd "github.com/buchenberg/shepherd-kernel-go"
)

// BackendName is the identifier reported by Backend and stored in
// WorkspaceState.Backend.
const BackendName = "containerd"

// WorkspaceState.Data keys. Pinning the encoding here means the git backend's
// states can never be mistaken for these, and vice versa: Apply rejects a state
// whose Backend is not BackendName.
const (
	// StateKeySnapshotKey is the committed snapshot this state resumes from.
	StateKeySnapshotKey = "snapshot_key"
	// StateKeyParentKey is the committed snapshot this one was prepared from,
	// recording the fork lineage.
	StateKeyParentKey = "parent_key"
	// StateKeyImage is the image the sandbox was provisioned from.
	StateKeyImage = "image"
)

// ErrNotImplemented is returned by every operation in this skeleton.
//
// It is deliberately distinct from shepherd.ErrUnsupported: Capabilities
// declares these operations supported, so reporting "unsupported" would be a
// lie that a caller could branch on.
var ErrNotImplemented = errors.New("containerd sandbox: not implemented")

// Config configures the containerd backend.
type Config struct {
	// Address is the containerd socket or endpoint. Empty uses containerd's
	// own default resolution.
	Address string

	// Namespace isolates this backend's containers and snapshots from other
	// containerd users on the same host.
	Namespace string

	// Image is the base image provisioned for the root scope. Required: a
	// sandbox with no base filesystem has nothing to run.
	Image string

	// Runtime is the OCI runtime name, for example "io.containerd.runc.v2".
	Runtime string
}

// ContainerdSandbox is a scope's execution substrate backed by Containerd.
//
// It is not safe for concurrent use across scopes: one instance corresponds to
// one sandbox, mirroring the kernel's one-Sandbox-per-scope model.
type ContainerdSandbox struct {
	cfg Config

	mu sync.Mutex
	// id is the active sandbox/container identity. Apply rebinds it, so it is
	// not stable for the instance's lifetime.
	id string
	// snapshotKey is the active (uncommitted) snapshot.
	snapshotKey string
}

// The compile-time assertion is the point of this file: if shepherd.Sandbox
// gains or changes a method, this package stops building until the adapter is
// updated to match.
var _ shepherd.Sandbox = (*ContainerdSandbox)(nil)

// New returns a sandbox bound to cfg. It performs no I/O; provisioning happens
// in Create.
func New(cfg Config) *ContainerdSandbox {
	return &ContainerdSandbox{cfg: cfg}
}

// Backend reports the backend identifier.
func (s *ContainerdSandbox) Backend() string { return BackendName }

// Capabilities reports the finished backend's support matrix.
//
// These are the target values, not current behaviour: every operation still
// reports ErrNotImplemented. Callers should gate on capabilities before
// invoking an optional operation, which is what makes those declarations
// load-bearing rather than decorative.
func (s *ContainerdSandbox) Capabilities() shepherd.SandboxCapabilities {
	return shepherd.SandboxCapabilities{
		Lifecycle: true,
		Exec:      true,
		FileIO:    true,
		Diff:      true,
		Isolated:  true,
		// Namespaces plus a separate rootfs, but not a VM boundary: a kernel
		// exploit escapes it, so this is not ContainFull.
		Containment: shepherd.ContainContained,
	}
}

// Create provisions the base snapshot from Config.Image and starts the task.
//
// Validation runs before the unimplemented return so the config contract is
// exercised by tests today; the provisioning itself is not wired.
func (s *ContainerdSandbox) Create(ctx context.Context, spec shepherd.SandboxSpec) error {
	if s.cfg.Image == "" {
		return fmt.Errorf("containerd sandbox: Config.Image is required")
	}
	// TODO: resolve/ensure the image, Prepare the base snapshot from it, create
	// the container and task, then record the identifiers under s.mu. Honor
	// spec.Workdir, spec.Env, and spec.Timeout; fall back to a backend default
	// when spec.Timeout is zero.
	return ErrNotImplemented
}

// Destroy removes this sandbox's snapshot and deletes its task and container.
//
// TODO: Remove the snapshot, stop and delete the task/container, then prune so
// a previously crashed run cannot leave orphans. Must never remove resources
// this sandbox did not create.
func (s *ContainerdSandbox) Destroy(ctx context.Context) error {
	return ErrNotImplemented
}

// Capture commits the active snapshot and returns it as a backend-neutral
// workspace state.
//
// Committing (rather than exposing the active upperdir) is what makes the state
// usable as a parent for a later fork. Capture is non-mutating from the
// caller's perspective: it records a commit, it does not alter the workspace.
func (s *ContainerdSandbox) Capture(ctx context.Context) (shepherd.WorkspaceState, error) {
	// TODO: Commit the active snapshot, Prepare a fresh active snapshot on top
	// of it so the sandbox keeps running, and return snapshotState(...).
	return shepherd.WorkspaceState{}, ErrNotImplemented
}

// Apply resumes the workspace from a previously captured state.
//
// Unlike the git backend, this is single-use per state: it prepares a new
// active snapshot from the committed key and rebinds this sandbox's identity,
// rather than replaying into an existing tree.
func (s *ContainerdSandbox) Apply(ctx context.Context, ws shepherd.WorkspaceState) error {
	if _, err := stateSnapshotKey(ws); err != nil {
		return err
	}
	// TODO: Prepare a new active snapshot from the committed key, rebind s.id
	// and s.snapshotKey under s.mu, and drop the previous active snapshot.
	return ErrNotImplemented
}

// Diff returns the unified diff and changed file paths between ws and the
// current workspace.
//
// TODO: in-container `git diff` for repositories, falling back to a
// snapshot-tree comparison, which is backend-native but needs a differ. Note
// that only the git path works for arbitrary repos.
func (s *ContainerdSandbox) Diff(ctx context.Context, ws shepherd.WorkspaceState, maxLines int) (string, []string, error) {
	if _, err := stateSnapshotKey(ws); err != nil {
		return "", nil, err
	}
	return "", nil, ErrNotImplemented
}

// Exec runs a command inside the sandbox.
//
// TODO: containerd task exec. Map ExecRequest onto the task's process spec,
// honoring Cwd, Env, Timeout, and Stdin, and return the exit code with captured
// stdout and stderr.
func (s *ContainerdSandbox) Exec(ctx context.Context, req shepherd.ExecRequest) (shepherd.ExecResult, error) {
	return shepherd.ExecResult{}, ErrNotImplemented
}

// ReadFile reads a file inside the sandbox workspace.
//
// TODO: read through the task (exec) or the snapshot mount, not the host
// filesystem, so reads cannot reach outside the sandbox.
func (s *ContainerdSandbox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	return nil, ErrNotImplemented
}

// WriteFile writes a file inside the sandbox workspace.
//
// TODO: write through the task (exec) or the snapshot mount, creating parent
// directories as needed and applying perm to new files.
func (s *ContainerdSandbox) WriteFile(ctx context.Context, path string, data []byte, perm fs.FileMode) error {
	return ErrNotImplemented
}

// snapshotState encodes a committed snapshot as a backend-neutral workspace
// state. Capture returns this shape and Apply consumes it.
func snapshotState(image, snapshotKey, parentKey string) shepherd.WorkspaceState {
	return shepherd.WorkspaceState{
		Backend:  BackendName,
		Revision: snapshotKey,
		Data: map[string]any{
			StateKeySnapshotKey: snapshotKey,
			StateKeyParentKey:   parentKey,
			StateKeyImage:       image,
		},
	}
}

// stateSnapshotKey extracts the committed snapshot key from a state, rejecting
// states produced by another backend so a git state can never be applied here.
func stateSnapshotKey(ws shepherd.WorkspaceState) (string, error) {
	if ws.Backend != BackendName {
		return "", fmt.Errorf("containerd sandbox: cannot use a %q workspace state", ws.Backend)
	}
	key, _ := ws.Data[StateKeySnapshotKey].(string)
	if key == "" {
		return "", fmt.Errorf("containerd sandbox: workspace state is missing %q", StateKeySnapshotKey)
	}
	return key, nil
}
