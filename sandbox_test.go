package containerd

import (
	"context"
	"encoding/base64"
	"errors"
	"io/fs"
	"strings"
	"sync"
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

// TestCreate_ProvisionsWorkdir pins that Create makes the configured workdir
// exist. No image is obliged to ship /workspace, and without this every later
// operation fails with a chdir error that reads as a broken sandbox rather than
// a missing directory.
func TestCreate_ProvisionsWorkdir(t *testing.T) {
	sb, _, tasks, _ := newTestSandbox(t, "")
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
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

// TestDestroy_RemovesActiveLayerKeepsStates pins the state-durability half of
// the teardown contract: the active layer is the sandbox's private writable
// layer and dies with it, while committed layers are states handed to callers
// and must survive — a caller may Apply one long after Destroy, and the kernel
// contract says states have no lifecycle.
func TestDestroy_RemovesActiveLayerKeepsStates(t *testing.T) {
	sb, snap, tasks, log := newTestSandbox(t, "abc")
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
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
	k1, err := stateSnapshotKey(first)
	if err != nil {
		t.Fatalf("first state: %v", err)
	}
	k2, err := stateSnapshotKey(second)
	if err != nil {
		t.Fatalf("second state: %v", err)
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

// TestApply_KeepsLaterLayersAsStates pins that resuming an earlier state leaves
// the layers captured after it alone: they are states the caller still holds,
// and states have no lifecycle. Disk comes back through ReleaseState, not by
// Apply deciding a state is garbage on the caller's behalf.
func TestApply_KeepsLaterLayersAsStates(t *testing.T) {
	sb, snap, _, _ := newTestSandbox(t, "abc")
	fl := &fakeLeases{}
	sb.leases = fl
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
	k1, err := stateSnapshotKey(first)
	if err != nil {
		t.Fatalf("first state: %v", err)
	}
	k2, err := stateSnapshotKey(second)
	if err != nil {
		t.Fatalf("second state: %v", err)
	}

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
	sb, _, tasks, _ := newTestSandbox(t, "")
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
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
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
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
	sb, _, tasks, _ := newTestSandbox(t, "")
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
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

// TestDiff_StagesIntoScratchIndex pins the non-mutating mechanism at the script
// level, so the invariant is visible without a live daemon. The live suite
// proves the end-to-end behaviour; this keeps a regression from landing in the
// first place.
func TestDiff_StagesIntoScratchIndex(t *testing.T) {
	sb, _, tasks, _ := newTestSandbox(t, "deadbeef")
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if _, _, err := sb.Diff(ctx, state, 0); err != nil {
		t.Fatalf("Diff: %v", err)
	}

	var sawDiff bool
	for _, req := range tasks.allReqs {
		if req.Command != "sh" || len(req.Args) < 2 {
			continue
		}
		script := req.Args[1]
		if !strings.Contains(script, "git diff --cached") {
			continue
		}
		sawDiff = true
		if !strings.Contains(script, "GIT_INDEX_FILE") {
			t.Error("Diff must stage into a scratch GIT_INDEX_FILE")
		}
		// The real index is the seed, and seeding is a read: cp from it, never
		// into it.
		if !strings.Contains(script, "cp -- .git/index") {
			t.Error("Diff should seed the scratch index from the real one")
		}
		if strings.Contains(script, "git reset") {
			t.Error("Diff must not unwind with git reset: that restores the index " +
				"to HEAD, not to the state it was found in")
		}
	}
	if !sawDiff {
		t.Fatal("Diff issued no scratch-index script")
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

// TestApply_RecordsDeltaBaseAndCaptureCarriesDelta pins the delta plumbing: an
// Apply of a state that carries a git HEAD records it as the sandbox's delta
// base, and every Capture from then on stores the base plus the patch against
// it, so the state can settle through ApplyDelta onto a moved workspace.
func TestApply_RecordsDeltaBaseAndCaptureCarriesDelta(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "abc")
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	baseline, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("baseline Capture: %v", err)
	}
	baseline.Data[StateKeyGitHead] = "basehead"

	if err := sb.Apply(ctx, baseline); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture after Apply: %v", err)
	}
	if got := state.Data[StateKeyDeltaBase]; got != "basehead" {
		t.Errorf("delta_base = %v, want the applied state's git HEAD", got)
	}
	patchB64, _ := state.Data[StateKeyDeltaPatch].(string)
	if patchB64 == "" {
		t.Fatal("delta_patch should be present when the range has changes")
	}
	patch, err := base64.StdEncoding.DecodeString(patchB64)
	if err != nil {
		t.Fatalf("delta_patch is not valid base64: %v", err)
	}
	if !strings.Contains(string(patch), "diff --git") {
		t.Errorf("delta_patch = %q, want a git patch", string(patch))
	}
}

// TestCapture_CleanDeltaOmitsPatch pins the clean-capture encoding: the base
// rides in the state, the patch does not, and ApplyDelta reads that as a no-op.
func TestCapture_CleanDeltaOmitsPatch(t *testing.T) {
	sb, _, tasks, _ := newTestSandbox(t, "abc")
	tasks.deltaPatchStdout = "\n"
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	baseline, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("baseline Capture: %v", err)
	}
	baseline.Data[StateKeyGitHead] = "basehead"
	if err := sb.Apply(ctx, baseline); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if got := state.Data[StateKeyDeltaBase]; got != "basehead" {
		t.Errorf("delta_base = %v, want basehead", got)
	}
	if _, ok := state.Data[StateKeyDeltaPatch]; ok {
		t.Error("delta_patch should be absent for a clean capture")
	}
}

// TestCapture_WithoutBaseCarriesNoDelta pins that a sandbox which never applied
// a git state produces states with no delta payload, and that this is not an
// error: those states settle Select-only.
func TestCapture_WithoutBaseCarriesNoDelta(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "deadbeef")
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if _, ok := state.Data[StateKeyDeltaBase]; ok {
		t.Error("delta_base should be absent without an applied git baseline")
	}
	if _, ok := state.Data[StateKeyDeltaPatch]; ok {
		t.Error("delta_patch should be absent without an applied git baseline")
	}
}

// TestApplyDelta_RejectsForeignBackend mirrors Apply's backend check: a git
// state can never be merged here.
func TestApplyDelta_RejectsForeignBackend(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "")
	err := sb.ApplyDelta(context.Background(), shepherd.WorkspaceState{Backend: "git"})
	if err == nil {
		t.Fatal("ApplyDelta must reject a git workspace state")
	}
	if !strings.Contains(err.Error(), "git") {
		t.Errorf("error should name the offending backend: %v", err)
	}
}

// TestApplyDelta_UnbasedDeltaIsUnsupported pins the refusal for a delta that
// carries no base: ErrUnsupported, not a silent no-op, so a caller that needs
// the merge verb can fail loudly at selection time.
func TestApplyDelta_UnbasedDeltaIsUnsupported(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "")
	err := sb.ApplyDelta(context.Background(), shepherd.WorkspaceState{
		Backend: BackendName,
		Data:    map[string]any{StateKeySnapshotKey: "k"},
	})
	if !errors.Is(err, shepherd.ErrUnsupported) {
		t.Fatalf("ApplyDelta without a base = %v, want ErrUnsupported", err)
	}
}

