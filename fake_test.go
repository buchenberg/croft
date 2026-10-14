package croft

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/errdefs"
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
			return nil, fmt.Errorf("prepare %s: parent %s: %w", key, parent, errdefs.ErrNotFound)
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
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.ls[key]; exists {
		return nil, fmt.Errorf("view %s: snapshot already exists", key)
	}
	p, ok := f.ls[parent]
	if !ok {
		return nil, fmt.Errorf("snapshot %s: %w", parent, errdefs.ErrNotFound)
	}
	if p.kind != snapshots.KindCommitted {
		return nil, fmt.Errorf("view %s: parent %s is not committed", key, parent)
	}
	// Writable in the fake: the write check enforces the lifecycle invariant
	// (a task on a committed layer is a bug), not view read-only-ness. The
	// reader task's cat/ls are reads; the fake does not distinguish.
	f.ls[key] = &fakeLayer{kind: snapshots.KindView, parent: parent, writable: true}
	f.log.add("view:%s<-%s", key, parent)
	return []mount.Mount{{Type: "overlay", Source: "overlay", Target: "/mnt/" + key}}, nil
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
type fakeTasks struct {
	log       *opLog
	snap      *fakeSnapshotter
	runErr    map[string]error // command -> error
	lastReq   ExecRequest
	lastStdin []byte
	// allReqs records every Exec in order, so a multi-exec operation such as a
	// chunked WriteFile can be asserted on as a sequence rather than only by its
	// last call.
	allReqs []ExecRequest
	// current maps task id → snapshot key for running tasks. A map, not a
	// single slot: the reader tasks (ReadFilesAt, readDirAt) run concurrently
	// with the main task, and StopTask(id) must stop only the named task.
	current map[string]string
	started int
	stopped int

	// catFiles maps resolved path → content for main-task `cat -- "$1"`
	// execs (ReadFile tests, and the proof test that Diff's post-images come
	// from the tar, not the workspace).
	// preFiles maps resolved path → content for reader-task `cat -- "$1"`
	// execs (pre-images in Diff and ReadFilesAt). Separate from catFiles so
	// tests can give a file different old and new content.
	// missingPaths is a set of resolved paths that do not exist in the
	// fake's filesystem model: cat reports them absent (exit 1), and the
	// type probe reports them as "x". By default every path exists as a
	// regular file.
	// lsDirs maps resolved dir → entry names for `ls -A1 -- "$1"` execs.
	// modes maps resolved path → symbolic mode string (e.g. "-rw-r--r--")
	// for `ls -ld -- "$1"` execs (mode-only change rendering in Diff).
	// readlinkTargets maps resolved path → symlink target for main-task
	// `readlink -- "$1"` execs (post-image symlink targets in Diff).
	// preReadlinkTargets maps resolved path → symlink target for reader-task
	// `readlink -- "$1"` execs (pre-image symlink targets in Diff).
	// symlinks, dirs, and specials are sets of resolved paths that drive the
	// reader type probe (readerTypeScript): symlink → "l", directory → "d",
	// special file (FIFO/socket/device) → "s", missing → "x", everything
	// else → "f". The fake also enforces the invariant that cat must never
	// run on a directory or special file (real cat fails on a directory and
	// BLOCKS in open(2) on a FIFO), so a regression that cats one fails the
	// test instead of silently succeeding.
	// These let reader-task, Changes, and Diff tests assert behavior without
	// a filesystem model.
	catFiles           map[string]string
	preFiles           map[string]string
	missingPaths       map[string]bool
	lsDirs             map[string][]string
	modes              map[string]string
	readlinkTargets    map[string]string
	preReadlinkTargets map[string]string
	symlinks           map[string]bool
	dirs               map[string]bool
	specials           map[string]bool

	// mu guards the mutable state above. The production sandbox is documented
	// safe for concurrent use, and TestCapture_ConcurrentWithApply drives it
	// from two goroutines, so the fake must honor the same contract.
	mu sync.Mutex
}

