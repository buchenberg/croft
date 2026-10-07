//go:build linux && !nolive

package containerd

// Live-daemon acceptance test for the containerd backend (plan 05 §3, T0.5).
//
// Everything else in this package runs against fakeSnapshotter and fakeTasks.
// Those fakes enforce the overlayfs snapshotter's invariants, so they cover the
// lifecycle logic — but they cannot cover client.go, which is the daemon
// adapter: namespace injection, image pull/unpack, rootfs-key derivation,
// container/task generations, the exec round-trip, and SIGKILL teardown. This
// file is the only thing that exercises it.
//
// # Gating
//
// Skipped unless SHEPHERD_CONTAINERD_ADDR is set, so `go test ./...` stays green
// on a machine with no daemon and in CI. The build tag keeps it out of non-Linux
// builds entirely: the backend is Linux-only by construction (overlayfs plus
// runc), so compiling it elsewhere would only produce a test that cannot pass.
//
// Run it with:
//
//	sudo env SHEPHERD_CONTAINERD_ADDR=/run/containerd/containerd.sock \
//	  go test -count=1 -v -run TestLive ./...
//
// # Why root
//
// The daemon's socket, and nothing else. containerd's default socket is
// root-only and these tests dial it directly.
//
// The client-side FIFO directory used to be a second reason: cio.NewCreator fell
// back to defaults.DefaultFIFODir and called os.MkdirAll on it *in the client
// process*, so a non-root client failed locally before any RPC reached the
// daemon. Config.FIFODir removes that (finding F2), and these tests set it to a
// temp dir.
//
// The rootfs mount was never a reason: handleMounts only ships mount descriptors
// to the shim, which mounts them server-side as root, so the client never opens
// /var/lib/containerd.
//
// # Findings recorded while writing this test
//
// F1 — FIXED. Create did not provision Config.Workdir, so on any image lacking
// /workspace every exec and file operation failed with a chdir error even though
// Create itself had succeeded and the init task was running. Create now runs
// mkdir -p on the workdir and tears the sandbox back down if that fails.
//
// F2 — FIXED. The FIFO directory was hardcoded to containerd's root-owned
// default with no knob, making the backend root-client-only; yaah is a developer
// CLI running as an ordinary user and could not have driven it. Config.FIFODir
// now threads into cio.WithFIFODir, and these tests set it to a t.TempDir() so
// the knob is exercised on every run. requireRoot remains, but for the daemon's
// socket permissions rather than anything the client creates.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	shepherd "github.com/buchenberg/shepherd-kernel-go"
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/containerd/containerd/v2/core/snapshots"
	"github.com/containerd/containerd/v2/defaults"
	"github.com/containerd/containerd/v2/pkg/namespaces"
)

// Environment knobs. Every value the test needs from its surroundings is
// configurable rather than baked in, so a different image, namespace, or
// snapshotter is a variable change and not a code change.
const (
	envAddr        = "SHEPHERD_CONTAINERD_ADDR"
	envImage       = "SHEPHERD_CONTAINERD_IMAGE"
	envNamespace   = "SHEPHERD_CONTAINERD_NAMESPACE"
	envSnapshotter = "SHEPHERD_CONTAINERD_SNAPSHOTTER"
	envWorkdir     = "SHEPHERD_CONTAINERD_WORKDIR"
	envTimeout     = "SHEPHERD_CONTAINERD_TIMEOUT"
)

// Defaults for the knobs above. defaultSnapshotter is client.go's own constant
// rather than a copy, so the test cannot drift from the value the adapter uses.
//
// The image needs both sh and git in-container: sandbox.go shells out to
// cat/mkdir/dirname/chmod for file I/O and to git for Diff. alpine/git carries
// both at ~15MB. Its BusyBox userland supports every command this backend uses
// (unlike yaah's sandboxWorkspace, which needs GNU find -printf and would
// require a glibc image).
const (
	defaultImage     = "docker.io/alpine/git:latest"
	defaultNamespace = "shepherd-live"
	defaultWorkdir   = "/workspace"
	// Deliberately well under `go test`'s own 10m default, so a hang surfaces as
	// a context-deadline error from the code under test rather than as a testing
	// panic with a goroutine dump. Raise it via SHEPHERD_CONTAINERD_TIMEOUT for a
	// cold image pull, and pass a matching `go test -timeout`.
	defaultTimeout = 5 * time.Minute
)

