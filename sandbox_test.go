package containerd

import (
	"context"
	"io/fs"
	"strings"
	"testing"

	shepherd "github.com/buchenberg/shepherd-kernel-go"
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

func TestContainerdSandbox_Backend(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "")
	if got := sb.Backend(); got != BackendName {
		t.Errorf("Backend() = %q, want %q", got, BackendName)
	}
}

// TestContainerdSandbox_Capabilities pins the declared contract. These are the
// values callers branch on, so a false here would make the operation unavailable
// even though it is implemented.
func TestContainerdSandbox_Capabilities(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "")
	caps := sb.Capabilities()

	if !caps.Lifecycle {
		t.Error("Lifecycle must be true: Create and Destroy manage real resources")
	}
	if !caps.Exec {
		t.Error("Exec must be true")
	}
	if !caps.FileIO {
		t.Error("FileIO must be true")
	}
	if !caps.Diff {
		t.Error("Diff must be true")
	}
	if !caps.Isolated {
		t.Error("Isolated must be true: the workspace is not the host filesystem")
	}
	if caps.Containment != shepherd.ContainContained {
		t.Errorf("Containment = %q, want %q (a container is not a VM boundary)",
			caps.Containment, shepherd.ContainContained)
	}
}

func TestCreate_ProvisionsBaseSnapshotAndStartsTask(t *testing.T) {
	sb, snap, tasks, log := newTestSandbox(t, "")
	ctx := context.Background()

	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
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

func TestCreate_ValidatesImage(t *testing.T) {
	log := &opLog{}
	snap := newFakeSnapshotter(log)
	sb := NewWithBackend(Config{}, snap, newFakeTasks(log, snap), &fakeImages{rootfs: "r"})

	err := sb.Create(context.Background(), shepherd.SandboxSpec{})
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
	sb, snap, tasks, log := newTestSandbox(t, "abc123")
	ctx := context.Background()

	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
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
	if _, err := sb.Exec(ctx, shepherd.ExecRequest{Command: "true"}); err != nil {
		t.Errorf("Exec after Capture: %v", err)
	}

	// The committed layer is read-only and still present.
	key, err := stateSnapshotKey(state)
	if err != nil {
		t.Fatalf("stateSnapshotKey: %v", err)
	}
	if !snap.has(key) {
		t.Errorf("committed layer %s should still exist", key)
	}
	if err := snap.write(key); err == nil {
		t.Error("the committed layer must not be writable")
	}
}

func TestCapture_RecordsStateAndGitHead(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "deadbeef")
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if state.Backend != BackendName {
		t.Errorf("Backend = %q, want %q", state.Backend, BackendName)
	}
	if state.Revision == "" {
		t.Error("Revision should hold the committed snapshot key")
	}
	if state.Data[StateKeyGitHead] != "deadbeef" {
		t.Errorf("git_head = %v, want deadbeef", state.Data[StateKeyGitHead])
	}
	if state.Data[StateKeyParentKey] != "rootfs-image-digest" {
		t.Errorf("parent = %v, want the image rootfs", state.Data[StateKeyParentKey])
	}
	if state.Data[StateKeyImage] != "example/dev:latest" {
		t.Errorf("image = %v, want example/dev:latest", state.Data[StateKeyImage])
	}

	// A non-git workspace simply omits the baseline rather than failing.
	sb2, _, _, _ := newTestSandbox(t, "")
	if err := sb2.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state2, err := sb2.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture (no git): %v", err)
	}
	if _, ok := state2.Data[StateKeyGitHead]; ok {
		t.Error("git_head should be absent for a non-git workspace")
	}
}

func TestCapture_RepeatedCapturesChain(t *testing.T) {
	sb, snap, _, _ := newTestSandbox(t, "abc")
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
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

	k1, _ := stateSnapshotKey(first)
	k2, _ := stateSnapshotKey(second)
	if k1 == k2 {
		t.Fatal("each capture must produce a distinct committed layer")
	}
	// The second capture's parent is the first committed layer, forming a chain.
	if second.Data[StateKeyParentKey] != k1 {
		t.Errorf("second parent = %v, want %v", second.Data[StateKeyParentKey], k1)
	}
	// Both committed layers survive, so either can be resumed.
	if !snap.has(k1) || !snap.has(k2) {
		t.Errorf("both committed layers should exist, have %v", snap.keys())
	}
}

