package croft

import (
	"context"
	"encoding/base64"
	"errors"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/containerd/v2/core/mount"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// opVerbs strips keys from an op log, so assertions are about operation ordering
// rather than the randomly generated snapshot keys.
func opVerbs(ops []string) []string {
	out := make([]string, 0, len(ops))
	for _, op := range ops {
		verb, _, _ := strings.Cut(op, ":")
		out = append(out, verb)
	}
	return out
}

func TestCreate_ProvisionsBaseSnapshotAndStartsTask(t *testing.T) {
	sb, snap, tasks, log := newTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sb.ID() == "" {
		t.Error("Create should assign a container id")
	}
	if !tasks.isRunning() {
		t.Error("Create should leave a running task")
	}
	// The base layer is prepared from the image rootfs, then the task starts on it.
	want := []string{"prepare", "start"}
	if got := opVerbs(log.snapshot()); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ops = %v, want %v", got, want)
	}
	if len(snap.keys()) != 2 {
		t.Errorf("layers = %v, want the rootfs plus one active layer", snap.keys())
	}
}

// TestCreate_ProvisionsWorkdir pins that Create makes the configured workdir
// exist. No image is obliged to ship /workspace, and without this every later
// operation fails with a chdir error that reads as a broken sandbox rather than
// a missing directory.
func TestCreate_ProvisionsWorkdir(t *testing.T) {
	sb, _, tasks, _ := newTestSandbox(t)
	ctx := context.Background()
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}

	var found bool
	for _, req := range tasks.allReqs {
		if req.Command != "mkdir" {
			continue
		}
		// Cwd must be "/" — the workdir is the thing being created, so the
		// command cannot run inside it.
		if req.Cwd != "/" {
			t.Errorf("mkdir cwd = %q, want / so it works before the workdir exists", req.Cwd)
		}
		if len(req.Args) == 2 && req.Args[0] == "-p" && req.Args[1] == "/workspace" {
			found = true
		}
	}
	if !found {
		t.Errorf("Create did not mkdir -p the default workdir; execs = %+v", tasks.allReqs)
	}
}

func TestCreate_ValidatesImage(t *testing.T) {
	log := &opLog{}
	snap := newFakeSnapshotter(log)
	sb := NewWithBackend(Config{}, snap, newFakeTasks(log, snap), &fakeImages{rootfs: "r"})

	err := sb.Create(context.Background())
	if err == nil {
		t.Fatal("Create without Config.Image must fail")
	}
	if !strings.Contains(err.Error(), "Image") {
		t.Errorf("error should name the missing field: %v", err)
	}
}

// TestCapture_StopsTaskBeforeCommitAndRestartsAfter is the design-critical test.
//
// containerd's Commit is a metadata rename, so a running task's mount survives
// it — but the committed layer immediately becomes a lowerdir of the next active
// layer, and OverlayFS lowerdirs must not be written. Capture must therefore stop
// the task before committing and restart it on the new layer. The fake
// snapshotter rejects writes to committed layers and the fake task service writes
// on every Exec, so skipping the stop makes this test fail.
func TestCapture_StopsTaskBeforeCommitAndRestartsAfter(t *testing.T) {
	sb, snap, tasks, log := newTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	log.mu.Lock()
	log.ops = nil // ignore Create's ops
	log.mu.Unlock()

	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	got := opVerbs(log.snapshot())
	want := []string{"stop", "commit", "prepare", "start"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("Capture ops = %v, want %v (stop must precede commit)", got, want)
	}

	// The task is running again, on a writable layer: a command still works.
	if !tasks.isRunning() {
		t.Fatal("Capture must leave a running task")
	}
	if _, err := sb.Exec(ctx, ExecRequest{Command: "true"}); err != nil {
		t.Errorf("Exec after Capture: %v", err)
	}

	// The committed layer is read-only and still present.
	key := state.SnapshotKey
	if !snap.has(key) {
		t.Errorf("committed layer %s should still exist", key)
	}
	if err := snap.write(key); err == nil {
		t.Error("the committed layer must not be writable")
	}
}

func TestCapture_RepeatedCapturesChain(t *testing.T) {
	sb, snap, _, _ := newTestSandbox(t)
	ctx := context.Background()
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}

	first, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("first Capture: %v", err)
	}
	second, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("second Capture: %v", err)
	}

	k1 := first.SnapshotKey
	k2 := second.SnapshotKey
	if k1 == k2 {
		t.Fatal("each capture must produce a distinct committed layer")
	}
	// The second capture's parent is the first committed layer, forming a chain.
	if second.ParentKey != k1 {
		t.Errorf("second parent = %v, want %v", second.ParentKey, k1)
	}
	// Both committed layers survive, so either can be resumed.
	if !snap.has(k1) || !snap.has(k2) {
		t.Errorf("both committed layers should exist, have %v", snap.keys())
	}
}

