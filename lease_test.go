package containerd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	shepherd "github.com/buchenberg/shepherd-kernel-go"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
)

// fakePin is one recorded resource operation, carrying the lease it targeted
// so tests can assert the pin landed on the states lease — and not, say, the
// sandbox's own — rather than only that *some* lease received it.
type fakePin struct {
	lease string
	res   leases.Resource
}

// fakeLeases records the lease lifecycle. The daemon's real manager is the only
// thing that can prove a snapshot is protected (see
// TestLive_LeaseHoldsSnapshots); this exists so the sandbox's own bookkeeping —
// create once, label, delete once, tolerate a vanished lease, pin and unpin
// states — is testable without a daemon.
//
// It mirrors the daemon where the adapter depends on it: creating a lease whose
// id already exists fails with errdefs.ErrAlreadyExists, which pinState relies
// on to adopt the shared states lease.
//
// Mutex-guarded because the concurrency test calls into it from many goroutines.
type fakeLeases struct {
	mu            sync.Mutex
	created       []leases.Lease
	deleted       []string
	existing      map[string]bool
	added         []fakePin
	removed       []fakePin
	lastLabel     map[string]string
	createErr     error
	deleteErr     error
	addResErr     error
	delResErr     error
	createBlocks  bool // Create waits for the context instead of returning
	createStarted chan struct{}
}

func (f *fakeLeases) Create(ctx context.Context, opts ...leases.Opt) (leases.Lease, error) {
	f.mu.Lock()
	block := f.createBlocks
	if f.createStarted != nil {
		select {
		case f.createStarted <- struct{}{}:
		default:
		}
	}
	err := f.createErr
	f.mu.Unlock()

	if block {
		<-ctx.Done()
		return leases.Lease{}, ctx.Err()
	}
	if err != nil {
		return leases.Lease{}, err
	}

	l := leases.Lease{}
	for _, o := range opts {
		if err := o(&l); err != nil {
			return leases.Lease{}, err
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if l.ID != "" && f.existing[l.ID] {
		return leases.Lease{}, errdefs.ErrAlreadyExists
	}
	if f.existing == nil {
		f.existing = make(map[string]bool)
	}
	f.existing[l.ID] = true
	f.lastLabel = l.Labels
	f.created = append(f.created, l)
	return l, nil
}

func (f *fakeLeases) Delete(_ context.Context, l leases.Lease, _ ...leases.DeleteOpt) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, l.ID)
	return nil
}

func (f *fakeLeases) counts() (created, deleted int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.created), len(f.deleted)
}

func (f *fakeLeases) List(context.Context, ...string) ([]leases.Lease, error) { return nil, nil }

func (f *fakeLeases) AddResource(_ context.Context, l leases.Lease, r leases.Resource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addResErr != nil {
		return f.addResErr
	}
	f.added = append(f.added, fakePin{lease: l.ID, res: r})
	return nil
}

func (f *fakeLeases) DeleteResource(_ context.Context, l leases.Lease, r leases.Resource) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.delResErr != nil {
		return f.delResErr
	}
	f.removed = append(f.removed, fakePin{lease: l.ID, res: r})
	return nil
}

// pinnedOn reports whether the states lease holds id as a snapshotter resource.
func (f *fakeLeases) pinnedOn(lease, id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.added {
		if p.lease == lease && p.res.ID == id && strings.HasPrefix(p.res.Type, "snapshots/") {
			return true
		}
	}
	return false
}

// releasedFrom reports whether a pin for id was dropped from the states lease.
func (f *fakeLeases) releasedFrom(lease, id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, p := range f.removed {
		if p.lease == lease && p.res.ID == id && strings.HasPrefix(p.res.Type, "snapshots/") {
			return true
		}
	}
	return false
}

func (f *fakeLeases) ListResources(context.Context, leases.Lease) ([]leases.Resource, error) {
	return nil, nil
}

