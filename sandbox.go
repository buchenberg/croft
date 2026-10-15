package croft

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

	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// sandboxLeaseLabel marks leases this adapter owns.
//
// A lease is the only thing keeping a sandbox's snapshots alive between
// generations, so an orphaned one — left by a process that died before Destroy —
// has to be findable without in-process bookkeeping. `ctr leases ls` filters on
// labels, and the snapshot keys the lease holds carry the sandbox id.
const sandboxLeaseLabel = "croft.lease"

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

// ErrNotImplemented is returned when the containerd surface could not be
// connected — the operation is supported, the daemon connection is what
// failed, so reporting anything else would be false.
var ErrNotImplemented = errors.New("containerd sandbox: not implemented")

// ErrStateUnavailable is returned when a WorkspaceState's snapshot layer has
// been released: the state's pin is gone and the daemon's garbage collector
// may reclaim (or already has reclaimed) the layer. Retrying with a fresh
// Capture is the only recovery.
var ErrStateUnavailable = errors.New("croft: state is no longer available")

// Config configures the containerd backend.
type Config struct {
	// Address is the containerd socket or endpoint. Empty uses containerd's
	// own default resolution.
	Address string

	// Namespace isolates this backend's containers and snapshots from other
	// containerd users on the same host.
	Namespace string

	// Image is the base image provisioned for the workspace. Required: a
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
	// The init process runs Config.InitArgs (or the inert sleep default).
	StartTask(ctx context.Context, id, snapshotKey string) error
	// StartReaderTask creates and starts a throwaway reader task. The init
	// process is ALWAYS the inert sleep default, never Config.InitArgs —
	// a configured initializer could rewrite files on the reader's writable
	// snapshot before the pre-image read, or repeat external side effects
	// during Changes/Diff/ReadFilesAt.
	StartReaderTask(ctx context.Context, id, snapshotKey string) error
	// StopTask stops and deletes task id. It must be idempotent so Destroy and
	// Capture are safe to retry.
	StopTask(ctx context.Context, id string) error
	// Exec runs a command inside task id.
	Exec(ctx context.Context, id string, req ExecRequest) (ExecResult, error)
}

// imageService resolves an image reference to the snapshotter key holding its
// unpacked rootfs, pulling and unpacking when necessary.
type imageService interface {
	RootfsSnapshot(ctx context.Context, ref string) (string, error)
}

// diffService is the daemon-side diff half of containerd this sandbox needs:
// it compares two mount sets and returns the produced change-set blob.
//
// Compare runs daemon-side — the client never mounts anything — and returns a
// descriptor for content the daemon wrote to its own content store. Read
// returns those bytes; the descriptor's digest also serves as the lease
// resource id for pinning the blob against the daemon's garbage collector
// (the "content" resource type, per the Task 0 spike).
type diffService interface {
	// Compare computes the change set between lower and upper mounts and
	// returns the descriptor of the produced blob.
	Compare(ctx context.Context, lower, upper []mount.Mount) (ocispec.Descriptor, error)

	// Read returns the bytes of a blob Compare produced. Callers pin the
	// blob for the duration (the daemon's GC may collect unpinned content).
	Read(ctx context.Context, desc ocispec.Descriptor) ([]byte, error)

	// Delete removes a blob Compare produced from the daemon's content
	// store. Best-effort: a blob already collected by the daemon's GC is
	// success.
	Delete(ctx context.Context, desc ocispec.Descriptor) error
}