func TestCapture_BeforeCreateFails(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "")
	if _, err := sb.Capture(context.Background()); err == nil {
		t.Fatal("Capture before Create must fail")
	}
}

// TestDestroy_RemovesOwnedLayersInChildFirstOrder checks teardown removes every
// layer this sandbox created but never the image rootfs, and does so children
// before parents (the fake refuses to remove a layer with children).
func TestDestroy_RemovesOwnedLayersInChildFirstOrder(t *testing.T) {
	sb, snap, tasks, log := newTestSandbox(t, "abc")
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := sb.Capture(ctx); err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if _, err := sb.Capture(ctx); err != nil {
		t.Fatalf("Capture: %v", err)
	}
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
	removals := 0
	for _, v := range verbs {
		if v == "remove" {
			removals++
		}
	}
	// Two committed layers plus the active one.
	if removals != 3 {
		t.Errorf("removed %d layers, want 3 (ops: %v)", removals, log.snapshot())
	}
	// Only the image rootfs remains.
	left := snap.keys()
	if len(left) != 1 || left[0] != "rootfs-image-digest" {
		t.Errorf("layers left = %v, want only the image rootfs", left)
	}
	if tasks.isRunning() {
		t.Error("Destroy must stop the task")
	}
}

// TestDestroy_IsIdempotent pins the contract the kernel relies on: Discard,
// Merge, and Halt may all call Destroy, and a second call must not error.
func TestDestroy_IsIdempotent(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "")
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := sb.Destroy(ctx); err != nil {
		t.Fatalf("first Destroy: %v", err)
	}
	if err := sb.Destroy(ctx); err != nil {
		t.Errorf("second Destroy = %v, want nil", err)
	}
	// Destroy on a never-created sandbox is also fine.
	fresh, _, _, _ := newTestSandbox(t, "")
	if err := fresh.Destroy(ctx); err != nil {
		t.Errorf("Destroy on unused sandbox = %v, want nil", err)
	}
}

// TestApply_PrunesOrphanedLayers verifies that resuming an earlier state removes
// the layers created after it, so fork/apply cycles do not accumulate disk.
func TestApply_PrunesOrphanedLayers(t *testing.T) {
	sb, snap, _, _ := newTestSandbox(t, "abc")
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
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
	k1, _ := stateSnapshotKey(first)
	k2, _ := stateSnapshotKey(second)

	// Resume the older state: the second committed layer is now unreachable.
	if err := sb.Apply(ctx, first); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if snap.has(k2) {
		t.Errorf("layer %s created after the resumed state should be pruned", k2)
	}
	if !snap.has(k1) {
		t.Errorf("layer %s is the resumed state and must survive", k1)
	}
	// The sandbox keeps working after Apply.
	if _, err := sb.Capture(ctx); err != nil {
		t.Fatalf("Capture after Apply: %v", err)
	}
}

func TestApply_StartsTaskOnResumedLayer(t *testing.T) {
	sb, _, tasks, log := newTestSandbox(t, "abc")
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
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

func TestApply_RejectsForeignBackend(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "")
	err := sb.Apply(context.Background(), shepherd.WorkspaceState{Backend: "git"})
	if err == nil {
		t.Fatal("Apply must reject a git workspace state")
	}
	if !strings.Contains(err.Error(), "git") {
		t.Errorf("error should name the offending backend: %v", err)
	}
}

func TestExec_BeforeCreateFails(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "")
	if _, err := sb.Exec(context.Background(), shepherd.ExecRequest{Command: "true"}); err == nil {
		t.Fatal("Exec before Create must fail")
	}
}

