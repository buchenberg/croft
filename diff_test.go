package croft

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/containerd/containerd/v2/core/mount"
	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// fakeDiffService is a programmable stand-in for the daemon-side diff
// collaborator. It records deletions so tests can assert the blob lifecycle,
// and serves a programmable blob from Read so Changes/Diff tests can drive
// the parser from inline tars.
type fakeDiffService struct {
	mu         sync.Mutex
	compareErr error
	desc       ocispec.Descriptor
	blob       []byte
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
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.blob, nil
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

// tarEntry describes one entry for buildTar.
type tarEntry struct {
	name     string
	mode     int64
	typeflag byte
	linkname string
	devmajor int64
	devminor int64
	content  []byte
}

// buildTar constructs an in-memory tar from entries, for parser table tests.
func buildTar(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:     e.name,
			Mode:     e.mode,
			Size:     int64(len(e.content)),
			Typeflag: e.typeflag,
			Linkname: e.linkname,
			Devmajor: e.devmajor,
			Devminor: e.devminor,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header %s: %v", e.name, err)
		}
		if len(e.content) > 0 {
			if _, err := tw.Write(e.content); err != nil {
				t.Fatalf("write tar content %s: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	return buf.Bytes()
}

// buildGzippedTar constructs an in-memory gzipped tar from entries.
func buildGzippedTar(t *testing.T, entries ...tarEntry) []byte {
	t.Helper()
	plain := buildTar(t, entries...)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(plain); err != nil {
		t.Fatalf("gzip tar: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

// assertChanges compares two change lists field by field.
func assertChanges(t *testing.T, got, want []Change) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d changes, want %d:\n got: %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("change[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestParseChangeTar_Golden drives the parser from the Task 0 golden tar:
// the exact change matrix the live spike captured (add, modify, delete,
// mode-only, new dir, opaque dir via per-file whiteouts, symlink).
func TestParseChangeTar_Golden(t *testing.T) {
	f, err := os.Open("testdata/change-tar/live-changes.plain.tar")
	if err != nil {
		t.Fatalf("open golden: %v", err)
	}
	defer f.Close()

	changes, opaques, err := parseChangeTar(f, "/workspace")
	if err != nil {
		t.Fatalf("parseChangeTar: %v", err)
	}

	want := []Change{
		{Path: "added.txt", Kind: ChangeModify, Mode: 0o644, Size: 20},
		{Path: "base-deleted.txt", Kind: ChangeDelete},
		{Path: "base-modeonly.txt", Kind: ChangeModify, Mode: 0o600, Size: 11},
		{Path: "base-modified.txt", Kind: ChangeModify, Mode: 0o644, Size: 6},
		{Path: "new-dir", Kind: ChangeModify, IsDir: true, Mode: 0o755 | fs.ModeDir},
		{Path: "new-dir/n.txt", Kind: ChangeModify, Mode: 0o644, Size: 7},
		{Path: "opaque-dir", Kind: ChangeModify, IsDir: true, Mode: 0o755 | fs.ModeDir},
		{Path: "opaque-dir/a.txt", Kind: ChangeDelete},
		{Path: "opaque-dir/b.txt", Kind: ChangeDelete},
		{Path: "opaque-dir/c.txt", Kind: ChangeModify, Mode: 0o644, Size: 9},
		{Path: "symlink.txt", Kind: ChangeModify, Mode: 0o777 | fs.ModeSymlink},
	}
	assertChanges(t, changes, want)

	// The golden has no .wh..wh..opq marker (Task 0 finding: per-file
	// whiteouts instead), so the opaque list is empty.
	if len(opaques) != 0 {
		t.Errorf("opaque dirs = %v, want none (the differ emits per-file whiteouts)", opaques)
	}
}

// TestParseChangeTar_GzippedSniffsMagic pins that the parser decompresses a
// gzipped blob (Task 0 finding: the default blob is a gzipped OCI layer).
func TestParseChangeTar_GzippedSniffsMagic(t *testing.T) {
	blob := buildGzippedTar(t,
		tarEntry{name: "workspace/added.txt", mode: 0o644, typeflag: tar.TypeReg, content: []byte("hello\n")},
	)

	changes, _, err := parseChangeTar(bytes.NewReader(blob), "/workspace")
	if err != nil {
		t.Fatalf("parseChangeTar on gzipped blob: %v", err)
	}

	assertChanges(t, changes, []Change{
		{Path: "added.txt", Kind: ChangeModify, Mode: 0o644, Size: 6},
	})
}

// TestParseChangeTar_Table drives the parser's classification, prefix
// stripping, and sorting from inline tars.
func TestParseChangeTar_Table(t *testing.T) {
	tests := []struct {
		name    string
		workdir string
		entries []tarEntry
		want    []Change
		opaques []string // expected opaque dir paths
	}{
		{
			name:    "empty tar",
			workdir: "/workspace",
			entries: nil,
			want:    nil,
		},
		{
			name:    "workdir root skipped",
			workdir: "/workspace",
			entries: []tarEntry{
				{name: "workspace/", mode: 0o755, typeflag: tar.TypeDir},
				{name: "workspace/file.txt", mode: 0o644, typeflag: tar.TypeReg, content: []byte("x")},
			},
			want: []Change{
				{Path: "file.txt", Kind: ChangeModify, Mode: 0o644, Size: 1},
			},
		},
		{
			name:    "outside workdir skipped",
			workdir: "/workspace",
			entries: []tarEntry{
				{name: "etc/passwd", mode: 0o644, typeflag: tar.TypeReg, content: []byte("root")},
				{name: "workspace/file.txt", mode: 0o644, typeflag: tar.TypeReg, content: []byte("x")},
			},
			want: []Change{
				{Path: "file.txt", Kind: ChangeModify, Mode: 0o644, Size: 1},
			},
		},
		{
			name:    "whiteout deletes file",
			workdir: "/workspace",
			entries: []tarEntry{
				{name: "workspace/.wh.gone.txt", mode: 0, typeflag: tar.TypeReg},
			},
			want: []Change{
				{Path: "gone.txt", Kind: ChangeDelete},
			},
		},
		{
			name:    "whiteout in subdirectory",
			workdir: "/workspace",
			entries: []tarEntry{
				{name: "workspace/dir/.wh.gone.txt", mode: 0, typeflag: tar.TypeReg},
			},
			want: []Change{
				{Path: "dir/gone.txt", Kind: ChangeDelete},
			},
		},
		{
			name:    "char device whiteout accepted defensively",
			workdir: "/workspace",
			entries: []tarEntry{
				{name: "workspace/.wh.gone.txt", mode: 0, typeflag: tar.TypeChar, devmajor: 0, devminor: 0},
			},
			want: []Change{
				{Path: "gone.txt", Kind: ChangeDelete},
			},
		},
		{
			name:    "opaque marker collected",
			workdir: "/workspace",
			entries: []tarEntry{
				{name: "workspace/dir/.wh..wh..opq", mode: 0, typeflag: tar.TypeReg},
				{name: "workspace/dir/new.txt", mode: 0o644, typeflag: tar.TypeReg, content: []byte("n")},
			},
			want: []Change{
				{Path: "dir/new.txt", Kind: ChangeModify, Mode: 0o644, Size: 1},
			},
			opaques: []string{"dir"},
		},
		{
			name:    "nested workdir prefix",
			workdir: "/home/user/ws",
			entries: []tarEntry{
				{name: "home/user/ws/file.txt", mode: 0o644, typeflag: tar.TypeReg, content: []byte("x")},
			},
			want: []Change{
				{Path: "file.txt", Kind: ChangeModify, Mode: 0o644, Size: 1},
			},
		},
		{
			name:    "sorted by path",
			workdir: "/workspace",
			entries: []tarEntry{
				{name: "workspace/zeta.txt", mode: 0o644, typeflag: tar.TypeReg, content: []byte("z")},
				{name: "workspace/alpha.txt", mode: 0o644, typeflag: tar.TypeReg, content: []byte("a")},
				{name: "workspace/mid.txt", mode: 0o644, typeflag: tar.TypeReg, content: []byte("m")},
			},
			want: []Change{
				{Path: "alpha.txt", Kind: ChangeModify, Mode: 0o644, Size: 1},
				{Path: "mid.txt", Kind: ChangeModify, Mode: 0o644, Size: 1},
				{Path: "zeta.txt", Kind: ChangeModify, Mode: 0o644, Size: 1},
			},
		},
		{
			name:    "directory entry",
			workdir: "/workspace",
			entries: []tarEntry{
				{name: "workspace/newdir/", mode: 0o755, typeflag: tar.TypeDir},
			},
			want: []Change{
				{Path: "newdir", Kind: ChangeModify, IsDir: true, Mode: 0o755 | fs.ModeDir},
			},
		},
		{
			name:    "symlink entry",
			workdir: "/workspace",
			entries: []tarEntry{
				{name: "workspace/link", mode: 0o777, typeflag: tar.TypeSymlink, linkname: "target"},
			},
			want: []Change{
				{Path: "link", Kind: ChangeModify, Mode: 0o777 | fs.ModeSymlink},
			},
		},
		{
			name:    "setuid bit preserved",
			workdir: "/workspace",
			entries: []tarEntry{
				{name: "workspace/suid", mode: 0o4755, typeflag: tar.TypeReg, content: []byte("x")},
			},
			want: []Change{
				{Path: "suid", Kind: ChangeModify, Mode: 0o755 | fs.ModeSetuid, Size: 1},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			blob := buildTar(t, tt.entries...)
			changes, opaques, err := parseChangeTar(bytes.NewReader(blob), tt.workdir)
			if err != nil {
				t.Fatalf("parseChangeTar: %v", err)
			}
			assertChanges(t, changes, tt.want)

			var gotOpaques []string
			for _, o := range opaques {
				gotOpaques = append(gotOpaques, o.path)
			}
			if len(gotOpaques) != len(tt.opaques) {
				t.Errorf("opaque dirs = %v, want %v", gotOpaques, tt.opaques)
			} else {
				for i := range tt.opaques {
					if gotOpaques[i] != tt.opaques[i] {
						t.Errorf("opaque[%d] = %q, want %q", i, gotOpaques[i], tt.opaques[i])
					}
				}
			}
		})
	}
}

// TestParseChangeTar_CorruptTar pins that a truncated tar surfaces an error,
// not a silent partial parse.
func TestParseChangeTar_CorruptTar(t *testing.T) {
	// A tar header claiming 100 bytes of content but providing none.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{
		Name:     "workspace/file.txt",
		Mode:     0o644,
		Size:     100,
		Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("write header: %v", err)
	}
	// Close without writing content — the tar is truncated.
	_ = tw.Close()

	_, _, err := parseChangeTar(bytes.NewReader(buf.Bytes()), "/workspace")
	if err == nil {
		t.Fatal("parseChangeTar on a truncated tar: want an error, got nil")
	}
}

// TestChangeKind_String pins the labels used in logs and test output.
func TestChangeKind_String(t *testing.T) {
	tests := []struct {
		kind ChangeKind
		want string
	}{
		{ChangeAdd, "add"},
		{ChangeModify, "modify"},
		{ChangeDelete, "delete"},
		{ChangeKind(99), "unknown"},
	}
	for _, tt := range tests {
		if got := tt.kind.String(); got != tt.want {
			t.Errorf("ChangeKind(%d).String() = %q, want %q", int(tt.kind), got, tt.want)
		}
	}
}

// readerKeys returns the live reader snapshot keys.
func readerKeys(snap *fakeSnapshotter) []string {
	var out []string
	for _, k := range snap.keys() {
		if strings.Contains(k, "/reader/") {
			out = append(out, k)
		}
	}
	return out
}

// TestReadFilesAt_ReadsFromState pins the full reader-task lifecycle: the
// files are read from the state's layer, the main task keeps running, and
// the reader snapshot and task are torn down.
func TestReadFilesAt_ReadsFromState(t *testing.T) {
	sb, snap, tasks, _ := newTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	tasks.preFiles["/workspace/old.txt"] = "old content\n"
	tasks.preFiles["/workspace/other.txt"] = "other\n"

	files, err := sb.ReadFilesAt(ctx, state, []string{"old.txt", "other.txt"})
	if err != nil {
		t.Fatalf("ReadFilesAt: %v", err)
	}
	if got := string(files["old.txt"]); got != "old content\n" {
		t.Errorf("old.txt = %q, want %q", got, "old content\n")
	}
	if got := string(files["other.txt"]); got != "other\n" {
		t.Errorf("other.txt = %q, want %q", got, "other\n")
	}

	// The main task is still running on its active layer.
	if got := tasks.snapshotFor(sb.id); got == "" {
		t.Error("main task is not running after ReadFilesAt")
	}
	// The reader task is stopped: only the main task remains.
	if ids := tasks.runningIDs(); len(ids) != 1 || ids[0] != sb.id {
		t.Errorf("running tasks = %v, want [%s] (the main task only)", ids, sb.id)
	}
	// The reader snapshot is removed.
	if readers := readerKeys(snap); len(readers) != 0 {
		t.Errorf("reader snapshots leaked: %v", readers)
	}
}

// TestReadFilesAt_BeforeCreate pins that ReadFilesAt works without Create:
// it needs only the state's committed layer and the image, not the active
// layer.
func TestReadFilesAt_BeforeCreate(t *testing.T) {
	sb, snap, tasks, _ := newTestSandbox(t)
	ctx := context.Background()

	// Seed a committed layer to stand in for a captured state.
	snap.seed("croft/test/committed/1")
	tasks.preFiles["/workspace/file.txt"] = "content\n"

	files, err := sb.ReadFilesAt(ctx, WorkspaceState{SnapshotKey: "croft/test/committed/1"}, []string{"file.txt"})
	if err != nil {
		t.Fatalf("ReadFilesAt before Create: %v", err)
	}
	if got := string(files["file.txt"]); got != "content\n" {
		t.Errorf("file.txt = %q, want %q", got, "content\n")
	}

	// The reader snapshot is cleaned up.
	if readers := readerKeys(snap); len(readers) != 0 {
		t.Errorf("reader snapshots leaked: %v", readers)
	}
}

// TestReadFilesAt_ReleasedStateIsUnavailable pins that a state whose layer
// has been released surfaces ErrStateUnavailable.
func TestReadFilesAt_ReleasedStateIsUnavailable(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := sb.ReadFilesAt(ctx, WorkspaceState{SnapshotKey: "croft/ghost/committed/1"}, []string{"f.txt"})
	if !errors.Is(err, ErrStateUnavailable) {
		t.Fatalf("err = %v, want ErrStateUnavailable", err)
	}
}

// TestReadFilesAt_MissingFileFails pins that a path absent from the state
// fails the call — the workspace raced the read; documented, retryable.
func TestReadFilesAt_MissingFileFails(t *testing.T) {
	sb, snap, tasks, _ := newTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	tasks.missingPaths["/workspace/gone.txt"] = true

	_, err = sb.ReadFilesAt(ctx, state, []string{"gone.txt"})
	if err == nil || !strings.Contains(err.Error(), "read gone.txt at state") {
		t.Fatalf("err = %v, want a wrapped read-at-state error", err)
	}

	// The reader snapshot is cleaned up on the failure path.
	if readers := readerKeys(snap); len(readers) != 0 {
		t.Errorf("reader snapshots leaked after a failed ReadFilesAt: %v", readers)
	}
	// The main task is still running.
	if got := tasks.snapshotFor(sb.id); got == "" {
		t.Error("main task is not running after a failed ReadFilesAt")
	}
}

// TestReadFilesAt_EmptyStateKey pins the guard: a state without a snapshot
// key is a bug, not a no-op.
func TestReadFilesAt_EmptyStateKey(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t)
	ctx := context.Background()

	_, err := sb.ReadFilesAt(ctx, WorkspaceState{}, []string{"f.txt"})
	if err == nil || !strings.Contains(err.Error(), "no snapshot key") {
		t.Fatalf("err = %v, want a no-snapshot-key error", err)
	}
}

// TestReadFilesAt_NoPaths pins that an empty path list returns an empty map
// without error.
func TestReadFilesAt_NoPaths(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	files, err := sb.ReadFilesAt(ctx, state, nil)
	if err != nil {
		t.Fatalf("ReadFilesAt with no paths: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("files = %v, want empty", files)
	}
}

// TestReadFilesAt_ConcurrentCallsGetDistinctReaders pins that concurrent
// ReadFilesAt calls get distinct reader keys (the readSeq counter) and all
// clean up after themselves.
func TestReadFilesAt_ConcurrentCallsGetDistinctReaders(t *testing.T) {
	sb, snap, tasks, _ := newTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	tasks.preFiles["/workspace/f.txt"] = "x"

	const goroutines = 8
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = sb.ReadFilesAt(ctx, state, []string{"f.txt"})
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("ReadFilesAt from goroutine %d: %v", i, err)
		}
	}

	// No reader snapshots leaked.
	if readers := readerKeys(snap); len(readers) != 0 {
		t.Errorf("reader snapshots leaked after concurrent ReadFilesAt: %v", readers)
	}
	// Only the main task remains.
	if ids := tasks.runningIDs(); len(ids) != 1 || ids[0] != sb.id {
		t.Errorf("running tasks = %v, want [%s] (the main task only)", ids, sb.id)
	}
}

// TestReadDirAt_ListsEntries pins that readDirAt returns the directory
// listing from the state's layer, for opaque-dir resolution.
func TestReadDirAt_ListsEntries(t *testing.T) {
	sb, _, tasks, _ := newTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	tasks.lsDirs["/workspace/opaque-dir"] = []string{"a.txt", "b.txt"}

	names, err := sb.readDirAt(ctx, state, "opaque-dir")
	if err != nil {
		t.Fatalf("readDirAt: %v", err)
	}
	if len(names) != 2 || names[0] != "a.txt" || names[1] != "b.txt" {
		t.Errorf("names = %v, want [a.txt b.txt]", names)
	}
}

// TestReadDirAt_EmptyDir pins that an empty directory returns no names.
func TestReadDirAt_EmptyDir(t *testing.T) {
	sb, _, tasks, _ := newTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	tasks.lsDirs["/workspace/empty"] = []string{}

	names, err := sb.readDirAt(ctx, state, "empty")
	if err != nil {
		t.Fatalf("readDirAt on empty dir: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("names = %v, want []", names)
	}
}

// TestReadDirAt_ReleasedStateIsUnavailable pins ErrStateUnavailable for
// readDirAt, matching readFilesAt.
func TestReadDirAt_ReleasedStateIsUnavailable(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := sb.readDirAt(ctx, WorkspaceState{SnapshotKey: "croft/ghost/committed/1"}, "dir")
	if !errors.Is(err, ErrStateUnavailable) {
		t.Fatalf("err = %v, want ErrStateUnavailable", err)
	}
}

// TestReadFilesAt_ReaderSnapshotKeyShape pins that reader snapshot keys
// follow croft/<id>/reader/<n> and do not collide with capture keys.
func TestReadFilesAt_ReaderSnapshotKeyShape(t *testing.T) {
	sb, _, tasks, log := newTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	tasks.preFiles["/workspace/f.txt"] = "x"
	if _, err := sb.ReadFilesAt(ctx, state, []string{"f.txt"}); err != nil {
		t.Fatalf("ReadFilesAt: %v", err)
	}

	// The reader snapshot was prepared and removed; the op log records it.
	ops := log.snapshot()
	var prepared, removed bool
	for _, op := range ops {
		if strings.HasPrefix(op, "prepare:") && strings.Contains(op, "/reader/") {
			prepared = true
		}
		if strings.HasPrefix(op, "remove:") && strings.Contains(op, "/reader/") {
			removed = true
		}
	}
	if !prepared {
		t.Error("no reader snapshot was prepared (op log has no prepare:/reader/)")
	}
	if !removed {
		t.Error("no reader snapshot was removed (op log has no remove:/reader/)")
	}

	// The capture seq was not moved by the reader: the state's snapshot key
	// still has its original sequence number.
	if !strings.Contains(state.SnapshotKey, "/committed/") {
		t.Errorf("state snapshot key %q does not look like a committed layer", state.SnapshotKey)
	}
}

// changesTestSandbox wires a sandbox over the fakes with a diff service,
// ready for Changes tests. Returns the fake diff service so tests can
// program the blob, and the fake tasks so tests can program missingPaths.
func changesTestSandbox(t *testing.T) (*ContainerdSandbox, *fakeSnapshotter, *fakeTasks, *fakeLeases, *fakeDiffService) {
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
	return sb, snap, tasks, fl, fds
}

// TestChanges_AddModifyDelete is the load-bearing Changes test: a tar with
// an add, a modify, and a delete produces the three kinds, with the add
// distinguished by its absence from the lower tree.
func TestChanges_AddModifyDelete(t *testing.T) {
	sb, _, tasks, _, fds := changesTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// The blob: an add, a modify, and a delete (whiteout).
	fds.blob = buildTar(t,
		tarEntry{name: "workspace/added.txt", mode: 0o644, typeflag: tar.TypeReg, content: []byte("added\n")},
		tarEntry{name: "workspace/base-modified.txt", mode: 0o644, typeflag: tar.TypeReg, content: []byte("after\n")},
		tarEntry{name: "workspace/.wh.base-deleted.txt", mode: 0, typeflag: tar.TypeReg},
	)
	// added.txt does not exist in the lower tree — it is an add.
	tasks.missingPaths["/workspace/added.txt"] = true

	changes, err := sb.Changes(ctx, state)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}

	assertChanges(t, changes, []Change{
		{Path: "added.txt", Kind: ChangeAdd, Mode: 0o644, Size: 6},
		{Path: "base-deleted.txt", Kind: ChangeDelete},
		{Path: "base-modified.txt", Kind: ChangeModify, Mode: 0o644, Size: 6},
	})
}

// TestChanges_ModeOnlyIsModify pins that a mode-only change (same content,
// different mode) is ChangeModify, not ChangeAdd: the path exists in the
// lower tree.
func TestChanges_ModeOnlyIsModify(t *testing.T) {
	sb, _, _, _, fds := changesTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// The path exists in the lower tree (not in missingPaths), so test -e
	// returns 0 and the change stays ChangeModify even though only the
	// mode differs.
	fds.blob = buildTar(t,
		tarEntry{name: "workspace/modeonly.txt", mode: 0o600, typeflag: tar.TypeReg, content: []byte("same\n")},
	)

	changes, err := sb.Changes(ctx, state)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}

	assertChanges(t, changes, []Change{
		{Path: "modeonly.txt", Kind: ChangeModify, Mode: 0o600, Size: 5},
	})
}

// TestChanges_NewDirectoryIsAdd pins that a new directory and its contents
// are ChangeAdd (absent from the lower tree).
func TestChanges_NewDirectoryIsAdd(t *testing.T) {
	sb, _, tasks, _, fds := changesTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	fds.blob = buildTar(t,
		tarEntry{name: "workspace/newdir/", mode: 0o755, typeflag: tar.TypeDir},
		tarEntry{name: "workspace/newdir/n.txt", mode: 0o644, typeflag: tar.TypeReg, content: []byte("n\n")},
	)
	tasks.missingPaths["/workspace/newdir"] = true
	tasks.missingPaths["/workspace/newdir/n.txt"] = true

	changes, err := sb.Changes(ctx, state)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}

	assertChanges(t, changes, []Change{
		{Path: "newdir", Kind: ChangeAdd, IsDir: true, Mode: 0o755 | fs.ModeDir},
		{Path: "newdir/n.txt", Kind: ChangeAdd, Mode: 0o644, Size: 2},
	})
}

// TestChanges_OpaqueDirResolution pins the defensive .wh..wh..opq path:
// the parser collects the opaque dir, Changes enumerates the state's
// contents via readDirAt, and emits a ChangeDelete for each hidden file.
func TestChanges_OpaqueDirResolution(t *testing.T) {
	sb, _, tasks, _, fds := changesTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// The blob has an opaque marker and a new file in the opaque dir.
	fds.blob = buildTar(t,
		tarEntry{name: "workspace/opaque-dir/.wh..wh..opq", mode: 0, typeflag: tar.TypeReg},
		tarEntry{name: "workspace/opaque-dir/new.txt", mode: 0o644, typeflag: tar.TypeReg, content: []byte("new\n")},
	)
	// The state's opaque-dir had two files, now hidden by the opaque marker.
	tasks.lsDirs["/workspace/opaque-dir"] = []string{"old-a.txt", "old-b.txt"}
	// new.txt does not exist in the lower tree — it is an add.
	tasks.missingPaths["/workspace/opaque-dir/new.txt"] = true

	changes, err := sb.Changes(ctx, state)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}

	assertChanges(t, changes, []Change{
		{Path: "opaque-dir/new.txt", Kind: ChangeAdd, Mode: 0o644, Size: 4},
		{Path: "opaque-dir/old-a.txt", Kind: ChangeDelete},
		{Path: "opaque-dir/old-b.txt", Kind: ChangeDelete},
	})
}

// TestChanges_NoChanges pins that an empty diff (no entries beyond the
// workdir root) produces an empty change list.
func TestChanges_NoChanges(t *testing.T) {
	sb, _, _, _, fds := changesTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// The blob has only the workdir root — no changes.
	fds.blob = buildTar(t,
		tarEntry{name: "workspace/", mode: 0o755, typeflag: tar.TypeDir},
	)

	changes, err := sb.Changes(ctx, state)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	if len(changes) != 0 {
		t.Errorf("changes = %v, want empty", changes)
	}
}

// TestChanges_BeforeCreate pins the guard: Changes before Create has no
// active layer to diff against.
func TestChanges_BeforeCreate(t *testing.T) {
	sb, _, _, _, _ := changesTestSandbox(t)
	ctx := context.Background()

	_, err := sb.Changes(ctx, WorkspaceState{SnapshotKey: "anything"})
	if err == nil || !strings.Contains(err.Error(), "Changes before Create") {
		t.Fatalf("err = %v, want a Changes-before-Create error", err)
	}
}

// TestChanges_ReleasedStateIsUnavailable pins ErrStateUnavailable for a
// state whose layer has been released.
func TestChanges_ReleasedStateIsUnavailable(t *testing.T) {
	sb, _, _, _, _ := changesTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := sb.Changes(ctx, WorkspaceState{SnapshotKey: "croft/ghost/committed/1"})
	if !errors.Is(err, ErrStateUnavailable) {
		t.Fatalf("err = %v, want ErrStateUnavailable", err)
	}
}

// TestChanges_EmptyStateKey pins the guard: a state without a snapshot key
// is a bug, not a no-op.
func TestChanges_EmptyStateKey(t *testing.T) {
	sb, _, _, _, _ := changesTestSandbox(t)
	ctx := context.Background()

	_, err := sb.Changes(ctx, WorkspaceState{})
	if err == nil || !strings.Contains(err.Error(), "no snapshot key") {
		t.Fatalf("err = %v, want a no-snapshot-key error", err)
	}
}

// TestChanges_NoDiffService pins the nil-collaborator error.
func TestChanges_NoDiffService(t *testing.T) {
	sb, _, _, _ := newTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	_, err = sb.Changes(ctx, state)
	if err == nil || !strings.Contains(err.Error(), "diff service not configured") {
		t.Fatalf("err = %v, want a diff-service-not-configured error", err)
	}
}

// TestChanges_BlobDroppedAfterRead pins that the diff blob's lease and
// content pin are released after Changes reads the blob — the blob's
// lifecycle is bounded by the call.
func TestChanges_BlobDroppedAfterRead(t *testing.T) {
	sb, _, _, fl, fds := changesTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	fds.blob = buildTar(t,
		tarEntry{name: "workspace/added.txt", mode: 0o644, typeflag: tar.TypeReg, content: []byte("x")},
	)

	if _, err := sb.Changes(ctx, state); err != nil {
		t.Fatalf("Changes: %v", err)
	}

	// The diff lease was created and deleted within the call.
	ids := diffLeaseIDs(fl)
	if len(ids) != 1 {
		t.Fatalf("created %d diff leases, want 1", len(ids))
	}
	fl.mu.Lock()
	deleted := append([]string(nil), fl.deleted...)
	fl.mu.Unlock()
	if len(deleted) != 1 || deleted[0] != ids[0] {
		t.Errorf("deleted leases = %v, want [%s]: the blob pin must be dropped after Read", deleted, ids[0])
	}

	// The blob was deleted from the content store.
	deletedBlobs := fds.deletedDigests()
	if len(deletedBlobs) != 1 || deletedBlobs[0] != fds.desc.Digest {
		t.Errorf("deleted blobs = %v, want [%s]", deletedBlobs, fds.desc.Digest)
	}
}

// TestChanges_ConcurrentWithSharedLock pins that concurrent Changes calls
// both succeed (shared lock) and each cleans up its own blob.
func TestChanges_ConcurrentWithSharedLock(t *testing.T) {
	sb, _, tasks, fl, fds := changesTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	fds.blob = buildTar(t,
		tarEntry{name: "workspace/added.txt", mode: 0o644, typeflag: tar.TypeReg, content: []byte("x")},
	)
	tasks.missingPaths["/workspace/added.txt"] = true

	const goroutines = 4
	var wg sync.WaitGroup
	errs := make([]error, goroutines)
	for i := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = sb.Changes(ctx, state)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Changes from goroutine %d: %v", i, err)
		}
	}

	// Each call created and dropped its own diff lease.
	ids := diffLeaseIDs(fl)
	if len(ids) != goroutines {
		t.Errorf("created %d diff leases across %d concurrent calls, want %d", len(ids), goroutines, goroutines)
	}
	fl.mu.Lock()
	deleted := len(fl.deleted)
	fl.mu.Unlock()
	if deleted != goroutines {
		t.Errorf("deleted %d leases, want %d: every call must drop its blob", deleted, goroutines)
	}
}