// ContainerdSandbox is a workspace backed by containerd.
//
// One instance corresponds to one workspace. Safe for concurrent use:
// lifecycle operations (Create, Destroy, Capture, Apply) hold an
// exclusive op-level lock, workspace operations (Exec, ReadFile,
// WriteFile, ReleaseState) a shared one — an in-flight Exec finishes
// before a Capture can commit the layer it is writing to, by
// construction rather than by luck. opMu is the outermost lock, and where
// the inner locks nest, leaseMu precedes mu (see client.go's lease
// lifecycle); opMu guards only the public API surface, so tests may drive
// the impls directly where a lock would be noise. Waiting for
// opMu does not honor context cancellation: a hung Exec delays a
// waiting Destroy until it returns, the standard Go trade.
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

	// opMu serializes the public API: exclusive for lifecycle operations
	// (Create, Destroy, Capture, Apply), shared for workspace operations
	// (Exec, ReadFile, WriteFile, ReleaseState). See the struct doc.
	opMu sync.RWMutex

	// diff compares snapshot layers daemon-side. Nil until connect wires
	// the real adapter, and nil forever on a NewWithBackend sandbox built
	// without WithDiffService — the change-set operations report that as a
	// configuration error, the same fail-loud convention as a nil lease
	// manager.
	diff diffService
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
	// baseKey is the snapshot key of the last state this sandbox Applied —
	// the layer its working tree diverged from. Create clears it, Apply sets
	// it, Capture records it verbatim into WorkspaceState.BaseKey, and
	// ApplyDelta leaves it untouched (a merge does not move the lineage
	// base). Empty means the sandbox never Applied: a fresh lineage with no
	// merge base.
	baseKey string
	// committedChain lists the committed layers this sandbox's active layer
	// descends from, oldest first. It records fork lineage — the next Capture's
	// parent key — not teardown: committed layers are states, outlive the
	// sandbox, and are never removed by Destroy or Apply.
	committedChain []string
	// seq orders snapshot keys within this sandbox.
	seq uint64
	// diffSeq orders diff-view snapshot keys within this sandbox. Separate
	// from seq: state identity is sequence-derived, and diff views must not
	// move the capture sequence.
	diffSeq uint64
	// readSeq orders reader snapshot keys within this sandbox. Separate from
	// seq for the same reason: reader snapshots are throwaway and must not
	// move the capture sequence.
	readSeq uint64
}

// The compile-time assertion is load-bearing: if Sandbox gains or
// changes a method, this package stops building until the adapter matches.
var _ Sandbox = (*ContainerdSandbox)(nil)

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

// WithDiffService supplies the daemon-side diff collaborator for hosts
// embedding the sandbox over an existing containerd client — pass the
// sandbox's own adapter or an equivalent namespace-wrapped
// client.DiffService(). Without it, a NewWithBackend sandbox has no
// change-set operations.
func WithDiffService(ds diffService) BackendOption {
	return func(s *ContainerdSandbox) { s.diff = ds }
}

// ID returns the current container identity, or "" before Create. It changes
// across Apply, so callers must not cache it.
func (s *ContainerdSandbox) ID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

// Create provisions the base snapshot from Config.Image and starts the task.
// The caller sets its own deadline on the context; the whole operation —
// daemon connection, lease acquisition, image resolution — runs under it.
func (s *ContainerdSandbox) Create(ctx context.Context) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	return s.createImpl(ctx)
}