func TestCapture_BeforeCreateFails(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t)
	if _, err := sb.Capture(context.Background()); err == nil {
		t.Fatal("Capture before Create must fail")
	}
}

// TestDestroy_RemovesActiveLayerKeepsStates pins the state-durability half of
// the teardown contract: the active layer is the sandbox's private writable
// layer and dies with it, while committed layers are states handed to callers
// and must survive — a caller may Apply one long after Destroy, and states
// have no lifecycle.
func TestDestroy_RemovesActiveLayerKeepsStates(t *testing.T) {
	sb, snap, tasks, log := newTestSandbox(t)
	ctx := context.Background()
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	first, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	second, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	k1 := first.SnapshotKey
	k2 := second.SnapshotKey
	log.mu.Lock()
	log.ops = nil
	log.mu.Unlock()

	if err := sb.Destroy(ctx); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	verbs := opVerbs(log.snapshot())
	if len(verbs) == 0 || verbs[0] != "stop" {
		t.Errorf("Destroy ops = %v, want the task stopped first", verbs)
	}
	// Exactly one removal: the active layer. The two committed layers back
	// states and must not be touched.
	removals := 0
	for _, op := range log.snapshot() {
		if strings.HasPrefix(op, "remove:") {
			removals++
			if !strings.Contains(op, "/active/") {
				t.Errorf("Destroy removed %q, which is not an active layer; committed "+
					"layers are states and must survive", op)
			}
		}
	}
	if removals != 1 {
		t.Errorf("Destroy performed %d removals, want 1 (ops: %v)", removals, log.snapshot())
	}
	// The states and the image rootfs survive; only the active layer is gone.
	left := snap.keys()
	if len(left) != 3 {
		t.Errorf("layers left = %v, want the image rootfs plus both committed states", left)
	}
	for _, key := range []string{"rootfs-image-digest", k1, k2} {
		if !snap.has(key) {
			t.Errorf("snapshot %q did not survive Destroy", key)
		}
	}
	if tasks.isRunning() {
		t.Error("Destroy must stop the task")
	}
}

// TestDestroy_IsIdempotent pins the contract the host relies on: discard,
// merge, and halt paths may all call Destroy, and a second call must not error.
func TestDestroy_IsIdempotent(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t)
	ctx := context.Background()
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := sb.Destroy(ctx); err != nil {
		t.Fatalf("first Destroy: %v", err)
	}
	if err := sb.Destroy(ctx); err != nil {
		t.Errorf("second Destroy = %v, want nil", err)
	}
	// Destroy on a never-created sandbox is also fine.
	fresh, _, _, _ := newTestSandbox(t)
	if err := fresh.Destroy(ctx); err != nil {
		t.Errorf("Destroy on unused sandbox = %v, want nil", err)
	}
}

// TestApply_KeepsLaterLayersAsStates pins that resuming an earlier state leaves
// the layers captured after it alone: they are states the caller still holds,
// and states have no lifecycle. Disk comes back through ReleaseState, not by
// Apply deciding a state is garbage on the caller's behalf.
func TestApply_KeepsLaterLayersAsStates(t *testing.T) {
	sb, snap, _, _ := newTestSandbox(t)
	fl := &fakeLeases{}
	sb.leases = fl
	ctx := context.Background()
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}

	first, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("first Capture: %v", err)
	}
	second, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("second Capture: %v", err)
	}
	k1 := first.SnapshotKey
	k2 := second.SnapshotKey

	// Resume the older state. The second committed layer is no longer an
	// ancestor of anything this sandbox runs on, but it is still a state.
	if err := sb.Apply(ctx, first); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !snap.has(k2) {
		t.Errorf("layer %s was removed by Apply: it backs the captured state %q and "+
			"the caller may still apply it", k2, k2)
	}
	if !snap.has(k1) {
		t.Errorf("layer %s is the resumed state and must survive", k1)
	}
	// The sandbox keeps working after Apply.
	if _, err := sb.Capture(ctx); err != nil {
		t.Fatalf("Capture after Apply: %v", err)
	}

	// The caller decides when the superseded state's disk comes back.
	if err := sb.ReleaseState(ctx, second); err != nil {
		t.Fatalf("ReleaseState: %v", err)
	}
	if !fl.releasedFrom(statesLeaseID, k2) {
		t.Errorf("releasing the superseded state did not drop its %s pin", k2)
	}
}

