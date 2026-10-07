package containerd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	shepherd "github.com/buchenberg/shepherd-kernel-go"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/image-spec/identity"
	specs "github.com/opencontainers/runtime-spec/specs-go"
)

// sandboxLabel marks containers this adapter owns, so a sandbox's containers can
// be found again without in-process bookkeeping. That matters because Capture and
// Apply replace the container on every generation, and because a crashed process
// must not leave containers only it can find.
const sandboxLabel = "shepherd.sandbox"

// defaultSnapshotter is containerd's overlayfs snapshotter. Other snapshotters
// (native/btrfs/zfs, erofs) differ in commit semantics, so this is configurable
// rather than assumed.
const defaultSnapshotter = "overlayfs"

// ErrStdinUnsupported is returned by Exec when ExecRequest.Stdin is non-empty.
// Callers can test for it with errors.Is to fall back to another transport.
//
// This is a containerd constraint, not a missing feature. The runc-v2 shim's
// execProcess.openStdin opens the exec's stdin FIFO O_WRONLY and keeps it in
// e.closers for the process's whole lifetime, so the FIFO always has a writer
// and an in-container reader never observes EOF. Failing immediately beats the
// alternative: a process blocked on stdin does not error, it hangs until the
// context expires, which for a caller-supplied timeout can be minutes.
var ErrStdinUnsupported = errors.New("containerd sandbox: exec stdin is unsupported")

// execSeq numbers exec IDs within the process.
var execSeq atomic.Uint64

// namespaceOrDefault resolves the namespace this backend operates in.
func namespaceOrDefault(cfg Config) string {
	if cfg.Namespace == "" {
		return "default"
	}
	return cfg.Namespace
}

// connect establishes a containerd client and wires the real backend.
//
// containerd resolves the namespace from the context, not from the constructor,
// so every snapshotter and container call is wrapped to inject
// Config.Namespace. That wrapping is what keeps the lifecycle in sandbox.go
// namespace-agnostic.
//
// The same wrapper injects the sandbox's lease, which is what stops the daemon's
// garbage collector reclaiming a snapshot mid-lifecycle. See ensureLease.
func (s *ContainerdSandbox) connect(ctx context.Context) error {
	c, err := containerd.New(s.cfg.Address)
	if err != nil {
		return fmt.Errorf("containerd sandbox: connect to daemon: %w", err)
	}

	ns := namespaceOrDefault(s.cfg)
	snapshotterName := s.cfg.Snapshotter
	if snapshotterName == "" {
		snapshotterName = defaultSnapshotter
	}

	images := &containerdImages{client: c, ns: ns, snapshotter: snapshotterName}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap = namespaceSnapshotter{
		Snapshotter: c.SnapshotService(snapshotterName),
		ns:          ns,
		lease:       &s.lease,
	}
	s.leases = c.LeasesService()
	s.images = images
	s.tasks = &containerdTasks{client: c, ns: ns, cfg: s.cfg, images: images}
	_ = ctx
	return nil
}

// ensureLease creates the sandbox's containerd lease if it does not hold one.
//
// A lease is what keeps this sandbox's snapshots alive. Without one, a snapshot
// is referenced only by the container currently rooted at it — and Capture stops
// that container before preparing its successor, so in that window the
// just-committed layer is unreferenced and containerd's GC may reclaim it:
//
//	prepare successor snapshot: parent snapshot shepherd/<id>/committed/1 does
//	not exist
//
// That was observed on 3 of 5 consecutive live runs, a different test failing
// each time. Registering the sandbox's snapshots under a lease makes them GC
// roots for as long as the lease exists, which is the sandbox's own lifetime.
//
// containerd associates a snapshot with a lease only when the lease id is in the
// context of the call that creates it, which is why the id lives behind a
// pointer the snapshotter wrapper reads (see namespaceSnapshotter.ctx) rather
// than being captured once here.
//
// A nil manager means the collaborators were substituted (tests). There is no
// daemon and no GC, so there is nothing to lease and no error.
func (s *ContainerdSandbox) ensureLease(ctx context.Context) error {
	if s.leases == nil || s.lease.get() != "" {
		return nil
	}
	lctx := namespaces.WithNamespace(ctx, namespaceOrDefault(s.cfg))
	l, err := s.leases.Create(lctx,
		leases.WithID("shepherd-"+newSandboxID()),
		leases.WithLabels(map[string]string{sandboxLeaseLabel: "sandbox"}),
	)
	if err != nil {
		return fmt.Errorf("containerd sandbox: create lease: %w", err)
	}
	s.lease.set(l.ID)
	return nil
}

