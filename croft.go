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
// Two states can be compared without applying either: Changes reports the
// sorted difference between a captured state and the current workspace as a
// []Change, and Diff renders that difference as a unified text diff. Both are
// read-only against the workspace and never stop or restart the task. A
// state's changes can also be merged into a diverged workspace: ApplyDelta
// runs a tree-level three-way merge against the merge base the state carries
// (BaseKey), refusing atomically on conflict — no git, no repository, no
// patch payload.
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
// base64, and chmod (ReadFile and WriteFile). The diff machinery adds
// ls and readlink: reader tasks use them to read pre-images, enumerate
// directories, and resolve symlink targets from a captured state. The
// delta machinery adds rm and ln: ApplyDelta clears replaced and deleted
// paths and recreates symlinks.
//
// # Status
//
// The snapshot lifecycle, teardown ordering, layer pruning, file I/O, the
// snapshot-native diff machinery (Changes, Diff, ReadFilesAt), and the
// snapshot-native delta (ApplyDelta) are implemented and unit-tested against
// containerd's real snapshots.Snapshotter interface, using an in-memory fake
// that enforces the same invariants as the overlayfs snapshotter: a key
// cannot be prepared twice, a parent must be committed, only an active layer
// can be committed (exactly once — an already committed layer is rejected), a
// committed layer is read-only, and a layer with children cannot be removed.
//
// The daemon adapter (client.go) compiles but has not been exercised on every
// supported daemon configuration; the live test suite runs it against a real
// containerd when one is reachable. The live suite covers the full lifecycle,
// the diff path end to end (Changes, Diff, ReadFilesAt against a real
// daemon-side diff service), and the delta path (two sandboxes merging over a
// shared base, conflict refusal, symlink and directory application). Treat
// container and OCI spec construction, the exec round-trip, and the
// daemon-side diff service as verified only where the live suite has run.
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
//
// # Diffing states
//
// Changes and Diff compare a captured state against the current workspace
// without applying either side. The daemon's diff service computes the
// change-set blob daemon-side; croft pins it under a short-lived per-call
// lease, parses the tar into a sorted []Change, and — for Diff — reads
// pre-images through a throwaway reader task and renders a unified text
// diff with a Myers diff engine. Both operations take the shared operation
// lock: they never interleave with Capture/Apply/Destroy, so they always see
// a quiescent workspace. A state whose layer has been released surfaces
// ErrStateUnavailable.
//
// # Merging states
//
// ApplyDelta merges a state's changes into the current workspace: a
// tree-level three-way merge of two daemon-side diffs — base→theirs (the
// incoming state) and base→ours (the active layer) — against the merge base
// the state carries (BaseKey, the layer its capturing sandbox last Applied).
// Theirs-only changes apply, ours-only changes are kept, identical edits are
// kept once; divergent edits to the same file, mode disagreement,
// modify-vs-delete, or a deleted subtree the other side wrote into are
// conflicts. The refusal is atomic — ErrDeltaConflict names the paths and
// the workspace is untouched; the application phase after a clean merge is
// not transactional, matching the git-era version it replaces. Granularity
// is per-file, not per-line; line-level merge is a scoped follow-up.
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
	// Stdin is delivered to the command as data: the bytes are staged to a
	// mode-0600 temp file inside the container (through the WriteFile
	// path, so arbitrary sizes chunk) and redirected into the process's
	// stdin, which sees EOF when the bytes run out — the shim's stdin
	// FIFO, which never signals EOF, is bypassed entirely. The staged file
	// is removed on every outcome path.
	//
	// This is data delivery, not an interactive session: nothing can be
	// written after the process starts (interactive sessions are a
	// deferred scope boundary). The exposure caveat inherits from
	// WriteFile — content travels base64 in exec arguments, visible in
	// /proc/*/cmdline while staging runs: stdin-shaped data, never
	// secrets.
	Stdin []byte

	// capOutput marks the request as public Exec traffic, where
	// Config.MaxOutputBytes applies. It is unexported and set only by the
	// sandbox's Exec path: internal protocol reads — ReadFile's cat, the
	// diff reader tasks, WriteFile's own acks — carry file bytes and
	// command output that the library parses, so capping them would
	// silently corrupt data transport (a large ReadFile would come back
	// with a truncation marker inside the file's bytes). Diagnostics are
	// capped; data is not. A backend implementation reading this field
	// sees it set exactly for caller-facing Execs.
	capOutput bool
}

// ExecResult is the outcome of one command execution. Stdout and Stderr are
// the capped strings described on Config.MaxOutputBytes when that knob is
// set.
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
	// Digest stability: when the active layer contains no changes, Capture
	// returns the parent state verbatim instead of minting a new key, so two
	// captures of unchanged content at an unchanged timeline are equal. A
	// capture at a moved timeline still has a different key.
	// Config.ContentDigest opts into a content-addressed digest for comparing
	// identical content across a moved timeline; it costs a full-tree diff
	// per capture and is off by default.
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
	//
	// A non-empty ExecRequest.Stdin is delivered to the command as data:
	// staged to a temp file in the container and redirected into the
	// process, which sees EOF (see ExecRequest.Stdin for the contract and
	// the exposure caveat). Stdout and Stderr are capped per stream by
	// Config.MaxOutputBytes when set; over-cap streams keep their head and
	// tail around a truncation marker. A command that backgrounds its work
	// (`nohup … &`) returns immediately and survives subsequent Execs —
	// and dies at Capture, like every process.
	Exec(ctx context.Context, req ExecRequest) (ExecResult, error)

	// ReadFile reads a file inside the sandbox workspace.
	ReadFile(ctx context.Context, path string) ([]byte, error)

	// WriteFile writes a file inside the sandbox workspace.
	WriteFile(ctx context.Context, path string, data []byte, perm fs.FileMode) error

	// Changes reports the differences between ws and the current workspace.
	// Paths are workspace-relative, slash-separated, and sorted. Returns
	// ErrStateUnavailable once ws's layer has been released.
	//
	// Changes is read-only against the workspace: it never stops or restarts
	// the task, never commits, never writes. It takes the shared operation
	// lock, so it cannot interleave with Capture/Apply/Destroy — it always
	// sees a quiescent workspace.
	Changes(ctx context.Context, ws WorkspaceState) ([]Change, error)

	// Diff returns a unified text diff between ws and the current
	// workspace. Binary files render as "Binary files ... differ"; mode-only
	// changes as "old mode"/"new mode" pairs; symlinks as target lines.
	// Returns ErrStateUnavailable once ws's layer has been released.
	//
	// Like Changes, Diff is read-only and takes the shared operation lock.
	Diff(ctx context.Context, ws WorkspaceState) (string, error)

	// ApplyDelta merges ws's changes — everything since ws.BaseKey — onto
	// the current workspace at the tree level. Mutating on success; the
	// workspace is untouched when it refuses. Errors: ErrDeltaConflict
	// (conflicting paths named in the error), ErrStateUnavailable (the base
	// or the state's layer has been released). A state with an empty
	// BaseKey was captured from a sandbox that never Applied and cannot be
	// merged.
	//
	// The merge is tree-level, not line-level: two divergent edits to the
	// same file conflict even when they touch different lines. Like
	// WriteFile, ApplyDelta takes the shared operation lock: it mutates
	// through the running task and never stops it.
	ApplyDelta(ctx context.Context, ws WorkspaceState) error
}