func TestApply_StartsTaskOnResumedLayer(t *testing.T) {
	sb, _, tasks, log := newTestSandbox(t)
	ctx := context.Background()
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	log.mu.Lock()
	log.ops = nil
	log.mu.Unlock()

	if err := sb.Apply(ctx, state); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	verbs := opVerbs(log.snapshot())
	// Apply stops the task, prepares a layer from the resumed state, restarts the
	// task, then removes the layer it abandoned.
	want := []string{"stop", "prepare", "start", "remove"}
	if strings.Join(verbs, ",") != strings.Join(want, ",") {
		t.Errorf("Apply ops = %v, want %v", verbs, want)
	}
	if !tasks.isRunning() {
		t.Error("Apply must leave a running task")
	}
}

func TestExec_BeforeCreateFails(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t)
	if _, err := sb.Exec(context.Background(), ExecRequest{Command: "true"}); err == nil {
		t.Fatal("Exec before Create must fail")
	}
}

// TestReadFile_PassesPathAsPositionalArgument pins the injection defence: the
// path must never be interpolated into the shell script.
func TestReadFile_PassesPathAsPositionalArgument(t *testing.T) {
	sb, _, tasks, _ := newTestSandbox(t)
	ctx := context.Background()
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}

	evil := "/tmp/$(rm -rf /); echo pwned"
	if _, err := sb.ReadFile(ctx, evil); err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	req := tasks.lastReq
	if req.Command != "sh" {
		t.Fatalf("command = %q, want sh", req.Command)
	}
	if len(req.Args) < 4 {
		t.Fatalf("args = %v, want script, placeholder, and path", req.Args)
	}
	if !strings.Contains(req.Args[1], `"$1"`) {
		t.Errorf("script should read its path from $1, got %q", req.Args[1])
	}
	if strings.Contains(req.Args[1], "rm -rf") {
		t.Error("the path must not appear in the script text")
	}
	if req.Args[3] != evil {
		t.Errorf("arg[3] = %q, want the path verbatim", req.Args[3])
	}
	if req.Cwd != "/workspace" {
		t.Errorf("cwd = %q, want the default workdir", req.Cwd)
	}
}

// TestWriteFile_EncodesContentInArgs pins the argv transport that replaced
// piping content on stdin. Two properties matter. The raw content must not
// appear anywhere in the command: base64 cannot carry shell metacharacters, and
// chunks are positional arguments rather than text spliced into the script, so
// the injection safety the stdin version had is preserved. And decoding the
// payload arguments must reconstruct the content exactly.
//
// Stdin must be empty. It is not merely unused: the runc-v2 shim holds a write
// end on an exec's stdin FIFO for the process's lifetime, so a stdin reader
// never sees EOF and depending on one hangs until the context expires.
func TestWriteFile_EncodesContentInArgs(t *testing.T) {
	sb, _, tasks, _ := newTestSandbox(t)
	ctx := context.Background()
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}

	content := []byte("#!/bin/sh\nrm -rf /\n")
	if err := sb.WriteFile(ctx, "bin/x.sh", content, fs.FileMode(0o755)); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if len(tasks.lastStdin) != 0 {
		t.Errorf("stdin = %q, want empty: WriteFile must not depend on stdin EOF", tasks.lastStdin)
	}

	// execShell builds ["-c", script, placeholder, path, mode, first, chunks...].
	args := tasks.lastReq.Args
	if len(args) < 7 {
		t.Fatalf("args = %v, want script, placeholder, path, mode, first flag, and a payload", args)
	}
	script := args[1]
	if strings.Contains(script, "rm -rf") {
		t.Error("file content must not appear in the script text")
	}
	// The script must reach path and payload through positional parameters, never
	// through values spliced into its own text.
	for _, want := range []string{`p=$1`, `"$@"`, "base64 -d"} {
		if !strings.Contains(script, want) {
			t.Errorf("script should contain %s, got %q", want, script)
		}
	}
	if args[3] != "/workspace/bin/x.sh" {
		t.Errorf("arg[3] = %q, want the resolved path", args[3])
	}
	if args[4] != "755" {
		t.Errorf("arg[4] = %q, want the octal mode", args[4])
	}
	if args[5] != "1" {
		t.Errorf("arg[5] = %q, want the first-batch flag so the file is truncated", args[5])
	}

	var payload strings.Builder
	for _, a := range args[6:] {
		if strings.Contains(a, "rm -rf") {
			t.Error("raw file content must not appear in the command arguments")
		}
		if len(a) > maxArgPayload {
			t.Errorf("payload argument is %d bytes, over the %d ceiling", len(a), maxArgPayload)
		}
		payload.WriteString(a)
	}
	decoded, err := base64.StdEncoding.DecodeString(payload.String())
	if err != nil {
		t.Fatalf("payload is not valid base64: %v", err)
	}
	if string(decoded) != string(content) {
		t.Errorf("decoded payload = %q, want %q", decoded, content)
	}
}