// createImpl is Create's body; opMu is already held (exclusive).
func (s *ContainerdSandbox) createImpl(ctx context.Context) error {
	if s.cfg.Image == "" {
		return fmt.Errorf("containerd sandbox: Config.Image is required")
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
	res, err := s.tasks.Exec(ctx, id, ExecRequest{
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
	s.baseKey = ""
	s.seq = 0
	s.mu.Unlock()

	return nil
}

// Destroy stops the task, removes the sandbox's active layer, and releases the
// sandbox lease.
//
// Committed layers survive. Every one was returned by Capture as a
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
	s.opMu.Lock()
	defer s.opMu.Unlock()
	return s.destroyImpl(ctx)
}

// destroyImpl is Destroy's body; opMu is already held (exclusive).
func (s *ContainerdSandbox) destroyImpl(ctx context.Context) error {
	if s.snap == nil || s.tasks == nil {
		return nil
	}

	s.mu.Lock()
	id := s.id
	active := s.activeKey
	rootfs := s.rootfsKey
	s.id, s.activeKey, s.committedChain, s.rootfsKey, s.baseKey = "", "", nil, "", ""
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
func (s *ContainerdSandbox) Capture(ctx context.Context) (WorkspaceState, error) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	return s.captureImpl(ctx)
}

// captureImpl is Capture's body; opMu is already held (exclusive).
func (s *ContainerdSandbox) captureImpl(ctx context.Context) (WorkspaceState, error) {
	if err := s.requireBackend(ctx); err != nil {
		return WorkspaceState{}, err
	}

	s.mu.Lock()
	id, active, parent, base := s.id, s.activeKey, s.parentKeyLocked(), s.baseKey
	// active empty with an id set is the cleanly-stopped state advance leaves
	// behind when a successor could not be started: there is no running layer
	// to commit, so this is the same "not created (anymore)" case.
	if id == "" || active == "" {
		s.mu.Unlock()
		return WorkspaceState{}, fmt.Errorf("containerd sandbox: Capture before Create")
	}
	s.seq++
	n := s.seq
	s.mu.Unlock()

	// A layer the task is still writing to must not be committed.
	if err := s.tasks.StopTask(ctx, id); err != nil {
		return WorkspaceState{}, fmt.Errorf("containerd sandbox: stop task before capture: %w", err)
	}

	committed := s.key(id, "committed", n)
	if err := s.snap.Commit(ctx, committed, active); err != nil {
		// The task is stopped; restart it on the old layer so the sandbox is
		// left usable rather than wedged.
		_ = s.restart(ctx, id, active)
		return WorkspaceState{}, fmt.Errorf("containerd sandbox: commit snapshot: %w", err)
	}

	// The state contract is that a WorkspaceState outlives the sandbox that
	// captured it, so the pin cannot be this sandbox's lease: Destroy releases
	// that one. Register the layer under the namespace's states lease instead.
	// Failure fails the Capture — the caller must not be handed a state the
	// daemon's GC may reclaim. Recovery shares the successor path with the
	// success case below (advance), on a cancel-free context since the failure
	// may itself be a cancellation.
	if err := s.pinState(ctx, committed); err != nil {
		_ = s.advance(context.WithoutCancel(ctx), id, committed, n)
		return WorkspaceState{}, fmt.Errorf("containerd sandbox: pin captured state: %w", err)
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
		return WorkspaceState{}, err
	}

	return snapshotState(s.cfg.Image, committed, parent, base), nil
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
// removed. Applying a different state moves the sandbox again: a state's
// layer becomes whatever runs next, and callers must not assume two sandboxes
// applying the same state share identity.
func (s *ContainerdSandbox) Apply(ctx context.Context, ws WorkspaceState) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	return s.applyImpl(ctx, ws)
}

// applyImpl is Apply's body; opMu is already held (exclusive).
func (s *ContainerdSandbox) applyImpl(ctx context.Context, ws WorkspaceState) error {
	if ws.SnapshotKey == "" {
		return fmt.Errorf("containerd sandbox: workspace state has no snapshot key")
	}
	committed := ws.SnapshotKey
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
		// The prepared successor leaks otherwise — Destroy removes only the
		// current activeKey — so it is removed here, cancel-free. The old
		// active layer still exists at this point (its removal happens only on
		// success below), so the task restarts on it, on the same cancel-free
		// context (StartTask may have failed *because* the caller's context
		// expired), and the sandbox is left usable rather than stopped. With
		// no prior active (the first Apply before Create) there is no task to
		// restart and the sandbox stays cleanly stopped. A removal failure is
		// surfaced — the layer would otherwise leak with no key to find it
		// by — and a restart failure is reported too, since both mean the
		// sandbox did not recover cleanly.
		cleanupCtx := context.WithoutCancel(ctx)
		startErr := fmt.Errorf("containerd sandbox: start task after apply: %w", err)
		if rerr := s.snap.Remove(cleanupCtx, next); rerr != nil {
			startErr = errors.Join(startErr, fmt.Errorf("remove prepared successor %s: %w", next, rerr))
		}
		if active != "" {
			if rerr := s.restart(cleanupCtx, id, active); rerr != nil {
				startErr = errors.Join(startErr, fmt.Errorf("restart on %s: %w", active, rerr))
			}
		}
		return startErr
	}

	// The applied state's committed layer is now an ancestor of this
	// sandbox's active layer, so it belongs in the chain — including when the
	// state came from another sandbox's lineage, where splitChain finds
	// nothing to retain and the chain would otherwise be empty, leaving the
	// next Capture to mis-record its parent as the image rootfs. When the key
	// was found in the chain, kept already ends with it; append only once.
	if n := len(kept); n == 0 || kept[n-1] != committed {
		kept = append(kept, committed)
	}

	s.mu.Lock()
	s.id = id
	s.activeKey = next
	s.committedChain = kept
	// The tree after Apply is exactly ws's layer, so ws is what the working
	// tree diverges from: the merge base for every state captured from here
	// until the next Apply.
	s.baseKey = committed
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
// This is the state-discard half of the "a WorkspaceState has no lifecycle"
// contract: the state stays valid for exactly as long as the caller
// wants it, and disk comes back when the caller says so.
//
// Idempotent: releasing an already-released state is a no-op.
//
// The pin is addressed by the releasing backend's own namespace and
// snapshotter — a WorkspaceState carries no config identity — so release
// through a backend configured the same way as the one that captured the
// state. A mismatched release reports success (the target resource is simply
// not found) while the real pin persists.
func (s *ContainerdSandbox) ReleaseState(ctx context.Context, ws WorkspaceState) error {
	s.opMu.RLock()
	defer s.opMu.RUnlock()
	return s.releaseStateImpl(ctx, ws)
}

// releaseStateImpl is ReleaseState's body; opMu is already held (shared).
func (s *ContainerdSandbox) releaseStateImpl(ctx context.Context, ws WorkspaceState) error {
	if ws.SnapshotKey == "" {
		return fmt.Errorf("containerd sandbox: workspace state has no snapshot key")
	}
	key := ws.SnapshotKey
	if err := s.requireBackend(ctx); err != nil {
		return err
	}
	return s.unpinState(ctx, key)
}

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

// Exec runs a command inside the sandbox.
func (s *ContainerdSandbox) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	s.opMu.RLock()
	defer s.opMu.RUnlock()
	return s.execImpl(ctx, req)
}

// execImpl is Exec's body; opMu is already held (shared).
func (s *ContainerdSandbox) execImpl(ctx context.Context, req ExecRequest) (ExecResult, error) {
	if err := s.requireBackend(ctx); err != nil {
		return ExecResult{}, err
	}
	s.mu.Lock()
	id := s.id
	s.mu.Unlock()
	if id == "" {
		return ExecResult{}, fmt.Errorf("containerd sandbox: Exec before Create")
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
	s.opMu.RLock()
	defer s.opMu.RUnlock()
	return s.readFileImpl(ctx, path)
}

// readFileImpl is ReadFile's body; opMu is already held (shared).
func (s *ContainerdSandbox) readFileImpl(ctx context.Context, path string) ([]byte, error) {
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
	s.opMu.RLock()
	defer s.opMu.RUnlock()
	return s.writeFileImpl(ctx, path, data, perm)
}

// writeFileImpl is WriteFile's body; opMu is already held (shared).
func (s *ContainerdSandbox) writeFileImpl(ctx context.Context, path string, data []byte, perm fs.FileMode) error {
	if err := s.requireBackend(ctx); err != nil {
		return err
	}
	path = s.resolve(path)
	// effectivePerm, not perm.Perm(): Perm() masks to 0777 and would drop
	// setuid/setgid/sticky, silently changing the file's identity. ApplyDelta
	// relies on this to preserve a state's special bits, and its merge rules
	// compare full modes — applying a merge that loses setuid would make the
	// next diff against the merged state report a change that was just
	// "applied".
	mode := strconv.FormatUint(uint64(effectivePerm(perm)), 8)

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

// parentKeyLocked returns the newest committed layer, or the image rootfs when
// this sandbox has not committed anything yet. Caller holds s.mu.
func (s *ContainerdSandbox) parentKeyLocked() string {
	if n := len(s.committedChain); n > 0 {
		return s.committedChain[n-1]
	}
	return s.rootfsKey
}

// restart starts a task on an existing snapshot, used to recover when a capture
// fails midway with the task already stopped.
func (s *ContainerdSandbox) restart(ctx context.Context, id, activeKey string) error {
	return s.tasks.StartTask(ctx, id, activeKey)
}

// shell runs a POSIX shell script with positional arguments.
func (s *ContainerdSandbox) shell(ctx context.Context, script string, args ...string) (ExecResult, error) {
	return s.execShell(ctx, nil, script, args...)
}

// execShell runs a shell script with optional stdin and positional arguments.
func (s *ContainerdSandbox) execShell(ctx context.Context, stdin []byte, script string, args ...string) (ExecResult, error) {
	s.mu.Lock()
	id := s.id
	s.mu.Unlock()
	if id == "" {
		return ExecResult{}, fmt.Errorf("containerd sandbox: sandbox not created")
	}
	// `sh -c script name arg1 arg2...`: $0 is a placeholder so the caller's
	// arguments start at $1.
	shArgs := append([]string{"-c", script, "croft"}, args...)
	res, err := s.tasks.Exec(ctx, id, ExecRequest{
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
	s.connectOnce.Do(func() { s.connectErr = s.connect() })
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
	return fmt.Sprintf("croft/%s/%s/%d", id, kind, n)
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

// snapshotState encodes a committed snapshot as a workspace state. Capture
// returns this shape and Apply consumes it. baseKey is the sandbox's current
// merge base ("" when it never Applied), recorded verbatim.
func snapshotState(image, snapshotKey, parentKey, baseKey string) WorkspaceState {
	return WorkspaceState{
		SnapshotKey: snapshotKey,
		ParentKey:   parentKey,
		BaseKey:     baseKey,
		Image:       image,
	}
}