// liveConfig is the resolved daemon configuration for one test run.
type liveConfig struct {
	Addr        string
	Image       string
	Namespace   string
	Snapshotter string
	Workdir     string
	Timeout     time.Duration
}

func liveEnv(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// loadLiveConfig resolves the environment or skips the test. The address is
// the only required value: without it there is no daemon to talk to, and
// skipping keeps this file inert on a machine that has not opted in.
func loadLiveConfig(t *testing.T) liveConfig {
	t.Helper()

	addr := strings.TrimSpace(os.Getenv(envAddr))
	if addr == "" {
		t.Skipf("%s not set: skipping live containerd verification (plan 05 §3)", envAddr)
	}
	requireRoot(t)

	cfg := liveConfig{
		Addr:        addr,
		Image:       liveEnv(envImage, defaultImage),
		Namespace:   liveEnv(envNamespace, defaultNamespace),
		Snapshotter: liveEnv(envSnapshotter, defaultSnapshotter),
		Workdir:     liveEnv(envWorkdir, defaultWorkdir),
		Timeout:     defaultTimeout,
	}

	if raw := strings.TrimSpace(os.Getenv(envTimeout)); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			t.Fatalf("%s=%q is not a valid duration: %v", envTimeout, raw, err)
		}
		cfg.Timeout = parsed
	}

	// Logged so a run records what it actually verified. This is what goes into
	// the README status note in place of "has never been executed".
	t.Logf("live containerd: addr=%s namespace=%s snapshotter=%s image=%s workdir=%s timeout=%s",
		cfg.Addr, cfg.Namespace, cfg.Snapshotter, cfg.Image, cfg.Workdir, cfg.Timeout)

	return cfg
}

// requireRoot skips unless the test runs as root. Config.FIFODir removes the
// client-side need for root (finding F2), so what is left is the daemon's
// socket: containerd's default is root-only and these tests dial it directly. A
// deployment that grants socket access to a group could relax this guard.
func requireRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		return
	}
	t.Skipf("live containerd tests need root for the daemon socket. The client-side "+
		"blocker is gone — Config.FIFODir is set to a temp dir instead of %s "+
		"(defaults.DefaultFIFODir) — so a deployment granting socket access to a group "+
		"could relax this guard",
		defaults.DefaultFIFODir)
}

// dial opens a raw client for the assertions that need to look at daemon state
// directly rather than through the sandbox: namespace isolation and snapshot
// teardown. The caller closes it.
func dial(t *testing.T, addr string) *containerd.Client {
	t.Helper()
	c, err := containerd.New(addr)
	if err != nil {
		t.Fatalf("connect to containerd at %s: %v", addr, err)
	}
	return c
}

// nsCtx scopes a context to the configured namespace. containerd resolves the
// namespace from the context, not the client, which is exactly what
// namespaceSnapshotter in client.go works around for the sandbox's own calls.
func nsCtx(ctx context.Context, ns string) context.Context {
	return namespaces.WithNamespace(ctx, ns)
}

// ensureNamespace creates the namespace if absent. A dedicated namespace keeps
// the test's containers and snapshots separable from anything else on the
// daemon, so a generation leaked by a crashed run is easy to find and clean.
//
// A creation error is logged rather than fatal: "already exists" is the common
// case, and a genuinely broken namespace makes the very next Create fail loudly
// with a better message than anything inferred here.
func ensureNamespace(t *testing.T, ctx context.Context, c *containerd.Client, ns string) {
	t.Helper()
	if err := c.NamespaceService().Create(nsCtx(ctx, ns), ns, nil); err != nil {
		t.Logf("ensure namespace %q: %v (continuing; it usually already exists)", ns, err)
	}
}

