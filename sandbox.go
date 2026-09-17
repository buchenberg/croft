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
//	Capture  stop the task, Commit the active snapshot, Prepare a fresh active
//	         snapshot on top of it, restart the task
//	Apply    Prepare a new active snapshot from a committed key, rebind, restart
//	Fork     Commit the parent's active snapshot, then Prepare the child with
//	         that committed snapshot as its parent (see ForkState)
//	Destroy  stop the task, then Remove the layers this sandbox created
//
// Apply is single-use per state: unlike the git backend, resuming from a
// committed snapshot rebinds this sandbox's identity rather than replaying into
// an existing tree. Callers must not assume the git backend's reusability.
package containerd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"
	"sync"

	shepherd "github.com/buchenberg/shepherd-kernel-go"
	"github.com/containerd/containerd/v2/core/snapshots"
)

// BackendName is the identifier reported by Backend and stored in
// WorkspaceState.Backend.
const BackendName = "containerd"

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
type ContainerdSandbox struct {
	cfg    Config
	snap   snapshots.Snapshotter
	tasks  taskService
	images imageService

	// connectOnce guards lazy connection to the containerd daemon so New does
	// no I/O and no error is silently dropped.
	connectOnce sync.Once
	connectErr  error

	mu sync.Mutex
	// id is the container/task identity. Apply rebinds it, so it is not stable
	// for the instance's lifetime.
	id string
	// rootfsKey is the image rootfs snapshot. It is owned by the image, not by
	// this sandbox, and is never removed here.
	rootfsKey string
	// activeKey is the writable snapshot the running task is rooted at.
	activeKey string
	// committedChain lists the committed layers this sandbox created, oldest
	// first. Teardown must run in reverse: a layer with children cannot be
	// removed until they are gone.
	committedChain []string
	// seq orders snapshot keys within this sandbox.
	seq uint64
}

// The compile-time assertion is load-bearing: if shepherd.Sandbox gains or
// changes a method, this package stops building until the adapter matches.
var _ shepherd.Sandbox = (*ContainerdSandbox)(nil)

// New returns a sandbox bound to cfg, backed by a real containerd client. The
// client is connected lazily on first use.
func New(cfg Config) *ContainerdSandbox {
	return &ContainerdSandbox{cfg: cfg}
}

// NewWithBackend returns a sandbox backed by the supplied implementations.
// Exported for tests and for embedding the sandbox in a host that already holds
// a containerd client.
func NewWithBackend(cfg Config, snap snapshots.Snapshotter, tasks taskService, images imageService) *ContainerdSandbox {
	return &ContainerdSandbox{cfg: cfg, snap: snap, tasks: tasks, images: images}
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
	if err := s.requireBackend(ctx); err != nil {
		return err
	}
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
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

	s.mu.Lock()
	s.id = id
	s.rootfsKey = rootfsKey
	s.activeKey = active
	s.committedChain = nil
	s.seq = 0
	s.mu.Unlock()

	return nil
}