// releaseLease deletes the sandbox's lease, letting containerd reclaim whatever
// it was holding.
//
// Idempotent: an already-deleted lease is not an error, so Destroy stays safe to
// retry. The id is cleared before the delete so no snapshot call in flight
// re-registers under a lease that is going away — addSnapshotLease fails the
// whole Prepare/Commit if the lease named in the context does not exist.
func (s *ContainerdSandbox) releaseLease(ctx context.Context) error {
	id := s.lease.get()
	s.lease.set("")
	if id == "" || s.leases == nil {
		return nil
	}
	lctx := namespaces.WithNamespace(ctx, namespaceOrDefault(s.cfg))
	if err := s.leases.Delete(lctx, leases.Lease{ID: id}); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("containerd sandbox: delete lease %s: %w", id, err)
	}
	return nil
}

// namespaceSnapshotter injects the containerd namespace — and the sandbox's
// lease, when it holds one — into every call the sandbox makes. Only the methods
// sandbox.go uses are overridden; the rest are inherited from the embedded
// interface.
type namespaceSnapshotter struct {
	snapshots.Snapshotter
	ns string
	// lease is read per call rather than captured, because the sandbox acquires
	// its lease after this wrapper is built and drops it on Destroy.
	lease *leaseRef
}

func (n namespaceSnapshotter) ctx(ctx context.Context) context.Context {
	ctx = namespaces.WithNamespace(ctx, n.ns)
	if n.lease != nil {
		if id := n.lease.get(); id != "" {
			ctx = leases.WithLease(ctx, id)
		}
	}
	return ctx
}

func (n namespaceSnapshotter) Stat(ctx context.Context, key string) (snapshots.Info, error) {
	return n.Snapshotter.Stat(n.ctx(ctx), key)
}

func (n namespaceSnapshotter) Prepare(ctx context.Context, key, parent string, opts ...snapshots.Opt) ([]mount.Mount, error) {
	return n.Snapshotter.Prepare(n.ctx(ctx), key, parent, opts...)
}

func (n namespaceSnapshotter) Commit(ctx context.Context, name, key string, opts ...snapshots.Opt) error {
	return n.Snapshotter.Commit(n.ctx(ctx), name, key, opts...)
}

func (n namespaceSnapshotter) Remove(ctx context.Context, key string) error {
	return n.Snapshotter.Remove(n.ctx(ctx), key)
}

func (n namespaceSnapshotter) Mounts(ctx context.Context, key string) ([]mount.Mount, error) {
	return n.Snapshotter.Mounts(n.ctx(ctx), key)
}

// containerdImages resolves an image reference to the snapshot key holding its
// unpacked rootfs.
//
// containerd's unpacker commits each layer under its chainID — see
// core/unpack/unpacker.go, which computes identity.ChainIDs(diffIDs) and
// commits layer i as chainIDs[i].String(). The top layer's chainID is therefore
// the parent key for a container's first writable snapshot.
//
// The image's target digest is NOT that key: it identifies the manifest, which
// the snapshotter has never heard of. Passing it to Prepare fails with "parent
// snapshot sha256:... does not exist". This was found by the live daemon test;
// the fakes seed their rootfs key directly and so cannot catch it.
type containerdImages struct {
	client      *containerd.Client
	ns          string
	snapshotter string
}

func (i *containerdImages) ctx(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, i.ns)
}

func (i *containerdImages) RootfsSnapshot(ctx context.Context, ref string) (string, error) {
	img, err := i.image(ctx, ref)
	if err != nil {
		return "", err
	}
	diffIDs, err := img.RootFS(ctx)
	if err != nil {
		return "", fmt.Errorf("containerd sandbox: image %s: read rootfs diff ids: %w", ref, err)
	}
	if len(diffIDs) == 0 {
		return "", fmt.Errorf("containerd sandbox: image %s has no layers to derive a rootfs snapshot from", ref)
	}
	return identity.ChainID(diffIDs).String(), nil
}