// TestApplyDelta_CleanCaptureIsNoOp pins the no-op path: a base with no changes
// merges nothing and runs no container execs at all.
func TestApplyDelta_CleanCaptureIsNoOp(t *testing.T) {
	sb, _, tasks, _ := newTestSandbox(t, "abc")
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	afterCreate := len(tasks.allReqs)
	err := sb.ApplyDelta(ctx, shepherd.WorkspaceState{
		Backend: BackendName,
		Data:    map[string]any{StateKeyDeltaBase: "basehead"},
	})
	if err != nil {
		t.Fatalf("ApplyDelta (clean capture): %v", err)
	}
	if got := len(tasks.allReqs); got != afterCreate {
		t.Errorf("clean-capture no-op ran %d execs, want %d (none beyond Create's)", got, afterCreate)
	}
}

// TestApplyDelta_AppliesPatchAndCleansUp pins the happy path's shape: the patch
// is staged to a temp file, the merge script runs, and the temp file is removed.
func TestApplyDelta_AppliesPatchAndCleansUp(t *testing.T) {
	sb, _, tasks, _ := newTestSandbox(t, "abc")
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	patch := base64.StdEncoding.EncodeToString([]byte("diff --git a/f b/f\n+child\n"))
	delta := shepherd.WorkspaceState{
		Backend: BackendName,
		Data: map[string]any{
			StateKeyDeltaBase:  "basehead",
			StateKeyDeltaPatch: patch,
		},
	}
	if err := sb.ApplyDelta(ctx, delta); err != nil {
		t.Fatalf("ApplyDelta: %v", err)
	}

	var staged, merged, removed bool
	for _, req := range tasks.allReqs {
		script := ""
		if len(req.Args) >= 2 {
			script = req.Args[1]
		}
		switch {
		case strings.Contains(script, "base64 -d"):
			staged = true
		case strings.Contains(script, "git merge-tree"):
			merged = true
		case strings.Contains(script, "rm -f --"):
			removed = true
		}
	}
	if !staged {
		t.Error("the delta patch should be staged through WriteFile")
	}
	if !merged {
		t.Error("the merge script should have run")
	}
	if !removed {
		t.Error("the staged patch file should be removed after the merge")
	}
}