func newFakeTasks(log *opLog, snap *fakeSnapshotter) *fakeTasks {
	return &fakeTasks{
		log:                log,
		snap:               snap,
		runErr:             map[string]error{},
		current:            map[string]string{},
		catFiles:           map[string]string{},
		preFiles:           map[string]string{},
		missingPaths:       map[string]bool{},
		lsDirs:             map[string][]string{},
		modes:              map[string]string{},
		readlinkTargets:    map[string]string{},
		preReadlinkTargets: map[string]string{},
		symlinks:           map[string]bool{},
		dirs:               map[string]bool{},
		specials:           map[string]bool{},
	}
}

func (f *fakeTasks) StartTask(_ context.Context, id, snapshotKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.current[id]; exists {
		return fmt.Errorf("start %s: task already running on %s", id, f.current[id])
	}
	if !f.snap.has(snapshotKey) {
		return fmt.Errorf("start %s: snapshot %s not found", id, snapshotKey)
	}
	f.current[id] = snapshotKey
	f.started++
	f.log.add("start:%s", snapshotKey)
	return nil
}

// StartReaderTask delegates to StartTask: the fake does not model init
// commands, so inert-vs-configured makes no difference here.
func (f *fakeTasks) StartReaderTask(ctx context.Context, id, snapshotKey string) error {
	return f.StartTask(ctx, id, snapshotKey)
}

func (f *fakeTasks) StopTask(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key, exists := f.current[id]
	if !exists {
		return nil // idempotent
	}
	f.log.add("stop:%s", key)
	delete(f.current, id)
	f.stopped++
	return nil
}

// isRunning reports whether a task is currently rooted anywhere. Test
// convenience, avoiding a context and error in every assertion.
func (f *fakeTasks) isRunning() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.current) > 0
}

// snapshotFor returns the snapshot key the named task is rooted at, or ""
// when no task with that id is running.
func (f *fakeTasks) snapshotFor(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current[id]
}

// runningIDs returns the ids of all running tasks.
func (f *fakeTasks) runningIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.current))
	for id := range f.current {
		out = append(out, id)
	}
	return out
}

func (f *fakeTasks) Exec(_ context.Context, id string, req ExecRequest) (ExecResult, error) {
	f.mu.Lock()
	current, ok := f.current[id]
	if !ok {
		f.mu.Unlock()
		return ExecResult{}, fmt.Errorf("exec: no running task for %s", id)
	}
	// A command writes into the layer the task is rooted at. If Capture committed
	// that layer without stopping the task, this fails.
	if err := f.snap.write(current); err != nil {
		f.mu.Unlock()
		return ExecResult{}, err
	}
	f.lastReq = req
	f.lastStdin = req.Stdin
	f.allReqs = append(f.allReqs, req)
	runErr, hasRunErr := f.runErr[req.Command]
	// Snapshot the programmable responses under the lock. Reader tasks
	// (id contains "-reader-") serve pre-images from preFiles and see
	// missingPaths; the main task serves post-images from catFiles and
	// always sees paths as present (the current workspace is the source
	// of truth for post-images).
	arg := f.shArg(req)
	isReader := strings.Contains(id, "-reader-")
	catContent, catOK := f.catFiles[arg]
	if isReader {
		catContent, catOK = f.preFiles[arg]
	}
	pathGone := f.missingPaths[arg] && isReader
	lsListing, lsOK := f.lsDirs[arg]
	modeStr, modeOK := f.modes[arg]
	linkTarget, linkOK := f.readlinkTargets[arg]
	if isReader {
		linkTarget, linkOK = f.preReadlinkTargets[arg]
	}
	isSymlink := f.symlinks[arg]
	isDir := f.dirs[arg]
	isSpecial := f.specials[arg]
	f.mu.Unlock()

	if hasRunErr {
		return ExecResult{ExitCode: 1, Stderr: runErr.Error()}, nil
	}

	switch req.Command {
	case "sh":
		return f.shExec(req, arg, catOK, catContent, pathGone, lsOK, lsListing, modeOK, modeStr, linkOK, linkTarget, isSymlink, isDir, isSpecial), nil
	default:
		return ExecResult{ExitCode: 0}, nil
	}
}

// shArg extracts $1 from a `sh -c script croft arg` request, or "".
func (f *fakeTasks) shArg(req ExecRequest) string {
	// Args: ["-c", script, "croft", arg1, ...]
	if len(req.Args) >= 4 {
		return req.Args[3]
	}
	return ""
}