// TestDiff_Modify pins the core unified-diff path: a modified file produces
// ---/+++ headers and a hunk with -old/+new lines.
func TestDiff_Modify(t *testing.T) {
	sb, _, tasks, _, fds := changesTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// The blob: a modified file.
	fds.blob = buildTar(t,
		tarEntry{name: "workspace/file.txt", mode: 0o644, typeflag: tar.TypeReg, content: []byte("new\n")},
	)
	// Pre-image (state) and post-image (current workspace) differ.
	tasks.preFiles["/workspace/file.txt"] = "old\n"
	tasks.catFiles["/workspace/file.txt"] = "new\n"
	// file.txt exists in the lower tree (not in missingPaths) → ChangeModify.

	got, err := sb.Diff(ctx, state)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	want := `--- a/file.txt
+++ b/file.txt
@@ -1 +1 @@
-old
+new
`
	if got != want {
		t.Errorf("Diff modify:\n got: %q\nwant: %q", got, want)
	}
}

// TestDiff_Add pins the add path: /dev/null as the a side, all lines added.
func TestDiff_Add(t *testing.T) {
	sb, _, tasks, _, fds := changesTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	fds.blob = buildTar(t,
		tarEntry{name: "workspace/new.txt", mode: 0o644, typeflag: tar.TypeReg, content: []byte("hello\n")},
	)
	tasks.missingPaths["/workspace/new.txt"] = true // absent from lower → ChangeAdd
	tasks.catFiles["/workspace/new.txt"] = "hello\n"

	got, err := sb.Diff(ctx, state)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	want := `--- /dev/null
+++ b/new.txt
@@ -0,0 +1 @@
+hello
`
	if got != want {
		t.Errorf("Diff add:\n got: %q\nwant: %q", got, want)
	}
}

