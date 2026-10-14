package croft

import (
	"context"
	"fmt"

	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// diffBlob computes the change set between ws and the sandbox's current
// active layer, returning the daemon-produced blob's descriptor and a
// cancel-free drop function that releases every resource the call pinned.
//
// The blob is unreferenced content the daemon's GC may collect at any time,
// so the call pins it under a short-lived per-call lease (labeled
// croft.lease:"diff", findable by a future reclaim sweep) for exactly the
// caller's read window. drop removes the pin, best-effort deletes the blob,
// and deletes the lease — it runs on every failure path, cancel-free, exactly
// like Capture's unpin-on-failure branches.
//
// The lower side mounts through a View: committed snapshots refuse Mounts
// ("not active or view: failed precondition", Task 0 finding), and the view
// is removed before returning — Compare is the only consumer of the mounts.
// A state whose layer has been released surfaces as ErrStateUnavailable.
func (s *ContainerdSandbox) diffBlob(ctx context.Context, ws WorkspaceState) (ocispec.Descriptor, func(), error) {
	if s.diff == nil {
		return ocispec.Descriptor{}, nil, fmt.Errorf("containerd sandbox: diff service not configured")
	}

	s.mu.Lock()
	id := s.id
	active := s.activeKey
	s.diffSeq++
	viewKey := s.key(id, "diffview", s.diffSeq)
	s.mu.Unlock()

	if id == "" || active == "" {
		return ocispec.Descriptor{}, nil, fmt.Errorf("containerd sandbox: diff before Create")
	}

	mgr := s.leaseManager()

	// Create the per-call lease first: the View below creates a snapshot that
	// needs GC protection for the duration, and the Compare's blob needs a
	// pin. Nil manager means test fakes — no daemon, no GC, nothing to pin.
	var leaseID string
	if mgr != nil {
		l, err := mgr.Create(s.stateLeaseCtx(ctx),
			leases.WithID("croft-diff-"+newSandboxID()),
			leases.WithLabels(map[string]string{sandboxLeaseLabel: "diff"}),
		)
		if err != nil {
			return ocispec.Descriptor{}, nil, fmt.Errorf("containerd sandbox: create diff lease: %w", err)
		}
		leaseID = l.ID
		ctx = leases.WithLease(ctx, leaseID)
	}

	// drop releases everything the call pinned, cancel-free. It is extended
	// as resources are acquired, so every failure path drops exactly what was
	// pinned and nothing more.
	drop := func() {
		if leaseID != "" {
			_ = mgr.Delete(context.WithoutCancel(ctx), leases.Lease{ID: leaseID})
		}
	}

	// View the committed state's layer: Mounts refuses committed snapshots
	// ("not active or view: failed precondition", Task 0 finding).
	if _, err := s.snap.View(ctx, viewKey, ws.SnapshotKey); err != nil {
		drop()
		if errdefs.IsNotFound(err) {
			return ocispec.Descriptor{}, nil, fmt.Errorf("%w: %s", ErrStateUnavailable, ws.SnapshotKey)
		}
		return ocispec.Descriptor{}, nil, fmt.Errorf("containerd sandbox: view state %s: %w", ws.SnapshotKey, err)
	}

	lower, err := s.snap.Mounts(ctx, viewKey)
	if err != nil {
		_ = s.snap.Remove(context.WithoutCancel(ctx), viewKey)
		drop()
		return ocispec.Descriptor{}, nil, fmt.Errorf("containerd sandbox: mount state view %s: %w", viewKey, err)
	}

	upper, err := s.snap.Mounts(ctx, active)
	if err != nil {
		_ = s.snap.Remove(context.WithoutCancel(ctx), viewKey)
		drop()
		return ocispec.Descriptor{}, nil, fmt.Errorf("containerd sandbox: mount active %s: %w", active, err)
	}

	desc, err := s.diff.Compare(ctx, lower, upper)
	// The view is removed on every path: Compare is the only consumer of the
	// mounts, and a leaked view holds disk until namespace teardown.
	_ = s.snap.Remove(context.WithoutCancel(ctx), viewKey)
	if err != nil {
		drop()
		return ocispec.Descriptor{}, nil, fmt.Errorf("containerd sandbox: compare snapshots: %w", err)
	}

	// Pin the blob against the daemon's GC for the caller's read window. The
	// "content" resource type is the Task 0 spike's finding: the digest
	// serves as the lease resource id.
	if mgr != nil {
		res := leases.Resource{ID: desc.Digest.String(), Type: "content"}
		if err := mgr.AddResource(ctx, leases.Lease{ID: leaseID}, res); err != nil {
			drop()
			return ocispec.Descriptor{}, nil, fmt.Errorf("containerd sandbox: pin diff blob %s: %w", desc.Digest, err)
		}
		innerDrop := drop
		drop = func() {
			dctx := context.WithoutCancel(ctx)
			_ = mgr.DeleteResource(dctx, leases.Lease{ID: leaseID}, res)
			_ = s.diff.Delete(dctx, desc)
			innerDrop()
		}
	}

	return desc, drop, nil
}
