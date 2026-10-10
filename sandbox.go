// Package containerd implements shepherd's Sandbox interface on top of
// containerd's overlayfs snapshotter, giving each scope a real Linux filesystem
// boundary with cheap forking.
//
// # Design constraints
//
// Each of these contradicts a naive implementation, so read them before changing
// the lifecycle:
//
//   - The trace is not replayable. Workspace state resumes from a committed
//     snapshot, never by re-running recorded shell commands. The kernel records
//     declarations and observations, not invertible mutations; shell commands
//     are audit-only, so rebuilding a filesystem by replaying them cannot work.
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
//   - Merge is a rebase, not a copy. Making the child's committed snapshot the
//     parent's base is O(1). Copying upper directories is wrong: deletions are
//     whiteout character devices and directory opacity is a
//     trusted.overlay.opaque xattr, so a naive copy resurrects deleted files.
//
//   - Linux only. OverlayFS and the containerd daemon require Linux with root or
//     user namespaces. This backend can never be the default on Windows, so the
//     kernel's git backend remains the cross-platform default.
//
//   - Containment is "contained", not "full". A container is namespaces plus a
//     separate rootfs: strong, but a kernel exploit escapes it. Claiming
//     ContainFull would overstate the guarantee.
//
// # Status
//
// The snapshot lifecycle, teardown ordering, layer pruning, file I/O, and diff are
// implemented and unit-tested against containerd's real snapshots.Snapshotter
// interface, using an in-memory fake that enforces the same invariants as the
// overlayfs snapshotter: a key cannot be prepared twice, a parent must be
// committed, only a committed layer can be committed once, a committed layer is
// read-only, and a layer with children cannot be removed.
//
// The daemon adapter (client.go) compiles but has never been executed: it needs a
// Linux host with a running containerd daemon and root or user namespaces, which
// this package's development environment does not provide. Treat container and OCI
// spec construction and the exec round-trip as unverified until exercised on such
// a host. The lifecycle above them is what the tests cover.
//
// # Lifecycle
//
//	Create   resolve the image rootfs, Prepare the base active snapshot, start
//	         the task on its mounts
//	Capture  stop the task, Commit the active snapshot, pin it as a state under
//	         the namespace's states lease, Prepare a fresh active snapshot on
//	         top of it, restart the task
//	Apply    Prepare a new active snapshot from a committed key, rebind, restart
//	Fork     Commit the parent's active snapshot, then Prepare the child with
//	         that committed snapshot as its parent (see ForkState)
//	Destroy  stop the task, Remove the active layer, release the sandbox lease.
//	         Committed layers survive: they are states, and states outlive the
//	         sandbox that captured them
//	ReleaseState  drop a state's pin; the daemon's GC reclaims the layer once
//	         nothing references it
//
// Apply is single-use per state: unlike the git backend, resuming from a
// committed snapshot rebinds this sandbox's identity rather than replaying into
// an existing tree. Callers must not assume the git backend's reusability.
//
// Disk comes back through ReleaseState, Destroy's active-layer removal, or
// namespace teardown — in that order of specificity. Destroy deliberately does
// not remove committed layers: every one was returned to a caller as a state,
// and a sibling sandbox may be running on a child of the same layer.
package containerd

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	shepherd "github.com/buchenberg/shepherd-kernel-go"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/snapshots"
)

// BackendName is the identifier reported by Backend and stored in
// WorkspaceState.Backend.
const BackendName = "containerd"

// sandboxLeaseLabel marks leases this adapter owns.
//
// A lease is the only thing keeping a sandbox's snapshots alive between
// generations, so an orphaned one — left by a process that died before Destroy —
// has to be findable without in-process bookkeeping. `ctr leases ls` filters on
// labels, and the snapshot keys the lease holds carry the sandbox id.
const sandboxLeaseLabel = "shepherd.lease"

const (
	// maxArgPayload is the largest base64 piece WriteFile puts in a single
	// execve argument. Linux caps one argument at MAX_ARG_STRLEN (32 pages,
	// 128 KiB) independently of ARG_MAX, so the payload is kept clear of it.
	// This is a kernel limit, not a tunable, hence a constant.
	maxArgPayload = 96 * 1024

	// defaultWriteChunkBytes is the raw payload per WriteFile exec when
	// Config.WriteChunkBytes is unset. It expands to roughly 700 KiB of base64,
	// i.e. about eight maxArgPayload arguments — comfortably inside the ~2 MiB
	// ARG_MAX while keeping the exec count low for typical source files.
	defaultWriteChunkBytes = 512 * 1024
)

// WorkspaceState.Data keys. Pinning the encoding here means a git backend's
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
	// StateKeyGitHead is the in-container git HEAD at capture time, when the
	// workspace is a git repository. Diff needs it as a baseline.
	StateKeyGitHead = "git_head"
	// StateKeyDeltaBase is the git HEAD the sandbox last applied (the fork
	// baseline for an isolated child), recorded so the captured state carries
	// its own delta base. ApplyDelta needs it as the merge base.
	StateKeyDeltaBase = "delta_base"
	// StateKeyDeltaPatch is the base64 (StdEncoding) git binary patch from
	// StateKeyDeltaBase to the captured working tree, present only when that
	// range has changes. An absent key with a present StateKeyDeltaBase is a
	// clean capture: nothing to merge.
	StateKeyDeltaPatch = "delta_patch"
)

