// Package croft implements an isolated, snapshot-backed workspace on top of
// containerd's overlayfs snapshotter: a small enclosed plot of filesystem a
// program works in, whose states can be captured, resumed, layered, and
// released, with commands and file I/O executed in-band inside the boundary.
//
// A croft is provisioned from an image (Create), worked through Exec, ReadFile,
// and WriteFile, and checkpointed with Capture, which commits the writable
// overlayfs layer into an immutable state the caller can later Apply — from the
// same sandbox or a fresh one — or hand to another sandbox entirely, which is
// the fork primitive: states outlive the sandboxes that capture them.
//
// # Design constraints
//
// Each of these contradicts a naive implementation, so read them before
// changing the lifecycle:
//
//   - Use the snapshotter, not unix.Mount. An active snapshot cannot serve as
//     another snapshot's parent; only a committed one can. Prepare -> Commit ->
//     Remove is the supported primitive, and it already handles lowerdir chain
//     depth and mount-option length limits that hand-rolled mounting would force
//     you to reimplement.
//
//   - A committed layer must stop being written to. containerd's Commit is a
//     metadata operation (storage.CommitActive) that renames the key; the
//     snapshot directory is keyed by an immutable id, so a running task's mount
//     survives it. Convenient, but dangerous: the freshly committed layer
//     immediately becomes a lowerdir of the next active layer, and OverlayFS
//     lowerdirs must not be written. Capture therefore stops the task before
//     committing and restarts it on the new mounts. See Capture.
//
//   - Sharing a state is a rebase, not a copy. Making a committed snapshot the
//     parent of another sandbox's active layer is O(1). Copying upper
//     directories is wrong: deletions are whiteout character devices and
//     directory opacity is a trusted.overlay.opaque xattr, so a naive copy
//     resurrects deleted files.
//
//   - Linux only. OverlayFS and the containerd daemon require Linux with root
//     or user namespaces.
//
//   - Containment is "contained", not "full". A container is namespaces plus a
//     separate rootfs: strong, but a kernel exploit escapes it. Claiming
//     ContainFull would overstate the guarantee.
//
// # The image contract
//
// The workspace's file operations shell out inside the container, so the
// image must provide a shell and the small coreutils they use (see the
// README for the exact list).
//
// # Status
//
// The snapshot lifecycle, teardown ordering, layer pruning, file I/O, and diff
// are implemented and unit-tested against containerd's real
// snapshots.Snapshotter interface, using an in-memory fake that enforces the
// same invariants as the overlayfs snapshotter: a key cannot be prepared twice,
// a parent must be committed, only a committed layer can be committed once, a
// committed layer is read-only, and a layer with children cannot be removed.
//
// The daemon adapter (client.go) compiles but has not been exercised on every
// supported daemon configuration; the live test suite runs it against a real
// containerd when one is reachable. Treat container and OCI spec construction
// and the exec round-trip as verified only where the live suite has run.
//
// # Lifecycle
//
//	Create   resolve the image rootfs, Prepare the base active snapshot, start
//	         the task on its mounts
//	Capture  stop the task, Commit the active snapshot, pin it as a state under
//	         the namespace's states lease, Prepare a fresh active snapshot on
//	         top of it, restart the task
//	Apply    Prepare a new active snapshot from a committed key, rebind, restart
//	Destroy  stop the task, Remove the active layer, release the sandbox lease.
//	         Committed layers survive: they are states, and states outlive the
//	         sandbox that captured them
//	ReleaseState  drop a state's pin; the daemon's GC reclaims the layer once
//	         nothing references it
//
// Apply rebinds this sandbox's identity to the applied state's layer rather
// than replaying into an existing tree; callers must not assume a state can
// only be applied once, but must not assume two sandboxes applying the same
// state share identity either.
//
// Disk comes back through ReleaseState, Destroy's active-layer removal, or
// namespace teardown — in that order of specificity. Destroy deliberately does
// not remove committed layers: every one was returned to a caller as a state,
// and a sibling sandbox may be running on a child of the same layer.
package croft

import (
	"context"
	"errors"
	"io/fs"
	"time"
)

// ErrUnsupported is returned by an optional capability that a sandbox does not
// implement. Callers should check Capabilities (or type-assert the optional
// interface) before calling; the error exists so a misconfigured caller fails
// loudly instead of silently doing nothing.
var ErrUnsupported = errors.New("croft: capability not supported")

