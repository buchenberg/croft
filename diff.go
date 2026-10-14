package croft

import (
	"archive/tar"
	"bufio"
	"bytes"
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
	changes, opaques, _, err := parseChangeTarInternal(r, workdir, false)
	return changes, opaques, err
}

// parseChangeTarWithContent is parseChangeTar that also reads the content of
// ordinary entries (adds/modifies) into a map keyed by workspace-relative
// path. Diff uses this to get post-images directly from the diff blob,
// without re-reading the workspace — saving one exec per changed file and
// eliminating the race between the diff computation and the post-image read.
func parseChangeTarWithContent(r io.Reader, workdir string) ([]Change, []opaqueDir, map[string][]byte, error) {
	return parseChangeTarInternal(r, workdir, true)
}

// parseChangeTarInternal is the shared parser. When withContent is true,
// ordinary entries' content is buffered into the returned map.
func parseChangeTarInternal(r io.Reader, workdir string, withContent bool) ([]Change, []opaqueDir, map[string][]byte, error) {
	src, err := sniffGunzip(r)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("containerd sandbox: sniff diff blob: %w", err)
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
	var contents map[string][]byte
	if withContent {
		contents = make(map[string][]byte)
	}

	tr := tar.NewReader(src)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, nil, fmt.Errorf("containerd sandbox: read change tar: %w", err)
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
		changePath := strings.TrimSuffix(rel, "/")
		changes = append(changes, Change{
			Path:  changePath,
			Kind:  ChangeModify,
			IsDir: hdr.Typeflag == tar.TypeDir,
			Mode:  changeMode(hdr),
			Size:  hdr.Size,
		})

		// Buffer the content for Diff's post-images. Directories and
		// symlinks have no meaningful content here (symlink targets ride
		// in Linkname, not in the entry body).
		if withContent && hdr.Typeflag == tar.TypeReg && hdr.Size > 0 {
			content, err := io.ReadAll(tr)
			if err != nil {
				return nil, nil, nil, fmt.Errorf("containerd sandbox: read content for %s: %w", changePath, err)
			}
			contents[changePath] = content
		}
	}

	// Sort by path (Task 0 finding: entries arrive in walk order, not sorted).
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })

	return changes, opaques, contents, nil
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

// withReaderTask prepares a throwaway reader snapshot from ws's committed
// layer, starts a task on it, and calls fn with the reader's task id. The
// task is stopped and the snapshot removed on return — cancel-free, on every
// path, exactly like Create's failure-cleanup shape.
//
// The reader task id is derived from the sandbox id and the readSeq counter,
// so StopTask(readerID) stops only the reader and never the main task. The
// snapshot key is croft/<id>/reader/<n> — readable in the snapshotter, and
// distinct from the capture seq (state identity is sequence-derived).
//
// A state whose layer has been released surfaces as ErrStateUnavailable.
func (s *ContainerdSandbox) withReaderTask(ctx context.Context, ws WorkspaceState, fn func(ctx context.Context, readerID string) error) error {
	if ws.SnapshotKey == "" {
		return fmt.Errorf("containerd sandbox: workspace state has no snapshot key")
	}
	if err := s.requireBackend(ctx); err != nil {
		return err
	}

	s.mu.Lock()
	id := s.id
	if id == "" {
		// ReadFilesAt works before Create: the reader task does not need the
		// active layer, only the state's committed layer and the image.
		id = newSandboxID()
	}
	s.readSeq++
	n := s.readSeq
	s.mu.Unlock()

	readerKey := s.key(id, "reader", n)
	readerID := fmt.Sprintf("%s-reader-%d", id, n)

	// Prepare a reader snapshot from the state's committed layer.
	if _, err := s.snap.Prepare(ctx, readerKey, ws.SnapshotKey); err != nil {
		if errdefs.IsNotFound(err) {
			return fmt.Errorf("%w: %s", ErrStateUnavailable, ws.SnapshotKey)
		}
		return fmt.Errorf("containerd sandbox: prepare reader snapshot from %s: %w", ws.SnapshotKey, err)
	}
	defer func() {
		_ = s.snap.Remove(context.WithoutCancel(ctx), readerKey)
	}()

	if err := s.tasks.StartTask(ctx, readerID, readerKey); err != nil {
		return fmt.Errorf("containerd sandbox: start reader task: %w", err)
	}
	defer func() {
		_ = s.tasks.StopTask(context.WithoutCancel(ctx), readerID)
	}()

	return fn(ctx, readerID)
}