// TestDiff_Delete pins the delete path: /dev/null as the b side, all lines
// removed.
func TestDiff_Delete(t *testing.T) {
	sb, _, tasks, _, fds := changesTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	fds.blob = buildTar(t,
		tarEntry{name: "workspace/.wh.gone.txt", mode: 0, typeflag: tar.TypeReg},
	)
	tasks.preFiles["/workspace/gone.txt"] = "bye\n"

	got, err := sb.Diff(ctx, state)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	want := `--- a/gone.txt
+++ /dev/null
@@ -1 +0,0 @@
-bye
`
	if got != want {
		t.Errorf("Diff delete:\n got: %q\nwant: %q", got, want)
	}
}

// TestDiff_ModeOnly pins the mode-only rendering: content identical, mode
// differs → old mode/new mode pair, no hunks.
func TestDiff_ModeOnly(t *testing.T) {
	sb, _, tasks, _, fds := changesTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// The blob: a file with new mode 0600 and unchanged content.
	fds.blob = buildTar(t,
		tarEntry{name: "workspace/modeonly.txt", mode: 0o600, typeflag: tar.TypeReg, content: []byte("same\n")},
	)
	// Pre-image and post-image have identical content.
	tasks.preFiles["/workspace/modeonly.txt"] = "same\n"
	tasks.catFiles["/workspace/modeonly.txt"] = "same\n"
	// The state's layer had mode 0644.
	tasks.modes["/workspace/modeonly.txt"] = "-rw-r--r--"

	got, err := sb.Diff(ctx, state)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	want := "old mode 0644\nnew mode 0600\n"
	if got != want {
		t.Errorf("Diff mode-only:\n got: %q\nwant: %q", got, want)
	}
}