// image returns the image, pulling and unpacking it when it is not present
// locally.
func (i *containerdImages) image(ctx context.Context, ref string) (containerd.Image, error) {
	if img, err := i.client.GetImage(i.ctx(ctx), ref); err == nil {
		// An image can be present but not unpacked (no snapshots), which would
		// otherwise surface as a confusing not-found from Prepare.
		if unpacked, err := img.IsUnpacked(i.ctx(ctx), i.snapshotter); err == nil && unpacked {
			return img, nil
		}
	}
	img, err := i.client.Pull(i.ctx(ctx), ref, containerd.WithPullUnpack)
	if err != nil {
		return nil, fmt.Errorf("containerd sandbox: pull %s: %w", ref, err)
	}
	return img, nil
}

// containerdTasks manages one sandbox's containers and commands.
//
// Each StartTask creates a *new* container generation, because a container's
// rootfs snapshot is fixed at creation and Capture/Apply change it. Previous
// generations are found through the sandbox label rather than an in-process map,
// so a restarted process can still clean them up.
type containerdTasks struct {
	client *containerd.Client
	ns     string
	cfg    Config
	images *containerdImages
}

func (t *containerdTasks) ctx(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, t.ns)
}

// generationID derives a containerd container id from a sandbox id and its
// snapshot key. containerd ids may not contain '/', so the key's separators are
// flattened.
func generationID(sandboxID, snapshotKey string) string {
	flat := strings.NewReplacer("/", "_", ":", "_", "@", "_").Replace(snapshotKey)
	return sandboxID + "-" + flat
}

// initArgs is the long-lived process a container runs so it stays up between
// commands; Exec provides the actual work.
func (t *containerdTasks) initArgs() []string {
	if len(t.cfg.InitArgs) > 0 {
		return t.cfg.InitArgs
	}
	return []string{"/bin/sh", "-c", "sleep infinity"}
}

// StartTask creates and starts a container generation rooted at snapshotKey.
func (t *containerdTasks) StartTask(ctx context.Context, id, snapshotKey string) error {
	ctx = t.ctx(ctx)

	img, err := t.images.image(ctx, t.cfg.Image)
	if err != nil {
		return err
	}

	containerID := generationID(id, snapshotKey)
	opts := []containerd.NewContainerOpts{
		containerd.WithSnapshot(snapshotKey),
		containerd.WithImage(img),
		containerd.WithContainerLabels(map[string]string{sandboxLabel: id}),
		containerd.WithNewSpec(
			oci.WithImageConfig(img),
			oci.WithProcessArgs(t.initArgs()...),
		),
	}
	if t.cfg.Runtime != "" {
		opts = append(opts, containerd.WithRuntime(t.cfg.Runtime, nil))
	}

	ctr, err := t.client.NewContainer(ctx, containerID, opts...)
	if err != nil {
		return fmt.Errorf("containerd sandbox: create container %s: %w", containerID, err)
	}
	// The init process's stdio is never consumed — Exec carries the real I/O.
	// cio.WithStdio would be actively wrong here: it ties the container's stdin
	// to the *client's* os.Stdin and starts a copier, so an interactive host
	// would have its own input consumed by a background container. Discard both
	// directions instead. A reader is still supplied for stdin because copyIO
	// passes it to io.CopyBuffer without a nil check.
	task, err := ctr.NewTask(ctx, cio.NewCreator(
		cio.WithFIFODir(t.cfg.FIFODir),
		cio.WithStreams(bytes.NewReader(nil), io.Discard, io.Discard),
	))
	if err != nil {
		// The container is useless without a task; drop it. The snapshot is left
		// alone — the sandbox owns snapshot lifetime.
		_ = ctr.Delete(ctx)
		return fmt.Errorf("containerd sandbox: create task for %s: %w", containerID, err)
	}
	if err := task.Start(ctx); err != nil {
		_, _ = task.Delete(ctx, containerd.WithProcessKill)
		_ = ctr.Delete(ctx)
		return fmt.Errorf("containerd sandbox: start task for %s: %w", containerID, err)
	}
	return nil
}

