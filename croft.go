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
// image must provide a shell and the small coreutils croft itself uses:
// mkdir (Create provisions the workdir), and cat, dirname, printf,
// base64, and chmod (ReadFile and WriteFile).
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
	"io/fs"
	"time"
)

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
// provision it, capture it, resume it, and execute in it.
//
// The capture/apply pair is the state lifecycle: Capture returns a WorkspaceState
// that outlives the sandbox (the snapshot is pinned against the daemon's
// garbage collector), Apply rebinds a sandbox to a previously captured state,
// and any sandbox configured the same way can apply it — that is how a state
// crosses from one sandbox to another. ReleaseState is the only discard verb.
//
// Every method takes a context so remote daemons honor cancellation and
// deadlines.
//
// Non-mutating contract: Capture must leave the caller's workspace metadata
// unchanged on return.
type Sandbox interface {
	// Create provisions the workspace: resolves the image rootfs, prepares
	// the base active snapshot, and starts the task. The caller bounds the
	// operation with the context.
	Create(ctx context.Context) error

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

	// ReleaseState drops the pin holding a captured state's snapshot. The
	// daemon's garbage collector reclaims the layer once nothing references
	// it; until then the state stays valid. Idempotent.
	ReleaseState(ctx context.Context, ws WorkspaceState) error

	// Exec runs a command in the sandbox and returns its result.
	Exec(ctx context.Context, req ExecRequest) (ExecResult, error)

	// ReadFile reads a file inside the sandbox workspace.
	ReadFile(ctx context.Context, path string) ([]byte, error)

	// WriteFile writes a file inside the sandbox workspace.
	WriteFile(ctx context.Context, path string, data []byte, perm fs.FileMode) error
}