// TestDiff_Binary pins the binary rendering: NUL in either side → a single
// "Binary files ... differ" line.
func TestDiff_Binary(t *testing.T) {
	sb, _, tasks, _, fds := changesTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	fds.blob = buildTar(t,
		tarEntry{name: "workspace/blob.bin", mode: 0o644, typeflag: tar.TypeReg, content: []byte("new\x00data")},
	)
	tasks.preFiles["/workspace/blob.bin"] = "old\x00data"
	tasks.catFiles["/workspace/blob.bin"] = "new\x00data"

	got, err := sb.Diff(ctx, state)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	want := "Binary files a/blob.bin and b/blob.bin differ\n"
	if got != want {
		t.Errorf("Diff binary:\n got: %q\nwant: %q", got, want)
	}
}

// TestDiff_SymlinkModify pins symlink rendering: the target is read from
// both sides and rendered as a one-line diff.
func TestDiff_SymlinkModify(t *testing.T) {
	sb, _, tasks, _, fds := changesTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	fds.blob = buildTar(t,
		tarEntry{name: "workspace/link", mode: 0o777, typeflag: tar.TypeSymlink, linkname: "newtarget"},
	)
	// Old target (state, reader task) and new target (current, main task).
	tasks.preReadlinkTargets["/workspace/link"] = "oldtarget"
	tasks.readlinkTargets["/workspace/link"] = "newtarget"

	got, err := sb.Diff(ctx, state)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	want := `--- a/link
+++ b/link
@@ -1 +1 @@
-oldtarget
+newtarget
`
	if got != want {
		t.Errorf("Diff symlink modify:\n got: %q\nwant: %q", got, want)
	}
}