// TestNamespaceSnapshotter_InjectsLeaseOnlyWhenHeld is the load-bearing unit
// test for the GC fix. containerd associates a snapshot with a lease only when
// the lease id is in the context of the call that creates it, so a lease that
// never reaches this context protects nothing and the flake would return.
func TestNamespaceSnapshotter_InjectsLeaseOnlyWhenHeld(t *testing.T) {
	var ref leaseRef
	n := namespaceSnapshotter{ns: "shepherd-test", lease: &ref}

	// No lease held: the namespace is still injected, and nothing is registered
	// under a lease that does not exist (addSnapshotLease errors when it does).
	got := n.ctx(context.Background())
	if ns, ok := namespaces.Namespace(got); !ok || ns != "shepherd-test" {
		t.Fatalf("namespace = %q (present %v), want shepherd-test", ns, ok)
	}
	if id, ok := leases.FromContext(got); ok {
		t.Fatalf("lease = %q present with none held, want absent", id)
	}

	// Lease held: both the namespace and the lease, which is what makes the
	// sandbox's snapshots GC roots for the lease's lifetime.
	ref.set("lease-1")
	got = n.ctx(context.Background())
	if id, ok := leases.FromContext(got); !ok || id != "lease-1" {
		t.Fatalf("lease = %q (present %v), want lease-1", id, ok)
	}
	if ns, ok := namespaces.Namespace(got); !ok || ns != "shepherd-test" {
		t.Fatalf("namespace = %q (present %v) alongside a lease, want shepherd-test", ns, ok)
	}
}

// TestNamespaceSnapshotter_NilRefIsSafe covers a sandbox whose wrapper was built
// without a lease reference, which is how the substituted backends behave.
func TestNamespaceSnapshotter_NilRefIsSafe(t *testing.T) {
	n := namespaceSnapshotter{ns: "shepherd-test"}
	if _, ok := leases.FromContext(n.ctx(context.Background())); ok {
		t.Fatal("lease present from a wrapper with no lease reference")
	}
}

func TestLeaseRef_SetGetClear(t *testing.T) {
	var ref leaseRef
	if got := ref.get(); got != "" {
		t.Fatalf("zero-value get() = %q, want empty", got)
	}
	ref.set("a")
	if got := ref.get(); got != "a" {
		t.Fatalf("get() = %q, want a", got)
	}
	ref.set("")
	if got := ref.get(); got != "" {
		t.Fatalf("get() after clear = %q, want empty", got)
	}
}

func TestEnsureLease_CreatesOnceAndLabelsIt(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "")
	fl := &fakeLeases{}
	sb.leases = fl

	if err := sb.ensureLease(context.Background()); err != nil {
		t.Fatalf("ensureLease: %v", err)
	}
	if len(fl.created) != 1 {
		t.Fatalf("created %d leases, want 1", len(fl.created))
	}
	if got := sb.lease.get(); got == "" || got != fl.created[0].ID {
		t.Fatalf("sandbox lease = %q, want the created lease %q", got, fl.created[0].ID)
	}
	// The label is what makes an orphan from a crashed process findable, since
	// the lease and the sandbox share no other identifier.
	if got := fl.lastLabel[sandboxLeaseLabel]; got != "sandbox" {
		t.Fatalf("label %s = %q, want sandbox", sandboxLeaseLabel, got)
	}

	// A second call must not create a second lease: it has to span the whole
	// sandbox lifetime, including the gaps between generations.
	if err := sb.ensureLease(context.Background()); err != nil {
		t.Fatalf("ensureLease (second): %v", err)
	}
	if len(fl.created) != 1 {
		t.Fatalf("created %d leases across two calls, want 1", len(fl.created))
	}
}

func TestEnsureLease_NoManagerIsNoop(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "")
	// The default test wiring installs a fake manager; clear it to exercise
	// the no-manager path.
	sb.leases = nil
	if err := sb.ensureLease(context.Background()); err != nil {
		t.Fatalf("ensureLease with no manager: %v", err)
	}
	if got := sb.lease.get(); got != "" {
		t.Fatalf("lease = %q, want empty", got)
	}
}

func TestEnsureLease_PropagatesFailure(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "")
	sb.leases = &fakeLeases{createErr: errors.New("boom")}

	err := sb.ensureLease(context.Background())
	if err == nil || !strings.Contains(err.Error(), "create lease") {
		t.Fatalf("err = %v, want a wrapped create-lease error", err)
	}
	if got := sb.lease.get(); got != "" {
		t.Fatalf("lease = %q after a failed create, want empty", got)
	}
}