// TestWriteFile_SplitsAcrossExecs pins the chunking: a payload larger than
// Config.WriteChunkBytes travels as several execs, and only the first carries
// the truncating flag — so a retry after a partial failure cannot append to
// stale content, and batches after the first cannot wipe what preceded them.
func TestWriteFile_SplitsAcrossExecs(t *testing.T) {
	log := &opLog{}
	snap := newFakeSnapshotter(log)
	snap.seed("rootfs-image-digest")
	tasks := newFakeTasks(log, snap)
	images := &fakeImages{rootfs: "rootfs-image-digest"}
	sb := NewWithBackend(Config{Image: "example/dev:latest", WriteChunkBytes: 4}, snap, tasks, images)

	ctx := context.Background()
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}

	content := []byte("abcdefghij") // 10 bytes at 4 per batch is 3 execs
	// Create provisions the workdir with its own exec, so count only what
	// WriteFile adds rather than everything the sandbox has issued.
	before := len(tasks.allReqs)
	if err := sb.WriteFile(ctx, "f.txt", content, fs.FileMode(0o600)); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	writes := tasks.allReqs[before:]
	if len(writes) != 3 {
		t.Fatalf("WriteFile issued %d execs, want 3 (one per 4-byte batch)", len(writes))
	}

	// Decoded per exec, not concatenated first: each exec decodes its own batch
	// and appends the bytes, so every batch carries its own '=' padding.
	// Concatenating the encoded batches would put padding mid-stream and fail to
	// decode — the same mistake the in-container script must not make.
	var reassembled []byte
	for i, req := range writes {
		if len(req.Args) < 7 {
			t.Fatalf("exec %d: args = %v, want a payload argument", i, req.Args)
		}
		wantFirst := "0"
		if i == 0 {
			wantFirst = "1"
		}
		if req.Args[5] != wantFirst {
			t.Errorf("exec %d: first flag = %q, want %q", i, req.Args[5], wantFirst)
		}
		if len(req.Stdin) != 0 {
			t.Errorf("exec %d: stdin = %q, want empty", i, req.Stdin)
		}
		dec, err := base64.StdEncoding.DecodeString(strings.Join(req.Args[6:], ""))
		if err != nil {
			t.Fatalf("exec %d: payload is not valid base64: %v", i, err)
		}
		reassembled = append(reassembled, dec...)
	}
	if string(reassembled) != string(content) {
		t.Errorf("reassembled payload = %q, want %q", reassembled, content)
	}
}

// TestWriteFile_EmptyContentStillTruncates pins the degenerate case: writing
// zero bytes must still create the file rather than being a no-op, so a caller
// clearing a file gets the same result as writing content.
func TestWriteFile_EmptyContentStillTruncates(t *testing.T) {
	sb, _, tasks, _ := newTestSandbox(t)
	ctx := context.Background()
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	before := len(tasks.allReqs)
	if err := sb.WriteFile(ctx, "empty.txt", nil, fs.FileMode(0o644)); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	writes := tasks.allReqs[before:]
	if len(writes) != 1 {
		t.Fatalf("WriteFile issued %d execs, want exactly 1", len(writes))
	}
	args := writes[0].Args
	if args[5] != "1" {
		t.Errorf("first flag = %q, want 1 so the file is created and truncated", args[5])
	}
	if len(args) != 6 {
		t.Errorf("args = %v, want no payload arguments for empty content", args)
	}
}

func TestSnapshotState_RoundTrip(t *testing.T) {
	state := snapshotState("example/dev:latest", "layer-7", "layer-3")

	if state.SnapshotKey != "layer-7" {
		t.Errorf("SnapshotKey = %q, want layer-7", state.SnapshotKey)
	}
	if state.ParentKey != "layer-3" {
		t.Errorf("ParentKey = %q, want layer-3", state.ParentKey)
	}
	if state.Image != "example/dev:latest" {
		t.Errorf("Image = %q, want example/dev:latest", state.Image)
	}

}