// ErrNotImplemented is returned when the containerd surface could not be
// connected. It is deliberately distinct from shepherd.ErrUnsupported:
// Capabilities declares these operations supported, so reporting "unsupported"
// would be false.
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
	// Empty uses containerd's default.
	Runtime string

	// Snapshotter selects the containerd snapshotter plugin. Empty uses
	// "overlayfs". Other snapshotters differ in commit semantics, so this is not
	// assumed.
	Snapshotter string

	// InitArgs is the long-lived command a container runs so it stays up between
	// commands; Exec provides the actual work. Empty uses
	// `/bin/sh -c "sleep infinity"`, which requires a shell in the image.
	InitArgs []string

	// Workdir is the workspace path inside the container. Relative file paths
	// and commands resolve here. Empty defaults to "/workspace".
	Workdir string

	// WriteChunkBytes bounds how much file content a single WriteFile exec
	// carries, in raw bytes before base64 expansion. Empty uses
	// defaultWriteChunkBytes. Larger values mean fewer container execs per file
	// and a larger argv; each exec's payload is split into arguments under the
	// kernel's per-argument ceiling regardless, so this only controls how much
	// travels per exec.
	WriteChunkBytes int

	// FIFODir is where the containerd *client* creates the FIFOs that carry an
	// exec's stdio. Empty uses containerd's default (/run/containerd/fifo),
	// which is root-owned — and because the client creates these itself, an
	// unprivileged caller fails locally with EACCES before any RPC reaches the
	// daemon. Set this to a directory the calling user can write to in order to
	// drive the backend without root.
	FIFODir string
}

// workdir returns the effective in-container workspace path.
func (c Config) workdir() string {
	if c.Workdir == "" {
		return "/workspace"
	}
	return c.Workdir
}

// taskService is the container/task half of containerd this sandbox needs.
//
// containerd exposes no single interface for this, so it is declared here. The
// production adapter lives in client.go; tests substitute an in-memory fake.
//
// StartTask takes a snapshot key rather than mounts because containerd resolves
// a container's rootfs from its snapshot itself; the layer identity is the only
// thing the caller needs to pass.
type taskService interface {
	// StartTask creates and starts task id, rooted at the snapshot key.
	StartTask(ctx context.Context, id, snapshotKey string) error
	// StopTask stops and deletes task id. It must be idempotent so Destroy and
	// Capture are safe to retry.
	StopTask(ctx context.Context, id string) error
	// Running reports whether task id exists and is running.
	Running(ctx context.Context, id string) (bool, error)
	// Exec runs a command inside task id.
	Exec(ctx context.Context, id string, req shepherd.ExecRequest) (shepherd.ExecResult, error)
}

// imageService resolves an image reference to the snapshotter key holding its
// unpacked rootfs, pulling and unpacking when necessary.
type imageService interface {
	RootfsSnapshot(ctx context.Context, ref string) (string, error)
}

// ContainerdSandbox is a scope's execution substrate backed by containerd.
//
// One instance corresponds to one sandbox, mirroring the kernel's
// one-Sandbox-per-scope model. Safe for concurrent use.
// leaseRef carries the sandbox's current containerd lease id.
//
// It is written by Create and Destroy and read on every snapshotter call, so it
// is atomic rather than mutex-guarded: the snapshotter wrapper must not contend
// on the sandbox lock for each Prepare, Commit, and Remove.
type leaseRef struct{ v atomic.Pointer[string] }

// get returns the current lease id, or "" when the sandbox holds none.
func (r *leaseRef) get() string {
	if p := r.v.Load(); p != nil {
		return *p
	}
	return ""
}

// set replaces the current lease id; "" clears it.
func (r *leaseRef) set(id string) {
	if id == "" {
		r.v.Store(nil)
		return
	}
	r.v.Store(&id)
}

type ContainerdSandbox struct {
	cfg    Config
	snap   snapshots.Snapshotter
	tasks  taskService
	images imageService
	// leases is the daemon's lease manager. Nil when the collaborators were
	// substituted (tests): a fake snapshotter has no garbage collector to hold
	// anything back from, so there is nothing to lease.
	leases leases.Manager

	// connectOnce guards lazy connection to the containerd daemon so New does
	// no I/O and no error is silently dropped.
	connectOnce sync.Once
	connectErr  error

	mu sync.Mutex
	// leaseMu serializes lease acquisition and release. It is deliberately not
	// mu: holding a lock across the lease RPC must not block Exec or Diff, which
	// take mu briefly to read the sandbox id.
	leaseMu sync.Mutex
	// lease is the id of the containerd lease holding this sandbox's snapshots.
	// Atomic because snapshotter calls read it without mu.
	lease leaseRef
	// id is the container/task identity. Apply rebinds it, so it is not stable
	// for the instance's lifetime.
	id string
	// rootfsKey is the image rootfs snapshot. It is owned by the image, not by
	// this sandbox, and is never removed here.
	rootfsKey string
	// activeKey is the writable snapshot the running task is rooted at.
	activeKey string
	// committedChain lists the committed layers this sandbox's active layer
	// descends from, oldest first. It records fork lineage — the next Capture's
	// parent key — not teardown: committed layers are states, outlive the
	// sandbox, and are never removed by Destroy or Apply.
	committedChain []string
	// baseGitHead is the git HEAD recorded in the state this sandbox last
	// applied — the fork baseline for an isolated child. Captures diff against
	// it (deltaPatchScript) so the states they hand out can settle through
	// ApplyDelta. Empty when no applied state carried a git HEAD, which means
	// captures carry no delta payload and their settlement verb is Select only.
	baseGitHead string
	// seq orders snapshot keys within this sandbox.
	seq uint64
}

// The compile-time assertion is load-bearing: if shepherd.Sandbox gains or
// changes a method, this package stops building until the adapter matches.
var _ shepherd.Sandbox = (*ContainerdSandbox)(nil)

// StateReleaser is the optional state-discard surface this backend adds beyond
// shepherd.Sandbox. The kernel's WorkspaceState has no lifecycle — a state
// outlives the sandbox that captured it — and the lease pin that makes that
// true holds daemon disk. This is how a caller gives it back deliberately.
//
// Callers type-assert to it:
//
//	if r, ok := sb.(StateReleaser); ok { _ = r.ReleaseState(ctx, state) }
//
// Promoting it into core's Sandbox interface is a separate, deliberate
// kernel-API decision (every backend would have to answer what discarding a
// state means); the assertion above is the tripwire that forces that
// conversation.
type StateReleaser interface {
	// ReleaseState drops the pin holding ws's snapshot. The daemon's garbage
	// collector reclaims the layer once no other snapshot, container, or lease
	// references it; until then the state stays valid.
	ReleaseState(ctx context.Context, ws shepherd.WorkspaceState) error
}