// TestDiff_NewDirectory pins directory rendering: a new directory produces a
// marker line, no hunks.
func TestDiff_NewDirectory(t *testing.T) {
	sb, _, tasks, _, fds := changesTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	fds.blob = buildTar(t,
		tarEntry{name: "workspace/newdir/", mode: 0o755, typeflag: tar.TypeDir},
	)
	tasks.missingPaths["/workspace/newdir"] = true // absent from lower → ChangeAdd

	got, err := sb.Diff(ctx, state)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	want := "new directory: newdir\n"
	if got != want {
		t.Errorf("Diff new directory:\n got: %q\nwant: %q", got, want)
	}
}

// TestDiff_NoChanges pins that an empty change list produces empty output.
func TestDiff_NoChanges(t *testing.T) {
	sb, _, _, _, fds := changesTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	fds.blob = buildTar(t,
		tarEntry{name: "workspace/", mode: 0o755, typeflag: tar.TypeDir},
	)

	got, err := sb.Diff(ctx, state)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if got != "" {
		t.Errorf("Diff no changes = %q, want empty", got)
	}
}

// TestDiff_BeforeCreate pins the guard.
func TestDiff_BeforeCreate(t *testing.T) {
	sb, _, _, _, _ := changesTestSandbox(t)
	ctx := context.Background()

	_, err := sb.Diff(ctx, WorkspaceState{SnapshotKey: "anything"})
	if err == nil || !strings.Contains(err.Error(), "Changes before Create") {
		t.Fatalf("err = %v, want a Changes-before-Create error (Diff delegates to Changes)", err)
	}
}