// TestApplyDelta_ConflictRefusesWithPaths pins the refused-merge path: the
// script's conflict sentinel surfaces with the conflicting paths named, so a
// caller can tell a refusal (nothing consumed) from an operational failure.
func TestApplyDelta_ConflictRefusesWithPaths(t *testing.T) {
	sb, _, tasks, _ := newTestSandbox(t, "abc")
	tasks.mergeConflict = true
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	patch := base64.StdEncoding.EncodeToString([]byte("diff --git a/f.txt b/f.txt\n+child\n"))
	err := sb.ApplyDelta(ctx, shepherd.WorkspaceState{
		Backend: BackendName,
		Data: map[string]any{
			StateKeyDeltaBase:  "basehead",
			StateKeyDeltaPatch: patch,
		},
	})
	if err == nil {
		t.Fatal("a conflicting delta must fail the call")
	}
	if !strings.Contains(err.Error(), "conflicting paths") || !strings.Contains(err.Error(), "f.txt") {
		t.Errorf("error = %v, want it to name the conflicting paths", err)
	}
}

// TestApplyDelta_BeforeCreateFails mirrors the other lifecycle guards: a merge
// needs a running task to exec in.
func TestApplyDelta_BeforeCreateFails(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "")
	patch := base64.StdEncoding.EncodeToString([]byte("diff --git a/f b/f\n+child\n"))
	err := sb.ApplyDelta(context.Background(), shepherd.WorkspaceState{
		Backend: BackendName,
		Data: map[string]any{
			StateKeyDeltaBase:  "basehead",
			StateKeyDeltaPatch: patch,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "before Create") {
		t.Fatalf("ApplyDelta before Create = %v, want a lifecycle error", err)
	}
}

// applyBasedState applies a state carrying a git HEAD so the sandbox records a
// delta base, ready for the failure-path tests below.
func applyBasedState(t *testing.T, ctx context.Context, sb *ContainerdSandbox) {
	t.Helper()
	baseline, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("baseline Capture: %v", err)
	}
	baseline.Data[StateKeyGitHead] = "basehead"
	if err := sb.Apply(ctx, baseline); err != nil {
		t.Fatalf("Apply: %v", err)
	}
}