func TestZeroValueStatesAreRejected(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t)
	ctx := context.Background()

	t.Run("apply", func(t *testing.T) {
		if err := sb.Apply(ctx, WorkspaceState{}); err == nil {
			t.Fatal("Apply accepted a state with no snapshot key")
		}
	})

	t.Run("release", func(t *testing.T) {
		if err := sb.ReleaseState(ctx, WorkspaceState{}); err == nil {
			t.Fatal("ReleaseState accepted a state with no snapshot key")
		}
	})
}

func TestSplitChain(t *testing.T) {
	chain := []string{"c1", "c2", "c3"}

	kept, orphaned := splitChain(chain, "c2")
	if strings.Join(kept, ",") != "c1,c2" {
		t.Errorf("kept = %v, want c1,c2", kept)
	}
	if strings.Join(orphaned, ",") != "c3" {
		t.Errorf("orphaned = %v, want c3", orphaned)
	}

	// A key outside the chain (an image rootfs, or a foreign lineage) retains
	// nothing, because none of the chain is an ancestor of the new layer.
	kept, orphaned = splitChain(chain, "rootfs")
	if len(kept) != 0 {
		t.Errorf("kept = %v, want empty", kept)
	}
	if strings.Join(orphaned, ",") != "c1,c2,c3" {
		t.Errorf("orphaned = %v, want the whole chain", orphaned)
	}

	// An empty chain is not a special case.
	kept, orphaned = splitChain(nil, "c1")
	if len(kept) != 0 || len(orphaned) != 0 {
		t.Errorf("splitChain(nil) = %v, %v; want empty", kept, orphaned)
	}
}

// TestRequireBackend_WithoutDaemon pins that a sandbox pointed at no daemon
// fails with a clear error rather than a nil dereference.
func TestRequireBackend_WithoutDaemon(t *testing.T) {
	// A filesystem path that cannot be a containerd socket makes connection
	// fail deterministically instead of depending on whether a daemon is running
	// on the test host.
	sb := New(Config{Address: "unix:///nonexistent/croft-test.sock"})

	_, err := sb.Exec(context.Background(), ExecRequest{Command: "true"})
	if err == nil {
		t.Fatal("Exec without a reachable daemon must fail")
	}
	if !strings.Contains(err.Error(), "containerd") {
		t.Errorf("error = %v, want it to name containerd", err)
	}
}

// TestCapture_ConcurrentWithApply drives concurrent lifecycle operations:
// both hold the exclusive op-level lock, so they serialize — an in-flight
// Capture finishes before an Apply rebinding the sandbox starts, by
// construction rather than by luck. Errors are expected and ignored; the
// point is that the lock makes the interleaving safe, not lifecycle
// success.
func TestCapture_ConcurrentWithApply(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t)
	ctx := context.Background()
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 25; i++ {
			_, _ = sb.Capture(ctx)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 25; i++ {
			_ = sb.Apply(ctx, state)
		}
	}()
	wg.Wait()
}

// startTaskFailsOnce wraps a taskService whose next StartTask fails once, so a
// test can break exactly one roll-forward and observe the recovery — unlike
// failingStartTask, which keeps failing and turns the recovery restart into a
// second failure.
type startTaskFailsOnce struct {
	taskService
	failNext bool
}

func (f *startTaskFailsOnce) StartTask(ctx context.Context, id, key string) error {
	if f.failNext {
		f.failNext = false
		return errors.New("shim down")
	}
	return f.taskService.StartTask(ctx, id, key)
}

// TestApply_StartTaskFailureRemovesPreparedLayerAndRecovers pins the failure
// path after Prepare succeeds: the prepared successor must be removed (Destroy
// would otherwise leak it — it removes only the current activeKey), the task
// must restart on the old active layer, and the sandbox must stay usable —
// the recovery restart itself is asserted, not just a later Capture.
func TestApply_StartTaskFailureRemovesPreparedLayerAndRecovers(t *testing.T) {
	sb, snap, tasks, _ := newTestSandbox(t)
	ctx := context.Background()
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	before := len(snap.keys())
	onActive := tasks.current

	// Break exactly the roll-forward's StartTask; the recovery restart must
	// go through the same fake and succeed.
	sb.tasks = &startTaskFailsOnce{taskService: sb.tasks, failNext: true}

	if err := sb.Apply(ctx, state); err == nil {
		t.Fatal("Apply succeeded although the successor task could not start")
	}
	if tasks.current != onActive {
		t.Fatalf("after the failed Apply the task runs on %q, want the old active %q: "+
			"the sandbox must recover in place, not stay stopped", tasks.current, onActive)
	}
	if len(snap.keys()) != before {
		t.Errorf("snapshot count = %d after the failed Apply, want %d: the prepared successor must not leak",
			len(snap.keys()), before)
	}

	// The promised later Apply, on the recovered sandbox.
	if err := sb.Apply(ctx, state); err != nil {
		t.Fatalf("Apply after the recovered Apply: %v", err)
	}
}

