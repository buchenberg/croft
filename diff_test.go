package croft

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// fakeDiffService is a programmable stand-in for the daemon-side diff
// collaborator. It records deletions so tests can assert the blob lifecycle.
type fakeDiffService struct {
	mu         sync.Mutex
	compareErr error
	desc       ocispec.Descriptor
	deleted    []digest.Digest
}

func newFakeDiffService() *fakeDiffService {
	return &fakeDiffService{
		desc: ocispec.Descriptor{
			MediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
			Digest:    digest.FromString("test-diff-blob"),
			Size:      256,
		},
	}
}

func (f *fakeDiffService) Compare(_ context.Context, _, _ []mount.Mount) (ocispec.Descriptor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.compareErr != nil {
		return ocispec.Descriptor{}, f.compareErr
	}
	return f.desc, nil
}

func (f *fakeDiffService) Read(_ context.Context, _ ocispec.Descriptor) ([]byte, error) {
	return nil, nil
}

func (f *fakeDiffService) Delete(_ context.Context, desc ocispec.Descriptor) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, desc.Digest)
	return nil
}

func (f *fakeDiffService) deletedDigests() []digest.Digest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]digest.Digest(nil), f.deleted...)
}

// newDiffTestSandbox wires a sandbox over the fakes with a diff service and
// a fresh fake lease manager, ready for diffBlob tests.
func newDiffTestSandbox(t *testing.T) (*ContainerdSandbox, *fakeSnapshotter, *fakeLeases, *fakeDiffService) {
	t.Helper()
	log := &opLog{}
	snap := newFakeSnapshotter(log)
	snap.seed("rootfs-image-digest")
	tasks := newFakeTasks(log, snap)
	images := &fakeImages{rootfs: "rootfs-image-digest"}
	fl := &fakeLeases{}
	fds := newFakeDiffService()
	sb := NewWithBackend(Config{Image: "example/dev:latest"}, snap, tasks, images,
		WithLeases(fl), WithDiffService(fds))
	return sb, snap, fl, fds
}

// diffLeaseIDs returns the ids of leases created with the diff label prefix.
func diffLeaseIDs(fl *fakeLeases) []string {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	var ids []string
	for _, l := range fl.created {
		if strings.HasPrefix(l.ID, "croft-diff-") {
			ids = append(ids, l.ID)
		}
	}
	return ids
}

// diffLeaseLabel returns the croft.lease label of the named lease, or "".
func diffLeaseLabel(fl *fakeLeases, id string) string {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	for _, l := range fl.created {
		if l.ID == id {
			return l.Labels[sandboxLeaseLabel]
		}
	}
	return ""
}

// contentPinOn reports whether lease holds a content resource for digest.
func contentPinOn(fl *fakeLeases, lease, digest string) bool {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	for _, p := range fl.added {
		if p.lease == lease && p.res.Type == "content" && p.res.ID == digest {
			return true
		}
	}
	return false
}

// contentUnpinOn reports whether lease had a content resource for digest
// removed.
func contentUnpinOn(fl *fakeLeases, lease, digest string) bool {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	for _, p := range fl.removed {
		if p.lease == lease && p.res.Type == "content" && p.res.ID == digest {
			return true
		}
	}
	return false
}

// viewKeys returns the live diff-view snapshot keys.
func viewKeys(snap *fakeSnapshotter) []string {
	var out []string
	for _, k := range snap.keys() {
		if strings.Contains(k, "/diffview/") {
			out = append(out, k)
		}
	}
	return out
}

// TestDiffBlob_SuccessPinsAndDrops pins the full lifecycle: the blob is
// leased, the content resource is added, and drop removes the pin, deletes
// the blob, deletes the lease, and the view is gone.
func TestDiffBlob_SuccessPinsAndDrops(t *testing.T) {
	sb, snap, fl, fds := newDiffTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	desc, drop, err := sb.diffBlob(ctx, state)
	if err != nil {
		t.Fatalf("diffBlob: %v", err)
	}
	if desc.Digest != fds.desc.Digest {
		t.Errorf("descriptor digest = %s, want %s", desc.Digest, fds.desc.Digest)
	}

	// The view snapshot is removed before diffBlob returns.
	if views := viewKeys(snap); len(views) != 0 {
		t.Errorf("view snapshots leaked after diffBlob returned: %v", views)
	}

	// The diff lease exists and carries the diff label.
	ids := diffLeaseIDs(fl)
	if len(ids) != 1 {
		t.Fatalf("created %d diff leases, want 1", len(ids))
	}
	if got := diffLeaseLabel(fl, ids[0]); got != "diff" {
		t.Errorf("diff lease label %s = %q, want diff", sandboxLeaseLabel, got)
	}

	// The content resource is pinned on the diff lease.
	if !contentPinOn(fl, ids[0], desc.Digest.String()) {
		t.Error("diff blob is not pinned as a content resource on the diff lease")
	}

	// drop removes everything.
	drop()

	fl.mu.Lock()
	deleted := append([]string(nil), fl.deleted...)
	fl.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != ids[0] {
		t.Errorf("deleted leases = %v, want [%s]", deleted, ids[0])
	}
	if !contentUnpinOn(fl, ids[0], desc.Digest.String()) {
		t.Error("drop did not remove the content resource pin")
	}

	deletedBlobs := fds.deletedDigests()
	if len(deletedBlobs) != 1 || deletedBlobs[0] != desc.Digest {
		t.Errorf("deleted blobs = %v, want [%s]", deletedBlobs, desc.Digest)
	}
}