// TestCapture_BaseWithoutGitHeadFailsAndUnpins pins the loud refusal: once a
// base is recorded, a capture that cannot read the workspace's git HEAD fails
// rather than handing out a state with no delta payload, and it drops the pin
// on the committed layer first — no state is handed out, so nothing else could
// ever release it.
func TestCapture_BaseWithoutGitHeadFailsAndUnpins(t *testing.T) {
	sb, _, tasks, _ := newTestSandbox(t, "abc")
	fl := &fakeLeases{}
	sb.leases = fl
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	applyBasedState(t, ctx, sb)

	// The workspace stops being a readable repository between Apply and the
	// next Capture.
	tasks.gitHead = ""
	_, err := sb.Capture(ctx)
	if err == nil || !strings.Contains(err.Error(), "read git HEAD") {
		t.Fatalf("Capture without a readable HEAD = %v, want a loud refusal", err)
	}

	// added - removed counts the pins still held; the failed capture must have
	// dropped the pin it just took, leaving only the baseline's.
	if len(fl.added)-len(fl.removed) != 1 {
		t.Errorf("pins added=%d removed=%d; want exactly the baseline's pin left held", len(fl.added), len(fl.removed))
	}
}

// TestCapture_DeltaFailureFailsAndUnpins pins the delta-generation refusal and
// its pin drop: the state never reaches the caller, so the pin on its layer
// must not outlive the call.
func TestCapture_DeltaFailureFailsAndUnpins(t *testing.T) {
	sb, _, tasks, _ := newTestSandbox(t, "abc")
	fl := &fakeLeases{}
	sb.leases = fl
	tasks.runErr["sh"] = errors.New("delta script boom")
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	applyBasedState(t, ctx, sb)

	_, err := sb.Capture(ctx)
	if err == nil || !strings.Contains(err.Error(), "capture delta") {
		t.Fatalf("Capture with a failing delta script = %v, want a loud refusal", err)
	}
	if len(fl.added)-len(fl.removed) != 1 {
		t.Errorf("pins added=%d removed=%d; want exactly the baseline's pin left held", len(fl.added), len(fl.removed))
	}
}

// TestApply_StateWithoutGitHeadClearsBase pins the rebind rule: applying a
// state with no git HEAD moves the sandbox to a workspace that may not contain
// the old base's repository, so the base must not survive.
func TestApply_StateWithoutGitHeadClearsBase(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "abc")
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	applyBasedState(t, ctx, sb)

	plain, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("plain Capture: %v", err)
	}
	delete(plain.Data, StateKeyGitHead)
	if err := sb.Apply(ctx, plain); err != nil {
		t.Fatalf("Apply (no git head): %v", err)
	}

	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture after rebind: %v", err)
	}
	if _, ok := state.Data[StateKeyDeltaBase]; ok {
		t.Error("delta_base should not survive applying a state with no git HEAD")
	}
}

// TestCreate_ResetsBase pins the generation reset: a sandbox reused through
// Create after a previous life that applied a git state must not diff the new
// workspace against the old repository's HEAD.
func TestCreate_ResetsBase(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "abc")
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	applyBasedState(t, ctx, sb)

	// The supported reuse flow is Create -> Destroy -> Create.
	if err := sb.Destroy(ctx); err != nil {
		t.Fatalf("Destroy before re-Create: %v", err)
	}
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("re-Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture after re-Create: %v", err)
	}
	if _, ok := state.Data[StateKeyDeltaBase]; ok {
		t.Error("delta_base should not survive Create: the new generation's workspace may not contain the old base's repository")
	}
}

// TestCapture_ConcurrentWithApply drives the data race the base field's
// locked read exists for: Capture snapshots baseGitHead while Apply reassigns
// it. Errors are expected and ignored — the point is that -race sees the
// unsynchronized read, not lifecycle success.
func TestCapture_ConcurrentWithApply(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "abc")
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	state.Data[StateKeyGitHead] = "basehead"

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