// Destroy stops the task and removes the layers this sandbox created.
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
	chain := s.committedChain
	rootfs := s.rootfsKey
	s.id, s.activeKey, s.committedChain, s.rootfsKey = "", "", nil, ""
	s.mu.Unlock()

	var errs []error
	if id != "" {
		if err := s.tasks.StopTask(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("stop task %s: %w", id, err))
		}
	}

	// Children before parents: the active layer depends on the newest committed
	// layer, which depends on the next oldest, and so on.
	removals := make([]string, 0, len(chain)+1)
	removals = append(removals, active)
	for i := len(chain) - 1; i >= 0; i-- {
		removals = append(removals, chain[i])
	}
	for _, key := range removals {
		if key == "" || key == rootfs {
			continue
		}
		if err := s.snap.Remove(ctx, key); err != nil {
			errs = append(errs, fmt.Errorf("remove snapshot %s: %w", key, err))
		}
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
	if id == "" {
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

	next := s.key(id, "active", n)
	if _, err := s.snap.Prepare(ctx, next, committed); err != nil {
		return shepherd.WorkspaceState{}, fmt.Errorf("containerd sandbox: prepare successor snapshot: %w", err)
	}
	if err := s.tasks.StartTask(ctx, id, next); err != nil {
		return shepherd.WorkspaceState{}, fmt.Errorf("containerd sandbox: restart task after capture: %w", err)
	}

	s.mu.Lock()
	s.activeKey = next
	s.committedChain = append(s.committedChain, committed)
	s.mu.Unlock()

	state := snapshotState(s.cfg.Image, committed, parent)
	// Record the in-container git HEAD so Diff has a baseline. Absence is not an
	// error: a non-git workspace simply cannot be diffed semantically.
	if head, err := s.gitHead(ctx, id); err == nil && head != "" {
		state.Data[StateKeyGitHead] = head
	}
	return state, nil
}

// Apply resumes the workspace from a previously captured state by preparing a new
// active snapshot from the committed key and rebinding this sandbox.
//
// Layers created after the applied state are orphaned and removed, so repeated
// fork/apply cycles do not accumulate disk. Single-use per state: the sandbox now
// runs on the given committed layer, so applying a different state moves it
// again. This differs from the git backend, whose states are freely reusable.
func (s *ContainerdSandbox) Apply(ctx context.Context, ws shepherd.WorkspaceState) error {
	committed, err := stateSnapshotKey(ws)
	if err != nil {
		return err
	}
	if err := s.requireBackend(ctx); err != nil {
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
	rootfs := s.rootfsKey
	kept, orphaned := splitChain(s.committedChain, committed)
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
	s.mu.Unlock()

	// Best effort: a failure to clean up leaks disk, it does not invalidate the
	// state just resumed. Children before parents.
	if active != "" && active != next {
		_ = s.snap.Remove(ctx, active)
	}
	for i := len(orphaned) - 1; i >= 0; i-- {
		if orphaned[i] != rootfs {
			_ = s.snap.Remove(ctx, orphaned[i])
		}
	}
	return nil
}

// splitChain partitions a committed-layer chain at key: layers up to and
// including key are retained (they remain ancestors of a layer prepared from
// key), and the rest are orphaned. A key that is not in the chain — an image
// rootfs, or a state from another lineage — retains nothing and orphans the
// whole chain, because none of it is an ancestor of the new active layer.
func splitChain(chain []string, key string) (kept, orphaned []string) {
	for i, k := range chain {
		if k == key {
			return chain[:i+1], chain[i+1:]
		}
	}
	return nil, chain
}

// Diff returns the unified diff and changed file paths between ws and the
// current workspace, using the in-container git repository.
//
// Known gap: this stages with `git add -A` and then runs `git reset`, which
// restores the index to HEAD rather than to the state it was found in. A caller
// that had staged changes before Diff therefore loses them, which violates the
// Sandbox non-mutating contract (sandbox.go). The git backend avoids this by
// staging into a scratch GIT_INDEX_FILE (sandbox_git.go stagedIndex); the
// containerd backend needs the same treatment, tracked as a follow-up because
// the in-container path cannot be verified without a live daemon
// (plans/05-persistence-hygiene-release.md section 3).
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

	if _, err := s.run(ctx, id, "git", "add", "-A"); err != nil {
		return "", nil, fmt.Errorf("containerd sandbox: diff: git add -A: %w", err)
	}
	// Returns the index to HEAD, not to its prior state: a caller's pre-staged
	// changes are dropped. See the known-gap note on Diff.
	defer func() { _, _ = s.run(context.WithoutCancel(ctx), id, "git", "reset") }()

	namesOut, err := s.run(ctx, id, "git", "diff", "--cached", "--name-only", head)
	if err != nil {
		return "", nil, fmt.Errorf("containerd sandbox: diff: git diff --name-only: %w", err)
	}
	var files []string
	if trimmed := strings.TrimSpace(namesOut); trimmed != "" {
		files = strings.Split(trimmed, "\n")
	}

	full, err := s.run(ctx, id, "git", "diff", "--cached", head)
	if err != nil {
		return "", nil, fmt.Errorf("containerd sandbox: diff: git diff: %w", err)
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
// Like ReadFile this runs in-container; the content is piped on stdin so it is
// never embedded in the command line. perm is applied as an octal mode.
func (s *ContainerdSandbox) WriteFile(ctx context.Context, path string, data []byte, perm fs.FileMode) error {
	if err := s.requireBackend(ctx); err != nil {
		return err
	}
	path = s.resolve(path)
	script := `mkdir -p -- "$(dirname -- "$1")" && cat > "$1" && chmod "$2" "$1"`
	_, err := s.execShell(ctx, data, script, path, strconv.FormatUint(uint64(perm.Perm()), 8))
	if err != nil {
		return fmt.Errorf("containerd sandbox: write %s: %w", path, err)
	}
	return nil
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
