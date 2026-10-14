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
	// Mode is the new mode. For ChangeDelete the permission bits stay 0,
	// but the type bit (ModeSymlink) may be set from the state's layer so
	// consumers can tell a deleted symlink from a deleted regular file;
	// deleted directories are identified by IsDir.
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

	// The tar prefix is the workdir without leading/trailing slashes:
	// entries arrive as "workspace/added.txt" for Config.Workdir
	// "/workspace" (Task 0 finding). Trim both sides so a configured
	// "/workspace/" does not produce the prefix "workspace//".
	trimmed := strings.Trim(workdir, "/")
	prefix := ""
	if trimmed != "" {
		prefix = trimmed + "/"
	}

	var changes []Change
	var opaques []opaqueDir
	var contents map[string][]byte
	// byTarName maps a tar-internal entry name to its change path, so a
	// hard-link entry (TypeLink, no body — Linkname names the archived
	// original) can resolve to the original's already-buffered content.
	var byTarName map[string]string
	if withContent {
		contents = make(map[string][]byte)
		byTarName = make(map[string]string)
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

		// Buffer the content for Diff's post-images. Regular files carry
		// their content in the entry body; symlinks carry their target in
		// Linkname (no body); hard links carry no body either — Linkname
		// names the already-archived original, whose buffered content is
		// the link's content. Both are stored so Diff can render
		// post-images without re-reading the workspace.
		if withContent {
			byTarName[name] = changePath
			switch hdr.Typeflag {
			case tar.TypeReg:
				if hdr.Size > 0 {
					content, err := io.ReadAll(tr)
					if err != nil {
						return nil, nil, nil, fmt.Errorf("containerd sandbox: read content for %s: %w", changePath, err)
					}
					contents[changePath] = content
				}
			case tar.TypeSymlink:
				contents[changePath] = []byte(hdr.Linkname)
			case tar.TypeLink:
				// Resolve through the original entry archived earlier in
				// the tar. If the original was not seen (forward
				// reference, or outside the workdir), the content stays
				// absent and the link renders as an empty add — the
				// differ this targets does not emit hard links (Task 0
				// finding), so this is defensive only.
				if target, ok := byTarName[hdr.Linkname]; ok {
					if content, ok := contents[target]; ok {
						contents[changePath] = content
						changes[len(changes)-1].Size = int64(len(content))
					}
				}
			}
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
// caller's read window. drop removes the pin and deletes the lease — it runs
// on every failure path, cancel-free, exactly like Capture's unpin-on-failure
// branches. The blob itself is NOT deleted eagerly: concurrent calls may
// compute the same blob (same state + workspace → same digest), and eager
// deletion would break the other call's Read. The lease deletion unpins the
// blob; the daemon's GC reclaims it once nothing references it.
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
		// Wrap with the namespace AND the diff lease. Without the
		// namespace, the lease manager's AddResource/DeleteResource/Delete
		// calls fail with 'namespace is required'. stateLeaseCtx only adds
		// the namespace (client.go:219); leases.WithLease then adds the
		// diff lease. The namespaceSnapshotter injects the sandbox lease
		// for snapshot operations, but the lease manager (s.leases) is the
		// raw c.LeasesService() — it needs the namespace in the context.
		ctx = leases.WithLease(s.stateLeaseCtx(ctx), leaseID)
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
	//
	// The blob is NOT deleted eagerly in drop: concurrent calls may compute
	// the same blob (same state + workspace → same digest), and eager
	// deletion would break the other call's Read. The lease deletion in
	// drop unpins the blob; the daemon's GC reclaims it once nothing
	// references it.
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
// Before Create there is no sandbox lease for namespaceSnapshotter to inject,
// so the reader snapshot would be GC-vulnerable between Prepare and
// StartTask. A per-reader lease (croft-reader-<rand>, labeled
// croft.lease:"reader") closes that window; after Create the sandbox lease
// covers the snapshot and the extra lease is skipped.
//
// The reader's init process is ALWAYS the inert sleep default (see
// taskService.StartReaderTask) — a configured Config.InitArgs initializer
// could rewrite files on the snapshot before the pre-image read.
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

	// Before Create there is no sandbox lease protecting the reader
	// snapshot. Create a per-reader lease so the GC cannot reclaim the
	// snapshot between Prepare and StartTask. After Create,
	// namespaceSnapshotter injects the sandbox lease itself (and would
	// overwrite this one anyway), so the lease is skipped.
	mgr := s.leaseManager()
	var readerLeaseID string
	if mgr != nil && s.lease.get() == "" {
		l, err := mgr.Create(s.stateLeaseCtx(ctx),
			leases.WithID("croft-reader-"+newSandboxID()),
			leases.WithLabels(map[string]string{sandboxLeaseLabel: "reader"}),
		)
		if err != nil {
			return fmt.Errorf("containerd sandbox: create reader lease: %w", err)
		}
		readerLeaseID = l.ID
		ctx = leases.WithLease(s.stateLeaseCtx(ctx), readerLeaseID)
		defer func() {
			_ = mgr.Delete(context.WithoutCancel(ctx), leases.Lease{ID: readerLeaseID})
		}()
	}

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

	if err := s.tasks.StartReaderTask(ctx, readerID, readerKey); err != nil {
		return fmt.Errorf("containerd sandbox: start reader task: %w", err)
	}
	defer func() {
		_ = s.tasks.StopTask(context.WithoutCancel(ctx), readerID)
	}()

	return fn(ctx, readerID)
}

// Type characters reported by readerType's probe.
const (
	typeAbsent  = "x" // not present in the state's layer
	typeRegular = "f" // regular file
	typeDir     = "d" // directory
	typeSymlink = "l" // symlink, including a dangling one
	typeSpecial = "s" // exists but is not regular/dir/link: FIFO, socket, device
)

// specialFileMode masks the fs.FileMode type bits that identify a special
// file — a FIFO, socket, or device node. Directories (ModeDir) and symlinks
// (ModeSymlink) have their own rendering paths and are not included.
const specialFileMode = fs.ModeNamedPipe | fs.ModeSocket | fs.ModeDevice | fs.ModeIrregular

// readerTypeScript is the single-exec type probe run against the state's
// layer. One POSIX sh command reports the path's type as one character:
// "l" symlink (checked first, so a dangling link is still a link), "d"
// directory, "f" regular file, "s" special file (exists but is not a
// regular file, directory, or link — FIFO, socket, device), "x" absent.
//
// It consolidates the existence check (test -e || test -L) and the old-side
// type classification (test -d, test -L) into ONE exec per changed path, and
// it identifies special files so the pre-image cat never runs on them:
// open(2) on a FIFO blocks until a writer appears, which would hang the
// exec until the context deadline.
const readerTypeScript = `if [ -L "$1" ]; then printf l; elif [ -d "$1" ]; then printf d; elif [ -f "$1" ]; then printf f; elif [ -e "$1" ]; then printf s; else printf x; fi`

// readerType runs the type probe against one resolved path in the reader
// task and returns one of the type* constants. Unexpected probe output is
// an error, not a classification — a broken shell must not silently report
// every path as absent (which would misrender modifies as adds).
func (s *ContainerdSandbox) readerType(ctx context.Context, readerID, resolved string) (string, error) {
	out, err := s.readerExecOut(ctx, readerID, readerTypeScript, resolved)
	if err != nil {
		return "", err
	}
	switch t := strings.TrimSpace(out); t {
	case typeAbsent, typeRegular, typeDir, typeSymlink, typeSpecial:
		return t, nil
	default:
		return "", fmt.Errorf("type probe: unexpected output %q", out)
	}
}

// readerExecOut runs a shell command against one resolved path in the reader
// task and returns its raw stdout on success, or an error carrying the exit
// code and stderr. The output is NOT trimmed — for cat it is file content
// where a trailing newline is significant; readlink call sites strip the
// newline readlink appends explicitly.
func (s *ContainerdSandbox) readerExecOut(ctx context.Context, readerID, script, resolved string) (string, error) {
	res, err := s.tasks.Exec(ctx, readerID, ExecRequest{
		Command: "sh",
		Args:    []string{"-c", script, "croft", resolved},
		Cwd:     s.cfg.workdir(),
	})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return res.Stdout, nil
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
//
// Known limitation: a filename containing a newline would split into two
// entries. This path is defensive only — the walking differ this sandbox
// targets emits per-file whiteouts, never opaque markers (Task 0 finding) —
// and the image contract (sh + cat + ls + readlink) has no NUL-delimited
// listing primitive. If a differ that emits opaque markers ever becomes the
// target, switch the protocol (e.g. shell globbing, which preserves such
// names).
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
	// Paths already represented in the change list — modified or explicitly
	// whiteouted by the upper tree — are skipped: an opaque marker hides the
	// lower entries, but the upper tar can replace the same names, and a
	// synthesized delete alongside a modify would be contradictory.
	existing := make(map[string]bool, len(changes))
	for _, c := range changes {
		existing[c.Path] = true
	}
	for _, op := range opaques {
		names, err := s.readDirAt(ctx, ws, op.path)
		if err != nil {
			return nil, nil, err
		}
		for _, name := range names {
			p := path.Join(op.path, name)
			if existing[p] {
				continue
			}
			changes = append(changes, Change{Path: p, Kind: ChangeDelete})
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

	// Classify in the lower tree with one type-probe exec per path
	// (readerTypeScript): ChangeModify → ChangeAdd when the old side is
	// absent (a dangling symlink probes "l", so it stays a modify);
	// ChangeDelete → set IsDir / ModeSymlink from the old type so consumers
	// know what was deleted (permission bits stay 0 for deletes; a deleted
	// special file stays untyped — the Change contract distinguishes only
	// directories and symlinks for deletes).
	hasWork := false
	for _, c := range changes {
		if c.Kind == ChangeModify || c.Kind == ChangeDelete {
			hasWork = true
			break
		}
	}
	if hasWork {
		err := s.withReaderTask(ctx, ws, func(ctx context.Context, readerID string) error {
			for i := range changes {
				c := &changes[i]
				resolved := s.resolve(c.Path)
				switch c.Kind {
				case ChangeModify:
					old, err := s.readerType(ctx, readerID, resolved)
					if err != nil {
						return fmt.Errorf("containerd sandbox: check %s at state: %w", c.Path, err)
					}
					if old == typeAbsent {
						c.Kind = ChangeAdd
					}
				case ChangeDelete:
					old, err := s.readerType(ctx, readerID, resolved)
					if err != nil {
						return fmt.Errorf("containerd sandbox: classify %s at state: %w", c.Path, err)
					}
					switch old {
					case typeSymlink:
						c.Mode |= fs.ModeSymlink
					case typeDir:
						c.IsDir = true
					}
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
// task that does all three kinds of state-layer work — old-side type probes
// (one exec classifying existence and type: link/dir/regular/special),
// delete-type classification (same probe), and pre-image reads (cat, only
// ever on a probed regular file). The old flow created up to three separate
// reader tasks (one per concern), each paying Prepare+StartTask+StopTask+
// Remove, and later iterations paid two-to-four separate test execs per
// changed path. Post-images come from the tar itself
// (parseChangeTarWithContent), not from re-reading the workspace — saving
// one exec per add/modify file and eliminating the race between the diff
// computation and the post-image read.
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
					// One probe classifies the old side: existence AND type
					// in a single exec. Absent from the state's layer → add.
					// A dangling symlink probes "l" (the probe checks -L
					// first), so it stays a modify.
					old, err := s.readerType(ctx, readerID, resolved)
					if err != nil {
						return fmt.Errorf("containerd sandbox: check %s at state: %w", c.Path, err)
					}
					if old == typeAbsent {
						c.Kind = ChangeAdd
						continue // adds have no pre-image
					}

					switch {
					case c.IsDir:
						// Directories have no content to read — they render
						// as markers (or nothing, for modifies).
						continue

					case c.Mode&fs.ModeSymlink != 0:
						// New side is a symlink. The old target comes from
						// the state's layer — but only if the old side was
						// also a symlink (file→symlink replacements have no
						// old target; they render add-like).
						if old == typeSymlink {
							target, err := s.readerExecOut(ctx, readerID, `readlink -- "$1"`, resolved)
							if err != nil {
								return fmt.Errorf("containerd sandbox: readlink %s at state: %w", c.Path, err)
							}
							// Strip only the newline readlink appends.
							preImages[c.Path] = []byte(strings.TrimSuffix(target, "\n"))
						}
						continue

					case c.Mode&specialFileMode != 0:
						// New side is a special file (FIFO or device —
						// sockets do not survive on overlayfs): neither
						// side has readable content, and cat on the old
						// side could block (FIFO) or lie (device). It
						// renders as a type marker.
						continue

					case old != typeRegular:
						// Type replacement (old side directory, symlink, or
						// special file → new side regular file): no
						// pre-image is readable — cat would fail (directory),
						// read the referent instead of the link (symlink),
						// or block in open(2) (FIFO/device). The pre-image
						// stays absent and renderFileChange renders the
						// replacement add-like.
						continue
					}
					// Old side is a regular file: fall through to the read.

				case ChangeDelete:
					// One probe classifies the deleted path's old type so
					// rendering can route it: symlink → target line,
					// directory → marker, special file → delete-like with
					// no content (cat on a FIFO would block), regular file
					// → content diff.
					old, err := s.readerType(ctx, readerID, resolved)
					if err != nil {
						return fmt.Errorf("containerd sandbox: classify %s at state: %w", c.Path, err)
					}
					switch old {
					case typeSymlink:
						c.Mode |= fs.ModeSymlink
						target, err := s.readerExecOut(ctx, readerID, `readlink -- "$1"`, resolved)
						if err != nil {
							return fmt.Errorf("containerd sandbox: readlink %s at state: %w", c.Path, err)
						}
						// Strip only the newline readlink appends.
						preImages[c.Path] = []byte(strings.TrimSuffix(target, "\n"))
						continue
					case typeDir:
						c.IsDir = true
						continue // directories render markers, no content read
					case typeSpecial:
						continue // no content to read; renders delete-like
					}
					// Regular file — or, defensively, absent: the cat below
					// fails loudly, the documented missing-path contract.
					// Fall through to the pre-image read.
				}

				// Pre-image read — only ever on a path the probe reported as a
				// regular file (or, for deletes, defensively on an absent path,
				// where the cat failure below is the documented missing-path
				// contract). One cat per file. A read failure fails the whole
				// call — the plan's documented race contract (Task 4's
				// missing-path rule): retryable, and one silent empty pre-image
				// would render a delete-as-nothing or a modify-as-add, which is
				// worse.
				content, err := s.readerExecOut(ctx, readerID, `cat -- "$1"`, resolved)
				if err != nil {
					return fmt.Errorf("containerd sandbox: read %s at state: %w", c.Path, err)
				}
				preImages[c.Path] = []byte(content)
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
		// Symlink: render the target as a one-line diff. Pure — both
		// targets are already resolved (pre from the reader task, post
		// from the tar's Linkname).
		renderSymlinkChange(out, c, preImages, postImages)
		return nil

	case c.Mode&specialFileMode != 0:
		// Special file (FIFO, device, socket): no content to diff on
		// either side. Render a type marker, the way directories do.
		// Deletes never carry these bits (delete classification sets
		// only ModeSymlink/IsDir), so this covers adds and modifies.
		switch c.Kind {
		case ChangeAdd, ChangeModify:
			fmt.Fprintf(out, "new special file: %s\n", c.Path)
		}
		return nil

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
		post = postImages[c.Path]
		var hadPre bool
		pre, hadPre = preImages[c.Path]
		if !hadPre {
			// Type replacement: the old side was a directory, symlink, or
			// special file whose pre-image the reader task deliberately did
			// not read, so the new side is rendered add-like (/dev/null on
			// the old side). Without this, a replacement by an EMPTY
			// regular file leaves both pre and post nil: bytes.Equal would
			// treat the change as content-identical and the mode-only
			// branch below would silently drop it (modes equal) or render
			// a bogus mode pair with no content (modes differ).
			isAdd = true
		}
	}

	// Mode-only check: content identical, mode differs. Check BEFORE the
	// binary check — a chmod-only change on a binary file should render as
	// a mode pair, not "Binary files differ". Type replacements (isAdd set
	// above) never take this branch: their pre-image is absent, not empty.
	if c.Kind == ChangeModify && !isAdd && bytes.Equal(pre, post) {
		oldMode, err := s.readModeAt(ctx, ws, c.Path)
		if err != nil {
			return err
		}
		if effectivePerm(oldMode) != effectivePerm(c.Mode) {
			writeUnifiedHeaders(out, c.Path, false, false)
			fmt.Fprintf(out, "old mode %s\nnew mode %s\n", formatMode(oldMode), formatMode(c.Mode))
			return nil
		}
		// Content and mode both identical: no output (shouldn't happen).
		return nil
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

	// Unified diff.
	out.WriteString(renderUnified(c.Path, pre, post, isAdd, isDelete))
	return nil
}

// renderSymlinkChange renders a symlink's target as a one-line unified diff.
// Pure: post-image targets come from the diff blob's tar (postImages,
// captured from Linkname during parsing); pre-image targets come from
// preImages, read by the diffImpl reader task (readlink for symlinks on both
// sides). Neither side re-reads the workspace, so no exec can race the
// rendering. A missing old target (add, or a file→symlink type replacement)
// renders add-like; a missing new target (delete) renders delete-like.
func renderSymlinkChange(out *strings.Builder, c Change, preImages, postImages map[string][]byte) {
	var oldTarget, newTarget string
	var isAdd, isDelete bool

	switch c.Kind {
	case ChangeAdd:
		isAdd = true
		newTarget = string(postImages[c.Path])
	case ChangeDelete:
		isDelete = true
		oldTarget = string(preImages[c.Path])
	case ChangeModify:
		oldTarget = string(preImages[c.Path])
		newTarget = string(postImages[c.Path])
	}

	// Build the content slices: a side with no target (delete has no new
	// side, add has no old side, type replacement has no old side) is nil,
	// not a single empty line.
	var oldContent, newContent []byte
	if oldTarget != "" {
		oldContent = []byte(oldTarget + "\n")
	}
	if newTarget != "" {
		newContent = []byte(newTarget + "\n")
	}
	out.WriteString(renderUnified(c.Path, oldContent, newContent, isAdd, isDelete))
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