// liveSnapshotKeys returns every snapshot key visible in the namespace already
// carried by ctx. Walk is used instead of per-key Stat so a missing key is
// absence from the set rather than an error to classify, which keeps this free
// of an errdefs dependency.
func liveSnapshotKeys(t *testing.T, ctx context.Context, c *containerd.Client, snapshotter string) map[string]bool {
	t.Helper()
	keys := make(map[string]bool)
	err := c.SnapshotService(snapshotter).Walk(ctx, func(_ context.Context, info snapshots.Info) error {
		keys[info.Name] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk snapshots: %v", err)
	}
	return keys
}

// newLiveSandbox builds a sandbox against the real daemon and registers its
// teardown. Destroy is documented idempotent, so registering cleanup before
// Create is safe and catches a partial Create that allocated a snapshot but
// failed to start a task.
func newLiveSandbox(t *testing.T, ctx context.Context, cfg liveConfig) *ContainerdSandbox {
	t.Helper()

	c := dial(t, cfg.Addr)
	ensureNamespace(t, ctx, c, cfg.Namespace)
	if err := c.Close(); err != nil {
		t.Logf("close setup client: %v", err)
	}

	sb := New(Config{
		Address:     cfg.Addr,
		Namespace:   cfg.Namespace,
		Image:       cfg.Image,
		Snapshotter: cfg.Snapshotter,
		Workdir:     cfg.Workdir,
		// A per-test temp dir rather than containerd's root-owned default, which
		// exercises the FIFODir knob (finding F2) on every run. t.TempDir is
		// removed automatically when the test ends.
		FIFODir: t.TempDir(),
		// InitArgs is deliberately unset: the adapter's own
		// /bin/sh -c "sleep infinity" default is part of what needs verifying.
	})

	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		if err := sb.Destroy(dctx); err != nil {
			t.Errorf("Destroy during cleanup: %v", err)
		}
	})

	return sb
}

// mustExec runs a command and fails the test on any error or non-zero exit.
// cwd is explicit rather than defaulted so a caller can run outside the workdir
// when it needs to.
func mustExec(t *testing.T, ctx context.Context, sb *ContainerdSandbox, cwd, command string, args ...string) string {
	t.Helper()
	start := time.Now()
	res, err := sb.Exec(ctx, shepherd.ExecRequest{Command: command, Args: args, Cwd: cwd})
	// Per-command duration is what separates "this one command hangs" from
	// "every exec is slow", which are indistinguishable in a single total.
	t.Logf("exec %s %s: %s (exit %d)", command, strings.Join(args, " "),
		time.Since(start).Round(time.Millisecond), res.ExitCode)
	if err != nil {
		t.Fatalf("exec %s %v: %v (stderr: %s)", command, args, err, res.Stderr)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exec %s %v: exit %d (stderr: %s)", command, args, res.ExitCode, res.Stderr)
	}
	return res.Stdout
}

// bootstrapGit seeds a committed git repository in the workdir.
//
// Capture records StateKeyGitHead only when the workdir is a repository, and
// Diff needs that baseline — without a commit, Diff returns an error instead of
// a diff. Create provisions the workdir itself (finding F1), so this does not
// have to.
//
// git identity is set explicitly because alpine/git ships none, and a commit
// without it fails.
func bootstrapGit(t *testing.T, ctx context.Context, sb *ContainerdSandbox, workdir string) {
	t.Helper()

	mustExec(t, ctx, sb, workdir, "git", "init", "-q", ".")
	mustExec(t, ctx, sb, workdir, "git", "config", "user.email", "shepherd-live@test.invalid")
	mustExec(t, ctx, sb, workdir, "git", "config", "user.name", "shepherd-live")
	mustExec(t, ctx, sb, workdir, "sh", "-c", "printf 'baseline\\n' > base.txt")
	mustExec(t, ctx, sb, workdir, "git", "add", "-A")
	mustExec(t, ctx, sb, workdir, "git", "commit", "-q", "-m", "baseline")
}