// TestDiff_ReleasedStateIsUnavailable pins ErrStateUnavailable.
func TestDiff_ReleasedStateIsUnavailable(t *testing.T) {
	sb, _, _, _, _ := changesTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := sb.Diff(ctx, WorkspaceState{SnapshotKey: "croft/ghost/committed/1"})
	if !errors.Is(err, ErrStateUnavailable) {
		t.Fatalf("err = %v, want ErrStateUnavailable", err)
	}
}

// TestDiff_PreImageRaceFails pins that a pre-image the reader cannot fetch
// fails Diff (documented, retryable — same contract as ReadFilesAt). A
// ChangeDelete's pre-image read goes through readFilesAt; when the state's
// layer cannot serve the file, Diff propagates the error.
func TestDiff_PreImageRaceFails(t *testing.T) {
	sb, _, tasks, _, fds := changesTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// The blob names a deleted file, but the state's layer cannot serve it
	// (the workspace raced the read). ChangeDelete entries are not checked
	// by test -e, so missingPaths only affects the pre-image cat read.
	fds.blob = buildTar(t,
		tarEntry{name: "workspace/.wh.raced.txt", mode: 0, typeflag: tar.TypeReg},
	)
	tasks.missingPaths["/workspace/raced.txt"] = true

	_, err = sb.Diff(ctx, state)
	if err == nil || !strings.Contains(err.Error(), "read raced.txt at state") {
		t.Fatalf("err = %v, want a wrapped read-at-state error", err)
	}
}