var _ StateReleaser = (*ContainerdSandbox)(nil)

// New returns a sandbox bound to cfg, backed by a real containerd client. The
// client is connected lazily on first use.
func New(cfg Config) *ContainerdSandbox {
	return &ContainerdSandbox{cfg: cfg}
}

// NewWithBackend returns a sandbox backed by the supplied implementations.
// Exported for tests and for embedding the sandbox in a host that already holds
// a containerd client.
//
// Without WithLeases the sandbox has no lease manager, and state durability is
// inert: Capture still succeeds and hands out states, but nothing pins their
// snapshots against the daemon's GC (see pinState). A host embedding a real
// snapshotter should therefore pass WithLeases with the client's
// LeasesService(); test fakes have no GC and lose nothing by omitting it.
func NewWithBackend(cfg Config, snap snapshots.Snapshotter, tasks taskService, images imageService, opts ...BackendOption) *ContainerdSandbox {
	s := &ContainerdSandbox{cfg: cfg, snap: snap, tasks: tasks, images: images}
	for _, o := range opts {
		o(s)
	}
	return s
}

// BackendOption configures optional collaborators on a NewWithBackend sandbox.
type BackendOption func(*ContainerdSandbox)

// WithLeases supplies the daemon's lease manager for hosts embedding the
// sandbox over an existing containerd client — pass client.LeasesService().
// Without it, pinState and unpinState are no-ops and captured states are not
// durable (see NewWithBackend).
func WithLeases(mgr leases.Manager) BackendOption {
	return func(s *ContainerdSandbox) { s.leases = mgr }
}

// Backend reports the backend identifier.
func (s *ContainerdSandbox) Backend() string { return BackendName }

// Capabilities reports the backend's support matrix.
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

// ID returns the current container identity, or "" before Create. It changes
// across Apply, so callers must not cache it.
func (s *ContainerdSandbox) ID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

// Create provisions the base snapshot from Config.Image and starts the task.
func (s *ContainerdSandbox) Create(ctx context.Context, spec shepherd.SandboxSpec) error {
	if s.cfg.Image == "" {
		return fmt.Errorf("containerd sandbox: Config.Image is required")
	}
	// SandboxSpec.Timeout documents that it bounds the whole Create, so it has to
	// be in force before the daemon connection and the lease RPC — not after
	// them. A blocked lease service would otherwise hang past the caller's limit.
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}
	if err := s.requireBackend(ctx); err != nil {
		return err
	}
	// Before the first snapshot call: containerd only associates a snapshot with
	// a lease if one is already in the context of the call that creates it.
	if err := s.ensureLease(ctx); err != nil {
		return err
	}

	rootfsKey, err := s.images.RootfsSnapshot(ctx, s.cfg.Image)
	if err != nil {
		return fmt.Errorf("containerd sandbox: resolve image %s: %w", s.cfg.Image, err)
	}

	id := newSandboxID()
	active := s.key(id, "active", 0)

	mounts, err := s.snap.Prepare(ctx, active, rootfsKey)
	if err != nil {
		return fmt.Errorf("containerd sandbox: prepare base snapshot: %w", err)
	}
	// containerd resolves the container's rootfs from the snapshot itself, so the
	// returned mounts are not needed here; Prepare's side effect is the point.
	_ = mounts

	if err := s.tasks.StartTask(ctx, id, active); err != nil {
		// Do not leak the snapshot if the task could not start.
		_ = s.snap.Remove(ctx, active)
		return fmt.Errorf("containerd sandbox: start task: %w", err)
	}

	// Provision the workdir. Every exec and file operation resolves against it,
	// but no image is obliged to contain it — the default /workspace exists in
	// almost nothing. Without this, Create succeeds on a stock image and then
	// every later operation fails with a chdir error, which reads as a broken
	// sandbox rather than a missing directory.
	wd := s.cfg.workdir()
	res, err := s.tasks.Exec(ctx, id, shepherd.ExecRequest{
		Command: "mkdir",
		Args:    []string{"-p", wd},
		Cwd:     "/",
	})
	if err == nil && res.ExitCode != 0 {
		err = fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	if err != nil {
		_ = s.tasks.StopTask(context.WithoutCancel(ctx), id)
		_ = s.snap.Remove(context.WithoutCancel(ctx), active)
		return fmt.Errorf("containerd sandbox: provision workdir %s: %w", wd, err)
	}

	s.mu.Lock()
	s.id = id
	s.rootfsKey = rootfsKey
	s.activeKey = active
	s.committedChain = nil
	s.seq = 0
	s.mu.Unlock()

	return nil
}

