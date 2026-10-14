# croft

A croft is a small enclosed plot of land one party works on its own. This
library gives a program the same thing: an isolated, snapshot-backed workspace
on [containerd](https://containerd.io)'s overlayfs snapshotter — provisioned
from an image, worked through exec and file I/O, and checkpointed into states
that outlive the sandbox that captured them.

Croft is Linux-only (OverlayFS, a containerd daemon, root or user namespaces)
and is deliberately not a container runtime: it is the library a host uses
when it wants a real filesystem boundary per workspace, with cheap
checkpoints, rollback, and forks over that boundary.

## What it does

```
Create       resolve the image rootfs, prepare the base snapshot, start the task
Exec,
ReadFile,
WriteFile    work the workspace through the boundary
Capture      commit the active layer, pin it as a state, continue on a fresh layer
Apply        resume this (or any) sandbox from a captured state
Changes      list the differences between a state and the current workspace
Diff         render those differences as a unified text diff
ReleaseState drop a state's pin; the daemon's GC reclaims the layer
Destroy      stop the task, remove the active layer; committed states survive
```

The properties, honestly:

- **Layered** — every Capture commits an immutable overlayfs layer and
  continues on a new one; lineage is recorded in the state.
- **States outlive sandboxes** — a captured state survives Destroy, crashes,
  and process restarts, and any equally configured sandbox can resume it.
- **Rollback** — Apply rebinds a sandbox to any captured state, exactly and
  O(1) (a re-point, not a replay).
- **Fork** — there is no fork orchestration; Capture on the parent plus Apply
  on the child is the whole recipe, at layer-sharing cost.
- **Serialized** — lifecycle operations are exclusive, workspace operations
  shared: an in-flight Exec finishes before a Capture commits the layer it is
  writing to, by construction.
- **A state's identity is its snapshot key** — sequence-derived, so two
  captures of identical content are distinct states. Identity, not content
  equality; croft has no digest, and a host that wants a record identity
  hashes the three-field struct itself.

## Diffing states

Two states can be compared without applying either. `Changes` returns the
sorted difference between a captured state and the current workspace as a
`[]Change` (add / modify / delete, with mode and size); `Diff` renders that
difference as a unified text diff — `---`/`+++` headers, `@@` hunks, binary
files as "Binary files … differ", mode-only changes as `old mode`/`new mode`
pairs, symlinks as target lines. Both are read-only: they never stop or
restart the task, never commit, never write.

```go
state, _ := sb.Capture(ctx)
// ... work happens ...
changes, _ := sb.Changes(ctx, state)  // what differs?
text, _ := sb.Diff(ctx, state)        // unified diff, for display
files, _ := sb.ReadFilesAt(ctx, state, []string{"config.yaml"}) // pre-images
```

`ReadFilesAt` is a concrete method on `ContainerdSandbox` (not on the
`Sandbox` interface): it reads files from a state's committed layer without
applying it, via a throwaway reader task. A state whose layer has been
released makes `Changes`, `Diff`, and `ReadFilesAt` return
`ErrStateUnavailable`.

## Quick start

```go
sb := croft.New(croft.Config{
    Namespace: "croft",
    Image:     "docker.io/library/golang:1.26",
})
if err := sb.Create(ctx); err != nil { ... }   // the caller's ctx bounds it
defer sb.Destroy(ctx)

res, _ := sb.Exec(ctx, croft.ExecRequest{Command: "go", Args: []string{"version"}})
state, _ := sb.Capture(ctx)   // pinned against the daemon's GC
_ = sb.Apply(ctx, state)      // resume from it — rollback, crash-resume, fork
_ = sb.ReleaseState(ctx, state) // disk comes back when the caller says so
```

## The image contract

The workspace's file operations shell out inside the container, so the image
must provide a shell (`/bin/sh` — also the keep-alive command's default) and
the small coreutils croft itself uses: `mkdir` (Create provisions the
workdir), and `cat`, `dirname`, `printf`, `base64`, and `chmod` (ReadFile and
WriteFile). The diff machinery adds `ls` and `readlink` (reader tasks read
pre-images, enumerate directories, and resolve symlink targets from a
captured state). BusyBox and coreutils both carry all of them.

## Scale envelope

Workspaces are source-tree-sized. `ReadFile` and `WriteFile` buffer whole
files; `WriteFile` costs roughly one container exec per 512 KiB (configurable
via `WriteChunkBytes`) and carries its payload base64-encoded in exec
arguments, visible in `/proc/*/cmdline` while the exec runs — so large
artifacts are out of the design scope and secrets never travel through
`WriteFile`.

## Configuration

`croft.Config` carries the containerd address, namespace, image, runtime,
snapshotter, init command, in-container workdir, write chunking, and the
client-side FIFO directory (set the last to a user-writable path to drive
containerd without root). See `Config` in `croft.go` for the full contract of
each field.

## State format

`WorkspaceState` is a plain three-field record — `SnapshotKey`, `ParentKey`,
`Image` — so it can be stored, logged, and handed back by value. Daemon-visible
identifiers: snapshot keys use the `croft/` prefix; leases and containers are
labeled `croft.lease` and `croft.sandbox`; the states lease is `croft-states`.

## Testing

```sh
go test ./...          # unit suite; no daemon required

# live suite, against a real containerd (root is required for the socket):
sudo env CROFT_CONTAINERD_ADDR=/run/containerd/containerd.sock \
  go test -count=1 -v -run TestLive_ ./...
```

Croft's direct dependencies beyond the Go standard library are
`github.com/containerd/containerd/v2` (the daemon client),
`github.com/sergi/go-diff` (the Myers diff engine), and the
`github.com/opencontainers/*` spec packages (`go-digest`, `image-spec`,
`runtime-spec`). What
the live suite verifies — and how to run it against a real daemon — is in
[docs/verification.md](docs/verification.md); how the pieces fit together is
in [docs/overview.md](docs/overview.md).

## License

MIT — see [LICENSE](LICENSE).