// TestLive_FullLifecycle is the acceptance sequence from plan 05 §3: create
// from a public image, write, capture, mutate, apply, verify the rollback,
// exec, diff, destroy.
//
// The rollback step is the point of the whole backend. Everything before it is
// setup; if Apply does not restore the pre-mutation content across a real
// commit/prepare/restart cycle on a real overlayfs daemon, the fork-and-revert
// story does not work and nothing above it matters.
func TestLive_FullLifecycle(t *testing.T) {
	cfg := loadLiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	sb := newLiveSandbox(t, ctx, cfg)

	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sb.ID() == "" {
		t.Fatal("Create must assign a container id")
	}
	if got := sb.Backend(); got != BackendName {
		t.Errorf("Backend() = %q, want %q", got, BackendName)
	}

	bootstrapGit(t, ctx, sb, cfg.Workdir)

	// File I/O round-trip. Paths are relative on purpose, so this also covers
	// resolve() against the configured workdir.
	const want = "shepherd live verification\n"
	if err := sb.WriteFile(ctx, "live.txt", []byte(want), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := sb.ReadFile(ctx, "live.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != want {
		t.Errorf("ReadFile after WriteFile = %q, want %q", got, want)
	}

	// WriteFile applies perm via an in-container chmod, so verify it landed
	// rather than trusting the exit code.
	if mode := strings.TrimSpace(mustExec(t, ctx, sb, cfg.Workdir, "stat", "-c", "%a", "live.txt")); mode != "644" {
		t.Errorf("WriteFile perm = %s, want 644", mode)
	}

	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if state.Backend != BackendName {
		t.Errorf("state.Backend = %q, want %q", state.Backend, BackendName)
	}
	if key, _ := state.Data[StateKeySnapshotKey].(string); key == "" {
		t.Errorf("state.Data[%q] is empty: Apply cannot resume without it", StateKeySnapshotKey)
	}
	if state.Revision == "" {
		t.Error("state.Revision is empty")
	}
	if head, _ := state.Data[StateKeyGitHead].(string); head == "" {
		t.Errorf("state.Data[%q] is empty: Capture must record the in-container git HEAD "+
			"when the workdir is a repository, or Diff has no baseline", StateKeyGitHead)
	}

	// Mutate, confirm the mutation is really on the new layer, then roll back.
	const mutated = "mutated after capture\n"
	if err := sb.WriteFile(ctx, "live.txt", []byte(mutated), 0o644); err != nil {
		t.Fatalf("WriteFile (mutate): %v", err)
	}
	if before, err := sb.ReadFile(ctx, "live.txt"); err != nil {
		t.Fatalf("ReadFile (mutate check): %v", err)
	} else if string(before) != mutated {
		t.Fatalf("mutation did not land before Apply: got %q, want %q — the rollback "+
			"assertion below would be meaningless", before, mutated)
	}

	if err := sb.Apply(ctx, state); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	after, err := sb.ReadFile(ctx, "live.txt")
	if err != nil {
		t.Fatalf("ReadFile after Apply: %v", err)
	}
	if string(after) != want {
		t.Errorf("Apply did not roll back: got %q, want %q", after, want)
	}

	// Exec: exit code, stream separation, and Duration.
	res, err := sb.Exec(ctx, shepherd.ExecRequest{
		Command: "sh",
		Args:    []string{"-c", "echo to-stdout; echo to-stderr >&2"},
		Cwd:     cfg.Workdir,
	})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("Exec exit = %d, want 0 (stderr: %s)", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "to-stdout") {
		t.Errorf("Exec stdout = %q, want it to contain %q", res.Stdout, "to-stdout")
	}
	// client.go wires cio.WithStreams with separate stdout/stderr buffers, so
	// the streams must not be merged. This is easy to get wrong and invisible
	// to the fakes, which just echo canned strings.
	if !strings.Contains(res.Stderr, "to-stderr") {
		t.Errorf("Exec stderr = %q, want it to contain %q", res.Stderr, "to-stderr")
	}
	if strings.Contains(res.Stdout, "to-stderr") {
		t.Error("stderr leaked into stdout: the streams are not separated")
	}
	// Plan 05 §5 item 4: the git backend leaves Duration unset, the containerd
	// backend must populate it.
	if res.Duration <= 0 {
		t.Errorf("Exec Duration = %v, want > 0 (plan 05 §5 item 4)", res.Duration)
	}

	// A non-zero exit is a result, not an error: Exec returns the code with a
	// nil error, matching the interface contract callers branch on.
	failed, err := sb.Exec(ctx, shepherd.ExecRequest{
		Command: "sh",
		Args:    []string{"-c", "exit 7"},
		Cwd:     cfg.Workdir,
	})
	if err != nil {
		t.Errorf("Exec with a non-zero exit must not return an error, got: %v", err)
	}
	if failed.ExitCode != 7 {
		t.Errorf("Exec exit = %d, want 7", failed.ExitCode)
	}

	// Diff against the captured state, with a change made after it.
	if err := sb.WriteFile(ctx, "changed.txt", []byte("added after capture\n"), 0o644); err != nil {
		t.Fatalf("WriteFile (for diff): %v", err)
	}
	diff, files, err := sb.Diff(ctx, state, 0)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if len(files) == 0 {
		t.Error("Diff reported no changed files")
	}
	var found bool
	for _, f := range files {
		if strings.TrimSpace(f) == "changed.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("Diff files = %v, want it to include changed.txt", files)
	}
	if !strings.Contains(diff, "changed.txt") {
		t.Errorf("Diff output does not mention changed.txt:\n%s", diff)
	}

	// Destroy runs again in cleanup; it is idempotent, so this only asserts the
	// explicit call succeeds.
	if err := sb.Destroy(ctx); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
}

// TestLive_NamespaceIsolation verifies the namespace wrapping in client.go
// actually takes effect. containerd resolves namespaces from the context, and
// namespaceSnapshotter plus containerdTasks.ctx exist solely to inject
// Config.Namespace on every call. If that injection were dropped the backend
// would still work — it would just silently operate on the default namespace
// alongside every other containerd user on the host. The fakes cannot catch
// that, because they have no namespaces.
func TestLive_NamespaceIsolation(t *testing.T) {
	cfg := loadLiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	sb := newLiveSandbox(t, ctx, cfg)
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	id := sb.ID()

	c := dial(t, cfg.Addr)
	defer func() {
		if err := c.Close(); err != nil {
			t.Logf("close client: %v", err)
		}
	}()

	filter := fmt.Sprintf("labels.%q==%q", sandboxLabel, id)

	// The sandbox's container must be visible in the configured namespace, via
	// the same label query client.go uses to find its own generations.
	ctrs, err := c.Containers(nsCtx(ctx, cfg.Namespace), filter)
	if err != nil {
		t.Fatalf("list containers in namespace %q: %v", cfg.Namespace, err)
	}
	if len(ctrs) == 0 {
		t.Fatalf("no container labelled %s=%s in namespace %q: the namespace was not "+
			"injected, or the label was not applied", sandboxLabel, id, cfg.Namespace)
	}

	// And it must not have leaked into the default namespace. Skipped when the
	// test is configured to use default, where the two are the same place.
	if cfg.Namespace == "default" {
		t.Skip("configured namespace is default: cross-namespace isolation is not observable")
	}
	stray, err := c.Containers(nsCtx(ctx, "default"), filter)
	if err != nil {
		t.Fatalf("list containers in namespace default: %v", err)
	}
	if len(stray) != 0 {
		t.Errorf("found %d container(s) labelled %s=%s in namespace default, want none: "+
			"the backend escaped its configured namespace", len(stray), sandboxLabel, id)
	}
}

// TestLive_DestroyRemovesLayersKeepsImageRootfs verifies teardown against the
// real snapshotter: every layer this sandbox created is gone, and the image
// rootfs — which belongs to the image, not the sandbox — survives.
//
// Removal must run children-before-parents, because a snapshot with children
// cannot be removed. The fakes enforce that rule, but only a real snapshotter
// proves the committedChain bookkeeping and the reverse iteration in Destroy
// agree with actual overlayfs parent links.
func TestLive_DestroyRemovesLayersKeepsImageRootfs(t *testing.T) {
	cfg := loadLiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	sb := newLiveSandbox(t, ctx, cfg)
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	bootstrapGit(t, ctx, sb, cfg.Workdir)
	if err := sb.WriteFile(ctx, "layer.txt", []byte("forces a second generation\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Capture commits the active layer and prepares a successor, so the sandbox
	// owns at least one committed layer plus one active layer: enough chain to
	// exercise the ordering.
	if _, err := sb.Capture(ctx); err != nil {
		t.Fatalf("Capture: %v", err)
	}

	sb.mu.Lock()
	owned := append([]string{sb.activeKey}, sb.committedChain...)
	rootfs := sb.rootfsKey
	sb.mu.Unlock()

	if rootfs == "" {
		t.Fatal("rootfsKey is empty after Create")
	}
	if len(owned) < 2 {
		t.Fatalf("expected an active layer plus at least one committed layer, got %v", owned)
	}

	c := dial(t, cfg.Addr)
	defer func() {
		if err := c.Close(); err != nil {
			t.Logf("close client: %v", err)
		}
	}()

	before := liveSnapshotKeys(t, nsCtx(ctx, cfg.Namespace), c, cfg.Snapshotter)
	for _, key := range owned {
		if !before[key] {
			t.Errorf("snapshot %q is owned by the sandbox but absent before Destroy", key)
		}
	}
	if !before[rootfs] {
		t.Errorf("image rootfs %q absent before Destroy", rootfs)
	}

	if err := sb.Destroy(ctx); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	after := liveSnapshotKeys(t, nsCtx(ctx, cfg.Namespace), c, cfg.Snapshotter)
	for _, key := range owned {
		if after[key] {
			t.Errorf("snapshot %q survived Destroy: teardown leaked a layer", key)
		}
	}
	if !after[rootfs] {
		t.Errorf("image rootfs %q was removed by Destroy: it belongs to the image, "+
			"not the sandbox, and removing it breaks every other sandbox using that image", rootfs)
	}

	// Idempotency: Destroy clears its own bookkeeping, so a second call is a
	// no-op rather than a wave of not-found errors.
	if err := sb.Destroy(ctx); err != nil {
		t.Errorf("second Destroy must be a no-op, got: %v", err)
	}
}

// TestLive_DiffPreservesPreStagedChanges is the regression test for the Sandbox
// non-mutating contract: a caller's staged-but-uncommitted work must survive a
// Diff, and Diff must still report the change it was asked about.
//
// Diff used to stage with `git add -A` and unwind with `git reset`, which
// restored the index to HEAD rather than to the state it was found in. A live
// run on 2026-10-05 confirmed the loss — the index held staged.txt before Diff
// and was empty after. It now stages into a scratch GIT_INDEX_FILE the way
// sandbox_git.go's stagedIndex does, so the real index is only ever read.
func TestLive_DiffPreservesPreStagedChanges(t *testing.T) {
	cfg := loadLiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	sb := newLiveSandbox(t, ctx, cfg)
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	bootstrapGit(t, ctx, sb, cfg.Workdir)

	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// Stage a change without committing, as a caller legitimately might.
	mustExec(t, ctx, sb, cfg.Workdir, "sh", "-c", "printf 'staged by caller\\n' > staged.txt")
	mustExec(t, ctx, sb, cfg.Workdir, "git", "add", "staged.txt")

	stagedBefore := strings.TrimSpace(mustExec(t, ctx, sb, cfg.Workdir, "git", "diff", "--cached", "--name-only"))
	if !strings.Contains(stagedBefore, "staged.txt") {
		t.Fatalf("setup failed: staged.txt is not in the index before Diff (got %q)", stagedBefore)
	}

	diff, files, err := sb.Diff(ctx, state, 0)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	// The contract under test: the caller's index is exactly as it was found.
	stagedAfter := strings.TrimSpace(mustExec(t, ctx, sb, cfg.Workdir, "git", "diff", "--cached", "--name-only"))
	if stagedAfter != stagedBefore {
		t.Errorf("Diff mutated the caller's index: staged before=%q, after=%q", stagedBefore, stagedAfter)
	}

	// And Diff still reports the change it was asked about — a scratch index that
	// preserved the caller's state but staged nothing would pass the check above
	// while returning an empty diff.
	var found bool
	for _, f := range files {
		if strings.TrimSpace(f) == "staged.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("Diff files = %v, want it to include staged.txt", files)
	}
	if !strings.Contains(diff, "staged.txt") {
		t.Errorf("Diff output does not mention staged.txt:\n%s", diff)
	}
}

// TestLive_ExecRejectsStdin pins that Exec refuses a non-empty
// ExecRequest.Stdin immediately instead of starting a process that can never see
// EOF. This is why WriteFile sends content through argv.
//
// The runc-v2 shim's execProcess.openStdin opens the exec's stdin FIFO
// O_WRONLY and keeps it in e.closers for the process's whole lifetime
// (cmd/containerd-shim-runc-v2/process/exec.go), so the FIFO always has a
// writer and an in-container reader blocks until the context expires.
// Established by goroutine dump: the client had written the payload and closed
// its own write end, and the process was still blocked reading.
//
// If a future containerd releases the shim's hold, this fails — at which point
// the argv transport, with its /proc/<pid>/cmdline exposure, is worth
// reconsidering.
func TestLive_ExecRejectsStdin(t *testing.T) {
	cfg := loadLiveConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()

	sb := newLiveSandbox(t, ctx, cfg)
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	start := time.Now()
	_, err := sb.Exec(ctx, shepherd.ExecRequest{
		Command: "cat",
		Cwd:     cfg.Workdir,
		Stdin:   []byte("never sees EOF\n"),
	})
	elapsed := time.Since(start)
	t.Logf("exec cat with stdin refused in %s: %v", elapsed.Round(time.Millisecond), err)

	if err == nil {
		t.Fatal("Exec with stdin succeeded: the constraint appears lifted, so the argv " +
			"transport in WriteFile can be reconsidered and this test inverted")
	}
	if !errors.Is(err, ErrStdinUnsupported) {
		t.Errorf("err = %v, want it to wrap ErrStdinUnsupported", err)
	}
	// Refusing up front is the whole point: a caller must not spend its context
	// budget discovering this. The ceiling is generous enough to survive a slow
	// daemon yet still catch a regression to the old hang.
	if elapsed > 5*time.Second {
		t.Errorf("Exec with stdin took %s, want an immediate refusal", elapsed)
	}
}