// Destroy stops the task, removes the sandbox's active layer, and releases the
// sandbox lease.
//
// Committed layers survive. Every one was returned by Capture or ForkState as a
// WorkspaceState, and states have no lifecycle: a caller may Apply one long
// after this sandbox is gone, and sibling sandboxes may be running on children
// of the same layers right now. They stay pinned by the namespace's states
// lease (see pinState) and are reclaimed through ReleaseState or namespace
// teardown, never here.
//
// Idempotent: it clears its own bookkeeping, so a second call is a no-op rather
// than a wave of not-found errors. The image rootfs snapshot is never removed —
// it belongs to the image.
func (s *ContainerdSandbox) Destroy(ctx context.Context) error {
	if s.snap == nil || s.tasks == nil {
		return nil
	}

	s.mu.Lock()
	id := s.id
	active := s.activeKey
	rootfs := s.rootfsKey
	s.id, s.activeKey, s.committedChain, s.rootfsKey = "", "", nil, ""
	s.mu.Unlock()

	var errs []error
	if id != "" {
		if err := s.tasks.StopTask(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("stop task %s: %w", id, err))
		}
	}

	// The active layer is the only one the sandbox owns outright: it is the
	// private writable layer of the running task and backs no state.
	if active != "" && active != rootfs {
		if err := s.snap.Remove(ctx, active); err != nil {
			errs = append(errs, fmt.Errorf("remove snapshot %s: %w", active, err))
		}
	}

	// Last, so the removals above still run under the lease's protection and a
	// concurrent GC cannot race them.
	if err := s.releaseLease(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Capture commits the current workspace as a resumable state.
//
// The task is stopped before the commit and restarted afterwards. That is not
// gratuitous: containerd's Commit renames the snapshot key in metadata while the
// directory keeps its immutable id, so a running task's mount survives — but the
// committed layer immediately becomes a lowerdir of the next active layer, and
// OverlayFS lowerdirs must not be written to. Leaving the task running would let
// it write into a layer that is now read-only by contract, corrupting the layer
// beneath its successor.
//
// Stopping the task loses in-container process state. That is the price of a
// coherent snapshot with this API.
func (s *ContainerdSandbox) Capture(ctx context.Context) (shepherd.WorkspaceState, error) {
	if err := s.requireBackend(ctx); err != nil {
		return shepherd.WorkspaceState{}, err
	}

	s.mu.Lock()
	id, active, parent := s.id, s.activeKey, s.parentKeyLocked()
	// active empty with an id set is the cleanly-stopped state advance leaves
	// behind when a successor could not be started: there is no running layer
	// to commit, so this is the same "not created (anymore)" case.
	if id == "" || active == "" {
		s.mu.Unlock()
		return shepherd.WorkspaceState{}, fmt.Errorf("containerd sandbox: Capture before Create")
	}
	s.seq++
	n := s.seq
	s.mu.Unlock()

	// A layer the task is still writing to must not be committed.
	if err := s.tasks.StopTask(ctx, id); err != nil {
		return shepherd.WorkspaceState{}, fmt.Errorf("containerd sandbox: stop task before capture: %w", err)
	}

	committed := s.key(id, "committed", n)
	if err := s.snap.Commit(ctx, committed, active); err != nil {
		// The task is stopped; restart it on the old layer so the sandbox is
		// left usable rather than wedged.
		_ = s.restart(ctx, id, active)
		return shepherd.WorkspaceState{}, fmt.Errorf("containerd sandbox: commit snapshot: %w", err)
	}

	// The kernel's contract is that a WorkspaceState outlives the sandbox that
	// captured it, so the pin cannot be this sandbox's lease: Destroy releases
	// that one. Register the layer under the namespace's states lease instead.
	// Failure fails the Capture — the caller must not be handed a state the
	// daemon's GC may reclaim. Recovery shares the successor path with the
	// success case below (advance), on a cancel-free context since the failure
	// may itself be a cancellation.
	if err := s.pinState(ctx, committed); err != nil {
		_ = s.advance(context.WithoutCancel(ctx), id, committed, n)
		return shepherd.WorkspaceState{}, fmt.Errorf("containerd sandbox: pin captured state: %w", err)
	}

	if err := s.advance(ctx, id, committed, n); err != nil {
		// The pin succeeded but no state is being handed out, so the caller
		// can never ReleaseState this handle: drop the pin or it holds disk
		// until namespace teardown. The committed layer itself stays — Commit
		// already renamed the active key onto it, and advance has recorded it
		// as this sandbox's lineage — where the sandbox lease protects it for
		// the sandbox's remaining life and the daemon GC reclaims it after
		// Destroy. Cancel-free, for the same reason as the pin-failure branch.
		_ = s.unpinState(context.WithoutCancel(ctx), committed)
		return shepherd.WorkspaceState{}, err
	}

	state := snapshotState(s.cfg.Image, committed, parent)
	// Record the in-container git HEAD so Diff has a baseline. Absence is not an
	// error: a non-git workspace simply cannot be diffed semantically.
	if head, err := s.gitHead(ctx, id); err == nil && head != "" {
		state.Data[StateKeyGitHead] = head
		// Carry the delta payload so settlement's apply verb can merge this
		// state onto a workspace that has moved since the base. Only when a
		// base was recorded by Apply: a sandbox that never applied a git state
		// has no base to diff against, and its states settle Select-only.
		//
		// Unlike the git HEAD, failure here fails the Capture. A sealed output
		// whose state cannot settle-apply surfaces the gap only after a merge
		// was approved, which is too late to fix; refusing at capture keeps
		// the gap at sealing time, where the caller still holds the scope.
		if base := s.baseGitHeadLocked(); base != "" {
			res, err := s.shell(ctx, deltaPatchScript, base)
			if err != nil {
				return shepherd.WorkspaceState{}, fmt.Errorf("containerd sandbox: capture delta against base %s: %w", base, err)
			}
			state.Data[StateKeyDeltaBase] = base
			if patch := res.Stdout; strings.TrimSpace(patch) != "" {
				state.Data[StateKeyDeltaPatch] = base64.StdEncoding.EncodeToString([]byte(patch))
			}
		}
	}
	return state, nil
}

// advance is Capture's successor path, shared by the success case and the
// pin-failure recovery: it prepares a fresh active snapshot on top of the
// committed layer, starts the task on it, and records the lineage.
//
// The caller has already stopped the task and committed the active layer, so
// the old activeKey is gone — Commit renamed it. On any failure here there is
// therefore nothing to restart on, and bookkeeping must not keep pointing at
// a key the snapshotter no longer has: activeKey is cleared and the committed
// layer is recorded, leaving the sandbox cleanly stopped (Destroy or an Apply
// of an existing state recovers it) rather than wedged mid-lifecycle.
func (s *ContainerdSandbox) advance(ctx context.Context, id, committed string, n uint64) error {
	stopped := func(err error) error {
		s.mu.Lock()
		s.activeKey = ""
		s.committedChain = append(s.committedChain, committed)
		s.mu.Unlock()
		return err
	}

	next := s.key(id, "active", n)
	if _, err := s.snap.Prepare(ctx, next, committed); err != nil {
		return stopped(fmt.Errorf("containerd sandbox: prepare successor snapshot: %w", err))
	}
	if err := s.tasks.StartTask(ctx, id, next); err != nil {
		_ = s.snap.Remove(context.WithoutCancel(ctx), next)
		return stopped(fmt.Errorf("containerd sandbox: restart task after capture: %w", err))
	}

	s.mu.Lock()
	s.activeKey = next
	s.committedChain = append(s.committedChain, committed)
	s.mu.Unlock()
	return nil
}

// Apply resumes the workspace from a previously captured state by preparing a new
// active snapshot from the committed key and rebinding this sandbox.
//
// Layers created after the applied state are no longer ancestors of this
// sandbox's active layer, but they survive: each was handed to a caller as a
// WorkspaceState, and states outlive sandboxes. Reclaim them with ReleaseState
// when the caller is done with them. Only the abandoned active layer is
// removed. Single-use per state: the sandbox now runs on the given committed
// layer, so applying a different state moves it again. This differs from the
// git backend, whose states are freely reusable.
func (s *ContainerdSandbox) Apply(ctx context.Context, ws shepherd.WorkspaceState) error {
	committed, err := stateSnapshotKey(ws)
	if err != nil {
		return err
	}
	if err := s.requireBackend(ctx); err != nil {
		return err
	}
	// Apply prepares snapshots under the lease, so it needs it as much as Create
	// does. Reaching here without Create is normal: a resumed sandbox acquires
	// its lease lazily on first use.
	if err := s.ensureLease(ctx); err != nil {
		return err
	}

	// Plan under the lock; mutate only after the snapshot and task operations
	// succeed, so a failure cannot leave activeKey pointing at a layer that does
	// not exist.
	s.mu.Lock()
	id := s.id
	if id == "" {
		id = newSandboxID()
	}
	active := s.activeKey
	kept, _ := splitChain(s.committedChain, committed)
	s.seq++
	next := s.key(id, "active", s.seq)
	s.mu.Unlock()

	if active != "" {
		if err := s.tasks.StopTask(ctx, id); err != nil {
			return fmt.Errorf("containerd sandbox: stop task before apply: %w", err)
		}
	}

	mounts, err := s.snap.Prepare(ctx, next, committed)
	if err != nil {
		// The task is stopped; bring it back on the layer it had so the sandbox
		// is left usable rather than wedged.
		if active != "" {
			_ = s.restart(ctx, id, active)
		}
		return fmt.Errorf("containerd sandbox: prepare snapshot from %s: %w", committed, err)
	}
	_ = mounts
	if err := s.tasks.StartTask(ctx, id, next); err != nil {
		return fmt.Errorf("containerd sandbox: start task after apply: %w", err)
	}

	s.mu.Lock()
	s.id = id
	s.activeKey = next
	s.committedChain = kept
	// The applied state's git HEAD becomes this sandbox's delta base: captures
	// from here on diff against it, so the states they produce can settle
	// through ApplyDelta. A state without one (a non-git workspace) leaves any
	// previous base alone — applying it cannot have introduced a git baseline.
	if head, _ := ws.Data[StateKeyGitHead].(string); head != "" {
		s.baseGitHead = head
	}
	s.mu.Unlock()

	// Best effort: the abandoned active layer is this sandbox's private
	// writable layer and backs no state, so its leak is the only one Apply
	// cleans up itself.
	if active != "" && active != next {
		_ = s.snap.Remove(ctx, active)
	}
	return nil
}

// ReleaseState drops the pin holding a captured state's snapshot, letting the
// daemon's garbage collector reclaim the layer once no other snapshot,
// container, or lease references it.
//
// This is the state-discard half of the kernel's "a WorkspaceState has no
// lifecycle" contract: the state stays valid for exactly as long as the caller
// wants it, and disk comes back when the caller says so. It is deliberately not
// part of shepherd.Sandbox — see StateReleaser — because promoting a discard
// operation into the core interface is a kernel-level API decision.
//
// Idempotent: releasing an already-released state is a no-op.
//
// The pin is addressed by the releasing backend's own namespace and
// snapshotter — a WorkspaceState carries no config identity — so release
// through a backend configured the same way as the one that captured the
// state. A mismatched release reports success (the target resource is simply
// not found) while the real pin persists.
func (s *ContainerdSandbox) ReleaseState(ctx context.Context, ws shepherd.WorkspaceState) error {
	key, err := stateSnapshotKey(ws)
	if err != nil {
		return err
	}
	if err := s.requireBackend(ctx); err != nil {
		return err
	}
	return s.unpinState(ctx, key)
}

// ApplyDelta implements shepherd.DeltaApplier: it merges the changes captured
// in delta onto the current workspace without resetting it, so a parent that
// moved since the fork still lands the child's work. Settlement's apply verb
// (settlement.go, SettleApply) calls this after the overlap guard has refused
// same-path parent changes.
//
// The delta payload was recorded at Capture: a git binary patch from the
// delta's own base (the git HEAD of the state the capturing sandbox last
// applied) to its working tree. The merge itself runs at the tree level in the
// object database (applyDeltaScript), so a conflicting delta refuses with the
// working tree untouched — the DeltaApplier contract — and a clean one lands
// as unstaged working tree changes, leaving the caller's staging state alone.
//
// A delta with no changes is a no-op. A state captured without a base (a
// sandbox that never applied a git state) cannot be merged and returns
// ErrUnsupported rather than pretending: its scope settles Select-only.
func (s *ContainerdSandbox) ApplyDelta(ctx context.Context, delta shepherd.WorkspaceState) error {
	if delta.Backend != BackendName {
		return fmt.Errorf("containerd sandbox: cannot apply delta from backend %q", delta.Backend)
	}
	base, _ := delta.Data[StateKeyDeltaBase].(string)
	patchB64, _ := delta.Data[StateKeyDeltaPatch].(string)
	if base == "" {
		return fmt.Errorf("containerd sandbox: delta was captured without a recorded git base, so its changes cannot be merged onto a moved workspace: %w", shepherd.ErrUnsupported)
	}
	if patchB64 == "" {
		return nil // clean capture: nothing to merge
	}
	patch, err := base64.StdEncoding.DecodeString(patchB64)
	if err != nil {
		return fmt.Errorf("containerd sandbox: delta patch is not valid base64: %w", err)
	}
	if err := s.requireBackend(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	id := s.id
	s.mu.Unlock()
	if id == "" {
		return fmt.Errorf("containerd sandbox: ApplyDelta before Create")
	}

	// The patch travels through a temp file rather than an argument: delta
	// payloads are argument-sized only for small workspaces, and WriteFile's
	// chunked exec already handles arbitrary sizes.
	patchPath := "/tmp/shepherd-delta-" + newSandboxID() + ".patch"
	if err := s.WriteFile(ctx, patchPath, patch, 0o600); err != nil {
		return fmt.Errorf("containerd sandbox: stage delta patch: %w", err)
	}
	defer func() {
		_, _ = s.shell(ctx, "rm -f -- \"$1\"", patchPath)
	}()

	if _, err := s.execShell(ctx, nil, applyDeltaScript, patchPath, base); err != nil {
		// execShell flattens the exit code into the error text; exit 3 is the
		// script's conflict sentinel and names the paths in stderr.
		return fmt.Errorf("containerd sandbox: apply delta: %w", err)
	}
	return nil
}

var _ shepherd.DeltaApplier = (*ContainerdSandbox)(nil)

// splitChain partitions a committed-layer chain at key: layers up to and
// including key are retained (they remain ancestors of a layer prepared from
// key), and the rest are no longer ancestors of anything this sandbox runs on.
// They are still states the caller may hold and are not removed — the partition
// only rebinds lineage. A key that is not in the chain — an image rootfs, or a
// state from another lineage — retains nothing.
func splitChain(chain []string, key string) (kept, orphaned []string) {
	for i, k := range chain {
		if k == key {
			return chain[:i+1], chain[i+1:]
		}
	}
	return nil, chain
}

// diffNamesMarker separates Diff's two outputs inside one staging pass, so the
// changed-file list and the unified diff always describe the same workspace
// state. It is safe as a delimiter because git quotes paths containing control
// characters, so no filename can produce a bare newline-wrapped marker line.
const diffNamesMarker = "__shepherd_diff_names__"

// diffScript stages the working tree into a throwaway index once, then prints
// the changed file list followed by the unified diff, separated by
// diffNamesMarker. $1 is the baseline commit.
//
// One exec rather than two. Calling this twice would stage twice, and a write to
// the workspace between the two calls would make the returned file list and diff
// describe different states — a race the earlier mutating implementation did not
// have, because it staged once into the real index. This keeps both the
// single-staging consistency and the non-mutation.
//
// This mirrors sandbox_git.go's stagedIndex: seed the scratch from the real
// index when one exists, so git's stat cache survives and `git add -A` only
// hashes what changed; otherwise fall back to read-tree, because an empty index
// would report every tracked file as newly added. The real index is only ever
// read, never written, which is what makes Diff non-mutating.
//
// mktemp -d gives the scratch a 0700 directory rather than a bare file in a
// shared temp dir, and the trap removes it even when a command fails.
var diffScript = `set -e
head=$1
dir=$(mktemp -d)
trap 'rm -rf -- "$dir"' EXIT
export GIT_INDEX_FILE="$dir/index"
if [ -f .git/index ]; then
  cp -- .git/index "$GIT_INDEX_FILE"
elif git rev-parse --verify -q HEAD >/dev/null 2>&1; then
  git read-tree HEAD
fi
git add -A
git diff --cached --name-only "$head"
printf '\n` + diffNamesMarker + `\n'
git diff --cached "$head"`

// deltaPatchScript emits the git binary patch from $1 (the recorded delta base)
// to the current working tree, untracked files included. Capture stores it in
// StateKeyDeltaPatch so ApplyDelta can merge the state onto a workspace that has
// moved since the base.
//
// The staging mirrors diffScript — scratch GIT_INDEX_FILE seeded from the real
// index when one exists — for the same non-mutation and stat-cache reasons.
// `--binary` carries binary file changes, which a plain text diff would mark
// "Binary files differ" and drop.
var deltaPatchScript = `set -e
head=$1
dir=$(mktemp -d)
trap 'rm -rf -- "$dir"' EXIT
export GIT_INDEX_FILE="$dir/index"
if [ -f .git/index ]; then
  cp -- .git/index "$GIT_INDEX_FILE"
elif git rev-parse --verify -q HEAD >/dev/null 2>&1; then
  git read-tree HEAD
fi
git add -A
git diff --binary --cached "$head"`

// applyDeltaScript merges the delta ($1 = patch file, $2 = its recorded base)
// onto the current workspace without ever touching it on conflict.
//
// The merge runs at the tree level, purely in the object database:
//
//	theirs = the delta's content, reconstructed by applying the patch to the
//	         recorded base (the patch's preimages are the base's blobs)
//	ours   = the working tree as it stands, untracked included
//	base   = the recorded base's tree
//
// git merge-tree --write-tree merges the three and exits nonzero on conflict,
// having written nothing anywhere near the working tree — the refusal is
// atomic by construction. On success the merged tree exists, and the merge is
// materialized as a patch whose preimage is ours, i.e. exactly the working
// tree: `git apply` (no --index, no --3way) then lands it as unstaged working
// tree changes and cannot conflict, because the conflict was already answered.
//
// Exit 3 is the conflict sentinel so the caller can tell a refused merge (the
// workspace untouched, the delta unconsumed) from an operational failure.
// merge-tree prints the conflicted paths on stdout when it conflicts; the
// script forwards them to stderr for the error message.
//
// Seeding the ours index from the real one (cp .git/index) keeps git's stat
// cache so `git add -A` only rehashes what changed; add -A then overwrites
// every entry from the working tree, which is what makes ours the working tree
// rather than the index. The staged-vs-unstaged distinction is deliberately
// flattened: the merge is content-level, and materialization through plain
// `git apply` leaves the caller's staging state alone.
var applyDeltaScript = `set -e
patch=$1
base=$2
dir=$(mktemp -d)
trap 'rm -rf -- "$dir"' EXIT
export GIT_INDEX_FILE="$dir/theirs"
git read-tree "$base"
git apply --cached "$patch"
theirs=$(git write-tree)
export GIT_INDEX_FILE="$dir/ours"
if [ -f .git/index ]; then
  cp -- .git/index "$GIT_INDEX_FILE"
else
  git read-tree HEAD
fi
git add -A
ours=$(git write-tree)
unset GIT_INDEX_FILE
basetree=$(git rev-parse "$base^{tree}")
set +e
merged=$(git merge-tree --write-tree --merge-base="$basetree" "$ours" "$theirs" 2>"$dir/mt.err")
code=$?
set -e
if [ "$code" -eq 1 ]; then
  printf 'conflicting paths:\n%s\n' "$merged" >&2
  exit 3
fi
if [ "$code" -ne 0 ]; then
  cat "$dir/mt.err" >&2
  exit 1
fi
git diff --binary "$ours" "$merged" > "$dir/merge.patch"
if [ -s "$dir/merge.patch" ]; then
  git apply "$dir/merge.patch"
fi`

// Diff returns the unified diff and changed file paths between ws and the
// current workspace, using the in-container git repository.
//
// Non-mutating: staging happens in a scratch GIT_INDEX_FILE (diffScript), so a
// caller's pre-staged changes survive the call, as the Sandbox contract in
// sandbox.go requires. This used to stage with `git add -A` and unwind with
// `git reset`, which restored the index to HEAD rather than to the state it was
// found in and silently dropped the caller's staged work. Fixed 2026-10-05 and
// pinned by TestLive_DiffPreservesPreStagedChanges.
//
// Both outputs come from one staging pass, so they cannot describe different
// workspace states. See diffScript; pinned by TestDiff_StagesOncePerCall.
func (s *ContainerdSandbox) Diff(ctx context.Context, ws shepherd.WorkspaceState, maxLines int) (string, []string, error) {
	if _, err := stateSnapshotKey(ws); err != nil {
		return "", nil, err
	}
	head, _ := ws.Data[StateKeyGitHead].(string)
	if head == "" {
		return "", nil, fmt.Errorf("containerd sandbox: diff needs a git workspace; state has no %q", StateKeyGitHead)
	}

	s.mu.Lock()
	id := s.id
	s.mu.Unlock()
	if id == "" {
		return "", nil, fmt.Errorf("containerd sandbox: Diff before Create")
	}

	res, err := s.shell(ctx, diffScript, head)
	if err != nil {
		return "", nil, fmt.Errorf("containerd sandbox: diff: %w", err)
	}
	namesPart, full, ok := strings.Cut(res.Stdout, "\n"+diffNamesMarker+"\n")
	if !ok {
		return "", nil, fmt.Errorf("containerd sandbox: diff: output has no %q separator, so the "+
			"file list and the diff cannot be told apart", diffNamesMarker)
	}
	var files []string
	if trimmed := strings.TrimSpace(namesPart); trimmed != "" {
		files = strings.Split(trimmed, "\n")
	}
	if maxLines > 0 {
		lines := strings.Split(full, "\n")
		if len(lines) > maxLines {
			full = strings.Join(lines[:maxLines], "\n") + "\n...[diff truncated]"
		}
	}
	return full, files, nil
}

// Exec runs a command inside the sandbox.
func (s *ContainerdSandbox) Exec(ctx context.Context, req shepherd.ExecRequest) (shepherd.ExecResult, error) {
	if err := s.requireBackend(ctx); err != nil {
		return shepherd.ExecResult{}, err
	}
	s.mu.Lock()
	id := s.id
	s.mu.Unlock()
	if id == "" {
		return shepherd.ExecResult{}, fmt.Errorf("containerd sandbox: Exec before Create")
	}
	if req.Cwd == "" {
		req.Cwd = s.cfg.workdir()
	}
	return s.tasks.Exec(ctx, id, req)
}

// ReadFile reads a file inside the sandbox.
//
// There is no host path to open: the workspace lives in a container snapshot, so
// the read runs in-container. The path is passed as a positional argument rather
// than interpolated into the script so it cannot alter the command.
func (s *ContainerdSandbox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if err := s.requireBackend(ctx); err != nil {
		return nil, err
	}
	path = s.resolve(path)
	res, err := s.shell(ctx, `cat -- "$1"`, path)
	if err != nil {
		return nil, fmt.Errorf("containerd sandbox: read %s: %w", path, err)
	}
	return []byte(res.Stdout), nil
}

// WriteFile writes a file inside the sandbox, creating parent directories.
//
// Like ReadFile this runs in-container. perm is applied as an octal mode.
//
// The content travels as base64 in positional arguments rather than on stdin.
// That is forced, not preferred: the runc-v2 shim holds its own O_WRONLY handle
// on an exec's stdin FIFO for the process's whole lifetime
// (cmd/containerd-shim-runc-v2/process/exec.go, execProcess.openStdin), so a
// reader such as `cat` never observes EOF and a stdin-based write blocks until
// the context expires. Verified live against containerd 2.3.5 on 2026-10-05; see
// the KNOWN CONSTRAINT note on containerdTasks.Exec.
//
// base64 preserves the injection safety the stdin version had: its alphabet
// carries no shell metacharacters, and every chunk is a quoted positional
// argument rather than text spliced into the script. It does not preserve
// confidentiality — argv is world-readable through /proc/<pid>/cmdline, so the
// content is exposed to other processes on the host for the life of the exec.
// Callers writing secrets should not rely on this backend.
//
// Payloads larger than Config.WriteChunkBytes are split across several execs,
// and each exec's payload across several arguments under maxArgPayload. The
// first exec truncates, so retrying after a partial failure cannot append to
// stale content.
func (s *ContainerdSandbox) WriteFile(ctx context.Context, path string, data []byte, perm fs.FileMode) error {
	if err := s.requireBackend(ctx); err != nil {
		return err
	}
	path = s.resolve(path)
	mode := strconv.FormatUint(uint64(perm.Perm()), 8)

	chunk := s.cfg.WriteChunkBytes
	if chunk <= 0 {
		chunk = defaultWriteChunkBytes
	}

	// Chunks arrive as positional arguments after $3 and are concatenated by
	// printf before a single decode, so a chunk may be split at any byte
	// boundary. `set -e` makes a failed mkdir, decode, or chmod fail the exec
	// instead of leaving a half-written file behind an exit 0.
	const script = `set -e
p=$1; perm=$2; first=$3
shift 3
if [ "$first" = 1 ]; then
  mkdir -p -- "$(dirname -- "$p")"
  : > "$p"
fi
printf '%s' "$@" | base64 -d >> "$p"
chmod "$perm" "$p"`

	for off := 0; ; off += chunk {
		end := min(off+chunk, len(data))
		args := []string{path, mode, "0"}
		if off == 0 {
			args[2] = "1"
		}
		args = append(args, base64Args(data[off:end])...)
		if _, err := s.execShell(ctx, nil, script, args...); err != nil {
			return fmt.Errorf("containerd sandbox: write %s: %w", path, err)
		}
		if end == len(data) {
			return nil
		}
	}
}

// base64Args encodes data and splits the result into argv-sized pieces. The
// receiver concatenates the pieces before decoding, so split points need not
// respect base64 alignment. An empty payload yields no arguments, and the script
// then appends nothing.
func base64Args(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	enc := base64.StdEncoding.EncodeToString(data)
	var args []string
	for len(enc) > maxArgPayload {
		args = append(args, enc[:maxArgPayload])
		enc = enc[maxArgPayload:]
	}
	return append(args, enc)
}

// ForkState commits this sandbox's active layer and prepares a successor, so the
// committed layer can serve as the parent of a forked child.
//
// This is Capture without the state bookkeeping, exposed because a fork needs the
// same "commit then continue on a fresh layer" sequence.
func (s *ContainerdSandbox) ForkState(ctx context.Context) (shepherd.WorkspaceState, error) {
	return s.Capture(ctx)
}

// parentKeyLocked returns the newest committed layer, or the image rootfs when
// this sandbox has not committed anything yet. Caller holds s.mu.
func (s *ContainerdSandbox) parentKeyLocked() string {
	if n := len(s.committedChain); n > 0 {
		return s.committedChain[n-1]
	}
	return s.rootfsKey
}

// baseGitHeadLocked returns the recorded delta base. Caller holds s.mu.
func (s *ContainerdSandbox) baseGitHeadLocked() string { return s.baseGitHead }

// restart starts a task on an existing snapshot, used to recover when a capture
// fails midway with the task already stopped.
func (s *ContainerdSandbox) restart(ctx context.Context, id, activeKey string) error {
	return s.tasks.StartTask(ctx, id, activeKey)
}

// gitHead returns the workspace's git HEAD, or "" when it is not a repository.
func (s *ContainerdSandbox) gitHead(ctx context.Context, id string) (string, error) {
	out, err := s.run(ctx, id, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// run executes a command with the workspace as its working directory.
func (s *ContainerdSandbox) run(ctx context.Context, id, command string, args ...string) (string, error) {
	res, err := s.tasks.Exec(ctx, id, shepherd.ExecRequest{
		Command: command,
		Args:    args,
		Cwd:     s.cfg.workdir(),
	})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return res.Stdout, fmt.Errorf("%s %s: exit %d: %s",
			command, strings.Join(args, " "), res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, nil
}

// shell runs a POSIX shell script with positional arguments.
func (s *ContainerdSandbox) shell(ctx context.Context, script string, args ...string) (shepherd.ExecResult, error) {
	return s.execShell(ctx, nil, script, args...)
}

// execShell runs a shell script with optional stdin and positional arguments.
func (s *ContainerdSandbox) execShell(ctx context.Context, stdin []byte, script string, args ...string) (shepherd.ExecResult, error) {
	s.mu.Lock()
	id := s.id
	s.mu.Unlock()
	if id == "" {
		return shepherd.ExecResult{}, fmt.Errorf("containerd sandbox: sandbox not created")
	}
	// `sh -c script name arg1 arg2...`: $0 is a placeholder so the caller's
	// arguments start at $1.
	shArgs := append([]string{"-c", script, "shepherd"}, args...)
	res, err := s.tasks.Exec(ctx, id, shepherd.ExecRequest{
		Command: "sh",
		Args:    shArgs,
		Cwd:     s.cfg.workdir(),
		Stdin:   stdin,
	})
	if err != nil {
		return res, err
	}
	if res.ExitCode != 0 {
		return res, fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res, nil
}

// resolve makes a relative workspace path absolute against the in-container
// workdir.
func (s *ContainerdSandbox) resolve(path string) string {
	if strings.HasPrefix(path, "/") {
		return path
	}
	return strings.TrimSuffix(s.cfg.workdir(), "/") + "/" + strings.TrimPrefix(path, "./")
}

// requireBackend ensures the containerd surface is ready, connecting lazily only
// when no backend was injected. Injected backends (NewWithBackend) are used
// as-is: connecting over them would discard a caller's client.
func (s *ContainerdSandbox) requireBackend(ctx context.Context) error {
	if s.backendReady() {
		return nil
	}
	s.connectOnce.Do(func() { s.connectErr = s.connect(ctx) })
	if s.connectErr != nil {
		return s.connectErr
	}
	if !s.backendReady() {
		return ErrNotImplemented
	}
	return nil
}

// backendReady reports whether all three collaborators are present.
func (s *ContainerdSandbox) backendReady() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snap != nil && s.tasks != nil && s.images != nil
}

// key builds a snapshot key for this sandbox. Keys are namespaced by sandbox id
// and ordered by seq so the lineage is readable in `ctr snapshot ls`.
func (s *ContainerdSandbox) key(id, kind string, n uint64) string {
	return fmt.Sprintf("shepherd/%s/%s/%d", id, kind, n)
}

// newSandboxID returns a short random identifier. Identity comes from crypto/rand
// rather than a clock: containerd ids must be unique across hosts and restarts,
// and two sandboxes created in the same nanosecond would collide.
func newSandboxID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// An id is required to proceed and there is no meaningful fallback that
		// preserves uniqueness.
		panic("containerd sandbox: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(b[:])
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
