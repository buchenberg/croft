package containerd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	shepherd "github.com/buchenberg/shepherd-kernel-go"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
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

// execSeq numbers exec IDs within the process.
var execSeq atomic.Uint64

// connect establishes a containerd client and wires the real backend.
//
// containerd resolves the namespace from the context, not from the constructor,
// so every snapshotter and container call is wrapped to inject
// Config.Namespace. That wrapping is what keeps the lifecycle in sandbox.go
// namespace-agnostic.
//
// NOTE: this adapter is compiled but has never been executed. It requires a Linux
// host with a running containerd daemon and root or user namespaces. Verify the
// container/spec construction and the exec round-trip against a live daemon
// before relying on it.
func (s *ContainerdSandbox) connect(ctx context.Context) error {
	c, err := containerd.New(s.cfg.Address)
	if err != nil {
		return fmt.Errorf("containerd sandbox: connect to daemon: %w", err)
	}

	ns := s.cfg.Namespace
	if ns == "" {
		ns = "default"
	}
	snapshotterName := s.cfg.Snapshotter
	if snapshotterName == "" {
		snapshotterName = defaultSnapshotter
	}

	images := &containerdImages{client: c, ns: ns, snapshotter: snapshotterName}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snap = namespaceSnapshotter{Snapshotter: c.SnapshotService(snapshotterName), ns: ns}
	s.images = images
	s.tasks = &containerdTasks{client: c, ns: ns, cfg: s.cfg, images: images}
	_ = ctx
	return nil
}

// namespaceSnapshotter injects the containerd namespace into every call the
// sandbox makes. Only the methods sandbox.go uses are overridden; the rest are
// inherited from the embedded interface.
type namespaceSnapshotter struct {
	snapshots.Snapshotter
	ns string
}

func (n namespaceSnapshotter) ctx(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, n.ns)
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
// containerd unpacks an image's layers into the snapshotter keyed by the image
// target digest, so that digest is the parent key for a container's first
// writable snapshot.
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
	key := img.Target().Digest.String()
	if key == "" {
		return "", fmt.Errorf("containerd sandbox: image %s has no target digest to use as a rootfs snapshot", ref)
	}
	return key, nil
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
	task, err := ctr.NewTask(ctx, cio.NewCreator(cio.WithStdio))
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
			if err := task.Kill(ctx, syscall.SIGKILL); err != nil {
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
	ioCreator := cio.NewCreator(cio.WithStreams(bytes.NewReader(req.Stdin), &stdout, &stderr))
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
	var status containerd.ExitStatus
	select {
	case status = <-statusCh:
	case <-ctx.Done():
		_ = proc.Kill(context.WithoutCancel(ctx), syscall.SIGKILL)
		_, _ = proc.Delete(context.WithoutCancel(ctx))
		return shepherd.ExecResult{}, fmt.Errorf("containerd sandbox: exec %s: %w", req.Command, ctx.Err())
	}
	// Always release the process, even when the command failed.
	if _, delErr := proc.Delete(context.WithoutCancel(ctx)); delErr != nil && status.Error() == nil {
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