// StopTask stops and deletes every container generation belonging to id.
//
// Idempotent: a sandbox with no containers is not an error, because Destroy and
// Capture both call this and either may run first. The init process is
// `sleep infinity` and does not handle SIGTERM, so it is killed rather than
// politely stopped.
func (t *containerdTasks) StopTask(ctx context.Context, id string) error {
	ctx = t.ctx(ctx)

	ctrs, err := t.sandboxContainers(ctx, id)
	if err != nil {
		return err
	}
	var errs []error
	for _, ctr := range ctrs {
		if task, err := ctr.Task(ctx, nil); err == nil {
			// WithKillAll is required, not redundant. A plain Kill signals only the
			// init process; once init is gone, task.Delete's own WithProcessKill
			// short-circuits on Pid() == 0 (see its containerd#10441 guard) and
			// never signals the exec'd processes. Killing init alone therefore
			// orphans every in-flight exec — observed live as `cat` processes and
			// their shims outliving Destroy after a timed-out WriteFile.
			if err := task.Kill(ctx, syscall.SIGKILL, containerd.WithKillAll); err != nil {
				errs = append(errs, fmt.Errorf("kill task %s: %w", ctr.ID(), err))
			}
			if _, err := task.Delete(ctx, containerd.WithProcessKill); err != nil {
				errs = append(errs, fmt.Errorf("delete task %s: %w", ctr.ID(), err))
			}
		}
		// The snapshot is deliberately left in place: it is the layer Capture
		// just committed, or the state Apply is resuming.
		if err := ctr.Delete(ctx); err != nil {
			errs = append(errs, fmt.Errorf("delete container %s: %w", ctr.ID(), err))
		}
	}
	return errors.Join(errs...)
}

// Running reports whether any container generation for id has a live task.
func (t *containerdTasks) Running(ctx context.Context, id string) (bool, error) {
	ctx = t.ctx(ctx)

	ctrs, err := t.sandboxContainers(ctx, id)
	if err != nil {
		return false, err
	}
	for _, ctr := range ctrs {
		if _, err := ctr.Task(ctx, nil); err == nil {
			return true, nil
		}
	}
	return false, nil
}