// TestDiffBlob_CompareFailureDropsLease pins that a Compare failure drops
// the lease and removes the view, leaving no pin behind.
func TestDiffBlob_CompareFailureDropsLease(t *testing.T) {
	sb, snap, fl, fds := newDiffTestSandbox(t)
	fds.compareErr = errors.New("daemon diff unavailable")
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	_, _, err = sb.diffBlob(ctx, state)
	if err == nil || !strings.Contains(err.Error(), "compare snapshots") {
		t.Fatalf("err = %v, want a wrapped compare error", err)
	}

	// The view is removed on the failure path.
	if views := viewKeys(snap); len(views) != 0 {
		t.Errorf("view snapshots leaked after a failed diffBlob: %v", views)
	}

	// The diff lease was created and dropped.
	ids := diffLeaseIDs(fl)
	if len(ids) != 1 {
		t.Fatalf("created %d diff leases, want 1", len(ids))
	}
	fl.mu.Lock()
	deleted := append([]string(nil), fl.deleted...)
	var diffPins []fakePin
	for _, p := range fl.added {
		if p.lease == ids[0] {
			diffPins = append(diffPins, p)
		}
	}
	fl.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != ids[0] {
		t.Errorf("deleted leases = %v, want [%s]: the failure path must drop the lease", deleted, ids[0])
	}
	if len(diffPins) != 0 {
		t.Errorf("pins on the diff lease = %v, want none: no blob was produced to pin", diffPins)
	}
}

// TestDiffBlob_PinFailureDropsLease pins that an AddResource failure drops
// the lease. The blob is unpinned and left for the daemon's GC — that is
// best-effort cleanup, not a leak the caller can reach.
func TestDiffBlob_PinFailureDropsLease(t *testing.T) {
	sb, snap, fl, _ := newDiffTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// Arm the AddResource failure after Capture: Capture's own pinState
	// must succeed so the sandbox reaches a diffable state.
	fl.addResErr = errors.New("lease service unavailable")

	_, _, err = sb.diffBlob(ctx, state)
	if err == nil || !strings.Contains(err.Error(), "pin diff blob") {
		t.Fatalf("err = %v, want a wrapped pin error", err)
	}

	// The view is removed on the failure path.
	if views := viewKeys(snap); len(views) != 0 {
		t.Errorf("view snapshots leaked after a failed diffBlob: %v", views)
	}

	// The diff lease was created and dropped.
	ids := diffLeaseIDs(fl)
	if len(ids) != 1 {
		t.Fatalf("created %d diff leases, want 1", len(ids))
	}
	fl.mu.Lock()
	deleted := append([]string(nil), fl.deleted...)
	fl.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != ids[0] {
		t.Errorf("deleted leases = %v, want [%s]: the failure path must drop the lease", deleted, ids[0])
	}
}

// TestDiffBlob_ReleasedStateIsUnavailable pins that a state whose snapshot
// has been released surfaces ErrStateUnavailable, and the lease is dropped.
func TestDiffBlob_ReleasedStateIsUnavailable(t *testing.T) {
	sb, _, fl, _ := newDiffTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A state pointing at a snapshot key the snapshotter does not have.
	ghost := WorkspaceState{SnapshotKey: "croft/ghost/committed/1"}

	_, _, err := sb.diffBlob(ctx, ghost)
	if !errors.Is(err, ErrStateUnavailable) {
		t.Fatalf("err = %v, want ErrStateUnavailable", err)
	}

	// The diff lease was created and dropped on the failure path.
	ids := diffLeaseIDs(fl)
	if len(ids) != 1 {
		t.Fatalf("created %d diff leases, want 1", len(ids))
	}
	fl.mu.Lock()
	deleted := append([]string(nil), fl.deleted...)
	fl.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != ids[0] {
		t.Errorf("deleted leases = %v, want [%s]: the failure path must drop the lease", deleted, ids[0])
	}
}