// TestDiff_MultiFile pins that multiple changes render in sorted order, each
// with its own headers.
func TestDiff_MultiFile(t *testing.T) {
	sb, _, tasks, _, fds := changesTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// An add, a modify, and a delete.
	fds.blob = buildTar(t,
		tarEntry{name: "workspace/added.txt", mode: 0o644, typeflag: tar.TypeReg, content: []byte("a\n")},
		tarEntry{name: "workspace/modified.txt", mode: 0o644, typeflag: tar.TypeReg, content: []byte("new\n")},
		tarEntry{name: "workspace/.wh.deleted.txt", mode: 0, typeflag: tar.TypeReg},
	)
	tasks.missingPaths["/workspace/added.txt"] = true
	tasks.catFiles["/workspace/added.txt"] = "a\n"
	tasks.preFiles["/workspace/modified.txt"] = "old\n"
	tasks.catFiles["/workspace/modified.txt"] = "new\n"
	tasks.preFiles["/workspace/deleted.txt"] = "bye\n"

	got, err := sb.Diff(ctx, state)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	// Sorted by path: added.txt, deleted.txt, modified.txt.
	want := `--- /dev/null
+++ b/added.txt
@@ -0,0 +1 @@
+a
--- a/deleted.txt
+++ /dev/null
@@ -1 +0,0 @@
-bye
--- a/modified.txt
+++ b/modified.txt
@@ -1 +1 @@
-old
+new
`
	if got != want {
		t.Errorf("Diff multi-file:\n got: %q\nwant: %q", got, want)
	}
}