// readFilesAt reads the given paths from ws's committed layer, returning a
// map keyed by the original (unresolved) paths. A missing path fails the
// call — the workspace raced the read; documented, retryable.
//
// One `cat -- "$1"` exec per file: robust over clever marker protocols, and
// changed-file counts in practice are small.
func (s *ContainerdSandbox) readFilesAt(ctx context.Context, ws WorkspaceState, paths []string) (map[string][]byte, error) {
	var out map[string][]byte
	err := s.withReaderTask(ctx, ws, func(ctx context.Context, readerID string) error {
		out = make(map[string][]byte, len(paths))
		for _, p := range paths {
			resolved := s.resolve(p)
			res, err := s.tasks.Exec(ctx, readerID, ExecRequest{
				Command: "sh",
				Args:    []string{"-c", `cat -- "$1"`, "croft", resolved},
				Cwd:     s.cfg.workdir(),
			})
			if err != nil {
				return fmt.Errorf("containerd sandbox: read %s at state: %w", p, err)
			}
			if res.ExitCode != 0 {
				return fmt.Errorf("containerd sandbox: read %s at state: exit %d: %s",
					p, res.ExitCode, strings.TrimSpace(res.Stderr))
			}
			out[p] = []byte(res.Stdout)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ReadFilesAt reads files from a workspace state without applying it —
// inspect a checkpoint's contents as they were when captured. The returned
// map is keyed by the paths as given.
//
// This is a concrete method on ContainerdSandbox, not on the Sandbox
// interface: it is independently useful for hosts that hold a concrete
// sandbox, and promotion to the interface is a later decision (the
// StateReleaser precedent).
//
// The reader task runs concurrently with the sandbox's main task and is
// torn down before returning; the main task is untouched. A missing path
// fails the call (the workspace raced; retryable).
func (s *ContainerdSandbox) ReadFilesAt(ctx context.Context, ws WorkspaceState, paths []string) (map[string][]byte, error) {
	s.opMu.RLock()
	defer s.opMu.RUnlock()
	return s.readFilesAt(ctx, ws, paths)
}

// readDirAt lists a directory's entries at ws's committed layer, for
// opaque-dir resolution (Task 5): when a differ emits a .wh..wh..opq marker
// instead of per-file whiteouts, the parser reports the directory as opaque
// and Changes enumerates the state's contents to emit the deletes.
//
// Uses `ls -A1 --` — one name per line, no . or .., no metadata.
func (s *ContainerdSandbox) readDirAt(ctx context.Context, ws WorkspaceState, dir string) ([]string, error) {
	var names []string
	err := s.withReaderTask(ctx, ws, func(ctx context.Context, readerID string) error {
		resolved := s.resolve(dir)
		res, err := s.tasks.Exec(ctx, readerID, ExecRequest{
			Command: "sh",
			Args:    []string{"-c", `ls -A1 -- "$1"`, "croft", resolved},
			Cwd:     s.cfg.workdir(),
		})
		if err != nil {
			return fmt.Errorf("containerd sandbox: list %s at state: %w", dir, err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("containerd sandbox: list %s at state: exit %d: %s",
				dir, res.ExitCode, strings.TrimSpace(res.Stderr))
		}
		if out := res.Stdout; out != "" {
			names = strings.Split(strings.TrimSpace(out), "\n")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return names, nil
}

// Changes reports the differences between ws and the current workspace.
//
// It takes the shared operation lock: Capture/Apply/Destroy hold the
// exclusive lock, so Changes always sees a quiescent workspace and its
// reader tasks cannot be stopped mid-exec by a lifecycle op.
func (s *ContainerdSandbox) Changes(ctx context.Context, ws WorkspaceState) ([]Change, error) {
	s.opMu.RLock()
	defer s.opMu.RUnlock()
	return s.changesImpl(ctx, ws)
}

// computeChanges is the shared first half of Changes and Diff: validate,
// compute the daemon-side diff blob, read it, parse the tar, and resolve
// opaque dirs. When withContent is true, ordinary entries' content is
// buffered into the returned postImages map (Diff uses it for post-images
// without re-reading the workspace). Changes passes false — it does not
// need the content.
func (s *ContainerdSandbox) computeChanges(ctx context.Context, ws WorkspaceState, withContent bool) ([]Change, map[string][]byte, error) {
	if ws.SnapshotKey == "" {
		return nil, nil, fmt.Errorf("containerd sandbox: workspace state has no snapshot key")
	}
	if err := s.requireBackend(ctx); err != nil {
		return nil, nil, err
	}

	s.mu.Lock()
	id := s.id
	active := s.activeKey
	s.mu.Unlock()
	if id == "" || active == "" {
		return nil, nil, fmt.Errorf("containerd sandbox: Changes before Create")
	}

	// Compute the daemon-side diff blob, read it, and drop the pin. drop is
	// cancel-free, so it runs even if Read fails or the context is cancelled.
	desc, drop, err := s.diffBlob(ctx, ws)
	if err != nil {
		return nil, nil, err
	}
	blob, readErr := s.diff.Read(ctx, desc)
	drop()
	if readErr != nil {
		return nil, nil, fmt.Errorf("containerd sandbox: read diff blob: %w", readErr)
	}

	// Parse the blob into changes and opaque dirs (with content if requested).
	var changes []Change
	var opaques []opaqueDir
	var postImages map[string][]byte
	if withContent {
		changes, opaques, postImages, err = parseChangeTarWithContent(bytes.NewReader(blob), s.cfg.workdir())
	} else {
		changes, opaques, err = parseChangeTar(bytes.NewReader(blob), s.cfg.workdir())
	}
	if err != nil {
		return nil, nil, err
	}

	// Resolve opaque dirs (defensive .wh..wh..opq path): enumerate the state's
	// contents and emit a ChangeDelete for each file the opaque dir hid.
	for _, op := range opaques {
		names, err := s.readDirAt(ctx, ws, op.path)
		if err != nil {
			return nil, nil, err
		}
		for _, name := range names {
			changes = append(changes, Change{
				Path: path.Join(op.path, name),
				Kind: ChangeDelete,
			})
		}
	}

	return changes, postImages, nil
}

// changesImpl is Changes without the lock.
func (s *ContainerdSandbox) changesImpl(ctx context.Context, ws WorkspaceState) ([]Change, error) {
	changes, _, err := s.computeChanges(ctx, ws, false)
	if err != nil {
		return nil, err
	}

	// Determine ChangeAdd vs ChangeModify: the parser reports ChangeModify
	// for all ordinary entries (both adds and modifies arrive as ordinary
	// tar entries). Check existence in the lower tree — a path absent from
	// the state is an add.
	hasModifies := false
	for _, c := range changes {
		if c.Kind == ChangeModify {
			hasModifies = true
			break
		}
	}
	if hasModifies {
		err := s.withReaderTask(ctx, ws, func(ctx context.Context, readerID string) error {
			for i := range changes {
				if changes[i].Kind != ChangeModify {
					continue
				}
				resolved := s.resolve(changes[i].Path)
				res, err := s.tasks.Exec(ctx, readerID, ExecRequest{
					Command: "sh",
					Args:    []string{"-c", `test -e "$1"`, "croft", resolved},
					Cwd:     s.cfg.workdir(),
				})
				if err != nil {
					return fmt.Errorf("containerd sandbox: check %s at state: %w", changes[i].Path, err)
				}
				if res.ExitCode != 0 {
					changes[i].Kind = ChangeAdd
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}

	// Re-sort: opaque-dir deletes and add/modify reclassification may have
	// changed the order.
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })

	return changes, nil
}

// Diff returns a unified text diff between ws and the current workspace.
//
// It takes the shared operation lock: like Changes, it never interleaves
// with Capture/Apply/Destroy, so it always sees a quiescent workspace.
func (s *ContainerdSandbox) Diff(ctx context.Context, ws WorkspaceState) (string, error) {
	s.opMu.RLock()
	defer s.opMu.RUnlock()
	return s.diffImpl(ctx, ws)
}

// diffImpl is Diff without the lock.
//
// Efficiency notes: this does NOT call changesImpl. Instead it shares
// computeChanges (which parses the tar with content), then runs ONE reader
// task that does all three kinds of state-layer work — add/modify existence
// checks (test -e), delete-type classification (test -L/test -d), and
// pre-image reads (cat). The old flow created up to three separate reader
// tasks (one per concern), each paying Prepare+StartTask+StopTask+Remove.
// Post-images come from the tar itself (parseChangeTarWithContent), not from
// re-reading the workspace — saving one exec per add/modify file and
// eliminating the race between the diff computation and the post-image read.
func (s *ContainerdSandbox) diffImpl(ctx context.Context, ws WorkspaceState) (string, error) {
	// Compute the change list and extract post-images from the blob.
	changes, postImages, err := s.computeChanges(ctx, ws, true)
	if err != nil {
		return "", err
	}

	// One reader task: add/modify checks, delete-type classification, and
	// pre-image reads, all against the state's committed layer.
	preImages := make(map[string][]byte)
	needsReader := false
	for _, c := range changes {
		if c.Kind == ChangeModify || c.Kind == ChangeDelete {
			needsReader = true
			break
		}
	}
	if needsReader {
		err := s.withReaderTask(ctx, ws, func(ctx context.Context, readerID string) error {
			for i := range changes {
				c := &changes[i]
				resolved := s.resolve(c.Path)

				switch c.Kind {
				case ChangeModify:
					// Existence check: absent from the lower tree → add.
					res, err := s.tasks.Exec(ctx, readerID, ExecRequest{
						Command: "sh",
						Args:    []string{"-c", `test -e "$1"`, "croft", resolved},
						Cwd:     s.cfg.workdir(),
					})
					if err != nil {
						return fmt.Errorf("containerd sandbox: check %s at state: %w", c.Path, err)
					}
					if res.ExitCode != 0 {
						c.Kind = ChangeAdd
						continue // adds have no pre-image
					}
					// Fall through to the pre-image read below.

				case ChangeDelete:
					// Type classification: symlink → target rendering,
					// directory → marker (no pre-image read).
					res, err := s.tasks.Exec(ctx, readerID, ExecRequest{
						Command: "sh",
						Args:    []string{"-c", `test -L "$1"`, "croft", resolved},
						Cwd:     s.cfg.workdir(),
					})
					if err != nil {
						return fmt.Errorf("containerd sandbox: classify %s at state: %w", c.Path, err)
					}
					if res.ExitCode == 0 {
						c.Mode |= fs.ModeSymlink
						continue // symlinks render targets, no content read
					}
					res, err = s.tasks.Exec(ctx, readerID, ExecRequest{
						Command: "sh",
						Args:    []string{"-c", `test -d "$1"`, "croft", resolved},
						Cwd:     s.cfg.workdir(),
					})
					if err != nil {
						return fmt.Errorf("containerd sandbox: classify %s at state: %w", c.Path, err)
					}
					if res.ExitCode == 0 {
						c.IsDir = true
						continue // directories render markers, no content read
					}
					// Fall through to the pre-image read below.
				}

				// Pre-image read (ChangeModify that exists, or ChangeDelete
				// of a regular file). One cat per file.
				res, err := s.tasks.Exec(ctx, readerID, ExecRequest{
					Command: "sh",
					Args:    []string{"-c", `cat -- "$1"`, "croft", resolved},
					Cwd:     s.cfg.workdir(),
				})
				if err != nil {
					return fmt.Errorf("containerd sandbox: read %s at state: %w", c.Path, err)
				}
				if res.ExitCode != 0 {
					return fmt.Errorf("containerd sandbox: read %s at state: exit %d: %s",
						c.Path, res.ExitCode, strings.TrimSpace(res.Stderr))
				}
				preImages[c.Path] = []byte(res.Stdout)
			}
			return nil
		})
		if err != nil {
			return "", err
		}
	}

	// Re-sort: opaque-dir deletes and add/modify reclassification may have
	// changed the order.
	sort.Slice(changes, func(i, j int) bool { return changes[i].Path < changes[j].Path })

	// Render each change. Post-images come from the tar (postImages map);
	// pre-images from the reader task above.
	var out strings.Builder
	for _, c := range changes {
		if err := s.renderChange(ctx, &out, c, ws, preImages, postImages); err != nil {
			return "", err
		}
	}

	return out.String(), nil
}

// renderChange renders one change's diff section. postImages holds the
// post-image content extracted from the diff blob's tar (adds/modifies);
// preImages holds pre-images read from the state's layer.
func (s *ContainerdSandbox) renderChange(ctx context.Context, out *strings.Builder, c Change, ws WorkspaceState, preImages, postImages map[string][]byte) error {
	switch {
	case c.IsDir:
		// Directory: render a marker. Content diffs are per-file.
		switch c.Kind {
		case ChangeAdd:
			fmt.Fprintf(out, "new directory: %s\n", c.Path)
		case ChangeDelete:
			fmt.Fprintf(out, "deleted directory: %s\n", c.Path)
		}
		return nil

	case c.Mode&fs.ModeSymlink != 0:
		// Symlink: render the target as a one-line diff.
		return s.renderSymlinkChange(ctx, out, c, ws)

	default:
		// Regular file: unified diff with binary and mode-only handling.
		return s.renderFileChange(ctx, out, c, ws, preImages, postImages)
	}
}

// renderFileChange renders a regular file's diff section. Post-images come
// from the diff blob's tar (postImages), not from re-reading the workspace.
func (s *ContainerdSandbox) renderFileChange(ctx context.Context, out *strings.Builder, c Change, ws WorkspaceState, preImages, postImages map[string][]byte) error {
	var pre, post []byte
	var isAdd, isDelete bool

	switch c.Kind {
	case ChangeAdd:
		isAdd = true
		post = postImages[c.Path]
	case ChangeDelete:
		isDelete = true
		pre = preImages[c.Path]
	case ChangeModify:
		pre = preImages[c.Path]
		post = postImages[c.Path]
	}

	// Binary check: NUL in the first 8 KiB of either side.
	if isBinary(pre) || isBinary(post) {
		oldSide, newSide := "a/"+c.Path, "b/"+c.Path
		if isAdd {
			oldSide = "/dev/null"
		}
		if isDelete {
			newSide = "/dev/null"
		}
		fmt.Fprintf(out, "Binary files %s and %s differ\n", oldSide, newSide)
		return nil
	}

	// Mode-only check: content identical, mode differs.
	if c.Kind == ChangeModify && bytes.Equal(pre, post) {
		oldMode, err := s.readModeAt(ctx, ws, c.Path)
		if err != nil {
			return err
		}
		if oldMode.Perm() != c.Mode.Perm() {
			fmt.Fprintf(out, "old mode %s\nnew mode %s\n", formatMode(oldMode), formatMode(c.Mode))
			return nil
		}
		// Content and mode both identical: no output (shouldn't happen).
		return nil
	}

	// Unified diff.
	out.WriteString(renderUnified(c.Path, pre, post, isAdd, isDelete))
	return nil
}

// renderSymlinkChange renders a symlink's target as a one-line unified diff.
func (s *ContainerdSandbox) renderSymlinkChange(ctx context.Context, out *strings.Builder, c Change, ws WorkspaceState) error {
	var oldTarget, newTarget string
	var isAdd, isDelete bool

	switch c.Kind {
	case ChangeAdd:
		isAdd = true
		target, err := s.readlinkCurrent(ctx, c.Path)
		if err != nil {
			return err
		}
		newTarget = target
	case ChangeDelete:
		isDelete = true
		target, err := s.readlinkAt(ctx, ws, c.Path)
		if err != nil {
			return err
		}
		oldTarget = target
	case ChangeModify:
		var err error
		oldTarget, err = s.readlinkAt(ctx, ws, c.Path)
		if err != nil {
			return err
		}
		newTarget, err = s.readlinkCurrent(ctx, c.Path)
		if err != nil {
			return err
		}
	}

	// Build the content slices: a side with no target (delete has no new
	// side, add has no old side) is nil, not a single empty line.
	var oldContent, newContent []byte
	if oldTarget != "" {
		oldContent = []byte(oldTarget + "\n")
	}
	if newTarget != "" {
		newContent = []byte(newTarget + "\n")
	}
	out.WriteString(renderUnified(c.Path, oldContent, newContent, isAdd, isDelete))
	return nil
}

// readModeAt reads a file's mode from ws's committed layer, via ls -ld and
// symbolic-mode parsing.
func (s *ContainerdSandbox) readModeAt(ctx context.Context, ws WorkspaceState, path string) (fs.FileMode, error) {
	var mode fs.FileMode
	err := s.withReaderTask(ctx, ws, func(ctx context.Context, readerID string) error {
		resolved := s.resolve(path)
		res, err := s.tasks.Exec(ctx, readerID, ExecRequest{
			Command: "sh",
			Args:    []string{"-c", `ls -ld -- "$1"`, "croft", resolved},
			Cwd:     s.cfg.workdir(),
		})
		if err != nil {
			return fmt.Errorf("containerd sandbox: stat %s at state: %w", path, err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("containerd sandbox: stat %s at state: exit %d: %s",
				path, res.ExitCode, strings.TrimSpace(res.Stderr))
		}
		fields := strings.Fields(res.Stdout)
		if len(fields) == 0 {
			return fmt.Errorf("containerd sandbox: stat %s at state: empty ls output", path)
		}
		mode = parseSymbolicMode(fields[0])
		return nil
	})
	if err != nil {
		return 0, err
	}
	return mode, nil
}

// readlinkAt reads a symlink's target from ws's committed layer.
func (s *ContainerdSandbox) readlinkAt(ctx context.Context, ws WorkspaceState, path string) (string, error) {
	var target string
	err := s.withReaderTask(ctx, ws, func(ctx context.Context, readerID string) error {
		resolved := s.resolve(path)
		res, err := s.tasks.Exec(ctx, readerID, ExecRequest{
			Command: "sh",
			Args:    []string{"-c", `readlink -- "$1"`, "croft", resolved},
			Cwd:     s.cfg.workdir(),
		})
		if err != nil {
			return fmt.Errorf("containerd sandbox: readlink %s at state: %w", path, err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("containerd sandbox: readlink %s at state: exit %d: %s",
				path, res.ExitCode, strings.TrimSpace(res.Stderr))
		}
		target = strings.TrimSpace(res.Stdout)
		return nil
	})
	if err != nil {
		return "", err
	}
	return target, nil
}

// readlinkCurrent reads a symlink's target from the current workspace.
func (s *ContainerdSandbox) readlinkCurrent(ctx context.Context, path string) (string, error) {
	resolved := s.resolve(path)
	res, err := s.execShell(ctx, nil, `readlink -- "$1"`, resolved)
	if err != nil {
		return "", fmt.Errorf("containerd sandbox: readlink %s: %w", path, err)
	}
	return strings.TrimSpace(res.Stdout), nil
}