// Containment classifies how strongly a workspace is contained. The string
// values are a stable wire format, recorded by hosts in traces and state
// metadata.
type Containment string

const (
	// ContainFull is a boundary a kernel exploit cannot cross (a VM).
	ContainFull Containment = "full"
	// ContainContained is namespaces plus a separate rootfs: strong, but a
	// kernel exploit escapes it.
	ContainContained Containment = "contained"
	// ContainBuffered is a mediated, non-executable copy of host content.
	ContainBuffered Containment = "buffered"
	// ContainUncontained is direct mutation of the host filesystem.
	ContainUncontained Containment = "uncontained"
)

// SandboxCapabilities declares what a sandbox supports. Callers use it to
// decide whether an optional operation is available before invoking it.
type SandboxCapabilities struct {
	// Lifecycle reports whether Create and Destroy manage real resources.
	Lifecycle bool
	// Exec reports whether Exec is implemented.
	Exec bool
	// FileIO reports whether ReadFile and WriteFile are implemented.
	FileIO bool
	// Diff reports whether Diff is implemented.
	Diff bool
	// Isolated reports whether the workspace is isolated from the host
	// filesystem. A false value means the sandbox mutates the host tree in
	// place.
	Isolated bool
	// Containment is the witness-level containment classification, for
	// hosts that record it in traces and retained contexts.
	Containment Containment
}

// SandboxSpec carries per-call runtime options for Create. Backend-specific
// configuration (image, namespace, credentials) belongs on Config, not here.
type SandboxSpec struct {
	// Workdir is the workspace path for backends that mount or check out into
	// a caller-chosen directory. Ignored by backends that infer their own.
	Workdir string
	// Env is the environment for commands run during Create (for example a
	// bootstrap step). It is not the agent's execution environment.
	Env map[string]string
	// Timeout bounds the Create operation. Zero means the sandbox default.
	Timeout time.Duration
}

// ExecRequest describes one command execution inside a sandbox.
type ExecRequest struct {
	Command string
	Args    []string
	Cwd     string
	Env     map[string]string
	Timeout time.Duration
	Stdin   []byte
}

// ExecResult is the outcome of one command execution.
type ExecResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
}

// Sandbox is an isolated workspace: it owns a filesystem boundary and can
// provision it, capture it, resume it, diff it, and execute in it.
//
// The capture/apply pair is the state lifecycle: Capture returns a WorkspaceState
// that outlives the sandbox (the snapshot is pinned against the daemon's
// garbage collector), Apply rebinds a sandbox to a previously captured state,
// and any sandbox configured the same way can apply it — that is how a state
// crosses from one sandbox to another.
//
// Every method takes a context so remote daemons honor cancellation and
// deadlines.
//
// Non-mutating contract: Capture and Diff must leave the caller's workspace
// metadata unchanged on return. For the in-container git repository that means
// the index must be left as it was found.
type Sandbox interface {
	// Backend returns the backend identifier ("containerd" for this
	// package's implementation).
	Backend() string

	// Capabilities reports what this sandbox supports.
	Capabilities() SandboxCapabilities

	// Create provisions the workspace: resolves the image rootfs, prepares
	// the base active snapshot, and starts the task.
	Create(ctx context.Context, spec SandboxSpec) error

	// Destroy releases resources this sandbox provisioned. It must never
	// destroy resources it did not create.
	Destroy(ctx context.Context) error

	// Capture records the current workspace state: commits the active
	// snapshot, pins it, and continues on a fresh layer. Non-mutating to
	// the caller's workspace metadata.
	//
	// Contract caveat: the state's identity is the snapshot key, which is
	// sequence-derived, so two captures of identical content produce
	// different states. Digest equality means identity, not content
	// equality; a content-derived state identity is a recorded follow-up.
	Capture(ctx context.Context) (WorkspaceState, error)

	// Apply resumes the workspace from a previously captured state by
	// preparing a new active snapshot from the committed key and rebinding
	// this sandbox. Layers created after the applied state survive as
	// states; reclaim them with ReleaseState.
	Apply(ctx context.Context, ws WorkspaceState) error

	// Exec runs a command in the sandbox and returns its result.
	Exec(ctx context.Context, req ExecRequest) (ExecResult, error)

	// ReadFile reads a file inside the sandbox workspace.
	ReadFile(ctx context.Context, path string) ([]byte, error)

	// WriteFile writes a file inside the sandbox workspace.
	WriteFile(ctx context.Context, path string, data []byte, perm fs.FileMode) error
}