// TestApply_PreservesForeignLineageInCapture pins the cross-sandbox fork
// flow: a fresh sandbox applying a state captured by another sandbox records
// the applied layer as its lineage, so the next Capture's ParentKey names the
// applied state's snapshot rather than falling back to the image rootfs.
func TestApply_PreservesForeignLineageInCapture(t *testing.T) {
	log := &opLog{}
	snap := newFakeSnapshotter(log)
	snap.seed("rootfs-image-digest")
	images := &fakeImages{rootfs: "rootfs-image-digest"}
	ctx := context.Background()

	parent := NewWithBackend(Config{Image: "example/dev:latest"}, snap, newFakeTasks(log, snap), images, WithLeases(&fakeLeases{}))
	if err := parent.Create(ctx); err != nil {
		t.Fatalf("Create (parent): %v", err)
	}
	state, err := parent.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture (parent): %v", err)
	}

	child := NewWithBackend(Config{Image: "example/dev:latest"}, snap, newFakeTasks(log, snap), images, WithLeases(&fakeLeases{}))
	if err := child.Apply(ctx, state); err != nil {
		t.Fatalf("cross-sandbox Apply: %v", err)
	}
	after, err := child.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture (child): %v", err)
	}
	if after.ParentKey != state.SnapshotKey {
		t.Errorf("ParentKey after a cross-sandbox Apply = %q, want the applied state's %q",
			after.ParentKey, state.SnapshotKey)
	}
}

// TestWorkspaceOpDelaysLifecycle pins the lock wiring the serialization
// contract rests on: workspace operations hold the shared op-level lock, so
// an in-flight one delays a waiting lifecycle operation until it returns —
// and the reverse path completes once the shared holder lets go. A regression
// that put Exec under the exclusive lock, or Capture under the shared one,
// fails here rather than in a tenant's workload.
func TestWorkspaceOpDelaysLifecycle(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t)
	ctx := context.Background()
	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Simulate an in-flight workspace operation by holding the shared lock
	// the way Exec does — no fake collaboration needed; the point is the
	// lock, not the exec.
	sb.opMu.RLock()

	captured := make(chan error, 1)
	go func() { _, err := sb.Capture(ctx); captured <- err }()

	select {
	case <-captured:
		sb.opMu.RUnlock()
		t.Fatal("Capture completed while a workspace operation held the shared lock")
	case <-time.After(50 * time.Millisecond):
		// Still blocked: the exclusive lock is waiting. Let it through and
		// confirm it completes.
		sb.opMu.RUnlock()
		select {
		case err := <-captured:
			if err != nil {
				t.Fatalf("Capture after the shared holder released: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Capture did not complete after the shared holder released")
		}
	}
}

// stubDiff stands in for the daemon-side diff collaborator in wiring tests.
type stubDiff struct{}

func (stubDiff) Compare(_ context.Context, lower, upper []mount.Mount) (ocispec.Descriptor, error) {
	return ocispec.Descriptor{}, nil
}

func (stubDiff) Read(_ context.Context, _ ocispec.Descriptor) ([]byte, error) { return nil, nil }

func (stubDiff) Delete(_ context.Context, _ ocispec.Descriptor) error { return nil }

// TestWithDiffServiceWiresTheCollaborator pins the option's contract: a
// NewWithBackend sandbox has no diff service unless the option supplies one,
// and the option supplies exactly the collaborator it is given. The
// change-set operations' nil check rides on this.
func TestWithDiffServiceWiresTheCollaborator(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t)
	if sb.diff != nil {
		t.Error("a NewWithBackend sandbox built without WithDiffService must have no diff service")
	}

	wired := NewWithBackend(Config{Image: "example/dev:latest"}, nil, nil, nil, WithDiffService(stubDiff{}))
	if wired.diff == nil {
		t.Error("WithDiffService must wire the diff collaborator")
	}
}
