package containerd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	shepherd "github.com/buchenberg/shepherd-kernel-go"
	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/containerd/v2/pkg/namespaces"
	"github.com/containerd/errdefs"
)

// fakeLeases records the lease lifecycle. The daemon's real manager is the only
// thing that can prove a snapshot is protected (see
// TestLive_LeaseHoldsSnapshots); this exists so the sandbox's own bookkeeping —
// create once, label, delete once, tolerate a vanished lease — is testable
// without a daemon.
type fakeLeases struct {
	created   []leases.Lease
	deleted   []string
	lastLabel map[string]string
	createErr error
	deleteErr error
}

func (f *fakeLeases) Create(_ context.Context, opts ...leases.Opt) (leases.Lease, error) {
	if f.createErr != nil {
		return leases.Lease{}, f.createErr
	}
	l := leases.Lease{}
	for _, o := range opts {
		if err := o(&l); err != nil {
			return leases.Lease{}, err
		}
	}
	f.lastLabel = l.Labels
	f.created = append(f.created, l)
	return l, nil
}

func (f *fakeLeases) Delete(_ context.Context, l leases.Lease, _ ...leases.DeleteOpt) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deleted = append(f.deleted, l.ID)
	return nil
}

func (f *fakeLeases) List(context.Context, ...string) ([]leases.Lease, error) { return nil, nil }

func (f *fakeLeases) AddResource(context.Context, leases.Lease, leases.Resource) error {
	return nil
}

func (f *fakeLeases) DeleteResource(context.Context, leases.Lease, leases.Resource) error {
	return nil
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
	if sb.leases != nil {
		t.Fatal("a substituted backend should carry no lease manager")
	}
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
