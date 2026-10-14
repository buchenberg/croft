package croft

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/containerd/containerd/v2/core/leases"
	"github.com/containerd/errdefs"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// ChangeKind classifies a workspace change.
type ChangeKind int

const (
	// ChangeAdd is a path that exists in the current workspace but not in
	// the state.
	ChangeAdd ChangeKind = iota
	// ChangeModify is a path that exists in both the state and the current
	// workspace but differs (content, mode, or type).
	ChangeModify
	// ChangeDelete is a path that exists in the state but not in the
	// current workspace.
	ChangeDelete
)

// String returns a short lowercase label for the kind, for logs and tests.
func (k ChangeKind) String() string {
	switch k {
	case ChangeAdd:
		return "add"
	case ChangeModify:
		return "modify"
	case ChangeDelete:
		return "delete"
	default:
		return "unknown"
	}
}

// Change describes one difference between a WorkspaceState and the current
// workspace.
type Change struct {
	// Path is workspace-relative, slash-separated. Changes are sorted by Path.
	Path string
	// Kind classifies the change.
	Kind ChangeKind
	// IsDir reports whether the path is a directory.
	IsDir bool
	// Mode is the new mode; 0 for ChangeDelete.
	Mode fs.FileMode
	// Size is the new size in bytes; 0 for ChangeDelete and directories.
	Size int64
}

// opaqueDir records a directory whose contents were replaced wholesale
// (rm -rf + recreate). The walking differ emits per-file whiteouts for the
// old contents (Task 0 finding), so this list is empty in practice; it
// exists for the defensive .wh..wh..opq marker path. Task 5 resolves these
// via readDirAt against the state's layer.
type opaqueDir struct {
	path string // workspace-relative, slash-separated
}

// parseChangeTar parses a daemon-produced change-set blob into a sorted
// change list and a list of opaque directories.
//
// The blob is a tar of complete changed files plus whiteout entries for
// deletions (Task 0 findings). The reader sniffs the gzip magic and
// decompresses whatever media type the daemon chose — the default blob is a
// gzipped OCI layer, not a plain tar.
//
// Entries arrive prefixed with the in-container workdir (e.g.
// "workspace/added.txt" for Config.Workdir "/workspace") and in walk order,
// not sorted. The parser strips the workdir prefix, skips the workdir-root
// entry itself (its metadata is croft's provisioning, not user data), skips
// entries outside the workdir, classifies whiteouts into ChangeDelete, and
// sorts the result by path.
//
// The parser cannot distinguish ChangeAdd from ChangeModify — both arrive as
// ordinary tar entries — so it reports ChangeModify for all non-whiteout
// entries. Changes (Task 5) determines ChangeAdd by consulting the lower
// tree.
func parseChangeTar(r io.Reader, workdir string) ([]Change, []opaqueDir, error) {
	src, err := sniffGunzip(r)
	if err != nil {
		return nil, nil, fmt.Errorf("containerd sandbox: sniff diff blob: %w", err)
	}

	// The tar prefix is the workdir without the leading slash: entries
	// arrive as "workspace/added.txt" for Config.Workdir "/workspace"
	// (Task 0 finding).
	trimmed := strings.TrimPrefix(workdir, "/")
	prefix := ""
	if trimmed != "" {
		prefix = trimmed + "/"
	}

	var changes []Change
	var opaques []opaqueDir

	tr := tar.NewReader(src)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("containerd sandbox: read change tar: %w", err)
		}

		name := hdr.Name
		if prefix != "" {
			if name == trimmed || name == prefix {
				// The workdir root itself: its metadata is croft's
				// provisioning, not user data.
				continue
			}
			if !strings.HasPrefix(name, prefix) {
				// Outside the workspace: not a workspace change.
				continue
			}
		}
		rel := strings.TrimPrefix(name, prefix)
		if rel == "" || rel == "." {
			continue
		}

		dir, base := path.Split(rel)
		if strings.HasPrefix(base, ".wh.") {
			if base == ".wh..wh..opq" {
				// Opaque directory marker (defensive: not emitted by this
				// differ, which uses per-file whiteouts).
				opaques = append(opaques, opaqueDir{path: strings.TrimSuffix(dir, "/")})
				continue
			}
			// Whiteout: .wh.<name> deletes <name>. Task 0 finding: the
			// form is .wh.-prefixed regular files, not char devices; the
			// parser accepts both defensively (a char-device whiteout has
			// the same .wh. name).
			target := dir + strings.TrimPrefix(base, ".wh.")
			changes = append(changes, Change{Path: target, Kind: ChangeDelete})
			continue
		}

		// Ordinary entry: add or modify (the parser cannot distinguish;
		// Changes determines ChangeAdd by consulting the lower tree).
		// Directory entries carry a trailing slash in the tar name; the
		// change path is slash-separated without it.
		changes = append(changes, Change{
			Path:  strings.TrimSuffix(rel, "/"),
			Kind:  ChangeModify,
			IsDir: hdr.Typeflag == tar.TypeDir,
			Mode:  changeMode(hdr),
			Size:  hdr.Size,
		})
	}

	// Sort by path (Task 0 finding: entries arrive in walk order, not sorted).
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })

	return changes, opaques, nil
}

// sniffGunzip wraps r with a gzip reader when the stream starts with the
// gzip magic bytes (0x1f 0x8b), and passes it through unchanged otherwise.
// The diff blob's media type is whatever the daemon chose (Task 0 finding:
// a gzipped OCI layer by default), so the parser sniffs rather than trusts
// the descriptor.
func sniffGunzip(r io.Reader) (io.Reader, error) {
	br := bufio.NewReader(r)
	magic, err := br.Peek(2)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if len(magic) == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		return gzip.NewReader(br)
	}
	return br, nil
}

// changeMode converts a tar header's mode and type into an fs.FileMode,
// mapping the Unix permission bits and the type flag onto the fs.FileMode
// type bits (ModeDir, ModeSymlink, etc.).
func changeMode(hdr *tar.Header) fs.FileMode {
	mode := fs.FileMode(hdr.Mode & 0o777)
	if hdr.Mode&0o4000 != 0 {
		mode |= fs.ModeSetuid
	}
	if hdr.Mode&0o2000 != 0 {
		mode |= fs.ModeSetgid
	}
	if hdr.Mode&0o1000 != 0 {
		mode |= fs.ModeSticky
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		mode |= fs.ModeDir
	case tar.TypeSymlink:
		mode |= fs.ModeSymlink
	case tar.TypeChar:
		mode |= fs.ModeDevice | fs.ModeCharDevice
	case tar.TypeBlock:
		mode |= fs.ModeDevice
	case tar.TypeFifo:
		mode |= fs.ModeNamedPipe
	}
	return mode
}

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