func TestReleaseLease_DeletesOnceAndClears(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "")
	fl := &fakeLeases{}
	sb.leases = fl
	sb.lease.set("lease-1")

	if err := sb.releaseLease(context.Background()); err != nil {
		t.Fatalf("releaseLease: %v", err)
	}
	if len(fl.deleted) != 1 || fl.deleted[0] != "lease-1" {
		t.Fatalf("deleted = %v, want [lease-1]", fl.deleted)
	}
	if got := sb.lease.get(); got != "" {
		t.Fatalf("lease = %q after release, want empty", got)
	}

	// Destroy is documented idempotent, so a second release must be silent
	// rather than deleting whatever the id now happens to be.
	if err := sb.releaseLease(context.Background()); err != nil {
		t.Fatalf("releaseLease (second): %v", err)
	}
	if len(fl.deleted) != 1 {
		t.Fatalf("deleted = %v after two releases, want one", fl.deleted)
	}
}

func TestReleaseLease_ToleratesAlreadyDeleted(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "")
	sb.leases = &fakeLeases{deleteErr: fmt.Errorf("lease gone: %w", errdefs.ErrNotFound)}
	sb.lease.set("gone")

	if err := sb.releaseLease(context.Background()); err != nil {
		t.Fatalf("releaseLease on a vanished lease: %v; Destroy must stay idempotent", err)
	}
}

func TestReleaseLease_PropagatesRealFailure(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "")
	sb.leases = &fakeLeases{deleteErr: errors.New("daemon down")}
	sb.lease.set("lease-1")

	err := sb.releaseLease(context.Background())
	if err == nil || !strings.Contains(err.Error(), "delete lease") {
		t.Fatalf("err = %v, want a wrapped delete-lease error", err)
	}
}

// TestCreate_Destroy_CreateAcquiresAFreshLease pins that a reused instance
// re-leases itself. Destroy drops the lease, so without ensureLease running on
// every Create a second generation would silently run unprotected.
func TestCreate_Destroy_CreateAcquiresAFreshLease(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "")
	fl := &fakeLeases{}
	sb.leases = fl
	ctx := context.Background()

	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(fl.created) != 1 {
		t.Fatalf("Create created %d leases, want 1", len(fl.created))
	}
	first := fl.created[0].ID

	if err := sb.Destroy(ctx); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if len(fl.deleted) != 1 || fl.deleted[0] != first {
		t.Fatalf("Destroy deleted %v, want [%s]", fl.deleted, first)
	}
	if got := sb.lease.get(); got != "" {
		t.Fatalf("lease = %q after Destroy, want empty", got)
	}

	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create (second generation): %v", err)
	}
	if len(fl.created) != 2 {
		t.Fatalf("created %d leases across two generations, want 2: a reused sandbox "+
			"must re-lease itself, or its snapshots are unprotected", len(fl.created))
	}
	if got := sb.lease.get(); got != fl.created[1].ID {
		t.Fatalf("lease = %q, want the second generation's %q", got, fl.created[1].ID)
	}
}

// TestEnsureLease_ConcurrentCallsCreateOneLease pins the check-then-create
// critical section. ContainerdSandbox is documented safe for concurrent use, and
// without the lock two concurrent Create/Apply calls both observe an empty
// reference, both create a lease, and the overwritten one is never released —
// pinning this sandbox's snapshots against the GC for good, which is the failure
// the lease exists to prevent.
func TestEnsureLease_ConcurrentCallsCreateOneLease(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "")
	fl := &fakeLeases{}
	sb.leases = fl

	const goroutines = 16
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	start := make(chan struct{})

	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = sb.ensureLease(context.Background())
		}()
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("ensureLease from goroutine %d: %v", i, err)
		}
	}
	created, _ := fl.counts()
	if created != 1 {
		t.Fatalf("created %d leases across %d concurrent calls, want 1: an overwritten "+
			"lease is never released and pins this sandbox's snapshots forever",
			created, goroutines)
	}
	if got := sb.lease.get(); got == "" {
		t.Fatal("lease is empty after concurrent acquisition succeeded")
	}
}

// TestReleaseLease_FailedDeleteKeepsTheIDForRetry pins that a transient delete
// failure leaves the id in place. Discarding it would make a later Destroy
// report success while the orphaned lease keeps snapshots pinned forever.
func TestReleaseLease_FailedDeleteKeepsTheIDForRetry(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "")
	fl := &fakeLeases{deleteErr: errors.New("daemon unavailable")}
	sb.leases = fl
	sb.lease.set("lease-1")

	if err := sb.releaseLease(context.Background()); err == nil {
		t.Fatal("releaseLease reported success despite a failing Delete")
	}
	if got := sb.lease.get(); got != "lease-1" {
		t.Fatalf("lease = %q after a failed delete, want lease-1 so a retry can reach it", got)
	}
	if _, deleted := fl.counts(); deleted != 0 {
		t.Fatalf("deleted %d leases on a failing Delete, want 0", deleted)
	}

	// The retry must actually delete, and only then clear the id.
	fl.deleteErr = nil
	if err := sb.releaseLease(context.Background()); err != nil {
		t.Fatalf("releaseLease after the daemon recovered: %v", err)
	}
	if _, deleted := fl.counts(); deleted != 1 {
		t.Fatalf("deleted %d leases after recovery, want 1", deleted)
	}
	if got := sb.lease.get(); got != "" {
		t.Fatalf("lease = %q after a successful retry, want empty", got)
	}
}