// TestReadFile_PassesPathAsPositionalArgument pins the injection defence: the
// path must never be interpolated into the shell script.
func TestReadFile_PassesPathAsPositionalArgument(t *testing.T) {
	sb, _, tasks, _ := newTestSandbox(t, "")
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
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

// TestWriteFile_PipesContentOnStdin pins that file content never reaches the
// command line.
func TestWriteFile_PipesContentOnStdin(t *testing.T) {
	sb, _, tasks, _ := newTestSandbox(t, "")
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	content := []byte("#!/bin/sh\nrm -rf /\n")
	if err := sb.WriteFile(ctx, "bin/x.sh", content, fs.FileMode(0o755)); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if string(tasks.lastStdin) != string(content) {
		t.Errorf("stdin = %q, want the file content", tasks.lastStdin)
	}
	for _, a := range tasks.lastReq.Args {
		if strings.Contains(a, "rm -rf") {
			t.Error("file content must not appear in the command arguments")
		}
	}
	// The mode is passed positionally as octal.
	args := tasks.lastReq.Args
	if args[len(args)-1] != "755" || args[len(args)-2] != "/workspace/bin/x.sh" {
		t.Errorf("args tail = %v, want the path and octal mode", args[len(args)-2:])
	}
}

func TestDiff_RequiresGitBaseline(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "")
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A state with no git_head cannot be diffed semantically.
	state := snapshotState("example/dev:latest", "some-layer", "")
	if _, _, err := sb.Diff(ctx, state, 0); err == nil {
		t.Fatal("Diff without a git baseline must fail")
	}
	// A foreign state is rejected before anything else.
	if _, _, err := sb.Diff(ctx, shepherd.WorkspaceState{Backend: "git"}, 0); err == nil {
		t.Fatal("Diff must reject a foreign state")
	}
}

func TestDiff_TruncatesAndReportsFiles(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "deadbeef")
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	diff, _, err := sb.Diff(ctx, state, 1)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if !strings.Contains(diff, "[diff truncated]") {
		t.Errorf("diff = %q, want a truncation marker", diff)
	}

	full, _, err := sb.Diff(ctx, state, 0)
	if err != nil {
		t.Fatalf("Diff (unbounded): %v", err)
	}
	if strings.Contains(full, "[diff truncated]") {
		t.Errorf("unbounded diff should not be truncated: %q", full)
	}
}

func TestSnapshotState_RoundTrip(t *testing.T) {
	state := snapshotState("example/dev:latest", "layer-7", "layer-3")

	if state.Backend != BackendName {
		t.Errorf("Backend = %q, want %q", state.Backend, BackendName)
	}
	if state.Revision != "layer-7" {
		t.Errorf("Revision = %q, want the snapshot key", state.Revision)
	}
	if state.Data[StateKeyParentKey] != "layer-3" {
		t.Errorf("parent = %v, want layer-3", state.Data[StateKeyParentKey])
	}

	key, err := stateSnapshotKey(state)
	if err != nil {
		t.Fatalf("stateSnapshotKey: %v", err)
	}
	if key != "layer-7" {
		t.Errorf("key = %q, want layer-7", key)
	}

	// The state must digest stably, so trace records and drift detection work.
	d1, err := state.Digest()
	if err != nil {
		t.Fatalf("Digest: %v", err)
	}
	d2, err := snapshotState("example/dev:latest", "layer-7", "layer-3").Digest()
	if err != nil {
		t.Fatalf("Digest (second): %v", err)
	}
	if d1 != d2 {
		t.Errorf("digest is not stable: %s vs %s", d1, d2)
	}
}

func TestStateSnapshotKey_RejectsBadStates(t *testing.T) {
	t.Run("foreign backend", func(t *testing.T) {
		_, err := stateSnapshotKey(shepherd.WorkspaceState{
			Backend:  "git",
			Revision: "abc123",
			Data:     map[string]any{"head_sha": "abc123"},
		})
		if err == nil {
			t.Fatal("a git state must be rejected")
		}
	})

	t.Run("missing snapshot key", func(t *testing.T) {
		_, err := stateSnapshotKey(shepherd.WorkspaceState{Backend: BackendName, Data: map[string]any{}})
		if err == nil {
			t.Fatal("a state without a snapshot key must be rejected")
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
	sb := New(Config{Address: "unix:///nonexistent/shepherd-test.sock"})

	_, err := sb.Exec(context.Background(), shepherd.ExecRequest{Command: "true"})
	if err == nil {
		t.Fatal("Exec without a reachable daemon must fail")
	}
	if !strings.Contains(err.Error(), "containerd") {
		t.Errorf("error = %v, want it to name containerd", err)
	}
}