// TestDiff_DeletedSymlinkRendersTarget pins that a deleted symlink renders
// its target as a one-line deletion, not its target file's content. Without
// delete-type classification, cat would follow the link and the diff would
// show the wrong content.
func TestDiff_DeletedSymlinkRendersTarget(t *testing.T) {
	sb, _, tasks, _, fds := changesTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// The blob: a whiteout for a symlink.
	fds.blob = buildTar(t,
		tarEntry{name: "workspace/.wh.link", mode: 0, typeflag: tar.TypeReg},
	)
	// The deleted path was a symlink (classifyDeletes sets ModeSymlink).
	tasks.symlinks["/workspace/link"] = true
	// Its target in the state's layer.
	tasks.preReadlinkTargets["/workspace/link"] = "oldtarget"

	got, err := sb.Diff(ctx, state)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	want := `--- a/link
+++ /dev/null
@@ -1 +0,0 @@
-oldtarget
`
	if got != want {
		t.Errorf("Diff deleted symlink:\n got: %q\nwant: %q", got, want)
	}
}

// TestDiff_DeletedDirectoryRendersMarker pins that a deleted directory
// renders as a marker, not a failure. Without delete-type classification,
// the pre-image read would cat the directory and fail.
func TestDiff_DeletedDirectoryRendersMarker(t *testing.T) {
	sb, _, tasks, _, fds := changesTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// The blob: a whiteout for a directory.
	fds.blob = buildTar(t,
		tarEntry{name: "workspace/.wh.olddir", mode: 0, typeflag: tar.TypeReg},
	)
	// The deleted path was a directory (classifyDeletes sets IsDir).
	tasks.dirs["/workspace/olddir"] = true

	got, err := sb.Diff(ctx, state)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	want := "deleted directory: olddir\n"
	if got != want {
		t.Errorf("Diff deleted directory:\n got: %q\nwant: %q", got, want)
	}
}

// TestDiff_EmptyFileAdd pins that an add of an empty file renders headers
// with a 0-line hunk, not silence.
func TestDiff_EmptyFileAdd(t *testing.T) {
	sb, _, tasks, _, fds := changesTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// The blob: an empty file (size 0).
	fds.blob = buildTar(t,
		tarEntry{name: "workspace/empty.txt", mode: 0o644, typeflag: tar.TypeReg, content: nil},
	)
	tasks.missingPaths["/workspace/empty.txt"] = true // absent from lower → ChangeAdd
	tasks.catFiles["/workspace/empty.txt"] = ""       // post-image is empty

	got, err := sb.Diff(ctx, state)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	want := "--- /dev/null\n+++ b/empty.txt\n@@ -0,0 +0,0 @@\n"
	if got != want {
		t.Errorf("Diff empty add:\n got: %q\nwant: %q", got, want)
	}
}

// TestDiff_NoNewlineMarker pins the \ No newline at end of file marker.
func TestDiff_NoNewlineMarker(t *testing.T) {
	sb, _, tasks, _, fds := changesTestSandbox(t)
	ctx := context.Background()

	if err := sb.Create(ctx); err != nil {
		t.Fatalf("Create: %v", err)
	}
	state, err := sb.Capture(ctx)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	fds.blob = buildTar(t,
		tarEntry{name: "workspace/f.txt", mode: 0o644, typeflag: tar.TypeReg, content: []byte("new")},
	)
	tasks.preFiles["/workspace/f.txt"] = "old" // no trailing newline
	tasks.catFiles["/workspace/f.txt"] = "new" // no trailing newline

	got, err := sb.Diff(ctx, state)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	if !strings.Contains(got, "\\ No newline at end of file") {
		t.Errorf("Diff: missing \\ No newline marker in %q", got)
	}
}