// TestCreate_TimeoutCoversLeaseAcquisition pins that SandboxSpec.Timeout bounds
// the whole Create, including the lease RPC. Deriving the timeout after the
// lease call would let a blocked lease service hang past the caller's limit.
//
// Create runs in a goroutine so a regression fails on the deadline instead of
// hanging the suite.
func TestCreate_TimeoutCoversLeaseAcquisition(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "")
	sb.leases = &fakeLeases{createBlocks: true}

	done := make(chan error, 1)
	go func() {
		done <- sb.Create(context.Background(), shepherd.SandboxSpec{Timeout: 200 * time.Millisecond})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Create succeeded although the lease RPC never returned")
		}
		if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "create lease") {
			t.Fatalf("err = %v, want the deadline to surface through the lease failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Create did not return within 5s despite a 200ms Timeout: the timeout " +
			"does not cover backend connection and lease acquisition")
	}
}

// TestDiff_StagesOncePerCall pins that Diff produces the file list and the
// unified diff from a single staging pass. Two passes would each create their own
// scratch index, so a write to the workspace between them would make the returned
// list and diff describe different states.
func TestDiff_StagesOncePerCall(t *testing.T) {
	sb, _, tasks, _ := newTestSandbox(t, "deadbeef")
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	stagingRuns := func() int {
		n := 0
		for _, req := range tasks.allReqs {
			if req.Command == "sh" && len(req.Args) >= 2 &&
				strings.Contains(req.Args[1], "git add -A") {
				n++
			}
		}
		return n
	}

	before := stagingRuns()
	diff, files, err := sb.Diff(ctx, state, 0)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if got := stagingRuns() - before; got != 1 {
		t.Errorf("Diff staged %d times, want 1: a second staging can describe a different "+
			"workspace state than the first", got)
	}

	// Drift guard: the parser and the script must agree on the separator.
	if !strings.Contains(diffScript, diffNamesMarker) {
		t.Fatalf("diffScript does not emit %q, so Diff cannot separate its two outputs",
			diffNamesMarker)
	}

	// Both outputs must come back populated from that one pass.
	if len(files) != 1 || files[0] != "f" {
		t.Errorf("files = %v, want [f] from the staging pass", files)
	}
	if !strings.Contains(diff, "+change") {
		t.Errorf("diff = %q, want the unified-diff section", diff)
	}
}

// TestDiff_MissingSeparatorIsAnError pins that a malformed staging output is
// reported rather than silently returning an empty file list with a diff.
func TestDiff_MissingSeparatorIsAnError(t *testing.T) {
	sb, _, tasks, _ := newTestSandbox(t, "deadbeef")
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	tasks.diffStdout = "no separator here\n"

	if _, _, err := sb.Diff(ctx, state, 0); err == nil {
		t.Fatal("Diff accepted output with no separator, so the file list and diff " +
			"cannot be told apart")
	} else if !strings.Contains(err.Error(), diffNamesMarker) {
		t.Fatalf("err = %v, want it to name the missing separator", err)
	}
}

