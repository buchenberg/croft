package containerd

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	shepherd "github.com/buchenberg/shepherd-kernel-go"
	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
)

// opLog records the order of lifecycle operations. The ordering is the thing
// under test: Capture's correctness rests on stopping the task before committing
// and restarting it after, so tests assert the sequence rather than just the
// end state.
type opLog struct {
	mu  sync.Mutex
	ops []string
}

func (l *opLog) add(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ops = append(l.ops, fmt.Sprintf(format, args...))
}

func (l *opLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.ops...)
}

// fakeLayer models one overlayfs snapshot. writable mirrors the real invariant:
// an active snapshot accepts writes, a committed one does not.
type fakeLayer struct {
	kind     snapshots.Kind
	parent   string
	writable bool
	writes   int
}

// fakeSnapshotter is an in-memory stand-in for containerd's snapshotter. It
// enforces the invariants the real one does, so a test failure means the
// lifecycle is wrong rather than that the fake is permissive:
//
//   - a key cannot be prepared twice
//   - a parent must exist and be committed (an active layer cannot be a parent)
//   - only a committed layer can be committed, once
//   - a committed layer is not writable
//   - a layer with children cannot be removed
type fakeSnapshotter struct {
	log  *opLog
	mu   sync.Mutex
	ls   map[string]*fakeLayer
	next int
}

func newFakeSnapshotter(log *opLog) *fakeSnapshotter {
	return &fakeSnapshotter{log: log, ls: map[string]*fakeLayer{}}
}

// seed installs a committed layer, standing in for an image's unpacked rootfs.
func (f *fakeSnapshotter) seed(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ls[key] = &fakeLayer{kind: snapshots.KindCommitted, writable: false}
}

func (f *fakeSnapshotter) Stat(_ context.Context, key string) (snapshots.Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.ls[key]
	if !ok {
		return snapshots.Info{}, fmt.Errorf("snapshot %s: not found", key)
	}
	return snapshots.Info{Kind: l.kind, Name: key, Parent: l.parent}, nil
}

func (f *fakeSnapshotter) Update(_ context.Context, info snapshots.Info, _ ...string) (snapshots.Info, error) {
	return info, nil
}

func (f *fakeSnapshotter) Usage(_ context.Context, key string) (snapshots.Usage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.ls[key]; !ok {
		return snapshots.Usage{}, fmt.Errorf("snapshot %s: not found", key)
	}
	return snapshots.Usage{}, nil
}

func (f *fakeSnapshotter) Mounts(_ context.Context, key string) ([]mount.Mount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.ls[key]; !ok {
		return nil, fmt.Errorf("snapshot %s: not found", key)
	}
	return []mount.Mount{{Type: "overlay", Source: "overlay", Target: "/mnt/" + key}}, nil
}

func (f *fakeSnapshotter) Prepare(_ context.Context, key, parent string, _ ...snapshots.Opt) ([]mount.Mount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.ls[key]; exists {
		return nil, fmt.Errorf("prepare %s: snapshot already exists", key)
	}
	if parent != "" {
		p, ok := f.ls[parent]
		if !ok {
			return nil, fmt.Errorf("prepare %s: parent %s not found", key, parent)
		}
		if p.kind != snapshots.KindCommitted {
			// The real snapshotter cannot stack an active layer as a parent.
			return nil, fmt.Errorf("prepare %s: parent %s is not committed", key, parent)
		}
	}
	f.ls[key] = &fakeLayer{kind: snapshots.KindActive, parent: parent, writable: true}
	f.log.add("prepare:%s<-%s", key, parent)
	return []mount.Mount{{Type: "overlay", Source: "overlay", Target: "/mnt/" + key}}, nil
}

func (f *fakeSnapshotter) View(_ context.Context, key, parent string, _ ...snapshots.Opt) ([]mount.Mount, error) {
	return f.Mounts(context.Background(), parent)
}

func (f *fakeSnapshotter) Commit(_ context.Context, name, key string, _ ...snapshots.Opt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.ls[key]
	if !ok {
		return fmt.Errorf("commit %s: snapshot not found", key)
	}
	if l.kind != snapshots.KindActive {
		return fmt.Errorf("commit %s: snapshot is not active", key)
	}
	if _, exists := f.ls[name]; exists {
		return fmt.Errorf("commit %s: name already exists", name)
	}
	delete(f.ls, key)
	l.kind = snapshots.KindCommitted
	// The whole point: a committed layer stops accepting writes.
	l.writable = false
	f.ls[name] = l
	f.log.add("commit:%s->%s", key, name)
	return nil
}

func (f *fakeSnapshotter) Remove(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.ls[key]; !ok {
		return fmt.Errorf("remove %s: snapshot not found", key)
	}
	for name, l := range f.ls {
		if l.parent == key {
			return fmt.Errorf("remove %s: snapshot %s has child %s", key, key, name)
		}
	}
	delete(f.ls, key)
	f.log.add("remove:%s", key)
	return nil
}