// shExec dispatches the programmable cat/ls/probe/mode/readlink responses
// and falls back to the canned "file-contents" for everything else.
func (f *fakeTasks) shExec(req ExecRequest, arg string, catOK bool, catContent string, pathGone bool, lsOK bool, lsListing []string, modeOK bool, modeStr string, linkOK bool, linkTarget string, isSymlink bool, isDir bool, isSpecial bool) ExecResult {
	if len(req.Args) < 2 {
		return ExecResult{ExitCode: 0, Stdout: "file-contents"}
	}
	switch req.Args[1] {
	case `cat -- "$1"`:
		if pathGone {
			return ExecResult{ExitCode: 1, Stderr: "cat: " + arg + ": No such file or directory"}
		}
		// Enforce the invariant the type probe exists to protect: real cat
		// fails on a directory and blocks in open(2) on a FIFO. Production
		// must never cat either — fail loudly if it does.
		if isDir {
			return ExecResult{ExitCode: 1, Stderr: "cat: " + arg + ": Is a directory"}
		}
		if isSpecial {
			return ExecResult{ExitCode: 2, Stderr: "cat: " + arg + ": cannot read special file (would block)"}
		}
		if catOK {
			return ExecResult{ExitCode: 0, Stdout: catContent}
		}
		return ExecResult{ExitCode: 0, Stdout: "file-contents"}
	case `ls -A1 -- "$1"`:
		if lsOK {
			return ExecResult{ExitCode: 0, Stdout: strings.Join(lsListing, "\n")}
		}
		return ExecResult{ExitCode: 0, Stdout: ""}
	case `ls -ld -- "$1"`:
		if pathGone {
			return ExecResult{ExitCode: 1, Stderr: "ls: cannot access " + arg + ": No such file or directory"}
		}
		if modeOK {
			return ExecResult{ExitCode: 0, Stdout: modeStr + " 1 0 0 0 Jan  1 00:00 " + arg + "\n"}
		}
		return ExecResult{ExitCode: 0, Stdout: "-rw-r--r-- 1 0 0 0 Jan  1 00:00 " + arg + "\n"}
	case readerTypeScript:
		// The reader type probe. Order matches the production script:
		// symlink wins over missing (a dangling link is still a link),
		// then directory, then special, then missing, else regular.
		switch {
		case isSymlink:
			return ExecResult{ExitCode: 0, Stdout: typeSymlink}
		case isDir:
			return ExecResult{ExitCode: 0, Stdout: typeDir}
		case isSpecial:
			return ExecResult{ExitCode: 0, Stdout: typeSpecial}
		case pathGone:
			return ExecResult{ExitCode: 0, Stdout: typeAbsent}
		default:
			return ExecResult{ExitCode: 0, Stdout: typeRegular}
		}
	case `readlink -- "$1"`:
		// readlink reads the link itself: it succeeds for any symlink —
		// including a dangling one, whose TARGET is missing — and fails
		// for a path that is not a link.
		if !isSymlink {
			return ExecResult{ExitCode: 1, Stderr: "readlink: " + arg + ": No such file or directory"}
		}
		if linkOK {
			return ExecResult{ExitCode: 0, Stdout: linkTarget + "\n"}
		}
		return ExecResult{ExitCode: 0, Stdout: "target\n"}
	default:
		return ExecResult{ExitCode: 0, Stdout: "file-contents"}
	}
}

// newTestSandbox wires a sandbox over the fakes with a seeded image rootfs.
//
// A default lease manager is installed so every test exercises the pin path
// Capture takes on a real backend; tests that care about the lease lifecycle
// overwrite sb.leases with their own fake. The nil-manager no-op behavior is
// covered by tests that clear the manager explicitly.
func newTestSandbox(t *testing.T) (*ContainerdSandbox, *fakeSnapshotter, *fakeTasks, *opLog) {
	t.Helper()
	log := &opLog{}
	snap := newFakeSnapshotter(log)
	snap.seed("rootfs-image-digest")
	tasks := newFakeTasks(log, snap)
	images := &fakeImages{rootfs: "rootfs-image-digest"}
	sb := NewWithBackend(Config{Image: "example/dev:latest"}, snap, tasks, images, WithLeases(&fakeLeases{}))
	return sb, snap, tasks, log
}

// joinOps is a helper for asserting op ordering in tests.
func joinOps(ops []string) string { return strings.Join(ops, " ") }