// TestCapture_PinsCommittedStatesUnderStatesLease is the load-bearing unit test
// for state durability. The kernel's contract is that a WorkspaceState outlives
// the sandbox, and the sandbox lease cannot provide that: Destroy releases it.
// Every Capture must therefore pin its committed layer under the namespace's
// shared states lease, and that lease must be adopted (not duplicated) by later
// captures and sibling sandboxes.
func TestCapture_PinsCommittedStatesUnderStatesLease(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "abc")
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

	// Exactly one states lease, labeled so an operator can find it, despite two
	// captures having ensured it.
	var states []leases.Lease
	for _, l := range fl.created {
		if l.ID == statesLeaseID {
			states = append(states, l)
		}
	}
	if len(states) != 1 {
		t.Fatalf("created the states lease %d times across two captures, want 1", len(states))
	}
	if got := states[0].Labels[sandboxLeaseLabel]; got != "states" {
		t.Fatalf("states lease label %s = %q, want states", sandboxLeaseLabel, got)
	}

	// Both committed layers are pinned as snapshotter resources — on the states
	// lease specifically, not merely on whichever lease came first.
	if !fl.pinnedOn(statesLeaseID, k1) {
		t.Errorf("state %s is not pinned: a state must be a GC root beyond the sandbox lease", k1)
	}
	if !fl.pinnedOn(statesLeaseID, k2) {
		t.Errorf("state %s is not pinned: a state must be a GC root beyond the sandbox lease", k2)
	}
}

// TestCapture_PinFailureRollsForwardNotOut pins that a state is only handed to
// the caller when its pin exists. A committed-but-unpinned layer is exactly the
// GC-reclaimable window the states lease exists to close, so a failed pin must
// fail the Capture. Because Commit renames the active key, recovery rolls
// forward onto the committed layer as an internal layer: the sandbox stays
// usable and no half-state is handed out.
func TestCapture_PinFailureRollsForwardNotOut(t *testing.T) {
	sb, snap, tasks, _ := newTestSandbox(t, "abc")
	fl := &fakeLeases{addResErr: errors.New("lease service unavailable")}
	sb.leases = fl
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	before := len(snap.keys())

	if _, err := sb.Capture(ctx); err == nil || !strings.Contains(err.Error(), "pin captured state") {
		t.Fatalf("err = %v, want a wrapped pin-captured-state error", err)
	}
	if !tasks.isRunning() {
		t.Error("task is not running after a failed pin; the sandbox must roll forward " +
			"onto the committed layer and stay usable")
	}
	if len(fl.added) != 0 {
		t.Errorf("pinned resources = %v, want none: no state may be registered on a "+
			"failed capture", fl.added)
	}
	// One internal committed layer and its successor active layer were added —
	// the rename means the old active key is gone, so the count moves by one.
	if got := len(snap.keys()); got != before+1 {
		t.Errorf("snapshot count = %d after a failed pin, want %d (internal layer + "+
			"successor, rename minus old active)", got, before+1)
	}

	// Recovery: the same sandbox captures successfully once the lease service
	// answers again, and that capture is pinned.
	fl.addResErr = nil
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture after recovery: %v", err)
	}
	key, err := stateSnapshotKey(state)
	if err != nil {
		t.Fatalf("state after recovery: %v", err)
	}
	if !fl.pinnedOn(statesLeaseID, key) {
		t.Errorf("the post-recovery state %s is not pinned", key)
	}
}

// failingStartTask wraps a taskService whose StartTask fails once armed, so a
// test can break exactly the roll-forward's restart while Create succeeds.
type failingStartTask struct {
	taskService
	armed bool
}

func (f *failingStartTask) StartTask(ctx context.Context, id, key string) error {
	if f.armed {
		return errors.New("shim down")
	}
	return f.taskService.StartTask(ctx, id, key)
}

// TestCapture_RollForwardFailureLeavesCleanlyStoppedSandbox pins the wedge
// recovery for the leak path: the pin SUCCEEDS but the successor cannot be
// started (advance fails), so Capture errors without handing out a state. The
// just-created pin must be dropped — the caller has no handle to ever release
// it, and it would hold disk until namespace teardown — and bookkeeping must
// not keep pointing at the active key Commit renamed away. A ghost activeKey
// would fail every later Capture on a nonexistent key and make Destroy's first
// call error removing it; instead the sandbox reports itself stopped and
// Destroy succeeds in one call.
func TestCapture_RollForwardFailureLeavesCleanlyStoppedSandbox(t *testing.T) {
	sb, snap, _, _ := newTestSandbox(t, "abc")
	fl := &fakeLeases{}
	sb.leases = fl
	tasks := &failingStartTask{taskService: sb.tasks}
	sb.tasks = tasks
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	tasks.armed = true

	if _, err := sb.Capture(ctx); err == nil || !strings.Contains(err.Error(), "restart task after capture") {
		t.Fatalf("err = %v, want the advance error from the failed restart", err)
	}

	// The committed layer exists (Commit succeeded; Commit cannot be undone),
	// recorded as lineage — not as a resumable active layer.
	var committed string
	for _, k := range snap.keys() {
		if strings.Contains(k, "/committed/") {
			committed = k
		}
	}
	if committed == "" {
		t.Fatal("no committed layer after a failed roll-forward; the commit did happen")
	}

	// The pin that succeeded was dropped on the way out: the caller never
	// received a state, so the pin would otherwise be unreachable and permanent.
	if !fl.releasedFrom(statesLeaseID, committed) {
		t.Errorf("the orphaned pin on %s was not released; it would hold disk until "+
			"namespace teardown with no handle left to release it", committed)
	}

	// A later Capture must fail on the guard, not on committing a ghost key.
	if _, err := sb.Capture(ctx); err == nil || !strings.Contains(err.Error(), "Capture before Create") {
		t.Fatalf("Capture after a failed roll-forward = %v, want the cleanly-stopped guard error", err)
	}

	// Destroy succeeds on the first call: there is no ghost active key to
	// remove, so no not-found error and no second-call workaround.
	if err := sb.Destroy(ctx); err != nil {
		t.Fatalf("Destroy after a failed roll-forward: %v; the sandbox must be "+
			"cleanly stopped, not wedged on a key the snapshotter no longer has", err)
	}
}