func (f *fakeSnapshotter) Walk(ctx context.Context, fn snapshots.WalkFunc, _ ...string) error {
	f.mu.Lock()
	infos := make([]snapshots.Info, 0, len(f.ls))
	for name, l := range f.ls {
		infos = append(infos, snapshots.Info{Kind: l.kind, Name: name, Parent: l.parent})
	}
	f.mu.Unlock()
	for _, info := range infos {
		if err := fn(ctx, info); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeSnapshotter) Close() error { return nil }

// write records a write into a layer, enforcing read-only committed layers. The
// fake task service calls this on Exec, so a lifecycle that leaves a task
// running on a committed layer fails loudly.
func (f *fakeSnapshotter) write(key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	l, ok := f.ls[key]
	if !ok {
		return fmt.Errorf("write %s: snapshot not found", key)
	}
	if !l.writable {
		return fmt.Errorf("write %s: snapshot is committed and read-only", key)
	}
	l.writes++
	return nil
}

// has reports whether a layer still exists.
func (f *fakeSnapshotter) has(key string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.ls[key]
	return ok
}

// keys returns the live layer names, for teardown assertions.
func (f *fakeSnapshotter) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.ls))
	for k := range f.ls {
		out = append(out, k)
	}
	return out
}

// fakeImages resolves any reference to a seeded committed rootfs layer.
type fakeImages struct{ rootfs string }

func (f *fakeImages) RootfsSnapshot(_ context.Context, ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("image reference is empty")
	}
	return f.rootfs, nil
}

// fakeTasks models container lifecycle and commands.
//
// gitHead, when set, is returned by `git rev-parse HEAD` so Capture can record
// the workspace's git baseline.
type fakeTasks struct {
	log       *opLog
	snap      *fakeSnapshotter
	gitHead   string
	runErr    map[string]error // command -> error
	lastReq   shepherd.ExecRequest
	lastStdin []byte
	// current is the layer the running task is rooted at, "" when stopped.
	current string
	started int
	stopped int
}

func newFakeTasks(log *opLog, snap *fakeSnapshotter) *fakeTasks {
	return &fakeTasks{log: log, snap: snap, runErr: map[string]error{}}
}

func (f *fakeTasks) StartTask(_ context.Context, id, snapshotKey string) error {
	if f.current != "" {
		return fmt.Errorf("start %s: task already running on %s", id, f.current)
	}
	if !f.snap.has(snapshotKey) {
		return fmt.Errorf("start %s: snapshot %s not found", id, snapshotKey)
	}
	f.current = snapshotKey
	f.started++
	f.log.add("start:%s", snapshotKey)
	return nil
}

func (f *fakeTasks) StopTask(_ context.Context, id string) error {
	if f.current == "" {
		return nil // idempotent
	}
	f.log.add("stop:%s", f.current)
	f.current = ""
	f.stopped++
	return nil
}

func (f *fakeTasks) Running(_ context.Context, id string) (bool, error) {
	return f.isRunning(), nil
}

// isRunning reports whether a task is currently rooted anywhere. Test
// convenience, avoiding a context and error in every assertion.
func (f *fakeTasks) isRunning() bool { return f.current != "" }

func (f *fakeTasks) Exec(_ context.Context, id string, req shepherd.ExecRequest) (shepherd.ExecResult, error) {
	if f.current == "" {
		return shepherd.ExecResult{}, fmt.Errorf("exec: no running task for %s", id)
	}
	// A command writes into the layer the task is rooted at. If Capture committed
	// that layer without stopping the task, this fails.
	if err := f.snap.write(f.current); err != nil {
		return shepherd.ExecResult{}, err
	}
	f.lastReq = req
	f.lastStdin = req.Stdin

	if err, ok := f.runErr[req.Command]; ok {
		return shepherd.ExecResult{ExitCode: 1, Stderr: err.Error()}, nil
	}

	switch req.Command {
	case "git":
		if len(req.Args) >= 1 && req.Args[0] == "rev-parse" {
			if f.gitHead == "" {
				return shepherd.ExecResult{ExitCode: 128, Stderr: "fatal: not a git repository"}, nil
			}
			return shepherd.ExecResult{ExitCode: 0, Stdout: f.gitHead + "\n"}, nil
		}
		if len(req.Args) >= 1 && req.Args[0] == "diff" {
			return shepherd.ExecResult{ExitCode: 0, Stdout: "diff --git a/f b/f\n+change\n"}, nil
		}
		return shepherd.ExecResult{ExitCode: 0}, nil
	case "sh":
		return shepherd.ExecResult{ExitCode: 0, Stdout: "file-contents"}, nil
	default:
		return shepherd.ExecResult{ExitCode: 0}, nil
	}
}

// newTestSandbox wires a sandbox over the fakes with a seeded image rootfs.
func newTestSandbox(t *testing.T, gitHead string) (*ContainerdSandbox, *fakeSnapshotter, *fakeTasks, *opLog) {
	t.Helper()
	log := &opLog{}
	snap := newFakeSnapshotter(log)
	snap.seed("rootfs-image-digest")
	tasks := newFakeTasks(log, snap)
	tasks.gitHead = gitHead
	images := &fakeImages{rootfs: "rootfs-image-digest"}
	sb := NewWithBackend(Config{Image: "example/dev:latest"}, snap, tasks, images)
	return sb, snap, tasks, log
}

// joinOps is a helper for asserting op ordering in tests.
func joinOps(ops []string) string { return strings.Join(ops, " ") }
