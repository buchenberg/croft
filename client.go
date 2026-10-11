package containerd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	sbx "github.com/buchenberg/shepherd-kernel-go/sandbox"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/containers"
	"github.com/containerd/containerd/v2/core/content"
	"github.com/containerd/containerd/v2/core/images"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/pkg/cio"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/containerd/v2/pkg/oci"
	"github.com/containerd/errdefs"
	"github.com/opencontainers/image-spec/identity"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
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
	snapshotterName := s.snapshotterName()

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

// leaseManager returns the daemon's lease manager, or nil when the collaborators
// were substituted (tests).
//
// Lock ordering, where both are held: leaseMu then mu. Nothing takes mu and then
// calls into the lease lifecycle, so the two cannot deadlock.
func (s *ContainerdSandbox) leaseManager() leases.Manager {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.leases
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
	mgr := s.leaseManager()
	if mgr == nil {
		return nil
	}
	// The check and the create are one critical section. ContainerdSandbox is
	// documented safe for concurrent use, and without this two concurrent
	// Create/Apply calls both observe an empty reference, both create a lease,
	// and the overwritten one is never released — pinning this sandbox's
	// snapshots against the GC for good, which is the failure the lease exists to
	// prevent.
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()
	if s.lease.get() != "" {
		return nil
	}

	lctx := namespaces.WithNamespace(ctx, namespaceOrDefault(s.cfg))
	l, err := mgr.Create(lctx,
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
// retry.
func (s *ContainerdSandbox) releaseLease(ctx context.Context) error {
	s.leaseMu.Lock()
	defer s.leaseMu.Unlock()

	id := s.lease.get()
	mgr := s.leaseManager()
	if id == "" || mgr == nil {
		return nil
	}

	// Clear before the delete so no snapshot call in flight re-registers under a
	// lease that is going away: addSnapshotLease fails the whole Prepare/Commit
	// when the lease named in the context does not exist.
	s.lease.set("")

	lctx := namespaces.WithNamespace(ctx, namespaceOrDefault(s.cfg))
	if err := mgr.Delete(lctx, leases.Lease{ID: id}); err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		// Put the id back so a later Destroy retries the delete rather than
		// reporting success while the orphaned lease keeps this sandbox's
		// snapshots pinned forever. The outcome is genuinely ambiguous — the RPC
		// may have applied with the response lost — but retrying is harmless
		// either way, because a Delete that finds nothing is success above.
		s.lease.set(id)
		return fmt.Errorf("containerd sandbox: delete lease %s: %w", id, err)
	}
	return nil
}

// statesLeaseID is the deterministic id of the namespace-scoped lease that pins
// captured states. Leases live inside a containerd namespace, so one id per
// namespace is enough; being deterministic means a restarted process, or a
// different one entirely, finds the same lease without bookkeeping.
//
// The sandbox's own lease (ensureLease) dies with Destroy and would take every
// captured state with it, so states need a pin with a different lifetime. A
// resource on this lease is exactly that: the daemon's GC treats a leased
// snapshot as a root, the same mechanism the sandbox lease already relies on.
const statesLeaseID = "shepherd-states"

// snapshotterName resolves the configured snapshotter plugin name, matching
// connect's resolution so lease resources name the same plugin the sandbox
// actually uses.
func (s *ContainerdSandbox) snapshotterName() string {
	if s.cfg.Snapshotter != "" {
		return s.cfg.Snapshotter
	}
	return defaultSnapshotter
}

// stateLeaseCtx and stateResource are the one definition of how a state's pin
// is addressed: the states lease, scoped to the backend's namespace, and the
// snapshotter resource naming the layer. pinState and unpinState share them so
// the two can never drift — an unpin that constructed a slightly different
// resource would target one that does not exist, read as NotFound, and be
// treated as success while the real pin held disk forever.
func (s *ContainerdSandbox) stateLeaseCtx(ctx context.Context) context.Context {
	return namespaces.WithNamespace(ctx, namespaceOrDefault(s.cfg))
}

func (s *ContainerdSandbox) stateResource(key string) leases.Resource {
	return leases.Resource{ID: key, Type: "snapshots/" + s.snapshotterName()}
}

// pinState registers a committed snapshot under the states lease, making it a
// GC root independent of any sandbox's lifetime.
//
// containerd's resource type for snapshotter content is "snapshots/<plugin>"
// (core/metadata registers ctx-lease snapshots under exactly this type), so the
// daemon sees an explicit pin and an automatic one as the same kind of edge.
//
// A nil manager means no lease manager was configured — test fakes, or
// NewWithBackend without WithLeases. A test fake has no daemon GC and loses
// nothing, but on a real backend a missing manager silently disables state
// durability, which is why NewWithBackend flags the WithLeases option.
func (s *ContainerdSandbox) pinState(ctx context.Context, key string) error {
	mgr := s.leaseManager()
	if mgr == nil {
		return nil
	}
	lctx := s.stateLeaseCtx(ctx)
	if _, err := mgr.Create(lctx,
		leases.WithID(statesLeaseID),
		leases.WithLabels(map[string]string{sandboxLeaseLabel: "states"}),
	); err != nil && !errdefs.IsAlreadyExists(err) {
		return fmt.Errorf("containerd sandbox: ensure states lease: %w", err)
	}
	if err := mgr.AddResource(lctx, leases.Lease{ID: statesLeaseID}, s.stateResource(key)); err != nil {
		return fmt.Errorf("containerd sandbox: pin %s: %w", key, err)
	}
	return nil
}

// unpinState drops a state's pin, letting the daemon's garbage collector
// reclaim the layer once no other snapshot, container, or lease references it.
//
// A missing resource or lease is success, not an error: ReleaseState is
// documented idempotent, and a double release must not surface as a failure.
func (s *ContainerdSandbox) unpinState(ctx context.Context, key string) error {
	mgr := s.leaseManager()
	if mgr == nil {
		return nil
	}
	if err := mgr.DeleteResource(s.stateLeaseCtx(ctx), leases.Lease{ID: statesLeaseID}, s.stateResource(key)); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("containerd sandbox: unpin %s: %w", key, err)
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
	// i.ctx, not the raw ctx: RootFS reads the config blob through the
	// daemon's content store, which is namespace-scoped. On a cold daemon the
	// image object comes from Pull with an empty diffIDs cache, so this call
	// reaches the daemon — and without the namespace header it is rejected
	// with "namespace is required". On a warm daemon GetImage's IsUnpacked
	// probe (namespaced) has already populated the cache, which is why the
	// missing wrapper survived green live runs.
	diffIDs, err := img.RootFS(i.ctx(ctx))
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

// initSpecOpts derives the init process's spec settings from the image config
// blob alone.
//
// oci.WithImageConfig is not usable here: it resolves the image user's uid,
// gid and supplemental groups by reading /etc/passwd and /etc/group out of
// the unpacked rootfs in the CLIENT process (containerd 2.3+ opens the
// overlay snapshot directories directly through fsview), and the daemon
// creates every snapshot directory 0700 root:root — an unprivileged client
// therefore fails NewContainer with
//
//	create container …: open /var/lib/containerd/…/snapshots/N/fs: permission denied
//
// no matter how the daemon's socket is group-shared. The config blob, by
// contrast, is read through the daemon's content store, which socket access
// already authorizes.
//
// Kept from WithImageConfig: env, working directory, and a numeric uid[:gid]
// user. A *named* user is left unresolved (spec default: root), and a
// uid-only user leaves the gid at the spec default (0): resolving either
// needs /etc/passwd or /etc/group out of the rootfs — exactly the read
// this function exists to avoid. Supplemental groups are not resolved for
// the same reason. The sandbox contract's execs define their own
// credentials, so the image user only affects the init process sleeping
// in the background.
func (t *containerdTasks) initSpecOpts(ctx context.Context, img containerd.Image) ([]oci.SpecOpts, error) {
	ic, err := img.Config(ctx)
	if err != nil {
		return nil, fmt.Errorf("containerd sandbox: read image config descriptor: %w", err)
	}
	// The media type check is WithImageConfig's own guard: an unexpected
	// descriptor would otherwise unmarshal into a zero-value config —
	// empty env, "/" cwd, root user — instead of failing loudly.
	if !images.IsConfigType(ic.MediaType) {
		return nil, fmt.Errorf("containerd sandbox: unknown image config media type %s", ic.MediaType)
	}
	blob, err := content.ReadBlob(ctx, img.ContentStore(), ic)
	if err != nil {
		return nil, fmt.Errorf("containerd sandbox: read image config blob: %w", err)
	}
	var image v1.Image
	if err := json.Unmarshal(blob, &image); err != nil {
		return nil, fmt.Errorf("containerd sandbox: parse image config: %w", err)
	}
	cfg := image.Config

	opts := make([]oci.SpecOpts, 0, 3)
	if len(cfg.Env) > 0 {
		opts = append(opts, oci.WithEnv(cfg.Env))
	} else {
		// WithImageConfig's fallback: an image with no env still gets the
		// package's default PATH, not an empty environment.
		opts = append(opts, oci.WithDefaultPathEnv)
	}
	cwd := cfg.WorkingDir
	if cwd == "" {
		cwd = "/"
	}
	opts = append(opts, oci.WithProcessCwd(cwd))
	if cfg.User != "" {
		opts = append(opts, withNumericUser(cfg.User))
	}
	return opts, nil
}

// withNumericUser applies a numeric uid[:gid] image user directly onto the
// spec. The bound is runc's, as enforced by containerd's own WithUser: ids
// above math.MaxInt32 parse as uint32 but runc rejects them, so an
// out-of-range id fails the spec loudly rather than silently running the
// init as root. A *named* user, or a malformed pair such as "1000:staff",
// is ignored as a whole — resolving a name needs /etc/passwd out of the
// rootfs (see initSpecOpts), and half-applying a pair would give the init
// a uid with the root group. See initSpecOpts for why names are not
// resolved.
func withNumericUser(user string) oci.SpecOpts {
	const idBits = 31 // strconv bitSize for the MaxInt32 ids runc accepts
	// parseID returns the id, or nil for a non-numeric string (the named
	// user falls back to the spec default), or an error when the id is
	// syntactically numeric but out of runc's range.
	parseID := func(s, kind string) (uint64, bool, error) {
		v, err := strconv.ParseUint(s, 10, idBits)
		if err == nil {
			return v, true, nil
		}
		var numErr *strconv.NumError
		if errors.As(err, &numErr) && numErr.Err == strconv.ErrRange {
			return 0, true, fmt.Errorf("containerd sandbox: image user %q: %s %s is outside the range runc accepts", user, kind, s)
		}
		return 0, false, nil
	}
	return func(_ context.Context, _ oci.Client, _ *containers.Container, s *oci.Spec) error {
		uidStr, gidStr, hasGid := strings.Cut(user, ":")
		uid, numeric, err := parseID(uidStr, "uid")
		if err != nil || !numeric {
			return err
		}
		if !hasGid {
			s.Process.User.UID = uint32(uid)
			return nil
		}
		gid, numeric, err := parseID(gidStr, "gid")
		if err != nil || !numeric {
			return err
		}
		s.Process.User.UID = uint32(uid)
		s.Process.User.GID = uint32(gid)
		return nil
	}
}

// StartTask creates and starts a container generation rooted at snapshotKey.
func (t *containerdTasks) StartTask(ctx context.Context, id, snapshotKey string) error {
	ctx = t.ctx(ctx)

	img, err := t.images.image(ctx, t.cfg.Image)
	if err != nil {
		return err
	}

	specOpts, err := t.initSpecOpts(ctx, img)
	if err != nil {
		return err
	}
	specOpts = append(specOpts, oci.WithProcessArgs(t.initArgs()...))

	containerID := generationID(id, snapshotKey)
	opts := []containerd.NewContainerOpts{
		containerd.WithSnapshot(snapshotKey),
		containerd.WithImage(img),
		containerd.WithContainerLabels(map[string]string{sandboxLabel: id}),
		containerd.WithNewSpec(specOpts...),
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
func (t *containerdTasks) Exec(ctx context.Context, id string, req sbx.ExecRequest) (sbx.ExecResult, error) {
	// Rejected before any RPC: the process would start, block on a stdin that can
	// never reach EOF, and cost the caller its whole context budget to find out.
	if len(req.Stdin) > 0 {
		return sbx.ExecResult{}, fmt.Errorf("%w: %s would block until the context "+
			"expires because the runc-v2 shim keeps a write end on the exec's stdin FIFO; "+
			"pass data through arguments instead, as WriteFile does",
			ErrStdinUnsupported, req.Command)
	}

	ctx = t.ctx(ctx)
	start := time.Now()

	task, err := t.currentTask(ctx, id)
	if err != nil {
		return sbx.ExecResult{}, err
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
		return sbx.ExecResult{}, fmt.Errorf("containerd sandbox: exec %s: %w", req.Command, err)
	}

	// Wait returns a channel that receives exactly one status. Prefer the
	// caller's cancellation over blocking forever on a wedged process.
	statusCh, err := proc.Wait(ctx)
	if err != nil {
		_, _ = proc.Delete(context.WithoutCancel(ctx))
		return sbx.ExecResult{}, fmt.Errorf("containerd sandbox: wait for %s: %w", req.Command, err)
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
		return sbx.ExecResult{}, fmt.Errorf("containerd sandbox: start %s: %w", req.Command, err)
	}

	var status containerd.ExitStatus
	select {
	case status = <-statusCh:
	case <-ctx.Done():
		_ = proc.Kill(context.WithoutCancel(ctx), syscall.SIGKILL)
		_, _ = proc.Delete(context.WithoutCancel(ctx))
		return sbx.ExecResult{}, fmt.Errorf("containerd sandbox: exec %s: %w", req.Command, ctx.Err())
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
			return sbx.ExecResult{}, fmt.Errorf("containerd sandbox: exec %s: %w", req.Command, ctxErr)
		}
		return sbx.ExecResult{}, fmt.Errorf("containerd sandbox: exec %s: %w", req.Command, statusErr)
	}

	// Always release the process, even when the command failed.
	if _, delErr := proc.Delete(context.WithoutCancel(ctx)); delErr != nil {
		return sbx.ExecResult{}, fmt.Errorf("containerd sandbox: release exec %s: %w", req.Command, delErr)
	}

	return sbx.ExecResult{
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