// TestDiffBlob_CancelledContextStillDrops pins that drop is cancel-free: a
// cancelled caller context does not prevent the cleanup from running.
func TestDiffBlob_CancelledContextStillDrops(t *testing.T) {
	sb, _, fl, fds := newDiffTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// diffBlob succeeds on a cancellable context; the caller then cancels
	// before drop runs. drop must use a cancel-free context internally.
	dropCtx, cancel := context.WithCancel(ctx)
	desc, drop, err := sb.diffBlob(dropCtx, state)
	if err != nil {
		t.Fatalf("diffBlob: %v", err)
	}
	cancel()

	drop()

	ids := diffLeaseIDs(fl)
	if len(ids) != 1 {
		t.Fatalf("created %d diff leases, want 1", len(ids))
	}
	fl.mu.Lock()
	deleted := append([]string(nil), fl.deleted...)
	fl.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != ids[0] {
		t.Errorf("deleted leases = %v, want [%s]: drop must run on a cancelled context", deleted, ids[0])
	}
	if !contentUnpinOn(fl, ids[0], desc.Digest.String()) {
		t.Error("drop did not remove the content pin after cancellation")
	}
	deletedBlobs := fds.deletedDigests()
	if len(deletedBlobs) != 1 || deletedBlobs[0] != desc.Digest {
		t.Errorf("deleted blobs = %v, want [%s]: drop must delete the blob after cancellation", deletedBlobs, desc.Digest)
	}
}

// TestDiffBlob_NoLeaseManagerIsNoop pins the test-fake path: with no lease
// manager there is nothing to pin, and drop is a no-op.
func TestDiffBlob_NoLeaseManagerIsNoop(t *testing.T) {
	sb, snap, _, fds := newDiffTestSandbox(t)
	sb.leases = nil
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	desc, drop, err := sb.diffBlob(ctx, state)
	if err != nil {
		t.Fatalf("diffBlob with no lease manager: %v", err)
	}
	if desc.Digest != fds.desc.Digest {
		t.Errorf("descriptor digest = %s, want %s", desc.Digest, fds.desc.Digest)
	}

	// The view is still removed.
	if views := viewKeys(snap); len(views) != 0 {
		t.Errorf("view snapshots leaked: %v", views)
	}

	// drop is a no-op: it must not panic.
	drop()
}

// TestDiffBlob_NoDiffService pins the nil-collaborator error: a sandbox built
// without WithDiffService fails loudly rather than panicking.
func TestDiffBlob_NoDiffService(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	_, _, err = sb.diffBlob(ctx, state)
	if err == nil || !strings.Contains(err.Error(), "diff service not configured") {
		t.Fatalf("err = %v, want a diff-service-not-configured error", err)
	}
}

// TestDiffBlob_BeforeCreate pins the guard: diffBlob before Create has no
// active layer to diff against.
func TestDiffBlob_BeforeCreate(t *testing.T) {
	sb, _, _, _ := newDiffTestSandbox(t)
	ctx := context.Background()

	_, _, err := sb.diffBlob(ctx, WorkspaceState{SnapshotKey: "anything"})
	if err == nil || !strings.Contains(err.Error(), "diff before Create") {
		t.Fatalf("err = %v, want a diff-before-Create error", err)
	}
}

// TestDiffBlob_LeaseCreateFailure pins that a lease creation failure
// surfaces as an error with no drop to run.
func TestDiffBlob_LeaseCreateFailure(t *testing.T) {
	sb, _, fl, _ := newDiffTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// Arm the lease creation failure after Create and Capture: both need
	// their own leases to succeed first.
	fl.createErr = errors.New("lease service down")
	_, drop, err := sb.diffBlob(ctx, state)
	if err == nil || !strings.Contains(err.Error(), "create diff lease") {
		t.Fatalf("err = %v, want a wrapped create-diff-lease error", err)
	}
	if drop != nil {
		t.Error("drop must be nil when the lease was never created")
	}
}

// TestDiffBlob_ConcurrentCallsGetDistinctViews pins that concurrent diffBlob
// calls get distinct view keys (the diffSeq counter), so neither collides
// with the other's View.
func TestDiffBlob_ConcurrentCallsGetDistinctViews(t *testing.T) {
	sb, snap, _, _ := newDiffTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	const goroutines = 8
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	drops := make([]func(), goroutines)

	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, drop, err := sb.diffBlob(ctx, state)
			errs[i] = err
			drops[i] = drop
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("diffBlob from goroutine %d: %v", i, err)
		}
	}
	for _, drop := range drops {
		drop()
	}

	// No view leaked from any goroutine.
	if views := viewKeys(snap); len(views) != 0 {
		t.Errorf("view snapshots leaked after concurrent diffBlob: %v", views)
	}
}