// Exec runs a command in the sandbox's current container generation.
func (t *containerdTasks) Exec(ctx context.Context, id string, req shepherd.ExecRequest) (shepherd.ExecResult, error) {
	// Rejected before any RPC: the process would start, block on a stdin that can
	// never reach EOF, and cost the caller its whole context budget to find out.
	if len(req.Stdin) > 0 {
		return shepherd.ExecResult{}, fmt.Errorf("%w: %s would block until the context "+
			"expires because the runc-v2 shim keeps a write end on the exec's stdin FIFO; "+
			"pass data through arguments instead, as WriteFile does",
			ErrStdinUnsupported, req.Command)
	}

	ctx = t.ctx(ctx)
	start := time.Now()

	task, err := t.currentTask(ctx, id)
	if err != nil {
		return shepherd.ExecResult{}, err
	}

	var stdout, stderr bytes.Buffer
	spec := &specs.Process{
		Args: append([]string{req.Command}, req.Args...),
		Cwd:  req.Cwd,
		Env:  envSlice(req.Env),
	}
	if len(spec.Env) == 0 {
		spec.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	}

	execID := fmt.Sprintf("exec-%d", execSeq.Add(1))

	// req.Stdin is rejected at the top of this method (see ErrStdinUnsupported),
	// so stdin is always empty here. A reader is still supplied because copyIO
	// passes it to io.CopyBuffer without a nil check and a nil interface reader
	// panics; an empty bytes.Reader reaches EOF at once.
	//
	// FIFODir comes from Config: containerd's default is root-owned, and the
	// client creates these FIFOs itself, so an unprivileged caller needs a
	// writable directory of its own.
	ioCreator := cio.NewCreator(
		cio.WithFIFODir(t.cfg.FIFODir),
		cio.WithStreams(bytes.NewReader(nil), &stdout, &stderr),
	)
	proc, err := task.Exec(ctx, execID, spec, ioCreator)
	if err != nil {
		return shepherd.ExecResult{}, fmt.Errorf("containerd sandbox: exec %s: %w", req.Command, err)
	}

	// Wait returns a channel that receives exactly one status. Prefer the
	// caller's cancellation over blocking forever on a wedged process.
	statusCh, err := proc.Wait(ctx)
	if err != nil {
		_, _ = proc.Delete(context.WithoutCancel(ctx))
		return shepherd.ExecResult{}, fmt.Errorf("containerd sandbox: wait for %s: %w", req.Command, err)
	}

	// task.Exec only *creates* the process in the shim; Start is what runs it.
	// Wait is registered first so a command that exits immediately cannot miss
	// its own exit event.
	//
	// Omitting Start does not fail loudly: the process sits created, the shim
	// never opens its end of the stdout/stderr FIFOs, the client's read-side
	// opens block in openat forever, and the Wait channel never receives — so
	// every Exec hangs until the context expires. StartTask calls task.Start,
	// which is why Create works while Exec does not.
	if err := proc.Start(ctx); err != nil {
		_, _ = proc.Delete(context.WithoutCancel(ctx))
		return shepherd.ExecResult{}, fmt.Errorf("containerd sandbox: start %s: %w", req.Command, err)
	}

	var status containerd.ExitStatus
	select {
	case status = <-statusCh:
	case <-ctx.Done():
		_ = proc.Kill(context.WithoutCancel(ctx), syscall.SIGKILL)
		_, _ = proc.Delete(context.WithoutCancel(ctx))
		return shepherd.ExecResult{}, fmt.Errorf("containerd sandbox: exec %s: %w", req.Command, ctx.Err())
	}

	// A cancelled context makes both branches ready at once: process.Wait
	// delivers ExitStatus{code: UnknownExitStatus, err: <rpc error>} when its own
	// Wait RPC is cancelled, so the select above can take the status branch on a
	// timeout. status.Error() is the only way to tell the two apart. Without this
	// a deadline reports as a genuine exit 255 with a nil error, and callers
	// cannot distinguish "the command failed" from "we gave up waiting" — which
	// is how the stdin hang presented itself.
	if statusErr := status.Error(); statusErr != nil {
		_ = proc.Kill(context.WithoutCancel(ctx), syscall.SIGKILL)
		_, _ = proc.Delete(context.WithoutCancel(ctx))
		// Prefer the context's own error: "context deadline exceeded" says more
		// than the transport-level cancellation wrapped inside the status.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return shepherd.ExecResult{}, fmt.Errorf("containerd sandbox: exec %s: %w", req.Command, ctxErr)
		}
		return shepherd.ExecResult{}, fmt.Errorf("containerd sandbox: exec %s: %w", req.Command, statusErr)
	}

	// Always release the process, even when the command failed.
	if _, delErr := proc.Delete(context.WithoutCancel(ctx)); delErr != nil {
		return shepherd.ExecResult{}, fmt.Errorf("containerd sandbox: release exec %s: %w", req.Command, delErr)
	}

	return shepherd.ExecResult{
		ExitCode: int(status.ExitCode()),
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
		Duration: time.Since(start),
	}, nil
}

// sandboxContainers lists the container generations this adapter owns for a
// sandbox id.
func (t *containerdTasks) sandboxContainers(ctx context.Context, id string) ([]containerd.Container, error) {
	filter := fmt.Sprintf("labels.%q==%q", sandboxLabel, id)
	ctrs, err := t.client.Containers(ctx, filter)
	if err != nil {
		return nil, fmt.Errorf("containerd sandbox: list containers for %s: %w", id, err)
	}
	return ctrs, nil
}

// currentTask returns the live task for a sandbox id. At most one generation is
// expected to be running, because StartTask is always preceded by StopTask.
func (t *containerdTasks) currentTask(ctx context.Context, id string) (containerd.Task, error) {
	ctrs, err := t.sandboxContainers(ctx, id)
	if err != nil {
		return nil, err
	}
	for _, ctr := range ctrs {
		if task, err := ctr.Task(ctx, nil); err == nil {
			return task, nil
		}
	}
	return nil, fmt.Errorf("containerd sandbox: no running task for %s", id)
}

// envSlice renders an environment map. Order is unspecified; callers that need
// determinism should pass a single PATH entry as this adapter does by default.
func envSlice(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}