// TestReleaseState_DropsTheStatesLeasePin pins the discard path: releasing
// removes exactly the state's resource, stays idempotent, and tolerates a
// resource the daemon already reclaimed — while a real failure surfaces.
func TestReleaseState_DropsTheStatesLeasePin(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "abc")
	fl := &fakeLeases{}
	sb.leases = fl
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	key, err := stateSnapshotKey(state)
	if err != nil {
		t.Fatalf("state: %v", err)
	}

	if err := sb.ReleaseState(ctx, state); err != nil {
		t.Fatalf("ReleaseState: %v", err)
	}
	want := leases.Resource{ID: key, Type: "snapshots/" + defaultSnapshotter}
	if len(fl.removed) != 1 || fl.removed[0].res != want || fl.removed[0].lease != statesLeaseID {
		t.Fatalf("removed = %+v, want the %s resource %v on the states lease", fl.removed, statesLeaseID, want)
	}

	// Idempotent, and a resource the daemon already dropped is not an error.
	if err := sb.ReleaseState(ctx, state); err != nil {
		t.Fatalf("ReleaseState (second): %v", err)
	}
	fl.delResErr = fmt.Errorf("resource gone: %w", errdefs.ErrNotFound)
	if err := sb.ReleaseState(ctx, state); err != nil {
		t.Fatalf("ReleaseState on a vanished resource = %v, want nil", err)
	}

	// A real failure must surface: silently losing the release would pin disk
	// forever with the caller believing it was returned.
	fl.delResErr = errors.New("daemon down")
	if err := sb.ReleaseState(ctx, state); err == nil || !strings.Contains(err.Error(), "unpin") {
		t.Fatalf("err = %v, want a wrapped unpin error", err)
	}
}

// TestReleaseState_RejectsForeignAndIncompleteStates pins the guard rails: a
// git backend's state must never reach the containerd lease manager, and a
// containerd state without a snapshot key is a bug, not a no-op.
func TestReleaseState_RejectsForeignAndIncompleteStates(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "")
	fl := &fakeLeases{}
	sb.leases = fl
	ctx := context.Background()

	if err := sb.ReleaseState(ctx, shepherd.WorkspaceState{Backend: "git", Revision: "abc123"}); err == nil {
		t.Error("ReleaseState accepted a git state: it would have unpinned a key the " +
			"git backend knows nothing about")
	}
	if err := sb.ReleaseState(ctx, shepherd.WorkspaceState{Backend: BackendName}); err == nil {
		t.Error("ReleaseState accepted a state with no snapshot key")
	}
	if len(fl.removed) != 0 {
		t.Fatalf("removed = %v, want nothing: rejected states must not touch the lease", fl.removed)
	}
}

// TestReleaseState_NoManagerIsNoop covers the no-manager path: with no lease
// manager configured there is nothing to unpin, and ReleaseState must say so
// quietly rather than failing.
func TestReleaseState_NoManagerIsNoop(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t, "abc")
	sb.leases = nil
	ctx := context.Background()
	if err := sb.Create(ctx, shepherd.SandboxSpec{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if err := sb.ReleaseState(ctx, state); err != nil {
		t.Fatalf("ReleaseState with no manager: %v", err)
	}
}
